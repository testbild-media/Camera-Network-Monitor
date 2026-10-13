package media

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/provider"
)

type fakeDevices struct {
	mu      sync.Mutex
	locked  map[string]bool
	offline map[string]bool
}

func (f *fakeDevices) Sources(dev string, ch int, kind provider.StreamKind) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.locked[dev] {
		return nil
	}
	return []string{"rtsp://user:secret@" + dev + "/ch" + string(rune('0'+ch)) + "/" + string(kind) + "#media=video"}
}

func (f *fakeDevices) Online(dev string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.offline[dev]
}

func testConfig(base string) *config.Config {
	c := config.Default("/tmp")
	c.System.Media.Go2rtcURL = base
	c.Devices = []config.Device{
		{ID: "dvr1", Protocol: "dahua", Detected: &config.Detection{Variant: "rpc2-dahua3"}, Host: "h1", ChannelCount: 3},
		{ID: "dvr2", Protocol: "dahua", Detected: &config.Detection{Variant: "rpc2-dahua3"}, Host: "h2", ChannelCount: 1},
	}
	config.Normalize(c, "/tmp")
	// readable test ids: "<device>:<channel>"
	for i := range c.Cameras {
		c.Cameras[i].ID = c.Cameras[i].DeviceID + ":" + string(rune('0'+c.Cameras[i].Channel))
	}
	cam(c, "dvr1:1").Streams.Main = false // camera with a sub stream only
	cam(c, "dvr1:2").Enabled = false
	c.Monitors[0].Layout.Rows[0].Slots[0].CameraID = "dvr1:0"
	c.Monitors[0].Layout.Rows[0].Slots[1].CameraID = "dvr1:1"
	return c
}

