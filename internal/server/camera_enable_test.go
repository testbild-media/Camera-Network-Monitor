package server

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"cnm/internal/config"
)

// closedWithin reports whether a media socket is closed within d.
func closedWithin(t *testing.T, read func(context.Context) error, d time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for {
		if err := read(ctx); err != nil {
			return ctx.Err() == nil
		}
	}
}

// Disabling a camera ends every live session of it – admin browser and kiosk
// at once – refuses new ones, keeps all layout assignments and leaves other
// cameras untouched. Re-enabling allows sessions again (on demand only).
func TestDisablingCameraEndsAllStreams(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	_, token := r.kioskKey(admin, "m1")
	k := r.client().remote()
	if st := k.kioskLogin("m1", token); st != 200 {
		t.Fatalf("kiosk login: %d", st)
	}
	open := func(c *client, stream string) func(context.Context) error {
		conn, err := c.dialWS("/media/api/ws", url.Values{"src": {stream}})
		if err != nil {
			t.Fatalf("media ws %s: %v", stream, err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		return func(ctx context.Context) error { _, _, err := conn.Read(ctx); return err }
	}
	adminCam0 := open(admin, "cnm_dvr1_ch01_sub")
	kioskCam0 := open(k, "cnm_dvr1_ch01_sub")
	adminMain0 := open(admin, "cnm_dvr1_ch01_main")
	otherCam := open(admin, "cnm_dvr1_ch02_sub")
	for i := 0; i < 50 && r.media.open.Load() < 4; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if st, _ := admin.do("POST", "/api/settings/cameras/bulk", map[string]any{"ids": []string{"dvr1:0"}, "enabled": false}); st != 200 {
		t.Fatalf("disable: %d", st)
	}
	for name, read := range map[string]func(context.Context) error{"admin sub": adminCam0, "kiosk sub": kioskCam0, "admin main": adminMain0} {
		if !closedWithin(t, read, 3*time.Second) {
			t.Fatalf("%s session of the disabled camera still open", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := otherCam(ctx); err != nil {
		t.Fatalf("other camera was disturbed: %v", err)
	}
	// upstream (go2rtc) connections of the camera are gone too
	for i := 0; i < 100 && r.media.open.Load() > 1; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if n := r.media.open.Load(); n != 1 {
		t.Fatalf("%d upstream sessions open, want only the other camera", n)
	}
	// no new sessions, also not over the kiosk scope
	if code := admin.mediaStatus("cnm_dvr1_ch01_sub"); code != 403 {
		t.Fatalf("new session for disabled camera: %d", code)
	}
	if code := k.mediaStatus("cnm_dvr1_ch01_main"); code != 403 {
		t.Fatalf("kiosk session for disabled camera: %d", code)
	}
	// the camera stays assigned in its layout and is published as disabled
	if m, _ := r.store.Get().Monitor("m1"); !strings.Contains(strings.Join(m.CameraIDs(), ","), "dvr1:0") {
		t.Fatal("layout assignment lost")
	}
	_, resp := admin.do("GET", "/api/state", nil)
	state := resp["state"].(map[string]any)
	for _, c := range state["cameras"].([]any) {
		cm := c.(map[string]any)
		if cm["id"] == "dvr1:0" && (cm["enabled"] != false || cm["multiview"] != nil || cm["focus"] != nil) {
			t.Fatalf("disabled camera still offers streams: %v", cm)
		}
	}
	// re-enable: sessions possible again
	if st, _ := admin.do("POST", "/api/settings/cameras/bulk", map[string]any{"ids": []string{"dvr1:0"}, "enabled": true}); st != 200 {
		t.Fatal("enable")
	}
	if code := admin.mediaStatus("cnm_dvr1_ch01_sub"); code == 403 {
		t.Fatal("re-enabled camera still refused")
	}
}

// A disabled camera keeps its slots (also when they are rearranged) but can
// not be assigned to a new slot or monitor through the API.
func TestDisabledCameraCannotBeAssignedAnew(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	if st, _ := admin.do("POST", "/api/settings/cameras/bulk", map[string]any{"ids": []string{"dvr1:0"}, "enabled": false}); st != 200 {
		t.Fatal("disable")
	}
	m1, _ := r.store.Get().Monitor("m1")
	mon := *m1
	mon.Layout = config.TemplateLayout("2x2", []string{"dvr1:1", "dvr1:0"}) // rearranged: still once
	if st, body := admin.do("POST", "/api/settings/monitors", mon); st != 200 {
		t.Fatalf("keeping the existing assignment must work: %d %v", st, body)
	}
	mon.Layout = config.TemplateLayout("2x2", []string{"dvr1:0", "dvr1:0"}) // second slot = new assignment
	if st, body := admin.do("POST", "/api/settings/monitors", mon); st != 400 || !strings.Contains(body["error"].(map[string]any)["message"].(string), "deaktiviert") {
		t.Fatalf("new slot for disabled camera: %d %v", st, body)
	}
	m2, _ := r.store.Get().Monitor("m2")
	other := *m2
	other.Layout = config.TemplateLayout("2x2", []string{"dvr1:2", "dvr1:0"})
	if st, _ := admin.do("POST", "/api/settings/monitors", other); st != 400 {
		t.Fatalf("assigning the disabled camera to another monitor: %d", st)
	}
	fresh := config.Monitor{Label: "Neu", Enabled: true, Layout: config.TemplateLayout("1x1", []string{"dvr1:0"})}
	if st, _ := admin.do("POST", "/api/settings/monitors", fresh); st != 400 {
		t.Fatalf("new monitor with disabled camera: %d", st)
	}
	// replacing it with an enabled camera and removing it are allowed
	mon.Layout = config.TemplateLayout("2x2", []string{"dvr1:1", "dvr1:3"})
	if st, _ := admin.do("POST", "/api/settings/monitors", mon); st != 200 {
		t.Fatalf("replacing the disabled camera: %d", st)
	}
}

// Startup screen: per user, validated against role and existing monitors,
// with fallback when the target disappears; kiosks have no preferences.
func TestStartupScreenPreferences(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	if st, _ := admin.do("POST", "/api/settings/users", map[string]any{"username": "viewer2", "password": "viewer-pass", "role": "viewer"}); st != 200 {
		t.Fatal("create viewer")
	}
	v := r.client().remote()
	v.login("viewer2", "viewer-pass")
	if st, _ := v.do("PUT", "/api/me/preferences", map[string]any{"startupScreen": "monitor:m2", "cameraPicker": "group"}); st != 200 {
		t.Fatalf("viewer monitor startup: %d", st)
	}
	if st, _ := v.do("PUT", "/api/me/preferences", map[string]any{"startupScreen": "settings:cameras"}); st != 400 {
		t.Fatalf("viewer must not choose a settings tab: %d", st)
	}
	if st, _ := v.do("PUT", "/api/me/preferences", map[string]any{"startupScreen": "monitor:nope"}); st != 400 {
		t.Fatalf("unknown monitor accepted: %d", st)
	}
	if st, _ := v.do("PUT", "/api/me/preferences", map[string]any{"cameraPicker": "chaos"}); st != 400 {
		t.Fatalf("unknown picker mode accepted: %d", st)
	}
	_, me := v.do("GET", "/api/auth/me", nil)
	prefs := me["preferences"].(map[string]any)
	if prefs["startupScreen"] != "monitor:m2" || prefs["cameraPicker"] != "group" {
		t.Fatalf("prefs %v", prefs)
	}
	if st, _ := admin.do("PUT", "/api/me/preferences", map[string]any{"startupScreen": "settings:groups"}); st != 200 {
		t.Fatalf("admin settings startup: %d", st)
	}
	_, ame := admin.do("GET", "/api/auth/me", nil)
	if ame["preferences"].(map[string]any)["startupScreen"] != "settings:groups" {
		t.Fatalf("admin prefs %v", ame["preferences"])
	}
	// kiosks have no personal preferences
	_, token := r.kioskKey(admin, "m1")
	k := r.client().remote()
	if st := k.kioskLogin("m1", token); st != 200 {
		t.Fatalf("kiosk login %d", st)
	}
	if st, _ := k.do("PUT", "/api/me/preferences", map[string]any{"startupScreen": ""}); st != 403 {
		t.Fatalf("kiosk preferences: %d", st)
	}
	// monitor deleted → the effective startup screen falls back to standard
	if st, _ := admin.do("DELETE", "/api/settings/monitors/m2", nil); st != 200 {
		t.Fatal("delete monitor")
	}
	_, me = v.do("GET", "/api/auth/me", nil)
	if s, _ := me["preferences"].(map[string]any)["startupScreen"].(string); s != "" {
		t.Fatalf("deleted monitor still startup screen: %q", s)
	}
	// the preferences survive a reload of the configuration
	if u, _ := r.store.Get().User(r.store.Get().Users[1].ID); u.Preferences.CameraPicker != "group" {
		t.Fatalf("stored prefs %+v", u.Preferences)
	}
	// a demoted admin loses the settings target (stored value kept, not used)
	uid := r.store.Get().Users[0].ID
	if _, err := r.store.Update(func(c *config.Config) error {
		u, _ := c.User(uid)
		u.Role = config.RoleViewer
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := r.store.Get()
	u, _ := c.User(uid)
	if got := c.EffectiveStartupScreen(u); got != "" {
		t.Fatalf("viewer keeps settings startup: %q", got)
	}
}
