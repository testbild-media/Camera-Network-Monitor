package generic

import (
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cnm/internal/provider"
	"cnm/internal/rtsp"
	"cnm/internal/sim"
)

type onvifRig struct {
	soap *sim.ONVIFServer
	rtsp *sim.RTSPServer
	host string
	port int

	mu      sync.Mutex
	replays [][2]time.Time // ranges requested by PLAY
}

// startONVIF runs the SOAP simulator plus an RTSP server for live and
// replay streams (user admin / geheim, device clock 3 h ahead).
func startONVIF(t *testing.T, s *sim.ONVIFServer, clip *sim.Clip) *onvifRig {
	t.Helper()
	s.User, s.Pass, s.ClockOffset = "admin", "geheim", 3*time.Hour
	rig := &onvifRig{soap: s}
	r := &sim.RTSPServer{User: s.User, Pass: s.Pass, Route: sim.ONVIFRoute,
		Clip: func(ch, _ int) *sim.Clip {
			if ch < 1 || ch > s.Channels {
				return nil
			}
			return clip
		},
		ONVIFReplay: func(ch int, start, end time.Time) (time.Time, bool) {
			rig.mu.Lock()
			rig.replays = append(rig.replays, [2]time.Time{start, end})
			rig.mu.Unlock()
			return s.Replay(ch, start, end)
		}}
	if err := r.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	s.RTSPAddr = r.Addr()
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rig.rtsp = r
	h, p, _ := net.SplitHostPort(s.Addr())
	rig.host = h
	rig.port, _ = strconv.Atoi(p)
	return rig
}

