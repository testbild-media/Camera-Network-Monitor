package display

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/secret"
)

var mon24 = SynthMonitor{Vendor: "SIM", Product: 0xa0c5, Serial: 4242, Name: "SIM MONITOR", W: 1920, H: 1200, Clock: 154000, HTotal: 2080, VTotal: 1235}
var tv = SynthMonitor{Vendor: "SAM", Product: 0x0f00, Serial: 7, Name: "SAMSUNG", W: 3840, H: 2160, Clock: 594000, HTotal: 4400, VTotal: 2250, VICs: []int{16, 4, 31, 97, 95}}

func TestParseEDID(t *testing.T) {
	e, err := ParseEDID(SyntheticEDID(mon24))
	if err != nil {
		t.Fatal(err)
	}
	if e.Vendor != "SIM" || e.Name != "SIM MONITOR" || e.Serial != "00004242" || e.Product != 0xa0c5 {
		t.Fatalf("identity: %+v", e)
	}
	pref, ok := Largest(e.Modes)
	if !ok || !pref.Preferred || pref.String() != "1920x1200@59.950" {
		t.Fatalf("preferred: %+v", pref)
	}
	want := map[string]bool{"1920x1200@59.950": false, "1280x1024@60.020": false, "1024x768@60.004": false, "800x600@60.317": false, "640x480@59.940": false}
	for _, m := range e.Modes {
		if _, ok := want[m.String()]; ok {
			want[m.String()] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Fatalf("mode %s missing in %v", k, e.Modes)
		}
	}
	if e.Modes[0].String() != "1920x1200@59.950" {
		t.Fatalf("modes not sorted largest first: %v", e.Modes)
	}
}