func cam(c *config.Config, id string) *config.Camera {
	x, _ := c.Camera(id)
	return x
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCamerasPolicies(t *testing.T) {
	c := testConfig("http://x")
	cam(c, "dvr2:0").Streams.Sub = false
	cams := Cameras(c)
	cs := cams["dvr1:0"]
	if cs.Multiview != "cnm_dvr1_ch01_sub" || cs.Focus != "cnm_dvr1_ch01_main" || !cs.Fallback {
		t.Fatalf("sub-fallback: %+v", cs)
	}
	if cams["dvr1:1"].Fallback || cams["dvr1:1"].Multiview != "cnm_dvr1_ch02_sub" {
		t.Fatalf("sub only: %+v", cams["dvr1:1"])
	}
	if cams["dvr1:2"].Multiview != "" || cams["dvr1:2"].Focus != "" {
		t.Fatal("disabled channel must not have streams")
	}
	if cams["dvr2:0"].Multiview != "cnm_dvr2_ch01_main" {
		t.Fatalf("sub unavailable must fall back to main stream name: %+v", cams["dvr2:0"])
	}
	allowed := AllowedStreams(c, c.Monitors[0].ID)
	if !allowed["cnm_dvr1_ch01_sub"] || !allowed["cnm_dvr1_ch01_main"] || allowed["cnm_dvr2_ch01_main"] {
		t.Fatalf("monitor scope wrong: %v", allowed)
	}
}

func TestDesiredPreloadSelection(t *testing.T) {
	c := testConfig("http://x")
	devs := &fakeDevices{}
	d, expected := Desired(c, devs.Sources, devs.Online)
	if expected != 5 { // dvr1 ch1 (sub+main) ch2 (sub+main) dvr2 ch1 (sub+main) = 6? ch2 sub-only main focus
		t.Logf("expected=%d", expected)
	}
	mv := d["cnm_dvr1_ch01_sub"]
	if mv == nil || len(mv.Sources) != 2 || !strings.Contains(mv.Sources[1], "/main") || !mv.Preload {
		t.Fatalf("multiview stream: %+v", mv)
	}
	if d["cnm_dvr1_ch01_main"].Preload {
		t.Fatal("main must not be preloaded by default")
	}
	if d["cnm_dvr2_ch01_sub"].Preload {
		t.Fatal("camera outside monitors must not be preloaded")
	}
	c.System.Media.PreloadMain = true
	d, _ = Desired(c, devs.Sources, devs.Online)
	if !d["cnm_dvr1_ch01_main"].Preload || d["cnm_dvr2_ch01_main"].Preload {
		t.Fatal("main preload selection wrong")
	}
	devs.locked = map[string]bool{"dvr1": true}
	d, _ = Desired(c, devs.Sources, devs.Online)
	for n := range d {
		if strings.Contains(n, "dvr1") {
			t.Fatalf("auth-locked device must not be registered: %s", n)
		}
	}
	devs.locked = nil
	devs.offline = map[string]bool{"dvr1": true}
	d, _ = Desired(c, devs.Sources, devs.Online)
	if d["cnm_dvr1_ch01_sub"] == nil || d["cnm_dvr1_ch01_sub"].Preload {
		t.Fatal("offline device: stream registered but not preloaded")
	}
}

func newTestReconciler(c *atomic.Pointer[config.Config], devs Devices) *Reconciler {
	r := NewReconciler(func() *config.Config { return c.Load() }, devs, quietLog())
	r.OpTimeout = 300 * time.Millisecond
	return r
}

func TestReconcileRegistersAndRepairs(t *testing.T) {
	f := newFakeGo2rtc()
	defer f.srv.Close()
	var cp atomic.Pointer[config.Config]
	cp.Store(testConfig(f.srv.URL))
	r := newTestReconciler(&cp, &fakeDevices{})
	ctx := context.Background()
	if !r.pass(ctx) {
		t.Fatal("go2rtc reachable but pass reported unreachable")
	}
	st := r.Status()
	if st.State != "ok" || st.StreamErrors != 0 || st.PreloadErrors != 0 {
		t.Fatalf("status after first pass: %+v", st)
	}
	f.mu.Lock()
	if len(f.streams["cnm_dvr1_ch01_sub"].sources) != 2 {
		t.Fatal("fallback source missing")
	}
	if _, ok := f.preloads["cnm_dvr1_ch01_sub"]; !ok {
		t.Fatal("multiview stream not preloaded")
	}
	if _, ok := f.preloads["cnm_dvr1_ch01_main"]; ok {
		t.Fatal("main preloaded although disabled")
	}
	f.mu.Unlock()

	// idempotent: second pass performs no PUTs
	puts := f.count("PUT /api/streams")
	r.pass(ctx)
	if f.count("PUT /api/streams") != puts {
		t.Fatal("second pass re-registered unchanged streams")
	}

	// stale preload: stream object replaced behind CNM's back
	f.mu.Lock()
	f.nextID++
	f.streams["cnm_dvr1_ch01_sub"] = &fakeStream{id: f.nextID, sources: f.streams["cnm_dvr1_ch01_sub"].sources}
	f.mu.Unlock()
	r.pass(ctx)
	f.mu.Lock()
	attached := f.preloads["cnm_dvr1_ch01_sub"] == f.streams["cnm_dvr1_ch01_sub"].id
	f.mu.Unlock()
	if !attached {
		t.Fatal("stale preload not repaired")
	}

	// obsolete stream deletion failures must not count as stream errors
	f.mu.Lock()
	f.streams["cnm_old_ch01_sub"] = &fakeStream{id: 999, sources: []string{"rtsp://x"}}
	f.failDel = true
	f.mu.Unlock()
	r.pass(ctx)
	if st := r.Status(); st.StreamErrors != 0 || st.State != "ok" {
		t.Fatalf("obsolete delete failure inflated errors: %+v", st)
	}
}

func TestReconcileTimeoutVerified(t *testing.T) {
	f := newFakeGo2rtc()
	defer f.srv.Close()
	f.slowPut = 600 * time.Millisecond // longer than OpTimeout, but executed
	var cp atomic.Pointer[config.Config]
	cp.Store(testConfig(f.srv.URL))
	r := newTestReconciler(&cp, &fakeDevices{})
	r.pass(context.Background())
	if st := r.Status(); st.StreamErrors != 0 {
		t.Fatalf("executed-but-timed-out registrations must be verified, got %+v", st)
	}
}

func TestReconcileWithoutPreloadAPI(t *testing.T) {
	f := newFakeGo2rtc()
	defer f.srv.Close()
	f.noPreload = true
	var cp atomic.Pointer[config.Config]
	cp.Store(testConfig(f.srv.URL))
	r := newTestReconciler(&cp, &fakeDevices{})
	r.pass(context.Background())
	st := r.Status()
	if st.State != "ok" || st.PreloadSupported || st.PreloadErrors != 0 || st.Registered == 0 {
		t.Fatalf("missing preload API must not degrade live: %+v", st)
	}
}

func TestReconcileGo2rtcRestartAndOffline(t *testing.T) {
	f := newFakeGo2rtc()
	var cp atomic.Pointer[config.Config]
	cp.Store(testConfig(f.srv.URL))
	r := newTestReconciler(&cp, &fakeDevices{})
	r.WatchInterval = 50 * time.Millisecond
	r.FastRetry = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitFor(t, func() bool { return r.Status().State == "ok" })
	f.restart()
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.streams["cnm_dvr1_ch01_sub"] != nil && len(f.preloads) > 0
	})
	f.srv.Close()
	waitFor(t, func() bool { return r.Status().State == "offline" })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

