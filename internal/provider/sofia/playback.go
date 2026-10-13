package sofia

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"cnm/internal/provider"
	"cnm/internal/tsmux"
)

// Recording search and historical playback. The sequence was verified on
// real NETSurveillance hardware (Xiongmai AHB7004T-MHV2, V4.02.R11) with
// CNM v0.3.25; the single-socket variant of the public reference
// implementations closes the control connection on that firmware:
//
//	OPFileQuery (1440 → 1441) on the control connection, max. 64 files per
//	answer, Ret 119 = no files; page on with the last BeginTime
//	media connection: OPPlayBack Claim (1424 → 1425), never logged in itself,
//	only the control session id
//	control connection: OPPlayBack DownloadStart (1420 → 1421)
//	media connection: 1426 chunks with the live frame layout, an empty chunk
//	ends the file
//	control connection: OPPlayBack DownloadStop (1420)
//
// Playback is file based (PlayMode ByName); CNM skips to the requested
// instant using the wall clock in the I-frame headers and chains the
// following files until the end of the requested range.

// Message ids for recordings and time.
const (
	MsgPlayBack         = 1420 // DownloadStart / DownloadStop
	MsgPlayBackRsp      = 1421
	MsgPlayBackClaim    = 1424
	MsgPlayBackClaimRsp = 1425
	MsgPlayBackData     = 1426
	MsgFileQuery        = 1440
	MsgFileQueryRsp     = 1441
	MsgTimeQuery        = 1452
	MsgTimeQueryRsp     = 1453
)

const (
	dvripTime     = "2006-01-02 15:04:05"
	retNoFiles    = 119 // also 109 on some firmware
	filesPerPage  = 64
	maxQueryPages = 32
)

// recFile is one recorded file (times are device wall clock, location UTC).
type recFile struct {
	Name  string
	Start time.Time
	End   time.Time
}

// UnpackTime decodes the packed wall clock of DVRIP/Dahua frame headers:
// seconds (6 bit), minutes (6), hours (5), day (5), month (4), year-2000 (6).
func UnpackTime(v uint32) (time.Time, bool) {
	if v == 0 {
		return time.Time{}, false
	}
	t := time.Date(2000+int(v>>26&0x3F), time.Month(v>>22&0x0F), int(v>>17&0x1F), int(v>>12&0x1F), int(v>>6&0x3F), int(v&0x3F), 0, time.UTC)
	return t, t.Month() >= 1 && t.Month() <= 12
}

// PackTime is the inverse of UnpackTime (used by the simulator and tests).
func PackTime(t time.Time) uint32 {
	return uint32(t.Year()-2000)<<26 | uint32(t.Month())<<22 | uint32(t.Day())<<17 | uint32(t.Hour())<<12 | uint32(t.Minute())<<6 | uint32(t.Second())
}

// DeviceTime reads the device wall clock (OPTimeQuery).
func (c *Control) DeviceTime(ctx context.Context) (time.Time, error) {
	r, err := c.Request(ctx, MsgTimeQuery, MsgTimeQueryRsp, map[string]any{"Name": "OPTimeQuery", "SessionID": SessionString(c.session)})
	if err != nil {
		return time.Time{}, err
	}
	if !r.OK() {
		return time.Time{}, provider.NewError(provider.KindProtocol, nil, "OPTimeQuery abgelehnt (Code %d)", r.Ret())
	}
	t, err := time.Parse(dvripTime, strings.TrimSpace(str(r["OPTimeQuery"])))
	if err != nil {
		return time.Time{}, provider.NewError(provider.KindProtocol, err, "OPTimeQuery: unbekanntes Zeitformat")
	}
	return t, nil
}

