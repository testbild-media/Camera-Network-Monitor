package rtsp

import (
	"bytes"
	"testing"
	"time"
)

// countPES counts video PES starts (one per access unit) in MPEG-TS.
func countPES(ts []byte) int {
	n := 0
	for i := 0; i+188 <= len(ts); i += 188 {
		p := ts[i : i+188]
		if p[0] == 0x47 && int(p[1]&0x1F)<<8|int(p[2]) == 0x0100 && p[1]&0x40 != 0 {
			n++
		}
	}
	return n
}

func TestReplayWriter(t *testing.T) {
	from := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Second)
	var buf bytes.Buffer
	w := newReplayWriter(&buf, from, to)
	au := func(pts int64, key bool, ntp time.Time) AccessUnit {
		return AccessUnit{Data: []byte{0, 0, 0, 1, 0x41, 1}, Key: key, PTS90k: pts, NTP: ntp, Codec: H264}
	}
	steps := []struct {
		au   AccessUnit
		out  int64
		done bool
	}{
		{au(0, false, from), 0, false}, // before the first keyframe: skipped
		{au(3600, true, from.Add(40*time.Millisecond)), 90000, false},
		{au(7200, false, time.Time{}), 93600, false},
		{au(7200+90000*600, false, time.Time{}), 97200, false}, // gap of 10 min: one frame step
		{au(10800+90000*600, false, time.Time{}), 100800, false},
		{au(14400+90000*600, false, to.Add(time.Millisecond)), 100800, true}, // past the end
	}
	for i, s := range steps {
		if err := w.write(s.au); err != nil {
			t.Fatal(err)
		}
		if w.started && w.out != s.out || w.done != s.done {
			t.Fatalf("step %d: out=%d done=%v, want %d %v", i, w.out, w.done, s.out, s.done)
		}
	}
	if n := countPES(buf.Bytes()); n != 4 {
		t.Fatalf("%d frames written, want 4", n)
	}
}
