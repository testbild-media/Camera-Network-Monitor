package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/display"
)

func displayRig(t *testing.T) (*rig, *client, *display.Host) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	root := filepath.Join(t.TempDir(), "drm")
	mon24 := display.SynthMonitor{Vendor: "SIM", Serial: 99, Name: "SIM MONITOR", W: 1920, H: 1200, Clock: 154000, HTotal: 2080, VTotal: 1235}
	if err := display.WriteSysfsFixture(root, "card0", map[string]*display.SynthMonitor{"HDMI-A-1": &mon24, "HDMI-A-2": nil}); err != nil {
		t.Fatal(err)
	}
	h := display.NewHost(r.store, r.keys, r.srv.Hub.Presence, func() string { return r.http.URL }, quiet())
	h.SysfsRoot = root
	r.srv.Displays = h
	return r, admin, h
}

func portOf(body map[string]any, name string) map[string]any {
	for _, p := range body["ports"].([]any) {
		if m := p.(map[string]any); m["connector"] == name {
			return m
		}
	}
	return nil
}

func agentStatus(t *testing.T, h *display.Host, st display.Status) display.Desired {
	b, _ := json.Marshal(st)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/status", bytes.NewReader(b)))
	var d display.Desired
	if err := json.NewDecoder(rec.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLocalDisplaysAPI(t *testing.T) {
	r, admin, h := displayRig(t)

	// detection from sysfs/EDID
	code, body := admin.do("GET", "/api/settings/displays", nil)
	if code != 200 || body["supported"] != true {
		t.Fatalf("list: %d %v", code, body)
	}
	p := portOf(body, "HDMI-A-1")
	if p == nil || p["connected"] != true || p["screen"] != "SIM MONITOR" || p["display"] != nil {
		t.Fatalf("port: %v", p)
	}
	modes := p["modes"].([]any)
	if first := modes[0].(map[string]any); first["value"] != "1920x1200@59.950" || first["preferred"] != true {
		t.Fatalf("modes: %v", modes)
	}
	if q := portOf(body, "HDMI-A-2"); q == nil || q["connected"] != false {
		t.Fatalf("disconnected port: %v", q)
	}

	// validation
	for _, bad := range []map[string]any{
		{"connector": "HDMI-A-1", "enabled": true},                                           // no multiviewer
		{"connector": "HDMI A 1", "enabled": false},                                          // invalid name
		{"connector": "HDMI-A-1", "enabled": true, "monitorId": "m1", "mode": "huge"},        // invalid mode
		{"connector": "HDMI-A-1", "enabled": true, "monitorId": "m1", "rotation": 45},        // invalid rotation
		{"connector": "HDMI-A-1", "enabled": true, "monitorId": "nope", "mode": "1920x1080"}, // unknown monitor
	} {
		if code, _ := admin.do("POST", "/api/settings/displays", bad); code != 400 {
			t.Fatalf("accepted %v: %d", bad, code)
		}
	}

	// assign multiviewer m1 with an explicit mode
	code, saved := admin.do("POST", "/api/settings/displays", map[string]any{"connector": "HDMI-A-1", "enabled": true, "monitorId": "m1", "mode": "1920x1200@59.95", "label": "Wand links"})
	if code != 200 || saved["mode"] != "1920x1200@59.950" || saved["id"] == "" {
		t.Fatalf("save: %d %v", code, saved)
	}
	id := saved["id"].(string)
	d, _ := r.store.Get().Display(id)
	if d.TokenHash == "" || len(d.TokenEnc) == 0 {
		t.Fatal("display credential not created")
	}
	// the credential never reaches the browser
	for _, path := range []string{"/api/settings", "/api/settings/displays"} {
		_, b := admin.do("GET", path, nil)
		raw, _ := json.Marshal(b)
		if strings.Contains(string(raw), "tokenHash") || strings.Contains(string(raw), "tokenEnc") {
			t.Fatalf("%s leaks the display credential", path)
		}
	}

	// the agent gets the kiosk URL (only on the socket) …
	des := h.Desired()
	if len(des.Outputs) != 1 || !des.Outputs[0].Enabled || des.Outputs[0].Mode != "1920x1200@59.950" {
		t.Fatalf("desired: %+v", des)
	}
	u, err := url.Parse(des.Outputs[0].URL)
	if err != nil || u.Query().Get("mon") != "m1" || u.Query().Get("display") != "HDMI-A-1" {
		t.Fatalf("kiosk url: %v", des.Outputs[0].URL)
	}
	token := u.Query().Get("key")
	// … and the viewer signs in with it as a kiosk of exactly that monitor
	k := r.client().remote()
	if st := k.kioskLogin("m2", token); st != 403 {
		t.Fatalf("display credential opened another monitor: %d", st)
	}
	if st := k.kioskLogin("m1", token); st != 200 {
		t.Fatalf("display kiosk login: %d", st)
	}
	_, me := k.do("GET", "/api/auth/me", nil)
	pr := me["principal"].(map[string]any)
	if pr["kind"] != "kiosk" || pr["role"] != "viewer" || pr["username"] != "Display HDMI-A-1" {
		t.Fatalf("principal: %v", pr)
	}
	if code, _ := k.do("GET", "/api/settings", nil); code != 403 {
		t.Fatal("display kiosk reached settings")
	}

	// identify needs a connected viewer
	if code, _ := admin.do("POST", "/api/settings/displays/"+id+"/identify", nil); code != 409 {
		t.Fatalf("identify without viewer: %d", code)
	}
	ws, err := k.dialWS("/ws", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	waitFor(t, func() bool { n, _ := r.srv.Hub.Presence(config.DisplayKeyPrefix + id); return n == 1 })
	if code, _ := admin.do("POST", "/api/settings/displays/"+id+"/identify", nil); code != 200 {
		t.Fatalf("identify: %d", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	found := false
	for !found {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("identify event not received: %v", err)
		}
		found = strings.Contains(string(data), `"type":"identify"`) && strings.Contains(string(data), "Wand links · HDMI-A-1")
	}
	if des := h.Desired(); des.Outputs[0].ViewerDownMs != 0 {
		t.Fatalf("viewer presence not reported: %d", des.Outputs[0].ViewerDownMs)
	}

	// reload needs the agent
	if code, _ := admin.do("POST", "/api/settings/displays/"+id+"/reload", nil); code != 409 {
		t.Fatalf("reload without agent: %d", code)
	}
	agentStatus(t, h, display.Status{Version: "t", Compositor: display.CompositorStatus{State: "running", Version: "1.9"},
		Outputs:  []display.OutputStatus{{Name: "HDMI-A-1", Active: true, Modes: []display.Mode{{Width: 1920, Height: 1200, RefreshMHz: 59950}}}},
		Displays: []display.DisplayStatus{{DisplayID: id, Connector: "HDMI-A-1", State: display.StateRunning}}})
	if code, _ := admin.do("POST", "/api/settings/displays/"+id+"/reload", nil); code != 200 {
		t.Fatalf("reload: %d", code)
	}
	if h.Desired().Outputs[0].ReloadSeq != 1 {
		t.Fatal("reload not forwarded to the agent")
	}
	_, body = admin.do("GET", "/api/settings/displays", nil)
	p = portOf(body, "HDMI-A-1")
	if body["agent"].(map[string]any)["connected"] != true || p["status"].(map[string]any)["state"] != "running" || p["viewerConnected"] != true {
		t.Fatalf("agent status not merged: %v", p)
	}

	// switching the multiviewer ends the old kiosk session immediately
	if code, _ := admin.do("POST", "/api/settings/displays", map[string]any{"connector": "HDMI-A-1", "enabled": true, "monitorId": "m2"}); code != 200 {
		t.Fatal("reassign")
	}
	if code, _ := k.do("GET", "/api/state", nil); code != 401 {
		t.Fatalf("old display session still valid: %d", code)
	}
	if des := h.Desired(); !strings.Contains(des.Outputs[0].URL, "mon=m2") || !strings.Contains(des.Outputs[0].URL, "key="+url.QueryEscape(token)) {
		t.Fatal("credential must survive a reassignment")
	}

	// options; system settings keep them
	if code, _ := admin.do("POST", "/api/settings/displays/options", map[string]any{"browser": "firefox", "dailyRestart": "25:00"}); code != 400 {
		t.Fatal("invalid time accepted")
	}
	if code, _ := admin.do("POST", "/api/settings/displays/options", map[string]any{"browser": "firefox", "dailyRestart": "04:30"}); code != 200 {
		t.Fatal("options")
	}
	_, s := admin.do("GET", "/api/settings", nil)
	sys := s["system"].(map[string]any)
	if code, _ := admin.do("PUT", "/api/settings/system", sys); code != 200 {
		t.Fatal("system save")
	}
	if o := r.store.Get().System.Display; o.Browser != "firefox" || o.DailyRestart != "04:30" {
		t.Fatalf("display options lost: %+v", o)
	}

	// deleting the monitor switches the display off; deleting the display removes it
	if code, _ := admin.do("DELETE", "/api/settings/monitors/m2", nil); code != 200 {
		t.Fatal("delete monitor")
	}
	if d, _ := r.store.Get().Display(id); d.Enabled || d.MonitorID != "" {
		t.Fatalf("display of a deleted monitor: %+v", d)
	}
	if code, _ := admin.do("DELETE", "/api/settings/displays/"+id, nil); code != 200 || len(h.Desired().Outputs) != 0 {
		t.Fatal("delete display")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