// queryFiles lists recorded files of a channel (zero based) overlapping
// [from, to] (device wall clock). pages limits the number of requests.
func (c *Control) queryFiles(ctx context.Context, channel int, from, to time.Time, pages int) ([]recFile, error) {
	var out []recFile
	seen := map[string]bool{}
	cursor := from
	for page := 0; page < pages; page++ {
		r, err := c.Request(ctx, MsgFileQuery, MsgFileQueryRsp, map[string]any{
			"Name":      "OPFileQuery",
			"SessionID": SessionString(c.session),
			"OPFileQuery": map[string]any{
				"BeginTime":      cursor.Format(dvripTime),
				"EndTime":        to.Format(dvripTime),
				"Channel":        channel,
				"DriverTypeMask": "0x0000FFFF",
				"Event":          "*",
				"StreamType":     "0",
				"Type":           "h264",
			},
		})
		if err != nil {
			return nil, err
		}
		if r.Ret() == retNoFiles || r.Ret() == 109 {
			break
		}
		if !r.OK() {
			return nil, provider.NewError(provider.KindProtocol, nil, "OPFileQuery abgelehnt (Code %d)", r.Ret())
		}
		rows, _ := r["OPFileQuery"].([]any)
		var last time.Time
		for _, row := range rows {
			m, _ := row.(map[string]any)
			if m == nil {
				continue
			}
			s, e1 := time.Parse(dvripTime, str(m["BeginTime"]))
			e, e2 := time.Parse(dvripTime, str(m["EndTime"]))
			name := str(m["FileName"])
			if e1 != nil || e2 != nil || name == "" {
				continue
			}
			last = s
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, recFile{Name: name, Start: s, End: e})
		}
		if len(rows) < filesPerPage || !last.After(cursor) {
			break
		}
		cursor = last
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// ---- driver integration ---------------------------------------------------------------------

func (d *driver) control() (*Control, error) {
	d.mu.Lock()
	ctl := d.ctl
	d.mu.Unlock()
	if ctl == nil {
		return nil, provider.NewError(provider.KindNetwork, nil, "Gerät nicht verbunden")
	}
	select {
	case <-ctl.Done():
		return nil, provider.NewError(provider.KindNetwork, ctl.Err(), "Steuerverbindung getrennt")
	default:
	}
	return ctl, nil
}

// syncTime measures the device clock offset (best effort).
func (d *driver) syncTime(ctx context.Context, ctl *Control) {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if wall, err := ctl.DeviceTime(tctx); err == nil {
		d.clock.Set(provider.MeasureOffset(wall, time.Now()))
	}
}

// SearchRecordings lists recorded ranges of a channel (zero based).
func (d *driver) SearchRecordings(ctx context.Context, channel int, from, to time.Time) ([]provider.Recording, error) {
	ctl, err := d.control()
	if err != nil {
		return nil, err
	}
	files, err := ctl.queryFiles(ctx, channel, d.clock.ToDevice(from), d.clock.ToDevice(to), maxQueryPages)
	if err != nil {
		return nil, err
	}
	recs := make([]provider.Recording, 0, len(files))
	for _, f := range files {
		recs = append(recs, provider.Recording{Start: d.clock.FromDevice(f.Start), End: d.clock.FromDevice(f.End)})
	}
	return provider.MergeRecordings(recs, from, to, 2*time.Second), nil
}

// HasRecordings answers with a single query page.
func (d *driver) HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error) {
	ctl, err := d.control()
	if err != nil {
		return false, err
	}
	files, err := ctl.queryFiles(ctx, channel, d.clock.ToDevice(from), d.clock.ToDevice(to), 1)
	return len(files) > 0, err
}

// OpenPlayback streams [from, to] of a channel as MPEG-TS.
func (d *driver) OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	ctl, err := d.control()
	if err != nil {
		return nil, err
	}
	devFrom, devTo := d.clock.ToDevice(from), d.clock.ToDevice(to)
	// the file containing `from` may have started up to a few hours earlier
	files, err := ctl.queryFiles(ctx, channel, devFrom.Add(-6*time.Hour), devTo, maxQueryPages)
	if err != nil {
		return nil, err
	}
	var todo []recFile
	for _, f := range files {
		if f.End.After(devFrom) && f.Start.Before(devTo) {
			todo = append(todo, f)
		}
	}
	if len(todo) == 0 {
		return nil, provider.NewError(provider.KindNotFound, nil, "keine Aufnahme im gewählten Zeitraum")
	}
	pb := &playback{ctl: ctl, host: d.cfg.Host, port: d.port(), channel: channel, from: devFrom, to: devTo}
	first, err := pb.open(ctx, todo[0])
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	pb.pr = pr
	go pb.run(first, todo, pw)
	return &provider.PlaybackSource{Reader: pb, Format: "mpegts"}, nil
}

