package reolink

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"cnm/internal/provider"
	"cnm/internal/tsmux"
)

// recFile is one recorded file (device wall clock, location UTC).
type recFile struct {
	Name       string
	Start, End time.Time
}

// Tele lens recordings of dual-lens cameras: the public descriptions
// address them either as logical channel 1 of channel 0 or as stream
// channel 1. CNM tries the first and remembers what the device accepts.
const (
	teleLogical = 1
	teleChannel = 2
)

// searchTarget returns the Search channel parameters of a camera channel.
func (d *driver) searchTarget(channel int) (devCh int, logical bool) {
	d.mu.RLock()
	dual, mode := d.dualLens, d.search[channel]
	d.mu.RUnlock()
	if !dual || channel != 1 {
		return channel, false
	}
	if mode == teleChannel {
		return 1, false
	}
	return 0, true
}

// searchDay lists the files of one device day (Search may not span days).
func (d *driver) searchDay(ctx context.Context, channel int, from, to time.Time) ([]recFile, error) {
	devCh, logical := d.searchTarget(channel)
	files, err := d.searchRaw(ctx, devCh, logical, from, to)
	if err != nil && logical && provider.KindOf(err) != provider.KindAuth && provider.KindOf(err) != provider.KindNetwork {
		// the other addressing of the tele lens
		if files2, err2 := d.searchRaw(ctx, 1, false, from, to); err2 == nil {
			d.mu.Lock()
			d.search[channel] = teleChannel
			d.mu.Unlock()
			return files2, nil
		}
	}
	return files, err
}

func (d *driver) searchRaw(ctx context.Context, devCh int, logical bool, from, to time.Time) ([]recFile, error) {
	s := map[string]any{"channel": devCh, "onlyStatus": 0, "streamType": "main",
		"StartTime": toReoTime(from), "EndTime": toReoTime(to)}
	if logical {
		s["iLogicChannel"] = 1
	}
	var v struct {
		SearchResult struct {
			File []struct {
				Name      string  `json:"name"`
				StartTime reoTime `json:"StartTime"`
				EndTime   reoTime `json:"EndTime"`
			} `json:"File"`
		} `json:"SearchResult"`
	}
	if err := d.api.call(ctx, "Search", map[string]any{"Search": s}, &v); err != nil {
		return nil, err
	}
	var out []recFile
	for _, f := range v.SearchResult.File {
		st, en := f.StartTime.wall(), f.EndTime.wall()
		if f.Name != "" && en.After(st) {
			out = append(out, recFile{Name: f.Name, Start: st, End: en})
		}
	}
	return out, nil
}

// files lists the recorded files overlapping [from, to] (device wall clock),
// day by day, at most maxDays.
func (d *driver) files(ctx context.Context, channel int, from, to time.Time, maxDays int, stopAtFirst bool) ([]recFile, error) {
	wf, wt := d.clock.ToDevice(from), d.clock.ToDevice(to)
	var out []recFile
	day := time.Date(wf.Year(), wf.Month(), wf.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; day.Before(wt) && i < maxDays; i, day = i+1, day.AddDate(0, 0, 1) {
		st, en := day, day.Add(24*time.Hour-time.Second)
		if st.Before(wf) {
			st = wf
		}
		if en.After(wt) {
			en = wt
		}
		fs, err := d.searchDay(ctx, channel, st, en)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
		if stopAtFirst && len(out) > 0 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	// a file across midnight can be listed for both days
	seen := map[string]bool{}
	uniq := out[:0]
	for _, f := range out {
		if !seen[f.Name] {
			seen[f.Name] = true
			uniq = append(uniq, f)
		}
	}
	return uniq, nil
}

func (d *driver) SearchRecordings(ctx context.Context, channel int, from, to time.Time) ([]provider.Recording, error) {
	fs, err := d.files(ctx, channel, from, to, 32, false)
	if err != nil {
		return nil, err
	}
	recs := make([]provider.Recording, 0, len(fs))
	for _, f := range fs {
		recs = append(recs, provider.Recording{Start: d.clock.FromDevice(f.Start), End: d.clock.FromDevice(f.End)})
	}
	return provider.MergeRecordings(recs, from, to, 2*time.Second), nil
}

func (d *driver) HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error) {
	fs, err := d.files(ctx, channel, from, to, 32, true)
	return len(fs) > 0, err
}

// ---- playback over HTTP-FLV ---------------------------------------------------------------

// flvType is the stream "type" of HTTP-FLV playback: 0 main, 2 tele main.
func (d *driver) flvTarget(channel int) (devCh, typ int) {
	devCh, logical := d.searchTarget(channel)
	if logical {
		return 0, 2
	}
	return devCh, 0
}

// OpenPlayback chains the recorded files from `from` to `to`: each file is
// fetched as HTTP-FLV (the first one with "seek"), its video is put into one
// continuous MPEG-TS stream.
func (d *driver) OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	fs, err := d.files(ctx, channel, from, to, 3, false)
	if err != nil {
		return nil, err
	}
	wf, wt := d.clock.ToDevice(from), d.clock.ToDevice(to)
	var list []recFile
	for _, f := range fs {
		if f.End.After(wf) && f.Start.Before(wt) {
			list = append(list, f)
		}
	}
	if len(list) == 0 {
		return nil, provider.NewError(provider.KindNotFound, nil, "keine Aufnahme im gewählten Zeitraum")
	}
	devCh, typ := d.flvTarget(channel)
	pr, pw := io.Pipe()
	pctx, cancel := context.WithCancel(context.Background())
	go func() {
		err := d.stream(pctx, pw, list, devCh, typ, wf, wt)
		_ = pw.CloseWithError(err)
	}()
	return &provider.PlaybackSource{Reader: &playback{pr: pr, cancel: cancel}, Format: "mpegts", StartupTimeout: 30 * time.Second}, nil
}

