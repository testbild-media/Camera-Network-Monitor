package dahua

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cnm/internal/provider"
)

// Playback channel numbering. Dahua firmware numbers RTSP playback channels
// from 1, like the live channels. Some firmware (seen on 4-channel
// recorders) counts them from 0 instead: channel=N plays camera N+1,
// channel=0 is refused with 401 and channel=<count> does not exist (404).
// On such devices cameras 2…N play over RTSP with the shifted number and
// camera 1 over the CGI download (loadfile.cgi, also counting from 0), which
// only delivers whole recording files: CNM skips to the start time itself.
const (
	pbUnknown = iota
	pbFromOne
	pbFromZero
)

const rtspPlaybackTime = "2006_01_02_15_04_05"

// playbackQuery is the query of /cam/playback for a channel number as the
// device expects it.
func (d *rpcDriver) playbackQuery(num int, from, to time.Time) string {
	return "channel=" + strconv.Itoa(num) + "&subtype=0&starttime=" + d.clock.ToDevice(from).Format(rtspPlaybackTime) +
		"&endtime=" + d.clock.ToDevice(to).Format(rtspPlaybackTime)
}

// playbackNumbering detects (once per connection) how the device numbers
// its RTSP playback channels: the last camera with recordings is asked for
// one of its recordings under the usual number. Firmware that counts from 0
// has no such channel (404). Undecided results are not remembered.
func (d *rpcDriver) playbackNumbering(ctx context.Context) int {
	d.pbMu.Lock()
	defer d.pbMu.Unlock()
	if d.pbBase != pbUnknown {
		return d.pbBase
	}
	n := d.channels
	if n <= 0 {
		n = d.cfg.Channels
	}
	if !d.env.AllowLogin() {
		return pbFromOne
	}
	now := time.Now()
	for cam := n - 1; cam >= 0; cam-- {
		rec, ok, err := d.recentRecording(ctx, cam, now)
		if err != nil {
			return pbFromOne
		}
		if !ok {
			continue
		}
		from := rec.Start.Add(5 * time.Second)
		if rec.End.Sub(rec.Start) < 20*time.Second {
			from = rec.Start
		}
		to := from.Add(10 * time.Second)
		if to.After(rec.End) {
			to = rec.End
		}
		status, err := provider.RTSPDescribe(ctx, d.cfg.Host, d.cfg.Port("rtsp"), d.cfg.Username, d.cfg.Password,
			"/cam/playback?"+d.playbackQuery(cam+1, from, to))
		switch {
		case err != nil:
			return pbFromOne
		case status == 200:
			d.pbBase = pbFromOne
		case status == 404:
			d.pbBase = pbFromZero
		default:
			return pbFromOne
		}
		return d.pbBase
	}
	return pbFromOne // no recordings anywhere: nothing to play
}

// recordingFile is one entry of the device's recording list (device wall clock).
type recordingFile struct {
	start, end time.Time
}

// listFiles returns up to max recording files of a channel overlapping
// [from, to] (device wall clock), oldest first.
func (d *rpcDriver) listFiles(ctx context.Context, channel int, from, to time.Time, max int) ([]recordingFile, error) {
	r, err := d.rpc.call(ctx, "mediaFileFind.factory.create", nil, nil)
	if err != nil {
		return nil, err
	}
	var object int64
	if err := json.Unmarshal(r.Result, &object); err != nil || object == 0 {
		return nil, provider.NewError(provider.KindProtocol, err, "Aufnahmesuche: Suchobjekt konnte nicht erstellt werden")
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = d.rpc.call(cctx, "mediaFileFind.close", nil, object)
		_, _ = d.rpc.call(cctx, "mediaFileFind.destroy", nil, object)
	}()
	r, err = d.rpc.call(ctx, "mediaFileFind.findFile", map[string]any{"condition": map[string]any{
		"Channel": channel, "StartTime": from.Format(dahuaTime), "EndTime": to.Format(dahuaTime),
		"Types": []string{"dav"}, "Dirs": []string{}, "Flags": []string{"Timing", "Event", "Manual", "Marker"}, "VideoStream": "Main",
	}}, object)
	if err != nil || !r.ok() {
		return nil, err
	}
	var out []recordingFile
	for len(out) < max {
		count := min(max-len(out), 100)
		r, err := d.rpc.call(ctx, "mediaFileFind.findNextFile", map[string]any{"count": count}, object)
		if err != nil {
			return nil, err
		}
		var p struct {
			Infos []struct {
				StartTime string `json:"StartTime"`
				EndTime   string `json:"EndTime"`
			} `json:"infos"`
		}
		if !r.ok() || decodeParams(r, &p) != nil || len(p.Infos) == 0 {
			break
		}
		for _, inf := range p.Infos {
			s, e1 := parseDahuaTime(inf.StartTime)
			e, e2 := parseDahuaTime(inf.EndTime)
			if e1 == nil && e2 == nil && e.After(s) {
				out = append(out, recordingFile{start: s, end: e})
			}
		}
		if len(p.Infos) < count {
			break
		}
	}
	return out, nil
}