func (r *onvifRig) driver(t *testing.T, model string, channels int, pass string) (*driver, *env) {
	t.Helper()
	d, e := drv(t, model, r.host, map[string]int{"http": r.port}, map[string]string{provider.OptChannels: strconv.Itoa(channels)}, "admin", pass)
	t.Cleanup(d.Close)
	return d, e
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func rtspPort(r *onvifRig) string {
	_, p, _ := net.SplitHostPort(r.rtsp.Addr())
	return p
}

func TestONVIFNVRChannelsAndLiveSources(t *testing.T) {
	// the device announces an unreachable LAN address (port forwarding):
	// XAddrs and stream URIs must be redirected to the configured host
	rig := startONVIF(t, &sim.ONVIFServer{Channels: 3, SubStreams: true, AdvertiseHost: "192.0.2.10"}, sim.Synthetic(20))
	d, _ := rig.driver(t, ProtoONVIF, 3, "geheim")
	// before the streams are resolved only go2rtc's ONVIF client can serve (channel 1)
	if got := d.LiveSources(0, provider.Main); len(got) != 1 || !strings.HasPrefix(got[0], "onvif://") || d.LiveSources(1, provider.Main) != nil {
		t.Fatalf("unresolved sources %v", got)
	}
	ctx := ctxT(t, 20*time.Second)
	info, err := d.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Channels != 3 || info.Model != "CNM SIM-ONVIF" {
		t.Fatalf("info %+v", info)
	}
	// channel order = GetVideoSources order although GetProfiles lists the
	// channels in reverse
	want := "rtsp://admin:geheim@127.0.0.1:" + rtspPort(rig) + "/onvif/live/2/0#backchannel=0#media=video"
	if got := d.LiveSources(1, provider.Main); len(got) != 2 || got[0] != want || !strings.Contains(got[1], "subtype=Profile_2_main") {
		t.Fatalf("channel 2 main = %v, want %s + go2rtc fallback", got, want)
	}
	if got := d.LiveSources(2, provider.Sub); len(got) != 2 || !strings.Contains(got[0], "/onvif/live/3/1#") {
		t.Fatalf("channel 3 sub = %v", got)
	}
	if d.LiveSources(3, provider.Main) != nil {
		t.Fatal("channel beyond the device must have no source")
	}
	// the announced live URL works (credentials injected, digest auth)
	live := strings.SplitN(d.LiveSources(0, provider.Main)[0], "#", 2)[0]
	c, err := rtsp.Dial(ctx, live, rtsp.Options{})
	if err != nil {
		t.Fatalf("live url: %v", err)
	}
	c.Close()
	h, err := d.Check(ctx)
	if err != nil || h.ClockOffset == nil || (*h.ClockOffset-3*time.Hour).Abs() > 2*time.Second {
		t.Fatalf("health %+v %v", h, err)
	}
	res, err := d.Test(ctx)
	if err != nil || !strings.Contains(res.Message, "3 Kanäle") || !strings.Contains(res.Message, "ONVIF über HTTP") {
		t.Fatalf("test result %+v %v", res, err)
	}
}

func TestONVIFNVRWrongPasswordLocks(t *testing.T) {
	rig := startONVIF(t, &sim.ONVIFServer{Channels: 2}, sim.Synthetic(10))
	d, e := rig.driver(t, ProtoONVIF, 2, "falsch")
	_, err := d.Connect(ctxT(t, 10*time.Second))
	if provider.KindOf(err) != provider.KindAuth || !e.locked.Load() {
		t.Fatalf("want auth error + lock, got %v", err)
	}
	if _, err := d.Connect(context.Background()); err != provider.ErrLoginBlocked || rig.soap.AuthFails.Load() != 1 {
		t.Fatalf("login retried after auth failure: %v (fails=%d)", err, rig.soap.AuthFails.Load())
	}
}

func onvifRecordings(base time.Time) []sim.ONVIFRecording {
	// listed with channel 3 first: mapping by result order would be wrong
	return []sim.ONVIFRecording{
		{Token: "REC_C", Channel: 2, Start: base, End: base.Add(time.Hour)},
		{Token: "REC_A", Channel: 0, Start: base.Add(10 * time.Minute), End: base.Add(50 * time.Minute)},
	}
}

func TestONVIFRecordingSearchMapsChannels(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	from, to := base.Add(-time.Hour), base.Add(30*time.Minute)
	for _, tc := range []struct {
		name                 string
		sourceTokens, noJobs bool
		chOf                 map[string]int // recording token → expected channel
	}{
		{"recording jobs", false, false, map[string]int{"REC_C": 2, "REC_A": 0}},
		{"video source token", true, true, map[string]int{"REC_C": 2, "REC_A": 0}},
		{"result order", false, true, map[string]int{"REC_C": 0, "REC_A": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &sim.ONVIFServer{Channels: 3, Recordings: onvifRecordings(base), SourceTokens: tc.sourceTokens, NoJobs: tc.noJobs}
			rig := startONVIF(t, s, sim.Synthetic(10))
			d, _ := rig.driver(t, ProtoONVIF, 3, "geheim")
			ctx := ctxT(t, 20*time.Second)
			if _, err := d.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			spans := map[string][2]time.Time{
				"REC_C": {base, to},                       // clipped to the requested end
				"REC_A": {base.Add(10 * time.Minute), to}, // starts inside the range
			}
			for ch := 0; ch < 3; ch++ {
				recs, err := d.SearchRecordings(ctx, ch, from, to)
				if err != nil {
					t.Fatal(err)
				}
				var want []provider.Recording
				for tok, c := range tc.chOf {
					if c == ch {
						want = append(want, provider.Recording{Start: spans[tok][0], End: spans[tok][1]})
					}
				}
				if len(recs) != len(want) || (len(want) == 1 && (!recs[0].Start.Equal(want[0].Start) || !recs[0].End.Equal(want[0].End))) {
					t.Fatalf("channel %d: got %+v, want %+v", ch, recs, want)
				}
				has, err := d.HasRecordings(ctx, ch, from, to)
				if err != nil || has != (len(want) > 0) {
					t.Fatalf("channel %d: HasRecordings = %v, %v", ch, has, err)
				}
			}
			if n := rig.soap.Searches.Load(); n != 1 {
				t.Fatalf("%d searches, one search must serve all channels", n)
			}
		})
	}
}

func TestONVIFCameraProfileG(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	rig := startONVIF(t, &sim.ONVIFServer{Channels: 1, Recordings: []sim.ONVIFRecording{{Token: "SD_1", Start: base, End: base.Add(2 * time.Hour)}}}, sim.Synthetic(10))
	d, _ := drv(t, ProtoONVIF, rig.host, map[string]int{"http": rig.port}, nil, "admin", "geheim")
	defer d.Close()
	ctx := ctxT(t, 20*time.Second)
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	// live: the resolved RTSP URI (the leading metadata profile does not
	// count), go2rtc's ONVIF client with the exact token as fallback
	main, sub := d.LiveSources(0, provider.Main), d.LiveSources(0, provider.Sub)
	if len(main) != 2 || !strings.HasPrefix(main[0], "rtsp://admin:geheim@") || !strings.HasSuffix(main[1], "?subtype=Profile_1_main#backchannel=0#media=video") {
		t.Fatalf("camera live source %v", main)
	}
	if len(sub) != 0 { // this camera has no sub stream profile
		t.Fatalf("camera sub source %v", sub)
	}
	recs, err := d.SearchRecordings(ctx, 0, base.Add(time.Hour), base.Add(5*time.Hour))
	if err != nil || len(recs) != 1 || !recs[0].Start.Equal(base.Add(time.Hour)) || !recs[0].End.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("camera search: %+v %v", recs, err)
	}
	if recs, err := d.SearchRecordings(ctx, 1, base, base.Add(time.Hour)); err != nil || len(recs) != 0 {
		t.Fatalf("camera has one channel: %+v %v", recs, err)
	}
	h, err := d.Check(ctx)
	if err != nil || h.ClockOffset == nil {
		t.Fatalf("camera health %+v %v", h, err)
	}
	caps := provider.DeviceCapabilities(prov{}, d.cfg.Model, "")
	if caps.LiveMain != provider.Verified || caps.RecordingSearch != provider.Experimental || caps.SearchKind != "span" || caps.Playback != provider.Experimental || caps.Download != provider.Experimental {
		t.Fatalf("camera capabilities %+v", caps)
	}
}

