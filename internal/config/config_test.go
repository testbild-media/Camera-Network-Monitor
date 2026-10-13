package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const legacyConfig = `{
  "version": 1,
  "system": {
    "name": "Werkstatt",
    "listenPort": "8091",
    "healthIntervalMs": 20000,
    "media": { "go2rtcUrl": "http://127.0.0.1:1984/", "preloadMain": true }
  },
  "devices": [
    {
      "id": "dvr1", "label": "Eingang", "manufacturer": "Lupus", "model": "LE808HD",
      "host": "192.0.2.20",
      "credentials": { "username": "admin", "password": {"iv":"00","tag":"11","data":"22"} },
      "ports": { "http": 8008, "rtsp": "8554" },
      "channelCount": 3,
      "channels": [
        { "index": 0, "label": "Tor", "view": { "aspectRatio": "4/3", "objectFit": "crop", "multiviewSource": "sub_main_fallback" } },
        { "index": 1, "enabled": false, "streams": { "main": {"available": true}, "sub": false } }
      ],
      "options": { "storage": "no", "dvripMode": "two-socket" }
    },
    { "id": "dvr1", "label": "dup" }
  ],
  "monitors": [
    { "id": "m1", "label": "Wand", "layout": { "gap": 99, "rows": [ { "height": 10, "slots": [ { "cameraId": "dvr1:0", "width": 40 } ] } ] } }
  ],
  "users": [
    { "id": "u1", "username": "chef", "role": "Admin", "passwordHash": "abc:def", "createdAt": 1700000000000,
      "apiKeys": [ { "id": "k1", "label": "Lobby", "monitorId": "m1", "hash": "aa", "revoked": true } ] },
    { "username": "CHEF" }
  ]
}`

func TestParseLegacyConfig(t *testing.T) {
	c, rep, err := Parse([]byte(legacyConfig), "/data")
	if err != nil {
		t.Fatal(err)
	}
	if rep.SourceVersion != 1 {
		t.Fatalf("source version = %d", rep.SourceVersion)
	}
	if c.System.Name != "Werkstatt" || c.System.ListenPort != 8091 || c.System.HealthIntervalMs != 20000 {
		t.Fatalf("system: %+v", c.System)
	}
	if !c.System.Media.Enabled || !c.System.Media.PreloadMultiview || !c.System.Media.PreloadMain {
		t.Fatalf("media defaults/overrides wrong: %+v", c.System.Media)
	}
	if c.System.Media.Go2rtcURL != "http://127.0.0.1:1984" {
		t.Fatalf("go2rtc url not trimmed: %q", c.System.Media.Go2rtcURL)
	}
	if len(c.Devices) != 1 {
		t.Fatalf("duplicate device not dropped: %d", len(c.Devices))
	}
	d := c.Devices[0]
	if d.Protocol != ProtoDahua || d.Variant() != "rpc2-dahua3" || d.Ports["rtsp"] != 8554 || d.Ports["http"] != 8008 {
		t.Fatalf("device: %+v", d)
	}
	if !IsUUID(d.ID) || d.Metadata["legacyId"] != "dvr1" {
		t.Fatalf("device id not migrated to a UUID: %q %+v", d.ID, d.Metadata)
	}
	cams := c.DeviceCameras(d.ID)
	if len(cams) != 3 || len(c.Cameras) != 3 {
		t.Fatalf("cameras not created per channel: %d", len(cams))
	}
	ch0 := cams[0]
	if ch0.View.AspectRatio != Aspect4x3 || ch0.View.ObjectFit != FitCover || !ch0.Enabled {
		t.Fatalf("ch0: %+v", ch0)
	}
	ch1 := cams[1]
	if ch1.Enabled || !ch1.Streams.Main || ch1.Streams.Sub {
		t.Fatalf("ch1: %+v", ch1)
	}
	ch2 := cams[2]
	if !ch2.Enabled || ch2.View.AspectRatio != Aspect16x9 || ch2.View.ObjectFit != FitFill {
		t.Fatalf("ch2 defaults: %+v", ch2)
	}
	if _, ok := d.Options[OptRecordingStorage]; ok || d.Options[OptLiveProtocol] != "" { // live protocol option is obsolete
		t.Fatalf("options: %+v", d.Options)
	}
	if !d.Credentials.HasPassword() {
		t.Fatal("password lost")
	}
	m := c.Monitors[0]
	if m.Layout.Rows[0].Slots[0].CameraID != ch0.ID || ch0.Label != "Tor" {
		t.Fatalf("legacy slot reference not migrated to the camera id: %q", m.Layout.Rows[0].Slots[0].CameraID)
	}
	if m.Layout.Gap != MaxGap || m.Layout.Rows[0].Height != MaxRowHeight || m.Layout.Rows[0].Slots[0].Width != MaxSlotWidth {
		t.Fatalf("layout not clamped: %+v", m.Layout)
	}
	if len(c.Users) != 1 {
		t.Fatalf("duplicate username not dropped: %d", len(c.Users))
	}
	u := c.Users[0]
	if u.Role != RoleAdmin || !u.Enabled || u.CreatedAt != "2023-11-14T22:13:20Z" {
		t.Fatalf("user: %+v", u)
	}
	if len(u.APIKeys) != 1 || u.APIKeys[0].Enabled || u.APIKeys[0].TokenHash != "aa" {
		t.Fatalf("api key: %+v", u.APIKeys)
	}
}

