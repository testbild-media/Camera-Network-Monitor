package sofia

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/provider"
	"cnm/internal/sim"
)

func startSim(t *testing.T, legacy bool, chunk int) *sim.SofiaServer {
	t.Helper()
	clip := sim.Synthetic(40)
	s := &sim.SofiaServer{User: "admin", Pass: "pw123456", Legacy: legacy, Channels: 4, ChunkSize: chunk,
		Clip: func(int, bool) *sim.Clip { return clip }}
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func hostPort(t *testing.T, addr string) (string, int) {
	h, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

type testEnv struct {
	locked atomic.Bool
	url    string
}

func (e *testEnv) AllowLogin() bool                                   { return !e.locked.Load() }
func (e *testEnv) ReportAuthFailure(error)                            { e.locked.Store(true) }
func (e *testEnv) LegacyLiveURL(ch int, k provider.StreamKind) string { return e.url }

func newDriver(t *testing.T, addr, pass string) (*driver, *testEnv) {
	h, p := hostPort(t, addr)
	m, _ := prov{}.Info().FindModel(ProtoSofia)
	env := &testEnv{url: "http://bridge"}
	d, err := prov{}.NewDriver(provider.DeviceConfig{ID: "x", Model: m, Host: h, Username: "admin", Password: pass, Ports: map[string]int{"native": p}}, env)
	if err != nil {
		t.Fatal(err)
	}
	return d.(*driver), env
}

func TestFrameParserFragmentationAndResync(t *testing.T) {
	clip := sim.Synthetic(12)
	var stream []byte
	stream = append(stream, 0xde, 0xad, 0xbe, 0xef, 0x01) // garbage before the first frame
	for _, au := range clip.AUs {
		stream = append(stream, sim.SofiaFrames(au)...)
	}
	var p FrameParser
	var frames []Frame
	for i := 0; i < len(stream); i += 333 { // chunks cross frame boundaries
		end := i + 333
		if end > len(stream) {
			end = len(stream)
		}
		fs, err := p.Push(stream[i:end])
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, fs...)
	}
	if len(frames) != len(clip.AUs) {
		t.Fatalf("got %d frames, want %d", len(frames), len(clip.AUs))
	}
	for i, f := range frames {
		if !bytes.Equal(f.Payload, clip.AUs[i].Data) || f.Codec != CodecH264 {
			t.Fatalf("frame %d mismatch (codec %v)", i, f.Codec)
		}
	}
	if p.Resyncs == 0 {
		t.Fatal("garbage prefix should have triggered a resync")
	}
}

func TestAnnexBWriterStartsAtKeyframe(t *testing.T) {
	var buf bytes.Buffer
	w := NewAnnexBWriter(&buf)
	// P-frame first must be dropped, SEI before SPS must be skipped
	w.Write(Frame{Type: 0xFD, Codec: CodecH264, Payload: []byte{0, 0, 0, 1, 0x41, 1, 2}})
	key := append([]byte{0, 0, 1, 0x06, 9, 9}, []byte{0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 1, 0x65, 4}...)
	w.Write(Frame{Type: 0xFC, Codec: CodecH264, Payload: key})
	got := buf.Bytes()
	if !bytes.HasPrefix(got, []byte{0, 0, 0, 1, 0x67}) {
		t.Fatalf("stream must start with 4-byte start code + SPS, got % x", got[:8])
	}
	if err := w.Write(Frame{Type: 0xFD, Codec: CodecH265, Payload: []byte{0, 0, 1, 2}}); err != ErrCodecChanged {
		t.Fatal("codec change must end the stream")
	}
	// H.265 detection by NAL type
	if sniffCodec([]byte{0, 0, 0, 1, 0x40, 1, 0, 0, 1, 0x42, 1}) != CodecH265 {
		t.Fatal("H.265 VPS not detected")
	}
}

func TestModernDeviceDetection(t *testing.T) {
	s := startSim(t, false, 4096)
	d, _ := newDriver(t, s.Addr(), "pw123456")
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, err := d.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "SIM-DVRIP" || info.ChannelNames[0] != "XM CAM 1" {
		t.Fatalf("info: %+v", info)
	}
	if d.effectiveMode() != config.LiveModern {
		t.Fatalf("mode = %q", d.effectiveMode())
	}
	src := d.LiveSources(1, provider.Sub)
	if len(src) != 1 || src[0][:8] != "dvrip://" {
		t.Fatalf("sources: %v", src)
	}
	if _, err := d.Check(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyTwoSocketLive(t *testing.T) {
	s := startSim(t, true, 1000)
	d, _ := newDriver(t, s.Addr(), "pw123456")
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if d.effectiveMode() != config.LiveLegacy {
		t.Fatalf("mode = %q, want legacy", d.effectiveMode())
	}
	if src := d.LiveSources(0, provider.Main); len(src) != 1 || src[0] != "http://bridge" {
		t.Fatalf("legacy must use the CNM bridge: %v", src)
	}
	rc, err := d.OpenLegacyLive(ctx, 2, provider.Main)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 188*3)
	if _, err := io.ReadFull(rc, head); err != nil {
		t.Fatal(err)
	}
	// MPEG-TS: PAT, PMT, then the first video PES carrying the SPS
	if head[0] != 0x47 || head[188] != 0x47 || head[376] != 0x47 || !bytes.Contains(head[376:], []byte{0, 0, 0, 1, 0x67}) {
		t.Fatalf("bridged stream must be MPEG-TS starting at a keyframe, got % x", head[:16])
	}
	// keep reading a while: frames continue across chunk boundaries
	n, _ := io.CopyN(io.Discard, rc, 50000)
	if n < 50000 {
		t.Fatalf("stream stalled after %d bytes", n)
	}
	rc.Close()
	waitUntil(t, func() bool { return s.Streams.Load() == 0 })
}

func TestWrongPasswordIsAuthError(t *testing.T) {
	s := startSim(t, false, 4096)
	d, env := newDriver(t, s.Addr(), "wrong")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := d.Connect(ctx)
	if provider.KindOf(err) != provider.KindAuth || !env.locked.Load() {
		t.Fatalf("expected auth error + lock, got %v", err)
	}
	// further attempts are suppressed without contacting the device
	_, err = d.Connect(ctx)
	if provider.KindOf(err) != provider.KindAuth || s.FailedLogins.Load() != 1 {
		t.Fatalf("login retried after auth failure (fails=%d)", s.FailedLogins.Load())
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

// countTSFrames counts video PES packets (one per access unit) and returns
// the first video payload bytes.
func countTSFrames(t *testing.T, ts []byte) (int, []byte) {
	t.Helper()
	n := 0
	var first []byte
	for i := 0; i+188 <= len(ts); i += 188 {
		p := ts[i : i+188]
		if p[0] != 0x47 {
			t.Fatalf("lost TS sync at %d", i)
		}
		pid := int(p[1]&0x1F)<<8 | int(p[2])
		if pid == 0x0100 && p[1]&0x40 != 0 {
			n++
			if first == nil {
				first = append([]byte(nil), p[4:]...)
			}
		}
	}
	return n, first
}

func TestRecordingSearchAndPlaybackAcrossFiles(t *testing.T) {
	s := startSim(t, true, 4096)
	now := sim.DeviceNow()
	s.Recorded = func(int) (time.Time, time.Time) { return now.Add(-30 * time.Minute), now }
	s.FileLength = 10 * time.Minute
	d, _ := newDriver(t, s.Addr(), "pw123456")
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.clock.Offset(); !ok {
		t.Fatal("device time (OPTimeQuery) not measured")
	}
	h, err := d.Check(ctx)
	if err != nil || h.Storage != provider.StoragePresent {
		t.Fatalf("check: %+v %v", h, err)
	}
	real := func(dev time.Time) time.Time { return d.clock.FromDevice(dev) }
	recs, err := d.SearchRecordings(ctx, 1, real(now.Add(-2*time.Hour)), real(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].End.Sub(recs[0].Start) < 29*time.Minute {
		t.Fatalf("files must merge into one 30 min range: %+v", recs)
	}
	if has, err := d.HasRecordings(ctx, 1, real(now.Add(-time.Hour)), real(now)); !has || err != nil {
		t.Fatalf("HasRecordings = %v, %v", has, err)
	}
	// one minute across the boundary of two 10 minute files
	boundary := now.Add(-30 * time.Minute).Truncate(10 * time.Minute).Add(20 * time.Minute)
	src, err := d.OpenPlayback(ctx, 1, real(boundary.Add(-30*time.Second)), real(boundary.Add(30*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if src.Format != "mpegts" || src.Reader == nil {
		t.Fatalf("source: %+v", src)
	}
	ts, err := io.ReadAll(src.Reader)
	src.Reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	frames, first := countTSFrames(t, ts)
	// 60 s at 10 fps, plus at most the GOP that contains the start and the last I-frame
	if frames < 600 || frames > 625 {
		t.Fatalf("got %d frames for one minute at 10 fps", frames)
	}
	if !bytes.Contains(first, []byte{0, 0, 0, 1, 0x67}) {
		t.Fatal("playback must start with SPS + keyframe")
	}
	waitUntil(t, func() bool { return s.Streams.Load() == 0 })
}

func TestPlaybackOutsideRecordingsIsNotFound(t *testing.T) {
	s := startSim(t, false, 4096)
	now := sim.DeviceNow()
	s.Recorded = func(int) (time.Time, time.Time) { return now.Add(-30 * time.Minute), now.Add(-20 * time.Minute) }
	d, _ := newDriver(t, s.Addr(), "pw123456")
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := d.OpenPlayback(ctx, 0, d.clock.FromDevice(now.Add(-10*time.Minute)), d.clock.FromDevice(now))
	if provider.KindOf(err) != provider.KindNotFound {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestPackTimeRoundTrip(t *testing.T) {
	want := time.Date(2026, 10, 9, 23, 59, 58, 0, time.UTC)
	if got, ok := UnpackTime(sim.PackTime(want)); !ok || !got.Equal(want) || PackTime(want) != sim.PackTime(want) {
		t.Fatalf("got %v", got)
	}
}

// A recorder without disk answers StorageInfo with an empty list or null:
// that is "absent" (shown as "keine Festplatte"), not "unknown".
func TestStorageAbsent(t *testing.T) {
	for _, null := range []bool{false, true} {
		s := startSim(t, false, 4096)
		s.NoDisk, s.NullDisk = true, null
		d, _ := newDriver(t, s.Addr(), "pw123456")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := d.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		h, err := d.Check(ctx)
		if err != nil || h.Storage != provider.StorageAbsent {
			t.Fatalf("null=%v: %+v %v", null, h, err)
		}
		res, err := d.Test(ctx)
		if err != nil || !strings.Contains(res.Message, "keine Festplatte") {
			t.Fatalf("test message %+v %v", res, err)
		}
		cancel()
		d.Close()
	}
}
