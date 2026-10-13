package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/device"
	"cnm/internal/provider"
	"cnm/internal/secret"
	"cnm/internal/sim"

	_ "cnm/internal/provider/dahua"
	_ "cnm/internal/provider/sofia"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func needFFmpeg(t *testing.T) string {
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	return p
}

type rig struct {
	svc   *Service
	store *config.Store
	rtsp  *sim.RTSPServer
}

func setup(t *testing.T, hasDisk bool) *rig {
	ff := needFFmpeg(t)
	clip, err := sim.Generate(ff, 320, 180, 10, 2, 0)
	if err != nil {
		t.Skipf("cannot generate test clip: %v", err)
	}
	rpc := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 8, HasDisk: hasDisk, Recordings: sim.QuarterSchedule}
	if err := rpc.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpc.Close)
	rtsp := &sim.RTSPServer{User: "admin", Pass: "pw123456", Clip: func(int, int) *sim.Clip { return clip }, HasRecording: sim.InQuarterSchedule}
	if err := rtsp.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rtsp.Close)
	port := func(a string) int { _, p, _ := net.SplitHostPort(a); n, _ := strconv.Atoi(p); return n }
	keys, _ := secret.NewKeyring(secret.GenerateKey())
	enc, _ := keys.Encrypt("pw123456")
	encJSON, _ := json.Marshal(enc)
	c := config.Default(t.TempDir())
	c.Devices = []config.Device{{ID: "dvr1", Label: "DVR", Protocol: "dahua", Detected: &config.Detection{Variant: "rpc2-dahua3"}, Host: "127.0.0.1",
		Credentials: config.Credentials{Username: "admin", Password: encJSON},
		Ports:       map[string]int{"http": port(rpc.Addr()), "rtsp": port(rtsp.Addr())}, ChannelCount: 8}}
	c.System.Media.FFmpegBin = ff
	config.Normalize(c, t.TempDir())
	for i := range c.Cameras { // readable ids
		c.Cameras[i].ID = "dvr1:" + strconv.Itoa(c.Cameras[i].Channel)
	}
	store := config.NewMemoryStore(c)
	m := device.NewManager(store, keys, nil, quiet())
	m.Start()
	t.Cleanup(func() { m.Close(context.Background()) })
	for i := 0; i < 200; i++ {
		if st, ok := m.Status("dvr1"); ok && st.Online() && st.Storage != "unknown" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return &rig{svc: New(store.Get, m, quiet()), store: store, rtsp: rtsp}
}

func recordedStart() time.Time {
	t := time.Now().Add(-time.Hour).Truncate(15 * time.Minute).Add(2 * time.Minute)
	return t
}

func TestPlaybackStreamsFragmentedMP4(t *testing.T) {
	r := setup(t, true)
	from := recordedStart()
	w := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e := r.svc.Stream(ctx, w, "dvr1:0", from, from.Add(3*time.Second)); e != nil {
		t.Fatalf("stream: %v", e)
	}
	codec := w.Header().Get("X-CNM-Codec")
	if len(codec) < 6 || codec[:5] != "avc1." {
		t.Fatalf("codec header %q", codec)
	}
	body := w.Body.Bytes()
	if !bytes.Equal(body[4:8], []byte("ftyp")) || !bytes.Contains(body, []byte("moof")) || !bytes.Contains(body, []byte("mdat")) {
		t.Fatalf("not fragmented MP4 (%d bytes)", len(body))
	}
	if bytes.Contains(body, []byte("soun")) {
		t.Fatal("output must be video only")
	}
	if r.svc.Active() != 0 {
		t.Fatal("playback slot not released")
	}
}

func TestPlaybackClientDisconnectKillsFFmpeg(t *testing.T) {
	r := setup(t, true)
	from := recordedStart()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.svc.Stream(ctx, httptest.NewRecorder(), "dvr1:0", from, from.Add(10*time.Minute))
		close(done)
	}()
	for i := 0; i < 200 && r.rtsp.Sessions.Load() == 0; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	if r.rtsp.Sessions.Load() == 0 {
		t.Fatal("playback never reached the DVR")
	}
	cancel() // browser closed the stream
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after client disconnect")
	}
	for i := 0; i < 200 && r.rtsp.Sessions.Load() != 0; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	if r.rtsp.Sessions.Load() != 0 {
		t.Fatal("FFmpeg kept the DVR session open after the client left")
	}
}