// recentRecording finds a finished recording of a channel, preferring recent
// ones (old files may be overwritten while CNM looks at them).
func (d *rpcDriver) recentRecording(ctx context.Context, channel int, now time.Time) (provider.Recording, bool, error) {
	end := d.clock.ToDevice(now.Add(-2 * time.Minute))
	for _, back := range []time.Duration{2 * time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
		files, err := d.listFiles(ctx, channel, end.Add(-back), end, 1)
		if err != nil {
			return provider.Recording{}, false, err
		}
		if len(files) > 0 {
			f := files[0]
			return provider.Recording{Start: d.clock.FromDevice(f.start), End: d.clock.FromDevice(f.end)}, true, nil
		}
	}
	return provider.Recording{}, false, nil
}

// ---- CGI download (loadfile.cgi) --------------------------------------------------------------

// httpSkipTimeout bounds how long CNM may read a recording file up to the
// requested start time.
const httpSkipTimeout = 2 * time.Minute

// openHTTPPlayback plays [from, to] of a camera from the CGI download: the
// recording files overlapping the range are fetched one after another and
// cut to the range by the frame times.
func (d *rpcDriver) openHTTPPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	devFrom, devTo := d.clock.ToDevice(from), d.clock.ToDevice(to)
	files, err := d.listFiles(ctx, channel, devFrom, devTo, 200)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, provider.NewError(provider.KindNotFound, nil, "keine Aufnahme im gewählten Zeitraum")
	}
	pctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	hc := &http.Client{Transport: &provider.DigestTransport{User: d.cfg.Username, Pass: d.cfg.Password,
		Base: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 20 * time.Second, DisableCompression: true}}}
	go func() {
		// frame times have whole seconds: start with the keyframe of from's second
		cut := &dhavCut{from: devFrom.Truncate(time.Second), to: devTo}
		for _, f := range files {
			start := f.start
			if devFrom.After(start) {
				start = devFrom
			}
			end := f.end
			if devTo.Before(end) {
				end = devTo
			}
			done, err := d.downloadFile(pctx, hc, channel, start, end, cut, pw)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if done {
				break
			}
		}
		pw.Close()
	}()
	return &provider.PlaybackSource{Reader: &httpPlayback{pr: pr, cancel: cancel}, Format: "dhav", StartupTimeout: httpSkipTimeout}, nil
}

type httpPlayback struct {
	pr     *io.PipeReader
	cancel context.CancelFunc
}

func (p *httpPlayback) Read(b []byte) (int, error) { return p.pr.Read(b) }

func (p *httpPlayback) Close() error {
	p.cancel()
	return p.pr.Close()
}

// loadfileTime formats a device time for loadfile.cgi: the firmware reads
// the space only as %20 (a "+" or encoded colons make it pick another file).
func loadfileTime(t time.Time) string {
	return strings.ReplaceAll(t.Format(dahuaTime), " ", "%20")
}