type playback struct {
	pr     *io.PipeReader
	cancel context.CancelFunc
}

func (p *playback) Read(b []byte) (int, error) { return p.pr.Read(b) }
func (p *playback) Close() error {
	p.cancel()
	return p.pr.Close()
}

// stream writes the files as one MPEG-TS stream; times are device wall
// clock, PTS run from 0 at `from`.
func (d *driver) stream(ctx context.Context, w io.Writer, files []recFile, devCh, typ int, from, to time.Time) error {
	var mux *tsmux.Muxer
	var lastPTS int64 = -1
	var lastErr error
	var lastAt time.Time
	var shift time.Duration // gaps between files are left out (like the other recorders)
	wrote := false
	for _, f := range files {
		seek := 0
		if from.After(f.Start) {
			seek = int(from.Sub(f.Start) / time.Second)
		}
		fileStart := true
		err := d.streamFile(ctx, f, devCh, typ, seek, func(fr *flvFrame, at time.Time) (bool, error) {
			if !at.Before(to) {
				return false, nil
			}
			if !wrote && !fr.key {
				return true, nil // start at a keyframe
			}
			if fileStart && wrote && at.Sub(lastAt) > time.Second {
				shift += at.Sub(lastAt) - 40*time.Millisecond
			}
			fileStart = false
			lastAt = at
			if mux == nil {
				st := byte(tsmux.StreamTypeH264)
				if fr.hevc {
					st = tsmux.StreamTypeH265
				}
				mux = tsmux.New(w, st)
			}
			pts := int64((at.Sub(from) - shift) / (time.Second / 90000))
			if pts <= lastPTS {
				pts = lastPTS + 1
			}
			lastPTS = pts
			wrote = true
			if pts < 0 {
				pts = 0
			}
			return true, mux.WriteFrame(fr.data, uint64(pts), fr.key)
		})
		if err != nil {
			if ctx.Err() != nil || wrote {
				return err // aborted, or broken off mid-stream
			}
			lastErr = err // nothing from this file yet: try the next one
		}
		if !f.End.Before(to) {
			break
		}
	}
	if !wrote {
		if lastErr != nil {
			return lastErr
		}
		return provider.NewError(provider.KindNotFound, nil, "die Kamera liefert für den Zeitraum kein Bild")
	}
	return nil
}

// streamFile fetches one file as HTTP-FLV and hands every frame with its
// wall clock time to fn until fn returns false.
func (d *driver) streamFile(ctx context.Context, f recFile, devCh, typ, seek int, fn func(*flvFrame, time.Time) (bool, error)) error {
	q := url.Values{"port": {"1935"}, "app": {"bcs"}, "stream": {"playback.bcs"}, "channel": {strconv.Itoa(devCh)},
		"type": {strconv.Itoa(typ)}, "start": {f.Name}, "seek": {strconv.Itoa(seek)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.api.streamURL("/flv", q), nil)
	if err != nil {
		// never return err itself: it would carry the URL with the password
		return provider.NewError(provider.KindProtocol, nil, "Wiedergabe: ungültige Anfrage")
	}
	resp, err := d.api.hc.Do(req)
	if err != nil {
		return provider.ClassifyNetErr(err, "Wiedergabe")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return provider.NewError(provider.KindProtocol, nil, "Wiedergabe: die Kamera antwortet mit HTTP %d", resp.StatusCode)
	}
	rd := newFLVReader(resp.Body)
	base := int64(-1)
	for {
		fr, err := rd.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, errNotFLV) {
				return provider.NewError(provider.KindProtocol, nil, "Wiedergabe: die Kamera liefert keine FLV-Daten")
			}
			return provider.ClassifyNetErr(err, "Wiedergabe")
		}
		if base < 0 {
			// some firmware starts the timestamps at the seek position, others at 0
			base = 0
			if seek > 0 && fr.ms < int64(seek-5)*1000 {
				base = int64(seek) * 1000
			}
		}
		at := f.Start.Add(time.Duration(base+fr.ms) * time.Millisecond)
		more, err := fn(fr, at)
		if err != nil || !more {
			return err
		}
	}
}
