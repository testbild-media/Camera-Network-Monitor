package config

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The loader reads config.json into a generic tree first and then maps it onto
// the typed model. This keeps CNM able to read configurations written by
// earlier versions (different key spellings, string/number/bool variations,
// missing fields) without failing. Unknown keys are ignored; the original file
// is archived before the first migrated save (see Store).

type tree = map[string]any

// LoadReport describes what the loader had to adapt.
type LoadReport struct {
	SourceVersion int
	Notes         []string

	migratedProtocols int
}

func (r *LoadReport) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// Parse maps raw JSON onto a normalised Config.
func Parse(data []byte, dataDir string) (*Config, *LoadReport, error) {
	var root tree
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("config.json ist kein gültiges JSON: %w", err)
	}
	rep := &LoadReport{}
	c := &Config{}
	if v, ok := numVal(root["version"]); ok {
		rep.SourceVersion = int(v)
	} else if s, ok := root["version"].(string); ok {
		// semantic version strings ("0.3.25") count as schema 1
		rep.SourceVersion = 1
		rep.note("Versionskennung %q als Schema 1 interpretiert", s)
	}
	c.System = parseSystem(obj(root, "system", "settings"), dataDir, rep)
	var legacy []legacyCamera
	for _, d := range arr(root, "devices", "dvrs") {
		if m, ok := d.(tree); ok {
			if dev, chans, ok := parseDevice(m, rep); ok {
				c.Devices = append(c.Devices, dev)
				legacy = append(legacy, chans...)
			}
		}
	}
	if rep.migratedProtocols > 0 {
		rep.note("%d Geräte von Hersteller/Modell auf Protokoll umgestellt", rep.migratedProtocols)
	}
	for _, m := range arr(root, "monitors", "views") {
		if mm, ok := m.(tree); ok {
			c.Monitors = append(c.Monitors, parseMonitor(mm))
		}
	}
	if top := arr(root, "cameras"); top != nil {
		// schema 3+: first-class cameras
		for _, x := range top {
			if m, ok := x.(tree); ok {
				c.Cameras = append(c.Cameras, parseCamera(m))
			}
		}
		for _, x := range arr(root, "groups") {
			if m, ok := x.(tree); ok {
				g := Group{ID: str(m, "id"), Label: str(m, "label", "name")}
				for _, id := range arr(m, "cameraIds", "cameras") {
					if s, ok := id.(string); ok {
						g.CameraIDs = append(g.CameraIDs, s)
					}
				}
				c.Groups = append(c.Groups, g)
			}
		}
	} else if len(legacy) > 0 || len(c.Devices) > 0 {
		migrateLegacyCameras(c, legacy, rep)
	}
	// local displays (schema 3+) map 1:1 onto the model
	if raw, ok := root["displays"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			var ds []Display
			if json.Unmarshal(b, &ds) == nil {
				c.Displays = ds
			} else {
				rep.note("Display-Konfiguration nicht lesbar – ignoriert")
			}
		}
	}
	for _, u := range arr(root, "users", "accounts") {
		if mm, ok := u.(tree); ok {
			if usr, ok := parseUser(mm, rep); ok {
				c.Users = append(c.Users, usr)
			}
		}
	}
	dedupe(c, rep)
	assignDeviceUUIDs(c, rep)
	Normalize(c, dataDir)
	return c, rep, nil
}

// assignDeviceUUIDs gives every device a unique internal UUID. Older
// configurations used free-text ids ("recorder1"); the old value is kept in the
// metadata for traceability. Cameras follow the device; monitor slots and
// groups reference camera ids and are unaffected.
func assignDeviceUUIDs(c *Config, rep *LoadReport) {
	remap := map[string]string{}
	for i := range c.Devices {
		d := &c.Devices[i]
		if IsUUID(d.ID) {
			continue
		}
		id := NewUUID()
		remap[d.ID] = id
		if d.Metadata == nil {
			d.Metadata = map[string]any{}
		}
		if _, ok := d.Metadata["legacyId"]; !ok && d.ID != "" {
			d.Metadata["legacyId"] = d.ID
		}
		d.ID = id
	}
	if len(remap) == 0 {
		return
	}
	for i := range c.Cameras {
		if id, ok := remap[c.Cameras[i].DeviceID]; ok {
			c.Cameras[i].DeviceID = id
		}
	}
	rep.note("%d Geräte-IDs auf UUID umgestellt", len(remap))
}