func TestONVIFWithoutProfileG(t *testing.T) {
	rig := startONVIF(t, &sim.ONVIFServer{Channels: 2, NoProfileG: true}, sim.Synthetic(10))
	d, _ := rig.driver(t, ProtoONVIF, 2, "geheim")
	ctx := ctxT(t, 20*time.Second)
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := d.SearchRecordings(ctx, 0, now.Add(-time.Hour), now); provider.KindOf(err) != provider.KindUnsupported {
		t.Fatalf("search without Profile G: %v", err)
	}
	if has, err := d.HasRecordings(ctx, 0, now.Add(-time.Hour), now); has || err != nil {
		t.Fatalf("HasRecordings without Profile G = %v, %v", has, err)
	}
	if _, err := d.OpenPlayback(ctx, 0, now.Add(-time.Hour), now); provider.KindOf(err) != provider.KindUnsupported {
		t.Fatalf("playback without Profile G: %v", err)
	}
	if rig.soap.Searches.Load() != 0 {
		t.Fatal("search attempted although the device has no search service")
	}
}

// countTS counts video PES packets (one per access unit) and returns the
// payload of the first one.
func countTS(t *testing.T, ts []byte) (int, []byte) {
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

func TestONVIFReplayPlayback(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	rig := startONVIF(t, &sim.ONVIFServer{Channels: 2, Recordings: []sim.ONVIFRecording{
		{Token: "REC_2", Channel: 1, Start: base, End: base.Add(10 * time.Minute)},
	}}, sim.Synthetic(40))
	d, _ := rig.driver(t, ProtoONVIF, 2, "geheim")
	ctx := ctxT(t, 30*time.Second)
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	from, to := base.Add(2*time.Second+300*time.Millisecond), base.Add(5*time.Second)
	src, err := d.OpenPlayback(ctx, 1, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if src.Format != "mpegts" || src.Reader == nil || src.URL != "" {
		t.Fatalf("source %+v", src)
	}
	ts, err := io.ReadAll(src.Reader)
	src.Reader.Close()
	if err != nil {
		t.Fatalf("replay stream: %v", err)
	}
	rig.mu.Lock()
	ranges := append([][2]time.Time(nil), rig.replays...)
	rig.mu.Unlock()
	// Range: clock=<from, whole seconds>-<to>
	if len(ranges) != 1 || !ranges[0][0].Equal(base.Add(2*time.Second)) || !ranges[0][1].Equal(to) {
		t.Fatalf("PLAY ranges %v", ranges)
	}
	frames, first := countTS(t, ts)
	// 3 s at 10 fps, then the server stops (RTCP BYE)
	if frames != 30 {
		t.Fatalf("got %d frames for 3 s at 10 fps", frames)
	}
	if !bytes.Contains(first, []byte{0, 0, 0, 1, 0x67}) || !bytes.Contains(first, []byte{0, 0, 0, 1, 0x65}) {
		t.Fatal("replay must start with SPS + IDR")
	}
	waitUntil(t, func() bool { return rig.rtsp.Sessions.Load() == 0 })

	// no recording on channel 1 (index 0)
	if _, err := d.OpenPlayback(ctx, 0, from, to); provider.KindOf(err) != provider.KindNotFound {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestONVIFReplayClosedByConsumer(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	rig := startONVIF(t, &sim.ONVIFServer{Channels: 1, Recordings: []sim.ONVIFRecording{{Token: "R", Start: base, End: base.Add(time.Hour)}}}, sim.Synthetic(40))
	d, _ := rig.driver(t, ProtoONVIF, 1, "geheim")
	ctx := ctxT(t, 30*time.Second)
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	src, err := d.OpenPlayback(ctx, 0, base, base.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 188*4)
	if _, err := io.ReadFull(src.Reader, head); err != nil {
		t.Fatal(err)
	}
	src.Reader.Close()
	// TEARDOWN / connection close stops the device session
	waitUntil(t, func() bool { return rig.rtsp.Sessions.Load() == 0 })
}

func needFFmpeg(t *testing.T) string {
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	return p
}

// The replayed MPEG-TS must be remuxable to fragmented MP4 like in the
// playback pipeline.
func TestONVIFReplayRemuxableByFFmpeg(t *testing.T) {
	ff := needFFmpeg(t)
	clip, err := sim.Generate(ff, 320, 180, 10, 2, 0)
	if err != nil {
		t.Skip(err)
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	rig := startONVIF(t, &sim.ONVIFServer{Channels: 1, Recordings: []sim.ONVIFRecording{{Token: "R", Start: base, End: base.Add(time.Hour)}}}, clip)
	d, _ := drv(t, ProtoONVIF, rig.host, map[string]int{"http": rig.port}, nil, "admin", "geheim")
	defer d.Close()
	ctx := ctxT(t, 30*time.Second)
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	src, err := d.OpenPlayback(ctx, 0, base.Add(time.Minute), base.Add(time.Minute+2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Reader.Close()
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-f", "mpegts", "-i", "pipe:0",
		"-map", "0:v:0", "-c:v", "copy", "-f", "mp4", "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "pipe:1")
	cmd.Stdin = src.Reader
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, stderr.String())
	}
	if !bytes.Contains(out, []byte("moof")) || !bytes.Contains(out, []byte("avc1")) {
		t.Fatalf("no fragmented H.264 MP4 produced (%d bytes)", len(out))
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

// ONVIF camera: one video source = one channel, main/sub = first and second
// video profile (the leading metadata profile does not count); go2rtc's
// ONVIF client with the exact token as fallback.
func TestONVIFCameraProfiles(t *testing.T) {
	rig := startONVIF(t, &sim.ONVIFServer{Channels: 1, SubStreams: true}, sim.Synthetic(10))
	ctx := ctxT(t, 20*time.Second)
	d, _ := drv(t, ProtoONVIF, rig.host, map[string]int{"http": rig.port}, nil, "admin", "geheim")
	defer d.Close()
	info, err := d.Connect(ctx)
	if err != nil || info.Channels != 1 || info.Variant != "onvif" {
		t.Fatalf("%+v %v", info, err)
	}
	main, sub := d.LiveSources(0, provider.Main), d.LiveSources(0, provider.Sub)
	if len(main) != 2 || !strings.Contains(main[0], "/onvif/live/1/0") || !strings.Contains(main[1], "subtype=Profile_1_main") {
		t.Fatalf("main %v", main)
	}
	if len(sub) != 2 || !strings.Contains(sub[0], "/onvif/live/1/1") || !strings.Contains(sub[1], "subtype=Profile_1_sub") {
		t.Fatalf("sub %v", sub)
	}
	// without a resolution go2rtc resolves the first/second profile itself
	cold, _ := drv(t, ProtoONVIF, rig.host, map[string]int{"http": rig.port}, nil, "admin", "geheim")
	if got := cold.LiveSources(0, provider.Sub); len(got) != 1 || !strings.Contains(got[0], "onvif://admin:geheim@") || !strings.Contains(got[0], "subtype=1#") {
		t.Fatalf("unresolved %v", got)
	}
}