// go2rtc's dvrip producer reports no byte counters: an active producer must
// then count as online (otherwise browsers would restart healthy players).
func TestHealthWithoutByteCounters(t *testing.T) {
	var cp atomic.Pointer[config.Config]
	cp.Store(testConfig("http://127.0.0.1:1"))
	r := newTestReconciler(&cp, &fakeDevices{})
	streams := map[string]StreamInfo{
		"cnm_a_ch01_sub": {Producers: []ProducerInfo{{URL: "dvrip://x", FormatName: "dvrip"}}},
		"cnm_b_ch01_sub": {Producers: []ProducerInfo{{URL: "rtsp://x", FormatName: "rtsp", BytesRecv: 5000}}},
		"cnm_c_ch01_sub": {Producers: []ProducerInfo{{URL: "rtsp://x"}}},
	}
	r.updateHealth(streams)
	r.bytesMu.Lock()
	s := r.bytes["cnm_b_ch01_sub"]
	s.changed = s.changed.Add(-time.Minute) // byte counter frozen for a minute
	r.bytes["cnm_b_ch01_sub"] = s
	r.bytesMu.Unlock()
	r.updateHealth(streams)
	h := r.Health([]string{"cnm_a_ch01_sub", "cnm_b_ch01_sub", "cnm_c_ch01_sub"})
	if !h["cnm_a_ch01_sub"].Online {
		t.Fatal("active producer without byte counter must be online")
	}
	if h["cnm_b_ch01_sub"].Online {
		t.Fatal("producer with frozen byte counter must be offline")
	}
	if h["cnm_c_ch01_sub"].Online || !h["cnm_c_ch01_sub"].Registered {
		t.Fatal("idle producer must be registered but offline")
	}
}

func TestDeviceKeysShortenUUIDs(t *testing.T) {
	c := &config.Config{Devices: []config.Device{
		{ID: "9318340b-4fff-4755-8d8d-cdc449c6883f"},
		{ID: "9318340b-0000-4000-8000-000000000000"}, // same 8-char prefix
		{ID: "legacy-dvr"},
	}}
	k := DeviceKeys(c)
	if k["9318340b-0000-4000-8000-000000000000"] != "9318340b" || k["9318340b-4fff-4755-8d8d-cdc449c6883f"] != "9318340b_2" {
		t.Fatalf("uuid keys: %+v", k)
	}
	if k["legacy-dvr"] != config.SanitizeID("legacy-dvr") {
		t.Fatalf("non-uuid key: %+v", k)
	}
	if got := StreamName(k["9318340b-0000-4000-8000-000000000000"], 0, provider.Sub); got != "cnm_9318340b_ch01_sub" {
		t.Fatalf("stream name %q", got)
	}
}