// ---- playback session ----------------------------------------------------------------------

type playback struct {
	ctl      *Control
	host     string
	port     int
	channel  int
	from, to time.Time // device wall clock

	pr     *io.PipeReader
	mu     sync.Mutex
	cur    *fileStream
	closed bool
}

type fileStream struct {
	conn  net.Conn
	rd    *bufio.Reader
	param map[string]any
	file  recFile
	start time.Time // requested start inside the file (ByName honours it to the second)
	once  sync.Once
}

func (p *playback) Read(b []byte) (int, error) { return p.pr.Read(b) }

// Close stops the transfer on the device and releases the connections.
func (p *playback) Close() error {
	p.mu.Lock()
	p.closed = true
	cur := p.cur
	p.mu.Unlock()
	p.pr.Close()
	if cur != nil {
		p.stop(cur)
	}
	return nil
}

func (p *playback) cmd(action string, fs *fileStream) map[string]any {
	return map[string]any{
		"Name":      "OPPlayBack",
		"SessionID": SessionString(p.ctl.Session()),
		"OPPlayBack": map[string]any{
			"Action":    action,
			"Parameter": fs.param,
			"StartTime": fs.start.Format(dvripTime),
			"EndTime":   fs.file.End.Format(dvripTime),
		},
	}
}

// open claims a file on a fresh media connection and starts the download.
func (p *playback) open(ctx context.Context, f recFile) (*fileStream, error) {
	conn, err := provider.Dial(ctx, p.host, p.port, 5*time.Second)
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "DVRIP-Wiedergabe")
	}
	start := f.Start
	if p.from.After(start) && p.from.Before(f.End) {
		start = p.from
	}
	fs := &fileStream{conn: conn, rd: bufio.NewReaderSize(conn, 256<<10), file: f, start: start, param: map[string]any{
		"PlayMode": "ByName", "FileName": f.Name, "Channel": p.channel, "StreamType": 0, "Value": 0, "TransMode": "TCP",
	}}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err := conn.Write(encodePacket(p.ctl.Session(), 0, MsgPlayBackClaim, jsonPayload(p.cmd("Claim", fs)))); err != nil {
		conn.Close()
		return nil, provider.ClassifyNetErr(err, "DVRIP-Wiedergabe (Claim)")
	}
	for {
		h, payload, err := readPacket(fs.rd)
		if err != nil {
			conn.Close()
			return nil, provider.ClassifyNetErr(err, "DVRIP-Wiedergabe (Claim)")
		}
		if h.MsgID != MsgPlayBackClaimRsp {
			continue
		}
		r, err := decodeResponse(payload)
		if err != nil || !r.OK() {
			conn.Close()
			return nil, provider.NewError(provider.KindProtocol, err, "DVRIP-Wiedergabe: Claim abgelehnt (Code %d)", r.Ret())
		}
		break
	}
	_ = conn.SetDeadline(time.Time{})
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	r, err := p.ctl.Request(sctx, MsgPlayBack, MsgPlayBackRsp, p.cmd("DownloadStart", fs))
	cancel()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !r.OK() {
		conn.Close()
		return nil, provider.NewError(provider.KindProtocol, nil, "DVRIP-Wiedergabe: Start abgelehnt (Code %d)", r.Ret())
	}
	p.mu.Lock()
	p.cur = fs
	closed := p.closed
	p.mu.Unlock()
	if closed {
		p.stop(fs)
		return nil, errors.New("dvrip playback closed")
	}
	return fs, nil
}

// stop ends a file transfer (fire and forget, never blocks on the device).
func (p *playback) stop(fs *fileStream) {
	fs.once.Do(func() {
		fs.conn.Close()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = p.ctl.Request(ctx, MsgPlayBack, MsgPlayBackRsp, p.cmd("DownloadStop", fs))
		}()
	})
}