func TestOpenFreshCreatesDefaultMonitor(t *testing.T) {
	dir := t.TempDir()
	s, res, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fresh {
		t.Fatal("expected fresh")
	}
	c := s.Get()
	if len(c.Monitors) != 1 || c.Monitors[0].Label != "Monitor 1" || c.Monitors[0].Layout.Template != "2x2" {
		t.Fatalf("default monitor: %+v", c.Monitors)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatal("config not written")
	}
	// deleting the last monitor recreates the default
	if _, err := s.Update(func(c *Config) error { c.Monitors = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(s.Get().Monitors) != 1 {
		t.Fatal("default monitor not recreated")
	}
}

func TestAtomicSaveBackupAndRecovery(t *testing.T) {
	dir := t.TempDir()
	s, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var notified int
	s.Subscribe(func(old, new *Config) {
		if old.System.Name == new.System.Name {
			t.Error("subscriber got identical snapshots")
		}
		notified++
	})
	if _, err := s.Update(func(c *Config) error { c.System.Name = "Eins"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(func(c *Config) error { c.System.Name = "Zwei"; return nil }); err != nil {
		t.Fatal(err)
	}
	if notified != 2 {
		t.Fatalf("notified %d", notified)
	}
	bak, _ := os.ReadFile(filepath.Join(dir, BackupFileName))
	if !strings.Contains(string(bak), `"Eins"`) {
		t.Fatalf("backup does not hold previous version: %s", bak)
	}
	// failing update leaves state unchanged
	if _, err := s.Update(func(c *Config) error { c.System.Name = "X"; return Invalid("nein") }); err == nil {
		t.Fatal("expected error")
	}
	if s.Get().System.Name != "Zwei" {
		t.Fatal("failed update leaked")
	}
	// simulate a truncated main file: the backup must be used and the corrupt file kept
	os.WriteFile(filepath.Join(dir, FileName), []byte(`{"version":2,"sys`), 0o600)
	s2, res, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FromBackup || s2.Get().System.Name != "Eins" {
		t.Fatalf("recovery from backup failed: %+v name=%s", res, s2.Get().System.Name)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "config.json.corrupt-*"))
	if len(matches) != 1 {
		t.Fatal("corrupt file not preserved")
	}
}

func TestOpenArchivesLegacyConfigOnce(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte(legacyConfig), 0o600)
	_, res, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Archived == "" {
		t.Fatal("legacy config not archived")
	}
	orig, _ := os.ReadFile(res.Archived)
	if string(orig) != legacyConfig {
		t.Fatal("archive differs from original")
	}
	var written map[string]any
	data, _ := os.ReadFile(filepath.Join(dir, FileName))
	if err := json.Unmarshal(data, &written); err != nil || written["version"].(float64) != SchemaVersion {
		t.Fatalf("migrated config not written: %v", err)
	}
	// second open: nothing to archive anymore, file unchanged
	_, res2, err := Open(dir)
	if err != nil || res2.Archived != "" {
		t.Fatalf("second open: %+v %v", res2, err)
	}
}

func TestIsUUID(t *testing.T) {
	for i := 0; i < 20; i++ {
		if id := NewUUID(); !IsUUID(id) {
			t.Fatalf("NewUUID not recognised: %q", id)
		}
	}
	for _, s := range []string{"", "dvr1", "recorder1", "9318340b-4fff-4755-8d8d-cdc449c6883", "9318340b_4fff-4755-8d8d-cdc449c6883f", "9318340g-4fff-4755-8d8d-cdc449c6883f"} {
		if IsUUID(s) {
			t.Fatalf("%q accepted as UUID", s)
		}
	}
}

func TestTemplatesAndCameraIDs(t *testing.T) {
	for _, tpl := range LayoutTemplates {
		l := TemplateLayout(tpl.ID, []string{"a:0", "b:1"})
		n := 0
		for _, r := range l.Rows {
			n += len(r.Slots)
		}
		want := 0
		for _, c := range tpl.Rows {
			want += c
		}
		if n != want {
			t.Fatalf("%s: %d slots, want %d", tpl.ID, n, want)
		}
		if l.Rows[0].Slots[0].CameraID != "a:0" || l.Rows[0].Slots[1].CameraID != "b:1" {
			t.Fatalf("%s: camera assignment not kept", tpl.ID)
		}
		m := Monitor{Label: "x", Layout: l}
		if err := ValidateMonitorInput(&m); err != nil {
			t.Fatalf("%s: %v", tpl.ID, err)
		}
	}
	dev, idx, err := ParseLegacyCameraRef("dvr:a:3")
	if err != nil || dev != "dvr:a" || idx != 3 {
		t.Fatalf("parse: %s %d %v", dev, idx, err)
	}
	if _, _, err := ParseLegacyCameraRef("nope"); err == nil {
		t.Fatal("invalid id accepted")
	}
	if u := NewUUID(); len(u) != 36 || u[14] != '4' {
		t.Fatalf("uuid %q", u)
	}
	if SanitizeID("DVR 1/Eingang") != "dvr_1_eingang" {
		t.Fatalf("sanitize: %s", SanitizeID("DVR 1/Eingang"))
	}
}

func TestCameraSyncAndGroups(t *testing.T) {
	c := Default("/d")
	c.Devices = []Device{{ID: "a", Protocol: "dahua", Detected: &Detection{Variant: "rpc2-dahua3"}, ChannelCount: 4}, {ID: "b", ChannelCount: 8}}
	Normalize(c, "/d")
	if len(c.Cameras) != 12 {
		t.Fatalf("4ch + 8ch must provide 12 cameras, got %d", len(c.Cameras))
	}
	first := c.Cameras[0].ID
	c.Groups = []Group{{Label: "Außen", CameraIDs: []string{first, first, "unknown", c.Cameras[5].ID}}, {Label: "Hof", CameraIDs: []string{first}}}
	c.Monitors[0].Layout.Rows[0].Slots[0].CameraID = c.Cameras[11].ID
	Normalize(c, "/d")
	if g := c.Groups[0]; len(g.CameraIDs) != 2 || g.ID == "" {
		t.Fatalf("group not cleaned: %+v", g)
	}
	if got := c.CameraGroups(first); len(got) != 2 {
		t.Fatalf("camera must be allowed in several groups: %v", got)
	}
	// shrinking the device removes its surplus cameras everywhere
	c.Devices[1].ChannelCount = 4
	Normalize(c, "/d")
	if len(c.Cameras) != 8 {
		t.Fatalf("cameras after shrink: %d", len(c.Cameras))
	}
	if c.Monitors[0].Layout.Rows[0].Slots[0].CameraID != "" {
		t.Fatal("slot of a removed camera must become empty")
	}
	if _, ok := c.Camera(first); !ok {
		t.Fatal("existing camera ids must stay stable")
	}
	// deleting a device removes its cameras and group memberships
	c.Devices = c.Devices[1:]
	Normalize(c, "/d")
	if len(c.Cameras) != 4 || len(c.Groups[0].CameraIDs) != 1 || len(c.Groups[1].CameraIDs) != 0 {
		t.Fatalf("cascade failed: %d cameras, group %v", len(c.Cameras), c.Groups[0].CameraIDs)
	}
}

// TestFullRoundTrip guards the tolerant loader: every field written by this
// version must survive Parse(marshal(c)) unchanged.
func TestFullRoundTrip(t *testing.T) {
	c := Default("/d")
	c.System.Name = "Zentrale"
	c.System.Media.AllowWebRTC = true
	c.System.Display = DisplaySettings{Browser: BrowserFirefox, DailyRestart: "04:30"}
	c.Devices = []Device{{ID: NewUUID(), Label: "DVR", Protocol: "dahua", Detected: &Detection{Variant: "rpc2-dahua3"}, Host: "192.0.2.5",
		Credentials: Credentials{Username: "admin", Password: json.RawMessage(`"enc:v1:abc"`)}, Ports: map[string]int{"http": 8008, "rtsp": 8554},
		ChannelCount: 2, Options: map[string]string{"mainProfile": "1"}, Metadata: map[string]any{"legacyId": "dvr1"}}}
	Normalize(c, "/d")
	c.Cameras[0].Label = "Tor"
	c.Groups = []Group{{ID: "g1", Label: "Außen", CameraIDs: []string{c.Cameras[0].ID}}}
	c.Monitors[0].Layout.Rows[0].Slots[0].CameraID = c.Cameras[1].ID
	c.Users = []User{{ID: "u1", Username: "admin", Role: RoleAdmin, Enabled: true, PasswordHash: json.RawMessage(`"scrypt$32768$8$1$c2FsdA$a2V5"`), CreatedAt: "2026-10-07T10:00:00Z",
		Preferences: UserPreferences{StartupScreen: "settings:groups", CameraPicker: PickerGroup},
		APIKeys:     []APIKey{{ID: "k1", Label: "Wand", MonitorID: c.Monitors[0].ID, CreatedAt: "2026-10-07T10:00:00Z", Enabled: true, TokenHash: "aa", TokenEnc: json.RawMessage(`"enc"`)}}}}
	c.Displays = []Display{{ID: NewUUID(), Connector: "HDMI-A-1", Label: "Wand links", Enabled: true, MonitorID: c.Monitors[0].ID, Mode: "1920x1080@60.000", Rotation: 90, TokenHash: "bb", TokenEnc: json.RawMessage(`"enc2"`)}}
	Normalize(c, "/d")
	b1, _ := marshal(c)
	c2, _, err := Parse(b1, "/d")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := marshal(c2)
	if string(b1) != string(b2) {
		t.Fatalf("round trip changed the configuration:\n%s\n---\n%s", b1, b2)
	}
}

func TestSchemaV3RoundTrip(t *testing.T) {
	c := Default("/d")
	c.Devices = []Device{{ID: "a", ChannelCount: 2}}
	Normalize(c, "/d")
	c.Groups = []Group{{ID: "g1", Label: "X", CameraIDs: []string{c.Cameras[1].ID}}}
	b, _ := marshal(c)
	c2, rep, err := Parse(b, "/d")
	if err != nil || rep.SourceVersion != SchemaVersion {
		t.Fatal(err)
	}
	if c2.Cameras[1].ID != c.Cameras[1].ID || len(c2.Groups) != 1 || c2.Groups[0].CameraIDs[0] != c.Cameras[1].ID {
		t.Fatal("camera ids or groups not preserved")
	}
}

// Schema 3 devices (manufacturer + model) become protocols; where the old
// model fixed the login variant it is kept, and so is the camera count.
func TestMigrateManufacturerModelToProtocol(t *testing.T) {
	devs := `[
	 {"id":"a","label":"A","manufacturer":"lupus","model":"LE808HD","host":"192.0.2.1","channelCount":8},
	 {"id":"b","label":"B","manufacturer":"lupus","model":"LE820","host":"192.0.2.2","channelCount":8},
	 {"id":"c","label":"C","manufacturer":"lupus","model":"LE800HD-NANO","host":"192.0.2.3","channelCount":4},
	 {"id":"d","label":"D","manufacturer":"lupus","model":"DAHUA","host":"192.0.2.4","channelCount":16},
	 {"id":"e","label":"E","manufacturer":"netsurveillance","model":"DVRIP-8CH","host":"192.0.2.5","channelCount":8},
	 {"id":"f","label":"F","manufacturer":"hikvision","model":"HIK-IPC","host":"192.0.2.6","channelCount":1},
	 {"id":"g","label":"G","manufacturer":"generic","model":"ONVIF-NVR","host":"192.0.2.7","channelCount":4},
	 {"id":"h","label":"H","manufacturer":"generic","model":"RTSP-NVR","host":"192.0.2.8","channelCount":2},
	 {"id":"i","label":"I","manufacturer":"axis","model":"AXIS","host":"192.0.2.9","channelCount":1},
	 {"id":"j","label":"J","protocol":"sofia","detected":{"variant":"dvrip","channels":4},"host":"192.0.2.10","channelCount":4}]`
	c, rep, err := Parse([]byte(`{"version":3,"devices":`+devs+`,"cameras":[]}`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{ // label → protocol, variant, channels
		"A": {ProtoDahua, "rpc2-dahua3", "8"}, "B": {ProtoDahua, "rpc2-form", "8"}, "C": {ProtoDahua, "native", "4"},
		"D": {ProtoDahua, "rpc2-web3", "16"}, "E": {ProtoSofia, "dvrip", "8"}, "F": {ProtoHikvision, "isapi", "1"},
		"G": {ProtoONVIF, "", "4"}, "H": {ProtoRTSP, "", "2"}, "I": {ProtoAxis, "", "1"}, "J": {ProtoSofia, "dvrip", "4"},
	}
	for _, d := range c.Devices {
		w := want[d.Label]
		if d.Protocol != w[0] || d.Variant() != w[1] || strconv.Itoa(d.ChannelCount) != w[2] {
			t.Fatalf("%s: %s %q %d, want %v", d.Label, d.Protocol, d.Variant(), d.ChannelCount, w)
		}
		if d.Label != "J" && (d.Detected == nil || d.Detected.Channels != d.ChannelCount) {
			t.Fatalf("%s: camera count not kept as detected: %+v", d.Label, d.Detected)
		}
	}
	if len(c.Cameras) != 8+8+4+16+8+1+4+2+1+4 {
		t.Fatalf("%d cameras", len(c.Cameras))
	}
	found := false
	for _, n := range rep.Notes {
		found = found || strings.Contains(n, "9 Geräte von Hersteller/Modell auf Protokoll umgestellt")
	}
	if !found {
		t.Fatalf("report %v", rep.Notes)
	}
	// written back as schema 4 without manufacturer/model
	out, _ := json.Marshal(c)
	if strings.Contains(string(out), `"manufacturer"`) || strings.Contains(string(out), `"model"`) || c.Version != SchemaVersion {
		t.Fatalf("still old fields: %s", out)
	}
}
