package server

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"testing"
	"time"

	"cnm/internal/config"
	_ "cnm/internal/provider/reolink"
	"cnm/internal/ptz"
	"cnm/internal/sim"
)

// PTZ over the API: cameras report "ptz", users steer, a second user is
// told who steers, kiosk sessions have no access.
func TestPTZAPI(t *testing.T) {
	r := newRig(t)
	cam := &sim.ReolinkServer{User: "admin", Pass: "dvr-secret-pw", DualLens: true, PTZ: true, RTSPPort: 554, Loc: time.UTC,
		Clip: func(int, bool) *sim.Clip { return sim.Synthetic(10) }}
	if err := cam.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cam.Close)
	h, p, _ := net.SplitHostPort(cam.Addr())
	port, _ := strconv.Atoi(p)
	r.srv.PTZ = ptz.New(r.srv.Devices, r.store.Get, t.TempDir(), quiet())
	t.Cleanup(r.srv.PTZ.Close)

	admin := r.bootstrapAdmin()
	if _, err := r.store.Update(func(c *config.Config) error {
		pw := c.Devices[0].Credentials // same encrypted password
		c.Devices = append(c.Devices, config.Device{ID: "cam1", Label: "Hof", Protocol: "reolink", Host: h, Credentials: pw,
			Ports: map[string]int{"http": port}, ChannelCount: 2})
		c.Cameras = append(c.Cameras, config.Camera{ID: "cam1:0", DeviceID: "cam1", Channel: 0, Enabled: true},
			config.Camera{ID: "cam1:1", DeviceID: "cam1", Channel: 1, Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.srv.Devices.Start()
	t.Cleanup(func() { r.srv.Devices.Close(context.Background()) })
	deadline := time.Now().Add(10 * time.Second)
	for !r.srv.Devices.Online("cam1") {
		if time.Now().After(deadline) {
			t.Fatal("camera did not connect")
		}
		time.Sleep(50 * time.Millisecond)
	}

	resp, err := admin.hc.Get(r.http.URL + "/api/cameras")
	if err != nil {
		t.Fatal(err)
	}
	var cams []struct {
		ID  string `json:"id"`
		PTZ bool   `json:"ptz"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cams)
	resp.Body.Close()
	flags := map[string]bool{}
	for _, c := range cams {
		flags[c.ID] = c.PTZ
	}
	if resp.StatusCode != 200 || !flags["cam1:0"] || !flags["cam1:1"] || flags["dvr1:0"] {
		t.Fatalf("ptz flags %v", flags)
	}

	if st, body := admin.do("GET", "/api/cameras/cam1:1/ptz", nil); st != 200 || body["controller"] != nil {
		t.Fatalf("view: %d %v", st, body)
	}
	if st, body := admin.do("POST", "/api/cameras/cam1:0/ptz/move", map[string]any{"pan": 1}); st != 409 {
		t.Fatalf("move without control: %d %v", st, body)
	}
	if st, body := admin.do("POST", "/api/cameras/cam1:0/ptz/control", map[string]any{"take": true}); st != 200 {
		t.Fatalf("take: %d %v", st, body)
	}
	if st, body := admin.do("POST", "/api/cameras/cam1:1/ptz/move", map[string]any{"pan": -0.5, "tilt": 0.5}); st != 200 {
		t.Fatalf("move: %d %v", st, body)
	}
	if st, _ := admin.do("POST", "/api/cameras/cam1:1/ptz/guard", map[string]any{"action": "save"}); st != 200 {
		t.Fatalf("save guard: %d", st)
	}

	// a viewer sees who steers and may take over only explicitly
	if st, _ := admin.do("POST", "/api/settings/users", map[string]any{"username": "viewer2", "password": "viewer-pass", "role": "viewer"}); st != 200 {
		t.Fatal("create viewer")
	}
	viewer := r.client()
	viewer.login("viewer2", "viewer-pass")
	st, body := viewer.do("GET", "/api/cameras/cam1:0/ptz/status", nil)
	ctl, _ := body["controller"].(map[string]any)
	if st != 200 || ctl["username"] != "admin" || ctl["self"] != false {
		t.Fatalf("status for viewer: %d %v", st, body)
	}
	if st, _ := viewer.do("POST", "/api/cameras/cam1:0/ptz/control", map[string]any{"take": true}); st != 409 {
		t.Fatalf("take without force: %d", st)
	}
	if st, _ := viewer.do("POST", "/api/cameras/cam1:0/ptz/guard", map[string]any{"action": "save"}); st != 403 {
		t.Fatalf("viewer save guard: %d", st)
	}
	if st, _ := viewer.do("POST", "/api/cameras/cam1:0/ptz/control", map[string]any{"take": true, "force": true}); st != 200 {
		t.Fatalf("forced takeover: %d", st)
	}
	if st, _ := viewer.do("POST", "/api/cameras/cam1:0/ptz/control", map[string]any{"take": false}); st != 200 {
		t.Fatalf("release: %d", st)
	}
	if r.srv.PTZ.Pending() != 0 || !cam.Tracking() {
		t.Fatal("camera not restored")
	}
	ops := cam.PTZOps()
	if len(ops) == 0 || ops[0] != "LeftUp:32" || ops[len(ops)-1] != "GuardToPos" {
		t.Fatalf("ops %v", ops)
	}

	// kiosk sessions never steer
	keyID, token := r.kioskKey(admin, "m1")
	_ = keyID
	kiosk := r.client()
	if st := kiosk.kioskLogin("m1", token); st != 200 {
		t.Fatalf("kiosk login %d", st)
	}
	if st, _ := kiosk.do("GET", "/api/cameras/cam1:0/ptz", nil); st != 403 {
		t.Fatalf("kiosk ptz: %d", st)
	}
}