// ---- helpers --------------------------------------------------------------------

func obj(m tree, keys ...string) tree {
	for _, k := range keys {
		if v, ok := m[k].(tree); ok {
			return v
		}
	}
	return tree{}
}

func arr(m tree, keys ...string) []any {
	for _, k := range keys {
		if v, ok := m[k].([]any); ok {
			return v
		}
	}
	return nil
}

func has(m tree, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

func str(m tree, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(v)
		}
	}
	return ""
}

func numVal(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func num(m tree, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := numVal(m[k]); ok {
			return v, true
		}
	}
	return 0, false
}

func intOr(m tree, def int, keys ...string) int {
	if v, ok := num(m, keys...); ok && !math.IsNaN(v) {
		return int(math.Round(v))
	}
	return def
}

func boolVal(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case float64:
		return x != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "on", "1", "enabled", "ja":
			return true, true
		case "false", "no", "off", "0", "disabled", "nein":
			return false, true
		}
	}
	return false, false
}

func boolOr(m tree, def bool, keys ...string) bool {
	for _, k := range keys {
		if b, ok := boolVal(m[k]); ok {
			return b
		}
	}
	return def
}

func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func timeString(v any) string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return ""
		}
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return msToRFC(n)
		}
		return x
	case float64:
		return msToRFC(int64(x))
	}
	return ""
}

func msToRFC(n int64) string {
	if n < 1e11 { // seconds
		n *= 1000
	}
	return time.UnixMilli(n).UTC().Format(time.RFC3339)
}

// ---- sections ---------------------------------------------------------------------

