package device

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/secret"
	"cnm/internal/sim"

	_ "cnm/internal/provider/dahua"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func setup(t *testing.T, pass string) (*Manager, *config.Store, *sim.DahuaServer, *secret.Keyring) {
	t.Helper()
	srv := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 8, HasDisk: true}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	keys, _ := secret.NewKeyring(secret.GenerateKey())
	enc, _ := keys.Encrypt(pass)
	encJSON, _ := json.Marshal(enc)
	_, p, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(p)
	c := config.Default(t.TempDir())
	c.System.HealthIntervalMs = 5000
	c.System.ReconnectMinMs = 1000
	c.System.ReconnectMaxMs = 2000
	c.Devices = []config.Device{{ID: "dvr1", Label: "DVR", Protocol: "dahua", Detected: &config.Detection{Variant: "rpc2-dahua3"}, Host: "127.0.0.1",
		Credentials: config.Credentials{Username: "admin", Password: encJSON}, Ports: map[string]int{"http": port, "rtsp": 554}, ChannelCount: 8}}
	config.Normalize(c, t.TempDir())
	store := config.NewMemoryStore(c)
	m := NewManager(store, keys, nil, quiet())
	m.Start()
	t.Cleanup(func() { m.Close(t.Context()) })
	return m, store, srv, keys
}

func waitState(t *testing.T, m *Manager, want State) Status {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := m.Status("dvr1"); ok && st.State == want {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := m.Status("dvr1")
	t.Fatalf("state %q not reached, have %+v", want, st)
	return st
}

func TestConnectAndConfigChangeWithoutReconnect(t *testing.T) {
	m, store, srv, _ := setup(t, "pw123456")
	st := waitState(t, m, Connected)
	if st.Info == nil || st.Info.Serial != "SIM0001" {
		t.Fatalf("info missing: %+v", st)
	}
	logins := srv.Logins.Load()
	// label / channel presentation changes must not reconnect the device
	store.Update(func(c *config.Config) error {
		c.Devices[0].Label = "Neu"
		c.Cameras[0].View.AspectRatio = config.Aspect4x3
		c.Cameras[0].Label = "Tor"
		c.Monitors[0].Layout.Rows[0].Slots[0].CameraID = c.Cameras[0].ID
		return nil
	})
	time.Sleep(300 * time.Millisecond)
	if srv.Logins.Load() != logins {
		t.Fatal("layout/label change caused a DVR re-login")
	}
	if !m.Online("dvr1") || len(m.Sources("dvr1", 0, "sub")) != 1 {
		t.Fatal("device must stay online with sources")
	}
}

func TestAuthFailureIsNotRetried(t *testing.T) {
	m, store, srv, keys := setup(t, "wrong-password")
	st := waitState(t, m, Error)
	if !st.AuthLocked || st.ErrorKind != "auth" {
		t.Fatalf("expected auth lock: %+v", st)
	}
	time.Sleep(3 * time.Second) // > reconnect max: no automatic retries may happen
	if n := srv.FailedLogins.Load(); n != 1 {
		t.Fatalf("device saw %d failed logins, want exactly 1", n)
	}
	if m.Sources("dvr1", 0, "sub") != nil {
		t.Fatal("locked device must not provide media sources")
	}
	// manual reconnect: exactly one more attempt
	m.Reconnect("dvr1")
	time.Sleep(1500 * time.Millisecond)
	if n := srv.FailedLogins.Load(); n != 2 {
		t.Fatalf("manual reconnect: %d failed logins, want 2", n)
	}
	// fixing the password (config change) reconnects immediately
	enc, _ := keys.Encrypt("pw123456")
	b, _ := json.Marshal(enc)
	store.Update(func(c *config.Config) error { c.Devices[0].Credentials.Password = b; return nil })
	waitState(t, m, Connected)
}

func TestNetworkErrorsRetryWithBackoff(t *testing.T) {
	m, _, srv, _ := setup(t, "pw123456")
	waitState(t, m, Connected)
	srv.Close() // device disappears
	m.Reconnect("dvr1")
	st := waitState(t, m, Disconnected)
	if st.ErrorKind != "network" || st.NextRetry == nil || st.AuthLocked {
		t.Fatalf("network failure must be retried: %+v", st)
	}
}

// Only protocol and address configured: the first connection detects the
// variant and the channels, stores them (cameras appear) without a reconnect,
// and later connections use the stored variant directly.
func TestDetectionStoredAndCamerasCreated(t *testing.T) {
	srv := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 8, HasDisk: true}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	keys, _ := secret.NewKeyring(secret.GenerateKey())
	enc, _ := keys.Encrypt("pw123456")
	encJSON, _ := json.Marshal(enc)
	_, p, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(p)
	c := config.Default(t.TempDir())
	c.Devices = []config.Device{{ID: "dvr1", Label: "DVR", Protocol: "dahua", Host: "127.0.0.1",
		Credentials: config.Credentials{Username: "admin", Password: encJSON}, Ports: map[string]int{"http": port, "rtsp": 554}}}
	config.Normalize(c, t.TempDir())
	if len(c.Cameras) != 0 {
		t.Fatal("no cameras before the detection")
	}
	store := config.NewMemoryStore(c)
	m := NewManager(store, keys, nil, quiet())
	m.Start()
	t.Cleanup(func() { m.Close(t.Context()) })
	waitState(t, m, Connected)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && store.Get().Devices[0].Detected == nil {
		time.Sleep(20 * time.Millisecond)
	}
	d := store.Get().Devices[0]
	if d.Detected == nil || d.Detected.Variant != "rpc2-dahua3" || d.Detected.Channels != 8 || d.ChannelCount != 8 || d.Detected.Summary == "" {
		t.Fatalf("detection not stored: %+v %+v", d, d.Detected)
	}
	if n := len(store.Get().DeviceCameras("dvr1")); n != 8 {
		t.Fatalf("%d cameras after the detection", n)
	}
	logins := srv.Logins.Load()
	time.Sleep(500 * time.Millisecond)
	if srv.Logins.Load() != logins || !m.Online("dvr1") {
		t.Fatal("storing the detection reconnected the device")
	}
	// a reconnect uses the stored variant: one login, no rejected attempt
	m.Reconnect("dvr1")
	time.Sleep(800 * time.Millisecond)
	waitState(t, m, Connected)
	if srv.Rejected.Load() != 0 {
		t.Fatal("rejected logins")
	}
}
