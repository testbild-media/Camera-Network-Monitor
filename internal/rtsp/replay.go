package rtsp

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"cnm/internal/provider"
	"cnm/internal/tsmux"
)

// ClockStamp formats a replay range bound for "Range: clock=" (UTC, whole
// seconds; roundUp for the end of a range).
func ClockStamp(t time.Time, roundUp bool) string {
	t = t.UTC()
	if s := t.Truncate(time.Second); roundUp && s.Before(t) {
		t = s.Add(time.Second)
	}
	return t.Format("20060102T150405Z")
}

// OpenReplay plays a recorded range – PLAY with the given headers, e.g.
// ONVIF "Require: onvif-replay" + "Range: clock=…" or Axis "Range: clock=…" –
// and returns it as MPEG-TS (one PES per access unit, continuous timestamps).
// The stream ends with the range (NTP time extension or range length);
// Close sends TEARDOWN.
func OpenReplay(ctx context.Context, rawURL string, opt Options, play map[string]string, from, to time.Time) (io.ReadCloser, error) {
	cl, err := Dial(ctx, rawURL, opt)
	if err != nil {
		return nil, err
	}
	if err := cl.Play(ctx, play); err != nil {
		cl.Close()
		return nil, err
	}
	pr, pw := io.Pipe()
	r := &replay{cl: cl, pr: pr}
	go r.run(pw, newReplayWriter(pw, from, to))
	return r, nil
}

var errReplayDone = errors.New("rtsp replay: end of range")

type replay struct {
	cl   *Client
	pr   *io.PipeReader
	once sync.Once
}

func (p *replay) Read(b []byte) (int, error) { return p.pr.Read(b) }

// Close ends the replay (TEARDOWN) and the stream.
func (p *replay) Close() error {
	p.once.Do(func() {
		p.pr.Close()
		p.cl.Close()
	})
	return nil
}

func (p *replay) run(pw *io.PipeWriter, w *replayWriter) {
	defer p.cl.Close()
	err := p.cl.Read(func(au AccessUnit) error {
		if err := w.write(au); err != nil {
			return err
		}
		if w.done {
			return errReplayDone
		}
		return nil
	})
	switch {
	case err == nil, errors.Is(err, errReplayDone):
		err = nil
	case errors.Is(err, ErrIdle) && w.started:
		err = nil // replay servers simply stop sending at the end of the footage
	case errors.Is(err, ErrIdle):
		err = provider.NewError(provider.KindNotFound, nil, "keine Videodaten im gewählten Zeitraum")
	}
	if err == nil && !w.started {
		err = provider.NewError(provider.KindNotFound, nil, "keine Videodaten im gewählten Zeitraum")
	}
	if err != nil {
		pw.CloseWithError(err)
		return
	}
	pw.Close()
}

// replayWriter muxes replayed access units into MPEG-TS. It starts at the
// first keyframe, ends when the recording time (ONVIF NTP extension) passes
// the end of the range and turns gaps between recordings into a regular
// frame step so the output timeline stays continuous.
type replayWriter struct {
	w       io.Writer
	mux     *tsmux.Muxer
	codec   Codec
	to      time.Time
	limit   int64 // range length (90 kHz) as end criterion without NTP times
	sawNTP  bool
	started bool
	done    bool
	out     int64 // output PTS
	lastIn  int64
	step    int64
}

func newReplayWriter(w io.Writer, from, to time.Time) *replayWriter {
	return &replayWriter{w: w, to: to, limit: int64((to.Sub(from) + 5*time.Second) / time.Second * 90000), step: 3600}
}

func (w *replayWriter) write(au AccessUnit) error {
	if !au.NTP.IsZero() {
		w.sawNTP = true
		if au.NTP.After(w.to) {
			w.done = true
			return nil
		}
	}
	if !w.started {
		if !au.Key {
			return nil
		}
		st := byte(tsmux.StreamTypeH264)
		if au.Codec == H265 {
			st = tsmux.StreamTypeH265
		}
		w.mux, w.codec, w.started = tsmux.New(w.w, st), au.Codec, true
		w.out, w.lastIn = 90000, au.PTS90k
		return w.mux.WriteFrame(au.Data, uint64(w.out), true)
	}
	if au.Codec != w.codec {
		w.done = true // codec change: end the stream
		return nil
	}
	delta := au.PTS90k - w.lastIn
	w.lastIn = au.PTS90k
	switch {
	case delta <= -90000 || delta > 2*90000:
		delta = w.step // discontinuity (gap between recordings, timestamp reset)
	case delta > 0:
		w.step = delta
	}
	w.out += delta
	if !w.sawNTP && w.out-90000 > w.limit {
		w.done = true
		return nil
	}
	return w.mux.WriteFrame(au.Data, uint64(max(w.out, 0)), au.Key)
}