func TestPlaybackStructuredErrors(t *testing.T) {
	r := setup(t, true)
	ctx := context.Background()
	now := time.Now()
	if _, e := r.svc.Search(ctx, "dvr1:0", now, now.Add(-time.Minute)); e == nil || e.Code != CodeInvalidRange {
		t.Fatalf("start after end: %v", e)
	}
	if _, e := r.svc.Search(ctx, "dvr1:0", now.Add(-25*time.Hour), now); e == nil || e.Code != CodeRangeTooLarge {
		t.Fatalf("range > 24h: %v", e)
	}
	gapStart := time.Now().Add(-time.Hour).Truncate(15 * time.Minute).Add(11 * time.Minute)
	if _, e := r.svc.Search(ctx, "dvr1:0", gapStart, gapStart.Add(3*time.Minute)); e == nil || e.Code != CodeNoRecordings {
		t.Fatalf("gap without recordings: %v", e)
	}
	res, e := r.svc.Search(ctx, "dvr1:0", now.Add(-time.Hour), now)
	if e != nil || len(res.Ranges) < 3 {
		t.Fatalf("search: %v %+v", e, res)
	}
	w := httptest.NewRecorder()
	if e := r.svc.Stream(ctx, w, "dvr1:0", gapStart, gapStart.Add(time.Minute)); e == nil || e.Code != CodeNoRecordings {
		t.Fatalf("stream in gap: %v", e)
	}
	r.store.Update(func(c *config.Config) error {
		c.Devices[0].Protocol, c.Devices[0].Detected = "rtsp", nil
		return nil
	})
	if _, e := r.svc.Search(ctx, "dvr1:0", now.Add(-time.Hour), now); e == nil || e.Code != CodeUnsupported {
		t.Fatalf("live-only model must report playback unsupported: %v", e)
	}
}

// Recording storage is detected automatically: a DVR without disk yields
// NO_STORAGE and the camera is not offered for playback.
func TestStorageDetectedAutomatically(t *testing.T) {
	r := setup(t, false)
	ctx := context.Background()
	now := time.Now()
	if _, e := r.svc.Search(ctx, "dvr1:0", now.Add(-time.Hour), now); e == nil || e.Code != CodeNoStorage {
		t.Fatalf("expected NO_STORAGE from the detected disk state, got %v", e)
	}
	r.svc.indexPass(ctx)
	if a := r.svc.Availability("dvr1:0"); a != AvailNo {
		t.Fatalf("availability = %s, want no", a)
	}
}

// The availability index decides whether playback is offered at all.
func TestRecordingAvailabilityIndex(t *testing.T) {
	r := setup(t, true)
	changed := 0
	r.svc.OnAvailabilityChange(func() { changed++ })
	r.svc.indexPass(context.Background())
	if a := r.svc.Availability("dvr1:0"); a != AvailYes {
		t.Fatalf("availability = %s, want yes", a)
	}
	if changed == 0 {
		t.Fatal("availability change not signalled")
	}
	info, _ := r.svc.Info("dvr1:0")
	if !info.Available || info.Recordings != "yes" {
		t.Fatalf("info: %+v", info)
	}
	// unchanged state: no new signal
	before := changed
	r.svc.indexPass(context.Background())
	if changed != before {
		t.Fatal("index pass without changes must not signal")
	}
}

func TestCodecStringHEVC(t *testing.T) {
	// hvcC: version, profile_space/tier/profile_idc=0x01 (Main), compat flags 0x60000000,
	// constraint 0x90..., level 93
	hvcC := []byte{1, 0x01, 0x60, 0, 0, 0, 0x90, 0, 0, 0, 0, 0, 93}
	if got := hevcCodec("hvc1", hvcC); got != "hvc1.1.6.L93.90" {
		t.Fatalf("hevc codec string %q", got)
	}
}