func TestParseEDIDWithCTA(t *testing.T) {
	e, err := ParseEDID(SyntheticEDID(tv))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range e.Modes {
		got[m.String()] = true
	}
	for _, w := range []string{"3840x2160@60.000", "3840x2160@59.940", "3840x2160@30.000", "1920x1080@60.000", "1920x1080@59.940", "1920x1080@50.000", "1280x720@60.000"} {
		if !got[w] {
			t.Fatalf("%s missing: %v", w, e.Modes)
		}
	}
	if _, err := ParseEDID([]byte("garbage")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestModeParsingAndSelection(t *testing.T) {
	for in, want := range map[string]string{"1920x1080": "1920x1080", "1920x1080@60": "1920x1080@60.000", "1920x1080@59.94": "1920x1080@59.940", "1920x1080@60Hz": "1920x1080@60.000"} {
		m, ok := ParseMode(in)
		if !ok || m.String() != want {
			t.Fatalf("%s → %s", in, m)
		}
	}
	for _, bad := range []string{"x", "1920", "1920x1080@", "10x10", "1920x1080@60.0000"} {
		if _, ok := ParseMode(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
	list := []Mode{{1920, 1080, 60000, false}, {1920, 1080, 59940, false}, {1280, 720, 60000, false}}
	if m, _ := Best(list, Mode{Width: 1920, Height: 1080, RefreshMHz: 59900}); m.RefreshMHz != 59940 {
		t.Fatal("nearest refresh")
	}
	if m, _ := Best(list, Mode{Width: 1920, Height: 1080}); m.RefreshMHz != 60000 {
		t.Fatal("highest refresh")
	}
	if _, ok := Best(list, Mode{Width: 800, Height: 600}); ok {
		t.Fatal("missing size matched")
	}
	if (Mode{1920, 1080, 59940, false}).Label() != "1920 × 1080 @ 59.94 Hz" || (Mode{1280, 720, 60000, false}).Label() != "1280 × 720 @ 60 Hz" {
		t.Fatal("labels")
	}
}

func TestScanSysfs(t *testing.T) {
	root := t.TempDir()
	if err := WriteSysfsFixture(root, "card0", map[string]*SynthMonitor{"HDMI-A-1": &mon24, "HDMI-A-10": nil, "DP-1": &tv, "HDMI-A-2": nil}); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(root, "card0-Writeback-1"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "renderD128"), 0o755)
	// a mode only the kernel knows about
	_ = os.WriteFile(filepath.Join(root, "card0-HDMI-A-1", "modes"), []byte("1920x1200\n1600x900\n"), 0o644)
	conns, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range conns {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "DP-1,HDMI-A-1,HDMI-A-2,HDMI-A-10" {
		t.Fatalf("order/filter: %v", names)
	}
	h := conns[1]
	if !h.Connected || h.EDID == nil || h.EDID.Name != "SIM MONITOR" || h.Card != "card0" {
		t.Fatalf("connector: %+v", h)
	}
	found := false
	for _, m := range h.Modes {
		if m.String() == "1600x900" {
			found = true
		}
	}
	if !found {
		t.Fatalf("kernel-only mode missing: %v", h.Modes)
	}
	if conns[2].Connected || len(conns[2].Modes) != 0 {
		t.Fatalf("disconnected: %+v", conns[2])
	}
	if c, err := Scan(filepath.Join(root, "missing")); err != nil || len(c) != 0 {
		t.Fatal("missing sysfs must yield an empty list")
	}
}

func hostRig(t *testing.T) (*Host, *config.Store, string) {
	keys, _ := secret.NewKeyring(secret.GenerateKey())
	c := config.Default(t.TempDir())
	token := secret.GenerateToken()
	enc, _ := keys.Encrypt(token)
	encJSON, _ := json.Marshal(enc)
	c.Displays = []config.Display{
		{ID: config.NewUUID(), Connector: "HDMI-A-1", Enabled: true, MonitorID: c.Monitors[0].ID, Mode: "1920x1080@60.000", TokenHash: secret.HashToken(token), TokenEnc: encJSON},
		{ID: config.NewUUID(), Connector: "DP-1", Enabled: false},
	}
	config.Normalize(c, "")
	store := config.NewMemoryStore(c)
	down := time.Now().Add(-2 * time.Minute)
	h := NewHost(store, keys, func(keyID string) (int, time.Time) { return 0, down }, func() string { return "http://127.0.0.1:8090" }, nil)
	h.started = time.Now().Add(-10 * time.Minute)
	h.SysfsRoot = filepath.Join(t.TempDir(), "none")
	return h, store, token
}

func TestHostDesired(t *testing.T) {
	h, store, token := hostRig(t)
	d := h.Desired()
	if len(d.Outputs) != 2 {
		t.Fatalf("outputs: %+v", d.Outputs)
	}
	o := d.Outputs[0]
	mon := store.Get().Monitors[0]
	if !o.Enabled || !strings.Contains(o.URL, "key="+token) || !strings.Contains(o.URL, "mon="+mon.ID) || !strings.HasPrefix(o.URL, "http://127.0.0.1:8090/viewer?") || !strings.Contains(o.URL, "display=HDMI-A-1") {
		t.Fatalf("enabled output: %+v", o)
	}
	if o.ViewerDownMs < 110000 || o.MonitorLabel != mon.Label || o.Mode != "1920x1080@60.000" {
		t.Fatalf("viewer presence / labels: %+v", o)
	}
	if d.Outputs[1].Enabled || d.Outputs[1].URL != "" {
		t.Fatal("disabled display must not carry a credential")
	}
	// a disabled monitor switches the display off
	_, _ = store.Update(func(c *config.Config) error { c.Monitors[0].Enabled = false; return nil })
	if h.Desired().Outputs[0].Enabled {
		t.Fatal("display of a disabled monitor stays on")
	}
	// reload counter
	id := store.Get().Displays[0].ID
	h.Reload(id)
	if h.Desired().Outputs[0].ReloadSeq != 1 {
		t.Fatal("reload not published")
	}
}

func TestHostLongPollAndStatus(t *testing.T) {
	h, store, _ := hostRig(t)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	get := func(rev int64) (Desired, time.Duration) {
		start := time.Now()
		resp, err := http.Get(srv.URL + "/v1/desired?rev=" + itoa(rev) + "&epoch=" + h.epoch)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d Desired
		_ = json.NewDecoder(resp.Body).Decode(&d)
		return d, time.Since(start)
	}
	d, took := get(0)
	if took > time.Second || d.Rev == 0 {
		t.Fatal("unknown revision must return immediately")
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = store.Update(func(c *config.Config) error { c.Displays[0].Mode = ""; return nil })
	}()
	d2, took := get(d.Rev)
	if d2.Rev == d.Rev || took < 150*time.Millisecond || took > 5*time.Second || d2.Outputs[0].Mode != "" {
		t.Fatalf("long poll: rev %d→%d after %v", d.Rev, d2.Rev, took)
	}
	// another CNM instance (epoch) answers immediately even with the same revision
	start := time.Now()
	resp, err := http.Get(srv.URL + "/v1/desired?rev=" + itoa(d2.Rev) + "&epoch=old")
	if err != nil || time.Since(start) > time.Second {
		t.Fatalf("foreign epoch must not wait: %v", err)
	}
	resp.Body.Close()
	// status upload
	body := `{"version":"x","compositor":{"state":"running","version":"1.9"},"outputs":[{"name":"HDMI-A-1","active":true,"modes":[]}],"displays":[]}`
	resp, err = http.Post(srv.URL+"/v1/status", "application/json", strings.NewReader(body))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("status post: %v %v", err, resp)
	}
	resp.Body.Close()
	st, at := h.Agent()
	if st == nil || st.Compositor.Version != "1.9" || time.Since(at) > time.Second || !h.AgentConnected() {
		t.Fatalf("status not stored: %+v", st)
	}
	// long poll ends with the request context
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/desired?rev="+itoa(d2.Rev)+"&epoch="+h.epoch, nil)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected client timeout")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestConnectorsLimitedToDisplayGPU(t *testing.T) {
	root := t.TempDir()
	if err := WriteSysfsFixture(root, "card0", map[string]*SynthMonitor{"VGA-1": &mon24}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSysfsFixture(root, "card1", map[string]*SynthMonitor{"HDMI-A-1": &tv, "DP-1": nil}); err != nil {
		t.Fatal(err)
	}
	for card, pci := range map[string]string{"card0": "0000:03:00.0", "card1": "0000:01:00.0"} {
		dev := filepath.Join(root, "devices", pci)
		_ = os.MkdirAll(dev, 0o755)
		if err := os.Symlink(dev, filepath.Join(root, card, "device")); err != nil {
			t.Skip("symlinks not available:", err)
		}
	}
	h := &Host{SysfsRoot: root}
	all, _ := h.Connectors()
	if len(all) != 3 || all[0].PCI != "0000:03:00.0" {
		t.Fatalf("all connectors: %+v", all)
	}
	h.GPU = "pci-0000:01:00.0"
	only, _ := h.Connectors()
	if len(only) != 2 || only[0].Name != "DP-1" || only[1].Name != "HDMI-A-1" {
		t.Fatalf("display GPU filter: %+v", only)
	}
	if NormalizePCI("01:00.0") != "0000:01:00.0" {
		t.Fatal("short PCI address")
	}
}
