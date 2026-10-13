package dahua

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

	"cnm/internal/provider"
	"cnm/internal/sim"
)

type env struct{ locked atomic.Bool }

func (e *env) AllowLogin() bool                              { return !e.locked.Load() }
func (e *env) ReportAuthFailure(error)                       { e.locked.Store(true) }
func (e *env) LegacyLiveURL(int, provider.StreamKind) string { return "" }

func port(addr string) (string, int) {
	h, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

func startDahua(t *testing.T) *sim.DahuaServer {
	d := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 8, HasDisk: true, Recordings: sim.QuarterSchedule}
	if err := d.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func dahuaProto() provider.Model {
	m, _ := prov{}.Info().FindModel(ProtoDahua)
	return m
}

// newRPC is an RPC2 driver with the verified "Dahua3.0-Web3.0" login.
func newRPC(t *testing.T, addr, pass string) (*rpcDriver, *env) {
	h, p := port(addr)
	e := &env{}
	return newRPCDriver(provider.DeviceConfig{ID: "d", Model: dahuaProto(), Host: h, Username: "admin", Password: pass, Ports: map[string]int{"http": p, "rtsp": 554}}, e, VariantRPC2Classic), e
}

func TestRPC2LoginHealthSearch(t *testing.T) {
	srv := startDahua(t)
	d, _ := newRPC(t, srv.Addr(), "pw123456")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := d.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Serial != "SIM0001" || info.ChannelNames[2] != "SIM CAM 3" || info.Channels != 8 {
		t.Fatalf("info: %+v", info)
	}
	h, err := d.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// the device reports local wall clock: offset = time zone offset (+ drift)
	_, zone := time.Now().Zone()
	diff := *h.ClockOffset - time.Duration(zone)*time.Second
	if h.Storage != provider.StoragePresent || h.ClockOffset == nil || diff > 2*time.Second || diff < -2*time.Second {
		t.Fatalf("health: %+v", h)
	}
	to := time.Now().Truncate(time.Minute)
	from := to.Add(-time.Hour)
	recs, err := d.SearchRecordings(ctx, 0, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 3 || len(recs) > 5 {
		t.Fatalf("expected the quarter schedule, got %d ranges", len(recs))
	}
	for _, r := range recs {
		if r.End.Sub(r.Start) > 10*time.Minute+time.Second || r.Start.Before(from) || r.End.After(to) {
			t.Fatalf("bad range %v–%v", r.Start, r.End)
		}
	}
	// session loss (another client logged in) is healed with one re-login
	other, _ := newRPC(t, srv.Addr(), "pw123456")
	if _, err := other.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Check(ctx); err != nil {
		t.Fatalf("re-login after session loss failed: %v", err)
	}
	src, _ := d.OpenPlayback(ctx, 1, from, to)
	if !strings.Contains(src.URL, "/cam/playback?channel=2&subtype=0&starttime=") {
		t.Fatalf("playback url: %s", provider.Redact(src.URL))
	}
}

func TestRPC2WrongPasswordLocks(t *testing.T) {
	srv := startDahua(t)
	d, e := newRPC(t, srv.Addr(), "falsch")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		_, err := d.Connect(ctx)
		if provider.KindOf(err) != provider.KindAuth {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if !e.locked.Load() || srv.FailedLogins.Load() != 1 {
		t.Fatalf("auth failure must lock after the first attempt (failed logins on device: %d)", srv.FailedLogins.Load())
	}
}

func TestLoginHashes(t *testing.T) {
	// OldDigest is the 8 char legacy hash; Default is the two stage MD5.
	if len(oldDigest("admin")) != 8 {
		t.Fatal("old digest length")
	}
	if loginHash("Default", "admin", "admin", "Login to X", "123") != upperMD5("admin:123:"+upperMD5("admin:Login to X:admin")) {
		t.Fatal("default hash")
	}
}

func TestRTSPProbeAuth(t *testing.T) {
	clip := sim.Synthetic(10)
	r := &sim.RTSPServer{User: "admin", Pass: "pw123456", Clip: func(int, int) *sim.Clip { return clip }}
	if err := r.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	h, p := port(r.Addr())
	ctx := context.Background()
	if err := provider.RTSPProbe(ctx, h, p, "admin", "pw123456", nativeProbePath); err != nil {
		t.Fatalf("valid credentials rejected: %v", err)
	}
	err := provider.RTSPProbe(ctx, h, p, "admin", "nope", nativeProbePath)
	if provider.KindOf(err) != provider.KindAuth {
		t.Fatalf("expected auth error, got %v", err)
	}
	// native driver: auth failure locks, no further probes reach the device
	e := &env{}
	d := newNativeDriver(provider.DeviceConfig{Model: dahuaProto(), Host: h, Username: "admin", Password: "nope", Ports: map[string]int{"rtsp": p}}, e)
	_, _ = d.Connect(ctx)
	before := r.AuthFails.Load()
	_, err = d.Connect(ctx)
	if !e.locked.Load() || r.AuthFails.Load() != before || provider.KindOf(err) != provider.KindAuth {
		t.Fatal("native driver retried after auth failure")
	}
}

func TestCapabilitiesPerVariant(t *testing.T) {
	for _, tc := range []struct {
		variant        string
		playback, time provider.Support
	}{
		{"", provider.Verified, provider.Verified}, // not detected yet: the protocol's
		{VariantRPC2Classic, provider.Verified, provider.Verified},
		{VariantRPC2Form, provider.Verified, provider.Verified},
		{VariantRPC2Direct, provider.Experimental, provider.Experimental},
		{VariantNative, provider.Verified, provider.Unsupported},
	} {
		if c := provider.DeviceCapabilities(prov{}, dahuaProto(), tc.variant); c.Playback != tc.playback || c.DeviceTime != tc.time || !c.Download.Enabled() {
			t.Fatalf("%q: %+v", tc.variant, c)
		}
	}
}

func startNative(t *testing.T) (*sim.DahuaNativeServer, *nativeDriver, *env, time.Time) {
	t.Helper()
	now := sim.DeviceNow()
	n := &sim.DahuaNativeServer{User: "admin", Pass: "native-pass-1", Clip: sim.Synthetic(30),
		Recorded: func(int) []sim.NativeCluster {
			return []sim.NativeCluster{{Start: now.Add(-50 * time.Minute), End: now.Add(-30 * time.Minute)}, {Start: now.Add(-20 * time.Minute), End: now.Add(-5 * time.Minute)}}
		}}
	if err := n.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	h, p := port(n.Addr())
	e := &env{}
	d := newNativeDriver(provider.DeviceConfig{Model: dahuaProto(), Host: h, Username: "admin", Password: "native-pass-1", Ports: map[string]int{"native": p}}, e)
	t.Cleanup(d.Close)
	return n, d, e, now
}

func TestNativeRecordings(t *testing.T) {
	n, d, _, now := startNative(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	real := d.clock.FromDevice
	recs, err := d.SearchRecordings(ctx, 1, real(now.Add(-time.Hour)), real(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].End.Sub(recs[0].Start) != 20*time.Minute || recs[1].End.Sub(recs[1].Start) != 15*time.Minute {
		t.Fatalf("clusters: %+v", recs)
	}
	if has, _ := d.HasRecordings(ctx, 1, real(now.Add(-28*time.Minute)), real(now.Add(-22*time.Minute))); has {
		t.Fatal("gap reported as recorded")
	}
	src, err := d.OpenPlayback(ctx, 1, real(now.Add(-15*time.Minute)), real(now.Add(-14*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(src.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if src.Format != "dhav" || !bytes.HasPrefix(b, []byte("DHAV")) || bytes.Count(b, []byte("DHAV")) != 60*10 {
		t.Fatalf("DHAV stream: %d frames", bytes.Count(b, []byte("DHAV")))
	}
	src.Reader.Close()
	for i := 0; i < 100 && n.Deletes.Load() == 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if n.Logins.Load() != 1 || n.Downloads.Load() != 1 || n.Stops.Load() < 2 || n.Deletes.Load() != 1 {
		t.Fatalf("logins %d downloads %d stops %d deletes %d", n.Logins.Load(), n.Downloads.Load(), n.Stops.Load(), n.Deletes.Load())
	}
	// no recording in the gap
	if _, err := d.OpenPlayback(ctx, 1, real(now.Add(-28*time.Minute)), real(now.Add(-22*time.Minute))); provider.KindOf(err) != provider.KindNotFound {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestNativeWrongPasswordDoesNotLockLive(t *testing.T) {
	n, d, e, now := startNative(t)
	d.cfg.Password = "wrong"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := d.SearchRecordings(ctx, 0, d.clock.FromDevice(now.Add(-time.Hour)), d.clock.FromDevice(now))
	if provider.KindOf(err) != provider.KindAuth || e.locked.Load() || n.Failed.Load() != 1 {
		t.Fatalf("err %v locked %v", err, e.locked.Load())
	}
}

// detect runs the family driver like the supervisor does for a new device.
func detect(t *testing.T, cfg provider.DeviceConfig) (*provider.DeviceInfo, *familyDriver, *env, error) {
	t.Helper()
	cfg.Model = dahuaProto()
	e := &env{}
	drv, _ := prov{}.NewDriver(cfg, e)
	d := drv.(*familyDriver)
	t.Cleanup(d.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := d.Connect(ctx)
	return info, d, e, err
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, p := port(ln.Addr().String())
	ln.Close()
	return p
}

// Only the IP and "Dahua": CNM finds RPC2 and the verified login itself,
// the channel count comes from the device.
func TestDetectRPC2(t *testing.T) {
	srv := startDahua(t)
	h, p := port(srv.Addr())
	info, d, _, err := detect(t, provider.DeviceConfig{Host: h, Username: "admin", Password: "pw123456", Ports: map[string]int{"http": p, "rtsp": 554}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Variant != VariantRPC2Classic || info.Channels != 8 || !strings.Contains(info.Summary, "RPC2 über HTTP") {
		t.Fatalf("%+v", info)
	}
	if srv.Rejected.Load() != 0 {
		t.Fatal("a verified variant must not cost a rejected login")
	}
	to := time.Now().Truncate(time.Minute)
	if recs, err := d.SearchRecordings(context.Background(), 0, to.Add(-time.Hour), to); err != nil || len(recs) < 3 {
		t.Fatalf("search through the detected driver: %d %v", len(recs), err)
	}
}

// Current firmware rejects the classic web client: the next variant is
// used, at the cost of one rejected login.
func TestDetectRPC2CurrentFirmware(t *testing.T) {
	srv := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 4, HasDisk: true, Recordings: sim.QuarterSchedule, Web3: true}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	h, p := port(srv.Addr())
	info, d, _, err := detect(t, provider.DeviceConfig{Host: h, Username: "admin", Password: "pw123456", Ports: map[string]int{"http": p, "rtsp": 554}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Variant != VariantRPC2Direct || info.Channels != 4 || srv.Rejected.Load() != 1 {
		t.Fatalf("%+v rejected %d", info, srv.Rejected.Load())
	}
	if hl, err := d.Check(context.Background()); err != nil || hl.Storage != provider.StoragePresent {
		t.Fatalf("health %+v %v", hl, err)
	}
	// a stored variant connects directly (no rejected attempt)
	_, _, _, err = detect(t, provider.DeviceConfig{Host: h, Username: "admin", Password: "pw123456", Ports: map[string]int{"http": p, "rtsp": 554}, Variant: VariantRPC2Direct, Channels: 4})
	if err != nil || srv.Rejected.Load() != 1 {
		t.Fatalf("stored variant: %v, rejected %d", err, srv.Rejected.Load())
	}
}

// A wrong password costs at most two rejected logins (recorders lock
// accounts after a few failures) and locks the device in CNM.
func TestDetectWrongPasswordBounded(t *testing.T) {
	srv := startDahua(t)
	h, p := port(srv.Addr())
	_, _, e, err := detect(t, provider.DeviceConfig{Host: h, Username: "admin", Password: "falsch", Ports: map[string]int{"http": p, "rtsp": 554}})
	if provider.KindOf(err) != provider.KindAuth || !e.locked.Load() {
		t.Fatalf("%v locked=%v", err, e.locked.Load())
	}
	if n := srv.Rejected.Load(); n == 0 || n > maxDetectFailures {
		t.Fatalf("%d rejected logins", n)
	}
}

// Without RPC2 (no HTTP control) the native protocol is used: live and the
// credential check via RTSP, channels counted over RTSP, no native login.
func TestDetectNative(t *testing.T) {
	n := &sim.DahuaNativeServer{User: "admin", Pass: "native-pass-1", Clip: sim.Synthetic(10), Recorded: func(int) []sim.NativeCluster { return nil }}
	if err := n.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	clip := sim.Synthetic(10)
	r := &sim.RTSPServer{User: "admin", Pass: "native-pass-1", Clip: func(ch, _ int) *sim.Clip {
		if ch < 1 || ch > 4 {
			return nil
		}
		return clip
	}}
	if err := r.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	h, np := port(n.Addr())
	_, rp := port(r.Addr())
	info, _, _, err := detect(t, provider.DeviceConfig{Host: h, Username: "admin", Password: "native-pass-1", Ports: map[string]int{"http": freePort(t), "native": np, "rtsp": rp}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Variant != VariantNative || info.Channels != 4 || !strings.Contains(info.Summary, "nativ") || n.Logins.Load() != 0 {
		t.Fatalf("%+v native logins %d", info, n.Logins.Load())
	}
}

// Nothing Dahua-like answers: a network error (retried) naming both ports.
func TestDetectNothing(t *testing.T) {
	_, _, e, err := detect(t, provider.DeviceConfig{Host: "127.0.0.1", Username: "admin", Password: "x", Ports: map[string]int{"http": freePort(t), "native": freePort(t), "rtsp": 554}})
	if provider.KindOf(err) != provider.KindNetwork || e.locked.Load() || !strings.Contains(err.Error(), "HTTP-Port") || !strings.Contains(err.Error(), "nativer Port") {
		t.Fatalf("%v", err)
	}
}