// The native Dahua path delivers DHAV; the FFmpeg bridge must turn it into
// browser playable fMP4 (independent of the unverified request parameters).
func TestDHAVPipeline(t *testing.T) {
	ff := needFFmpeg(t)
	clip, err := sim.Generate(ff, 320, 180, 10, 2, 0)
	if err != nil {
		t.Skip(err)
	}
	data := sim.DHAV(clip, time.Now().Add(-time.Hour), 40)
	c := config.Default(t.TempDir())
	c.System.Media.FFmpegBin = ff
	svc := New(func() *config.Config { return c }, nil, quiet())
	src := &provider.PlaybackSource{Reader: io.NopCloser(bytes.NewReader(data)), Format: "dhav"}
	w := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if e := svc.pipe(ctx, ctx, w, "native:0", src, ""); e != nil {
		t.Fatalf("pipe: %v", e)
	}
	if codec := w.Header().Get("X-CNM-Codec"); len(codec) < 5 || codec[:5] != "avc1." {
		t.Fatalf("codec %q", codec)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("moof")) {
		t.Fatal("no fragments produced from DHAV input")
	}
}

// DVRIP playback delivers MPEG-TS from the CNM muxer; FFmpeg must turn it
// into fMP4 with the real codec string.
func TestDVRIPPlaybackPipeline(t *testing.T) {
	ff := needFFmpeg(t)
	clip, err := sim.Generate(ff, 320, 180, 10, 2, 0)
	if err != nil {
		t.Skip(err)
	}
	s := &sim.SofiaServer{User: "admin", Pass: "pw123456", Legacy: true, Channels: 4, Clip: func(int, bool) *sim.Clip { return clip }}
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	host, ps, _ := net.SplitHostPort(s.Addr())
	port, _ := strconv.Atoi(ps)
	p, m, err := provider.Resolve("sofia")
	if err != nil {
		t.Fatal(err)
	}
	drv, err := p.NewDriver(provider.DeviceConfig{Model: m, Host: host, Username: "admin", Password: "pw123456", Ports: map[string]int{"native": port}}, nopEnv{})
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := drv.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	src, err := drv.(provider.PlaybackOpener).OpenPlayback(ctx, 0, now.Add(-20*time.Minute), now.Add(-19*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Reader.Close()
	c := config.Default(t.TempDir())
	c.System.Media.FFmpegBin = ff
	svc := New(func() *config.Config { return c }, nil, quiet())
	w := httptest.NewRecorder()
	if e := svc.pipe(ctx, ctx, w, "dvrip:0", src, ""); e != nil {
		t.Fatalf("pipe: %v", e)
	}
	if codec := w.Header().Get("X-CNM-Codec"); len(codec) < 5 || codec[:5] != "avc1." {
		t.Fatalf("codec %q", codec)
	}
	if n := bytes.Count(w.Body.Bytes(), []byte("moof")); n < 10 {
		t.Fatalf("only %d fragments for one minute of recording", n)
	}
}

type nopEnv struct{}

func (nopEnv) AllowLogin() bool                              { return true }
func (nopEnv) ReportAuthFailure(error)                       {}
func (nopEnv) LegacyLiveURL(int, provider.StreamKind) string { return "" }

func TestDownloadFileName(t *testing.T) {
	at := time.Date(2026, 10, 9, 4, 55, 38, 0, time.Local)
	if got := fileName("Tor Süd/../x", at); got != "Tor_Süd..x_2026-10-09_04-55-38.mp4" {
		t.Fatalf("%q", got)
	}
	if got := fileName("///", at); got != "Aufnahme_2026-10-09_04-55-38.mp4" {
		t.Fatalf("%q", got)
	}
	if asciiName("Tor_Süd.mp4") != "Tor_Suued.mp4" && asciiName("Tor_Süd.mp4") != "Tor_Sued.mp4" {
		t.Fatal(asciiName("Tor_Süd.mp4"))
	}
}

func TestClassifyFailureIgnoresNumbersInURLs(t *testing.T) {
	svc := New(func() *config.Config { return config.Default(t.TempDir()) }, nil, quiet())
	e := svc.classifyFailure("cam", "rtsp://***@192.0.2.5:54012/Streaming/tracks/101?starttime=20261009T140100Z: Server returned 404 Not Found")
	if e.Code != CodeNoRecordings {
		t.Fatalf("%v", e)
	}
	e = svc.classifyFailure("cam", "rtsp://***@192.0.2.5:4013/x: Server returned 401 Unauthorized (authorization failed)")
	if e.Code != CodeMediaError || !strings.Contains(e.Message, "Anmeldung") {
		t.Fatalf("%v", e)
	}
	e = svc.classifyFailure("cam", "rtsp://***@192.0.2.5:14010/x?starttime=20261009T140100Z: Invalid data found when processing input")
	if strings.Contains(e.Message, "Anmeldung") {
		t.Fatalf("misclassified: %v", e)
	}
}