// downloadFile streams one recording file through the cutter; done reports
// that the end of the range was reached.
func (d *rpcDriver) downloadFile(ctx context.Context, hc *http.Client, channel int, start, end time.Time, cut *dhavCut, w io.Writer) (bool, error) {
	u := "http://" + provider.HostPort(d.cfg.Host, d.cfg.Port("http")) + "/cgi-bin/loadfile.cgi?action=startLoad&channel=" +
		strconv.Itoa(channel) + "&startTime=" + loadfileTime(start) + "&endTime=" + loadfileTime(end) + "&subtype=0"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return false, provider.ClassifyNetErr(err, "Aufnahme-Download")
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return false, provider.NewError(provider.KindAuth, nil, "Aufnahme-Download: Anmeldung abgelehnt")
	case res.StatusCode != http.StatusOK:
		return false, provider.NewError(provider.KindProtocol, nil, "Aufnahme-Download: HTTP %d", res.StatusCode)
	}
	return cut.copy(bufio.NewReaderSize(res.Body, 1<<20), w)
}

// ---- DHAV cutting -----------------------------------------------------------------------------

// DHAV frame: "DHAV" type(1) sub(1) channel(1) subnumber(1) number(4)
// length(4, whole frame) date(4, packed wall clock) ms(2) extLen(1) sum(1)
// ext payload "dhav" length(4).
const (
	dhavHdr    = 24
	dhavIFrame = 0xFD
	dhavPFrame = 0xFC
	dhavBFrame = 0xFB
	dhavMaxGOP = 64 << 20
)

// dhavCut passes the frames of [from, to] (device wall clock, whole
// seconds): it starts with the keyframe at or before from and ends after the
// last video frame not later than to. It keeps its state across files.
type dhavCut struct {
	from, to time.Time
	started  bool
	gop      [][]byte
	gopBytes int
}

func isDHAVVideo(t byte) bool { return t == dhavIFrame || t == dhavPFrame || t == dhavBFrame }

// copy reads DHAV frames from r and writes the selected ones to w. It
// resynchronises on the next "DHAV" after damaged data. done is true once a
// video frame later than the range end was seen.
func (c *dhavCut) copy(r *bufio.Reader, w io.Writer) (done bool, err error) {
	for {
		hdr, err := r.Peek(dhavHdr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return false, nil
			}
			return false, provider.ClassifyNetErr(err, "Aufnahme-Download")
		}
		if string(hdr[:4]) != "DHAV" {
			if _, err := r.Discard(1); err != nil {
				return false, nil
			}
			continue
		}
		n := int(binary.LittleEndian.Uint32(hdr[12:16]))
		if n < dhavHdr || n > dhavMaxGOP {
			_, _ = r.Discard(4)
			continue
		}
		typ := hdr[4]
		date := unpackTime(binary.LittleEndian.Uint32(hdr[16:20]))
		frame := make([]byte, n)
		if _, err := io.ReadFull(r, frame); err != nil {
			return false, nil // truncated last frame
		}
		out, done := c.push(typ, date, frame)
		for _, f := range out {
			if _, err := w.Write(f); err != nil {
				return true, nil // reader closed: stop quietly
			}
		}
		if done {
			return true, nil
		}
	}
}

// push decides about one frame; it returns the frames to write.
func (c *dhavCut) push(typ byte, date time.Time, frame []byte) ([][]byte, bool) {
	video := isDHAVVideo(typ)
	if c.started {
		if video && date.After(c.to) {
			return nil, true
		}
		return [][]byte{frame}, false
	}
	switch {
	case typ == dhavIFrame:
		c.gop, c.gopBytes = [][]byte{frame}, len(frame)
	case video && c.gop != nil:
		c.gop = append(c.gop, frame)
		c.gopBytes += len(frame)
		if c.gopBytes > dhavMaxGOP {
			c.gop, c.gopBytes = nil, 0
		}
	default:
		return nil, false // audio/aux before the start, or no keyframe yet
	}
	if date.Before(c.from) {
		return nil, false
	}
	if date.After(c.to) {
		return nil, true // the range lies in a gap of this file
	}
	c.started = true
	out := c.gop
	c.gop, c.gopBytes = nil, 0
	return out, false
}