func parseSystem(m tree, dataDir string, rep *LoadReport) System {
	def := DefaultSystem(dataDir)
	server := obj(m, "server", "http", "listen")
	media := obj(m, "media", "go2rtc", "mediaLayer")
	gm := obj(media, "go2rtc")
	ff := obj(m, "ffmpeg")
	s := System{
		Name:                firstNonEmpty(str(m, "name", "systemName", "title"), def.Name),
		ListenHost:          firstNonEmpty(str(m, "listenHost", "host", "bindHost"), str(server, "host", "listenHost"), def.ListenHost),
		ListenPort:          intOr(m, intOr(server, def.ListenPort, "port", "listenPort"), "listenPort", "port", "httpPort"),
		HealthIntervalMs:    intOr(m, def.HealthIntervalMs, "healthIntervalMs", "healthCheckIntervalMs", "healthcheckIntervalMs", "deviceHealthIntervalMs"),
		ReconnectMinMs:      intOr(m, def.ReconnectMinMs, "reconnectMinMs", "reconnectMinimumMs", "reconnectMin"),
		ReconnectMaxMs:      intOr(m, def.ReconnectMaxMs, "reconnectMaxMs", "reconnectMaximumMs", "reconnectMax"),
		SessionTTLHours:     intOr(m, def.SessionTTLHours, "sessionTtlHours", "sessionTTLHours"),
		KioskSessionTTLDays: intOr(m, def.KioskSessionTTLDays, "kioskSessionTtlDays", "kioskSessionTTLDays"),
	}
	if disp := obj(m, "display"); disp != nil {
		s.Display = DisplaySettings{Browser: str(disp, "browser"), DailyRestart: str(disp, "dailyRestart")}
	}
	if v, ok := num(m, "sessionTtlMs", "sessionTTLMs"); ok && v > 0 {
		s.SessionTTLHours = int(math.Max(1, math.Round(v/3.6e6)))
	}
	if v, ok := num(m, "kioskSessionTtlMs", "kioskSessionTTLMs"); ok && v > 0 {
		s.KioskSessionTTLDays = int(math.Max(1, math.Round(v/8.64e7)))
	}
	all := func(keys ...string) tree {
		// search keys in media, media.go2rtc and system level
		for _, t := range []tree{media, gm, m} {
			if has(t, keys...) {
				return t
			}
		}
		return tree{}
	}
	md := def.Media
	md.Enabled = boolOr(all("enabled", "mediaEnabled", "mediaLayerEnabled"), md.Enabled, "enabled", "mediaEnabled", "mediaLayerEnabled")
	md.PreloadMultiview = boolOr(all("preloadMultiview", "warmMultiview", "keepMultiviewWarm", "preload"), md.PreloadMultiview, "preloadMultiview", "warmMultiview", "keepMultiviewWarm", "preload")
	md.PreloadMain = boolOr(all("preloadMain", "warmMain", "keepMainWarm", "preloadMainstreams"), md.PreloadMain, "preloadMain", "warmMain", "keepMainWarm", "preloadMainstreams")
	md.AllowWebRTC = boolOr(all("allowWebRtc", "webrtcEnabled"), md.AllowWebRTC, "allowWebRtc", "webrtcEnabled")
	md.ReconcileIntervalMs = intOr(all("reconcileIntervalMs", "streamReconcileMs", "reconcileMs"), md.ReconcileIntervalMs, "reconcileIntervalMs", "streamReconcileMs", "reconcileMs")
	md.Go2rtcURL = firstNonEmpty(str(all("go2rtcUrl", "apiUrl", "go2rtcApiUrl", "url"), "go2rtcUrl", "apiUrl", "go2rtcApiUrl", "url"), md.Go2rtcURL)
	md.ManageGo2rtc = boolOr(all("manageGo2rtc", "manage", "managed", "selfManaged", "manageProcess"), md.ManageGo2rtc, "manageGo2rtc", "manage", "managed", "selfManaged", "manageProcess")
	md.Go2rtcBin = firstNonEmpty(str(all("go2rtcBin", "go2rtcBinary", "bin", "binary", "go2rtcPath"), "go2rtcBin", "go2rtcBinary", "bin", "binary", "go2rtcPath"), md.Go2rtcBin)
	md.FFmpegBin = firstNonEmpty(str(all("ffmpegBin", "ffmpegBinary", "ffmpegPath"), "ffmpegBin", "ffmpegBinary", "ffmpegPath"), str(ff, "bin", "path"), md.FFmpegBin)
	md.Go2rtcConfigPath = firstNonEmpty(str(all("go2rtcConfigPath", "configPath", "config"), "go2rtcConfigPath", "configPath", "config"), md.Go2rtcConfigPath)
	md.Go2rtcAPIListen = firstNonEmpty(str(all("go2rtcApiListen", "apiListen"), "go2rtcApiListen", "apiListen"), md.Go2rtcAPIListen)
	md.Go2rtcRTSPListen = firstNonEmpty(str(all("go2rtcRtspListen", "rtspListen"), "go2rtcRtspListen", "rtspListen"), md.Go2rtcRTSPListen)
	md.Go2rtcWebRTCListen = firstNonEmpty(str(all("go2rtcWebrtcListen", "webrtcListen"), "go2rtcWebrtcListen", "webrtcListen"), md.Go2rtcWebRTCListen)
	md.Go2rtcRestartDelayMs = intOr(all("go2rtcRestartDelayMs", "restartDelayMs"), md.Go2rtcRestartDelayMs, "go2rtcRestartDelayMs", "restartDelayMs")
	s.Media = md
	if len(m) == 0 {
		rep.note("system-Abschnitt fehlte; Standardwerte verwendet")
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// legacyCamera is a per-device channel entry of schema 1/2 configurations.
type legacyCamera struct {
	cam Camera
}

func parseDevice(m tree, rep *LoadReport) (Device, []legacyCamera, bool) {
	d := Device{
		ID:           strings.TrimSpace(str(m, "id", "deviceId")),
		Label:        str(m, "label", "name", "title"),
		Protocol:     strings.ToLower(strings.TrimSpace(str(m, "protocol"))),
		Host:         strings.TrimSpace(str(m, "host", "ip", "address", "hostname")),
		ChannelCount: intOr(m, 0, "channelCount", "channels_count", "numChannels"),
		Ports:        map[string]int{},
		Options:      map[string]string{},
	}
	if det := obj(m, "detected"); len(det) > 0 {
		d.Detected = &Detection{Variant: str(det, "variant"), Summary: str(det, "summary"), Channels: intOr(det, 0, "channels")}
	}
	legacyVariant, legacyChannels, migrated := "", 0, false
	if d.Protocol == "" {
		// up to schema 3: manufacturer + model
		man, mod := str(m, "manufacturer", "vendor", "provider", "brand"), str(m, "model", "type")
		d.Protocol, legacyVariant, legacyChannels = legacyProtocol(man, mod)
		if d.Protocol == "" && (man != "" || mod != "") {
			rep.note("Gerät %q: unbekannter Gerätetyp %q/%q – bitte das Protokoll neu wählen", d.Label, man, mod)
		} else if d.Protocol != "" {
			rep.migratedProtocols++
			migrated = true
		}
	}
	if d.ID == "" {
		d.ID = NewUUID()
	}
	cred := obj(m, "credentials", "auth", "login")
	d.Credentials.Username = firstNonEmpty(str(cred, "username", "user", "userName", "login"), str(m, "username", "user"))
	if p := cred["password"]; p != nil {
		d.Credentials.Password = rawJSON(p)
	} else if p := firstAny(cred, "passwordEnc", "encryptedPassword", "secret", "pass"); p != nil {
		d.Credentials.Password = rawJSON(p)
	} else if p := firstAny(m, "password", "passwordEnc", "encryptedPassword"); p != nil {
		d.Credentials.Password = rawJSON(p)
	}
	for k, v := range obj(m, "ports") {
		if n, ok := numVal(v); ok && n > 0 && n < 65536 {
			d.Ports[normalizePortKey(k)] = int(n)
		}
	}
	for _, k := range []string{"httpPort", "rtspPort", "nativePort", "tcpPort", "port"} {
		if n, ok := num(m, k); ok && n > 0 && n < 65536 {
			key := normalizePortKey(strings.TrimSuffix(k, "Port"))
			if _, exists := d.Ports[key]; !exists {
				d.Ports[key] = int(n)
			}
		}
	}
	opts := obj(m, "options", "settings")
	for k, v := range opts {
		switch x := v.(type) {
		case string:
			d.Options[k] = x
		case bool, float64:
			d.Options[k] = str(opts, k)
		}
	}
	// option aliases
	if v := firstNonEmpty(str(opts, "recordingStorage", "storage", "storageOverride", "hdd"), str(m, "recordingStorage", "storageOverride", "hdd")); v != "" {
		d.Options[OptRecordingStorage] = NormalizeStorage(v)
	}
	// the DVRIP live transport option is obsolete (always detected automatically)
	for _, alias := range []string{"storage", "storageOverride", "hdd", OptLiveProtocol, "dvripLiveProtocol", "dvripMode", "liveMode"} {
		delete(d.Options, alias)
	}
	if md := obj(m, "metadata", "meta"); len(md) > 0 {
		d.Metadata = md
	}
	var chans []legacyCamera
	for i, c := range arr(m, "channels", "cameras") {
		cm, ok := c.(tree)
		if !ok {
			continue
		}
		cam := parseChannel(cm, i)
		cam.DeviceID = d.ID
		chans = append(chans, legacyCamera{cam: cam})
	}
	if d.ChannelCount == 0 {
		d.ChannelCount = len(chans)
	}
	if d.ChannelCount == 0 {
		d.ChannelCount = legacyChannels
	}
	if migrated && d.Detected == nil && (legacyVariant != "" || d.ChannelCount > 0) {
		// migrated: keep connecting exactly as before, keep the camera count
		d.Detected = &Detection{Variant: legacyVariant, Channels: d.ChannelCount}
	}
	return d, chans, true
}

func firstAny(m tree, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func normalizePortKey(k string) string {
	switch strings.ToLower(k) {
	case "http", "web", "www", "httpport":
		return "http"
	case "rtsp", "rtspport":
		return "rtsp"
	case "native", "tcp", "service", "media", "dvrip", "nativeport", "sdk", "":
		return "native"
	}
	return strings.ToLower(k)
}

// migrateLegacyCameras turns per-device channel lists (schema 1/2) into
// first-class cameras with new ids and rewrites monitor slot references
// ("<deviceId>:<index>") accordingly.
func migrateLegacyCameras(c *Config, legacy []legacyCamera, rep *LoadReport) {
	idMap := map[string]string{}
	for _, l := range legacy {
		cam := l.cam
		cam.ID = NewUUID()
		idMap[l.cam.DeviceID+":"+strconv.Itoa(l.cam.Channel)] = cam.ID
		c.Cameras = append(c.Cameras, cam)
	}
	// channels without an explicit entry get cameras too (so slots can be mapped)
	for _, d := range c.Devices {
		for ch := 0; ch < d.ChannelCount; ch++ {
			key := d.ID + ":" + strconv.Itoa(ch)
			if _, ok := idMap[key]; !ok {
				cam := DefaultCamera(d.ID, ch)
				idMap[key] = cam.ID
				c.Cameras = append(c.Cameras, cam)
			}
		}
	}
	mapped := 0
	for mi := range c.Monitors {
		for ri := range c.Monitors[mi].Layout.Rows {
			for si := range c.Monitors[mi].Layout.Rows[ri].Slots {
				sl := &c.Monitors[mi].Layout.Rows[ri].Slots[si]
				if sl.CameraID == "" {
					continue
				}
				if id, ok := idMap[sl.CameraID]; ok {
					sl.CameraID = id
					mapped++
				}
			}
		}
	}
	if len(idMap) > 0 {
		rep.note("%d Kameras als eigenständige Objekte übernommen, %d Monitor-Slots umgestellt", len(idMap), mapped)
	}
}

func parseCamera(m tree) Camera {
	cam := parseChannel(m, intOr(m, 0, "channel", "index"))
	cam.ID = strings.TrimSpace(str(m, "id"))
	cam.DeviceID = strings.TrimSpace(str(m, "deviceId", "device"))
	return cam
}

func parseChannel(m tree, pos int) Camera {
	ch := DefaultCamera("", intOr(m, pos, "index", "channel", "idx"))
	ch.Enabled = boolOr(m, true, "enabled", "active")
	if b, ok := boolVal(m["disabled"]); ok && b {
		ch.Enabled = false
	}
	ch.Label = str(m, "label", "name", "title")
	st := obj(m, "streams")
	ch.Streams.Main = streamFlag(st["main"], true)
	ch.Streams.Sub = streamFlag(st["sub"], true)
	if b, ok := boolVal(m["mainAvailable"]); ok {
		ch.Streams.Main = b
	}
	if b, ok := boolVal(m["subAvailable"]); ok {
		ch.Streams.Sub = b
	}
	view := obj(m, "view", "presentation", "display")
	ch.View.AspectRatio = NormalizeAspect(firstNonEmpty(str(view, "aspectRatio", "aspect", "format"), str(m, "aspectRatio")))
	ch.View.ObjectFit = NormalizeFit(firstNonEmpty(str(view, "objectFit", "fit"), str(m, "objectFit", "fit")))
	// a former "multiviewSource" policy is ignored: stream choice is automatic
	if md := obj(m, "metadata", "meta"); len(md) > 0 {
		ch.Metadata = md
	}
	return ch
}

func streamFlag(v any, def bool) bool {
	if v == nil {
		return def
	}
	if b, ok := boolVal(v); ok {
		return b
	}
	if m, ok := v.(tree); ok {
		for _, k := range []string{"available", "enabled", "active"} {
			if b, ok := boolVal(m[k]); ok {
				return b
			}
		}
		return def
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) != ""
	}
	return def
}

func parseMonitor(m tree) Monitor {
	mon := Monitor{
		ID:      strings.TrimSpace(str(m, "id")),
		Label:   str(m, "label", "name", "title"),
		Enabled: boolOr(m, true, "enabled", "active"),
	}
	l := obj(m, "layout")
	mon.Layout.Template = str(l, "template", "preset")
	mon.Layout.Gap = intOr(l, intOr(m, DefaultLayoutGap, "gap"), "gap")
	for _, r := range arr(l, "rows") {
		rm, ok := r.(tree)
		if !ok {
			continue
		}
		row := Row{ID: str(rm, "id"), Height: 1}
		if h, ok := num(rm, "height", "weight", "size"); ok {
			row.Height = h
		}
		for _, s := range arr(rm, "slots", "cells", "tiles") {
			sm, ok := s.(tree)
			if !ok {
				continue
			}
			row.Slots = append(row.Slots, Slot{
				ID:       str(sm, "id"),
				CameraID: str(sm, "cameraId", "camera", "camId"),
				Width:    intOr(sm, 1, "width", "weight", "span", "size"),
				Label:    str(sm, "label", "name", "title"),
			})
		}
		mon.Layout.Rows = append(mon.Layout.Rows, row)
	}
	return mon
}

func parseUser(m tree, rep *LoadReport) (User, bool) {
	u := User{
		ID:        strings.TrimSpace(str(m, "id")),
		Username:  strings.TrimSpace(str(m, "username", "name", "login", "user")),
		Role:      strings.ToLower(str(m, "role")),
		Enabled:   boolOr(m, true, "enabled", "active"),
		CreatedAt: timeString(firstAny(m, "createdAt", "created")),
	}
	if b, ok := boolVal(m["disabled"]); ok && b {
		u.Enabled = false
	}
	if b, ok := boolVal(m["isAdmin"]); ok && b && u.Role == "" {
		u.Role = RoleAdmin
	}
	if u.Username == "" {
		rep.note("Benutzer ohne Namen übersprungen")
		return u, false
	}
	if h := firstAny(m, "passwordHash", "password_hash", "hash", "password", "pwHash"); h != nil {
		u.PasswordHash = rawJSON(h)
	}
	if p := obj(m, "preferences", "prefs"); p != nil {
		u.Preferences.StartupScreen = strings.TrimSpace(str(p, "startupScreen", "startup", "startPage"))
		u.Preferences.CameraPicker = strings.ToLower(strings.TrimSpace(str(p, "cameraPicker", "pickerMode")))
	}
	for _, k := range arr(m, "apiKeys", "kioskKeys", "keys") {
		km, ok := k.(tree)
		if !ok {
			continue
		}
		key := APIKey{
			ID:        str(km, "id"),
			Label:     str(km, "label", "name"),
			MonitorID: str(km, "monitorId", "monitor", "mon"),
			CreatedAt: timeString(firstAny(km, "createdAt", "created")),
			Enabled:   boolOr(km, true, "enabled", "active"),
			RevokedAt: timeString(firstAny(km, "revokedAt")),
			TokenHash: str(km, "tokenHash", "hash", "keyHash", "sha256"),
		}
		if b, ok := boolVal(km["revoked"]); ok && b {
			key.Enabled = false
		}
		if key.RevokedAt != "" {
			key.Enabled = false
		}
		if e := firstAny(km, "tokenEnc", "encryptedToken", "tokenEncrypted", "keyEnc", "secret"); e != nil {
			key.TokenEnc = rawJSON(e)
		}
		u.APIKeys = append(u.APIKeys, key)
	}
	return u, true
}

// dedupe drops duplicate ids that would break lookups.
func dedupe(c *Config, rep *LoadReport) {
	seen := map[string]bool{}
	devs := c.Devices[:0]
	for _, d := range c.Devices {
		if seen[d.ID] {
			rep.note("doppelte Device-ID %q verworfen", d.ID)
			continue
		}
		seen[d.ID] = true
		devs = append(devs, d)
	}
	c.Devices = devs
	seen = map[string]bool{}
	mons := c.Monitors[:0]
	for _, m := range c.Monitors {
		if m.ID != "" && seen[m.ID] {
			m.ID = NewID("m_")
			rep.note("doppelte Monitor-ID umbenannt")
		}
		seen[m.ID] = true
		mons = append(mons, m)
	}
	c.Monitors = mons
	seenUser := map[string]bool{}
	seenName := map[string]bool{}
	users := c.Users[:0]
	for _, u := range c.Users {
		ln := strings.ToLower(u.Username)
		if seenName[ln] {
			rep.note("doppelter Benutzername %q verworfen", u.Username)
			continue
		}
		seenName[ln] = true
		if u.ID == "" || seenUser[u.ID] {
			u.ID = NewID("u_")
		}
		seenUser[u.ID] = true
		users = append(users, u)
	}
	c.Users = users
}