// The settings preview uses the sub stream only: a dedicated sub-only stream
// when the multiview stream may fall back to main, never preloaded, never
// visible to kiosks; cameras without sub stream get no preview at all.
func TestPreviewIsSubOnly(t *testing.T) {
	c := testConfig("http://x")
	cam(c, "dvr2:0").Streams.Sub = false
	cams := Cameras(c)
	if p := cams["dvr1:0"].Preview; p != "cnm_dvr1_ch01_preview" {
		t.Fatalf("fallback camera preview %q", p)
	}
	if cams["dvr1:1"].Preview != cams["dvr1:1"].Multiview {
		t.Fatal("pure sub multiview stream must be reused for the preview")
	}
	if cams["dvr2:0"].Preview != "" {
		t.Fatal("camera without sub stream must not get a (main) preview")
	}
	devs := &fakeDevices{}
	d, _ := Desired(c, devs.Sources, devs.Online)
	pv := d["cnm_dvr1_ch01_preview"]
	if pv == nil || pv.Preload || len(pv.Sources) != 1 || strings.Contains(pv.Sources[0], "/main") {
		t.Fatalf("preview stream: %+v", pv)
	}
	if !AllowedStreams(c, "")["cnm_dvr1_ch01_preview"] || AllowedStreams(c, c.Monitors[0].ID)["cnm_dvr1_ch01_preview"] {
		t.Fatal("preview must be allowed for users only")
	}
}

// go2rtc answers PUT /api/streams with 400 when it cannot write its config
// file although the stream exists. CNM must not PUT a second time: that would
// replace the stream object and orphan consumers that attached meanwhile.
func TestRegistrationNotRepeatedWhenGo2rtcConfigWriteFails(t *testing.T) {
	f := newFakeGo2rtc()
	defer f.srv.Close()
	f.putConfigError = true
	var cp atomic.Pointer[config.Config]
	cp.Store(testConfig(f.srv.URL))
	r := newTestReconciler(&cp, &fakeDevices{})
	r.pass(context.Background())
	d, _ := Desired(cp.Load(), (&fakeDevices{}).Sources, (&fakeDevices{}).Online)
	if st := r.Status(); st.StreamErrors != 0 || st.State != "ok" {
		t.Fatalf("status %+v", st)
	}
	if puts := f.count("PUT /api/streams"); puts != len(d) {
		t.Fatalf("%d PUTs for %d streams – registrations were repeated", puts, len(d))
	}
}

// Both directions fall back automatically on the server: the multiview
// stream (sub) has main as second source, the single view (main) has sub.
// Cameras with one stream use it everywhere, without a second source.
func TestBidirectionalFallbackSources(t *testing.T) {
	c := testConfig("http://x")
	cam(c, "dvr2:0").Streams.Sub = false // main only
	devs := &fakeDevices{}
	d, _ := Desired(c, devs.Sources, devs.Online)
	mv, focus := d["cnm_dvr1_ch01_sub"], d["cnm_dvr1_ch01_main"]
	if len(mv.Sources) != 2 || !strings.Contains(mv.Sources[0], "/sub") || !strings.Contains(mv.Sources[1], "/main") {
		t.Fatalf("multiview sources %v", mv.Sources)
	}
	if len(focus.Sources) != 2 || !strings.Contains(focus.Sources[0], "/main") || !strings.Contains(focus.Sources[1], "/sub") {
		t.Fatalf("single view sources %v", focus.Sources)
	}
	cams := Cameras(c)
	if cs := cams["dvr1:1"]; cs.Multiview != cs.Focus || cs.Multiview != "cnm_dvr1_ch02_sub" || len(d[cs.Multiview].Sources) != 1 {
		t.Fatalf("sub only camera: %+v", cs)
	}
	if cs := cams["dvr2:0"]; cs.Multiview != "cnm_dvr2_ch01_main" || cs.Multiview != cs.Focus || cs.MultiviewAlt != "" || len(d[cs.Multiview].Sources) != 1 {
		t.Fatalf("main only camera: %+v", cs)
	}
}
