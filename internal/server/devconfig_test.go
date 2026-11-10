package server

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/sim"
)

// The device settings API reads and changes a connected device with the
// stored credentials; admins only.
func TestDeviceSettingsAPI(t *testing.T) {
	r := newRig(t)
	dvr := &sim.DahuaServer{User: "admin", Pass: "dvr-secret-pw", Channels: 4, HasDisk: true, Recordings: sim.QuarterSchedule}
	if err := dvr.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dvr.Close)
	h, p, _ := net.SplitHostPort(dvr.Addr())
	port, _ := strconv.Atoi(p)

	admin := r.bootstrapAdmin()
	r.srv.Devices.Start()
	t.Cleanup(func() { r.srv.Devices.Close(context.Background()) })
	// not connected yet (the configured address does not answer)
	if st, body := admin.do("GET", "/api/settings/devices/dvr1/config", nil); st != 409 {
		t.Fatalf("offline: %d %v", st, body)
	}
	if _, err := r.store.Update(func(c *config.Config) error {
		c.Devices[0].Host, c.Devices[0].Ports = h, map[string]int{"http": port, "rtsp": 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !r.srv.Devices.Online("dvr1") {
		if time.Now().After(deadline) {
			t.Fatal("device did not connect")
		}
		time.Sleep(50 * time.Millisecond)
	}

	st, body := admin.do("GET", "/api/settings/devices/dvr1/config", nil)
	if st != 200 {
		t.Fatalf("read: %d %v", st, body)
	}
	var got struct {
		Settings struct {
			Channels []struct {
				Name string `json:"name"`
				Main struct {
					Bitrate int    `json:"bitrate"`
					Res     string `json:"resolution"`
				} `json:"main"`
			} `json:"channels"`
		} `json:"settings"`
	}
	b, _ := json.Marshal(body)
	_ = json.Unmarshal(b, &got)
	if len(got.Settings.Channels) != 4 || got.Settings.Channels[0].Name != "SIM CAM 1" || got.Settings.Channels[0].Main.Bitrate != 4096 {
		t.Fatalf("settings %s", b)
	}

	change := map[string]any{"channels": []any{map[string]any{"channel": 0, "name": "Einfahrt",
		"main": map[string]any{"enabled": true, "codec": "H.264", "resolution": "1920x1080", "fps": 12, "control": "VBR", "bitrate": 2048, "quality": 4, "gop": 24}}}}
	st, body = admin.do("PUT", "/api/settings/devices/dvr1/config", change)
	if st != 200 || body["warnings"] != nil {
		t.Fatalf("change: %d %v", st, body)
	}
	b, _ = json.Marshal(body)
	_ = json.Unmarshal(b, &got)
	if got.Settings.Channels[0].Name != "Einfahrt" || got.Settings.Channels[0].Main.Bitrate != 2048 {
		t.Fatalf("after change %s", b)
	}
	// invalid values are refused before anything is sent
	writes := dvr.ConfigWrites.Load()
	bad := map[string]any{"channels": []any{map[string]any{"channel": 0, "main": map[string]any{"codec": "H.264", "resolution": "1920x1080", "fps": 0, "control": "VBR", "bitrate": 2048, "quality": 4, "gop": 24}}}}
	if st, _ := admin.do("PUT", "/api/settings/devices/dvr1/config", bad); st != 400 || dvr.ConfigWrites.Load() != writes {
		t.Fatalf("invalid change: %d", st)
	}
	// several channels are saved one after another: a channel the device
	// refuses is reported, the others are still saved
	stream := func(fps int) map[string]any {
		return map[string]any{"enabled": true, "codec": "H.264", "resolution": "1920x1080", "fps": fps, "control": "VBR", "bitrate": 1024, "quality": 4, "gop": 24}
	}
	multi := map[string]any{"channels": []any{
		map[string]any{"channel": 1, "name": "Hof", "main": stream(10)},
		map[string]any{"channel": 2, "name": "Tor", "main": stream(30)}, // the device allows at most 25
		map[string]any{"channel": 3, "name": "Garage", "main": stream(8)},
	}}
	st, body = admin.do("PUT", "/api/settings/devices/dvr1/config", multi)
	failures, _ := body["failures"].([]any)
	if st != 200 || len(failures) != 1 || !strings.HasPrefix(failures[0].(string), "Kanal 3: ") {
		t.Fatalf("multi: %d %v", st, body)
	}
	b, _ = json.Marshal(body)
	_ = json.Unmarshal(b, &got)
	if c := got.Settings.Channels; c[1].Name != "Hof" || c[1].Main.Bitrate != 1024 || c[2].Name == "Tor" || c[3].Name != "Garage" {
		t.Fatalf("after multi %s", b)
	}
	// device wide settings alone (no channel)
	st, body = admin.do("PUT", "/api/settings/devices/dvr1/config", map[string]any{"system": map[string]any{"name": "Garage"},
		"diskAlarms": map[string]any{"lowSpace": map[string]any{"enabled": true}, "lowSpacePercent": 7}})
	if sys, _ := body["settings"].(map[string]any)["system"].(map[string]any); st != 200 || sys["name"] != "Garage" || body["warnings"] != nil {
		t.Fatalf("system: %d %v", st, body)
	}
	// the recorder's users; a new password for CNM's own login is stored in
	// CNM and the device connects again with it
	st, body = admin.do("GET", "/api/settings/devices/dvr1/users", nil)
	if users, _ := body["users"].([]any); st != 200 || len(users) != 2 || users[0].(map[string]any)["self"] != true {
		t.Fatalf("users: %d %v", st, body)
	}
	if st, _ := admin.do("POST", "/api/settings/devices/dvr1/password", map[string]any{"user": "user", "newPassword": "abc"}); st != 400 {
		t.Fatalf("other user without old password: %d", st)
	}
	// mark a point in time before a change: the Windows clock advances in
	// steps of up to ~16 ms, a fast reconnect could otherwise share the step
	mark := func() time.Time {
		at := time.Now()
		time.Sleep(20 * time.Millisecond)
		return at
	}
	connectedAfter := func(at time.Time) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if st, ok := r.srv.Devices.Status("dvr1"); ok && st.State == "connected" && st.LastOK != nil && st.LastOK.After(at) {
				return
			}
			if time.Now().After(deadline) {
				st, _ := r.srv.Devices.Status("dvr1")
				t.Fatalf("device did not connect again: state %s, error %q, last ok %v, next retry %v", st.State, st.Error, st.LastOK, st.NextRetry)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	changed := mark()
	if st, body := admin.do("POST", "/api/settings/devices/dvr1/password", map[string]any{"user": "admin", "newPassword": "dvr-new-pw-2"}); st != 200 {
		t.Fatalf("password: %d %v", st, body)
	}
	if pw, err := r.srv.Keys.Decrypt(r.store.Get().Devices[0].Credentials.Password); err != nil || pw != "dvr-new-pw-2" {
		t.Fatalf("stored password: %v", err)
	}
	connectedAfter(changed)
	// several recorders at once: back to the old password
	changed = mark()
	st, body = admin.do("POST", "/api/settings/devices/password", map[string]any{"deviceIds": []string{"dvr1"}, "newPassword": "dvr-secret-pw"})
	if res, _ := body["results"].([]any); st != 200 || len(res) != 1 || res[0].(map[string]any)["ok"] != true {
		t.Fatalf("bulk: %d %v", st, body)
	}
	connectedAfter(changed)
	// recorder check: disk, recording and clock are fine on the simulator
	st, body = admin.do("POST", "/api/settings/devices/dvr1/check", nil)
	if st != 200 || body["checkedAt"] == nil || body["problems"] != nil {
		t.Fatalf("check: %d %v", st, body)
	}
	st, body = admin.do("GET", "/api/settings/devices/status", nil)
	if list, _ := body["devices"].([]any); st != 200 || len(list) != 1 || list[0].(map[string]any)["monitor"] == nil || list[0].(map[string]any)["label"] == nil {
		t.Fatalf("statuses: %d %v", st, body)
	}
	// this protocol has no readable event log and no remote formatting
	if st, body := admin.do("GET", "/api/settings/devices/dvr1/log", nil); st != 409 {
		t.Fatalf("log: %d %v", st, body)
	}
	if st, body := admin.do("POST", "/api/settings/devices/dvr1/format", map[string]any{"disk": 0, "part": 0, "confirm": "x"}); st != 409 {
		t.Fatalf("format: %d %v", st, body)
	}
	if st, body := admin.do("POST", "/api/settings/devices/dvr1/time", nil); st != 200 {
		t.Fatalf("time: %d %v", st, body)
	}
	if st, body := admin.do("POST", "/api/settings/devices/dvr1/reboot", nil); st != 200 || dvr.Reboots.Load() != 1 {
		t.Fatalf("reboot: %d %v", st, body)
	}
	if st, _ := admin.do("GET", "/api/settings/devices/nope/config", nil); st != 404 {
		t.Fatalf("unknown device: %d", st)
	}

	// viewers have no access
	if st, _ := admin.do("POST", "/api/settings/users", map[string]any{"username": "viewer2", "password": "viewer-pass", "role": "viewer"}); st != 200 {
		t.Fatal("create viewer")
	}
	v := r.client()
	v.login("viewer2", "viewer-pass")
	for _, req := range []struct{ m, p string }{{"GET", "/api/settings/devices/dvr1/config"}, {"PUT", "/api/settings/devices/dvr1/config"}, {"POST", "/api/settings/devices/dvr1/reboot"}} {
		if st, _ := v.do(req.m, req.p, change); st != 403 {
			t.Fatalf("viewer %s %s: %d", req.m, req.p, st)
		}
	}
	if dvr.Reboots.Load() != 1 {
		t.Fatal("viewer could reboot")
	}
}