// run pumps all files into an MPEG-TS stream.
func (p *playback) run(first *fileStream, files []recFile, pw *io.PipeWriter) {
	out := newPlaybackWriter(pw, p.from, p.to)
	err := p.pump(first, out)
	for i := 1; err == nil && !out.done && i < len(files); i++ {
		var fs *fileStream
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		fs, err = p.open(ctx, files[i])
		cancel()
		if err == nil {
			err = p.pump(fs, out)
		}
	}
	if err == nil {
		err = out.flush()
	}
	if err == nil && !out.started {
		err = provider.NewError(provider.KindNotFound, nil, "keine Videodaten im gewählten Zeitraum")
	}
	if err != nil {
		pw.CloseWithError(err)
		return
	}
	pw.Close()
}

// pump reads one file until its end, the end of the range or an error.
func (p *playback) pump(fs *fileStream, out *playbackWriter) error {
	defer p.stop(fs)
	var parser FrameParser
	for {
		_ = fs.conn.SetReadDeadline(time.Now().Add(mediaIdle))
		h, payload, err := readPacket(fs.rd)
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return io.ErrClosedPipe
			}
			return fmt.Errorf("dvrip playback: %w", err)
		}
		if h.MsgID != MsgPlayBackData && h.MsgID != MsgMonitorData {
			continue
		}
		if len(payload) == 0 {
			return nil // end of file
		}
		frames, err := parser.Push(payload)
		if err != nil {
			return err
		}
		for _, f := range frames {
			if err := out.write(f); err != nil {
				return err
			}
			if out.done {
				return nil
			}
		}
	}
}

// playbackWriter skips to the requested instant, ends at the range end and
// muxes the access units into MPEG-TS with frame rate based timestamps.
type playbackWriter struct {
	from, to time.Time
	gop      []Frame // GOP that contains `from` (held until the next I-frame)
	started  bool
	done     bool
	fps      int
	n        uint64
	mux      *tsmux.Muxer
	annexb   *AnnexBWriter
	w        io.Writer
}

func newPlaybackWriter(w io.Writer, from, to time.Time) *playbackWriter {
	pw := &playbackWriter{from: from, to: to, w: w, fps: 25}
	pw.annexb = NewFrameWriter(func(au []byte, key bool, codec Codec) error {
		if pw.mux == nil {
			st := byte(tsmux.StreamTypeH264)
			if codec == CodecH265 {
				st = tsmux.StreamTypeH265
			}
			pw.mux = tsmux.New(pw.w, st)
		}
		pts := 90000 + pw.n*90000/uint64(pw.fps)
		pw.n++
		return pw.mux.WriteFrame(au, pts, key)
	})
	return pw
}

func isKey(f Frame) bool { return f.Type == 0xFC || f.Type == 0xFE }

// flush emits a held GOP when the recording ended before the next I-frame.
func (w *playbackWriter) flush() error {
	if w.started || len(w.gop) == 0 {
		return nil
	}
	w.started = true
	for _, g := range w.gop {
		if err := w.annexb.Write(g); err != nil {
			return err
		}
	}
	w.gop = nil
	return nil
}

func (w *playbackWriter) write(f Frame) error {
	if !f.IsVideo() {
		return nil
	}
	if isKey(f) {
		if f.FPS > 0 && f.FPS <= 120 {
			w.fps = f.FPS
		}
		t, ok := UnpackTime(f.Stamp)
		if w.started {
			if ok && t.After(w.to) {
				w.done = true
				return nil
			}
		} else if ok && !t.After(w.from) {
			w.gop = append(w.gop[:0], f) // latest GOP starting at or before `from`
			return nil
		} else {
			// first I-frame after `from` (or without a clock): start with the
			// GOP containing `from` if there is one
			w.started = true
			for _, g := range w.gop {
				if err := w.annexb.Write(g); err != nil {
					return err
				}
			}
			w.gop = nil
		}
	} else if !w.started {
		if len(w.gop) > 0 && len(w.gop) < 1000 {
			w.gop = append(w.gop, f)
		}
		return nil
	}
	return w.annexb.Write(f)
}
