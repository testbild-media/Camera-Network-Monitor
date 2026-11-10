package dahua

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cnm/internal/provider"
)

// Device settings over RPC2 (configManager). Tables are read, changed in
// place and written back, so fields CNM does not know are kept unchanged.
//
//	Encode        table[ch].MainFormat[0..2] (regular, motion, alarm recording),
//	              table[ch].ExtraFormat[0] (sub stream); Video: Compression,
//	              Width, Height, FPS, BitRateControl, BitRate, Quality, GOP (frames)
//	ChannelTitle  table[ch].Name
//	RecordMode    table[ch].Mode: 0 schedule, 1 always (manual), 2 off
//	Record        table[ch].TimeSection[day 0–6 (+7 holiday)][6] "mask HH:MM:SS-HH:MM:SS",
//	              mask bits 1 regular, 2 motion, 4 alarm
//	MotionDetect  table[ch]: Enable, Level 1–6 (older), MotionDetectWindow[0].Sensitive
//	              0–100 (DetectVersion V3.0), EventHandler.RecordEnable/RecordChannels/TimeSection
//	LossDetect, BlindDetect  table[ch]: Enable, EventHandler{BeepEnable, RecordEnable, RecordChannels, TimeSection}
//	StorageNotExist, StorageFailure, StorageLowSpace (LowerLimit %)  table: Enable, EventHandler
//	General       table: MachineName; Locales: VideoStandard "PAL" | "NTSC"
//	Network       table: Hostname, DefaultInterface, <if>{IPAddress, SubnetMask, DefaultGateway,
//	              DnsServers, DhcpEnable, PhysicalAddress}; DVRIP.TCPPort, Web.Port, RTSP.Port
//	NTP           table: Enable, Address, Port, UpdatePeriod (minutes)
//	StorageGroup  table[0].OverWrite

// getConfig returns the raw table of a configuration (numbers kept exact).
func (d *rpcDriver) getConfig(ctx context.Context, name string) (any, error) {
	r, err := d.rpc.call(ctx, "configManager.getConfig", map[string]any{"name": name}, nil)
	if err != nil {
		return nil, err
	}
	if !r.ok() {
		return nil, provider.NewError(provider.KindUnsupported, nil, "Konfiguration %s nicht verfügbar", name)
	}
	var p struct {
		Table json.RawMessage `json:"table"`
	}
	if err := decodeParams(r, &p); err != nil || len(p.Table) == 0 {
		return nil, provider.NewError(provider.KindProtocol, err, "Konfiguration %s: unerwartete Antwort", name)
	}
	dec := json.NewDecoder(bytes.NewReader(p.Table))
	dec.UseNumber()
	var t any
	if err := dec.Decode(&t); err != nil {
		return nil, provider.NewError(provider.KindProtocol, err, "Konfiguration %s: unerwartete Antwort", name)
	}
	return t, nil
}

// setConfig writes a table back; restart reports that the device asks for
// a restart.
func (d *rpcDriver) setConfig(ctx context.Context, name string, table any) (restart bool, err error) {
	r, err := d.rpc.call(ctx, "configManager.setConfig", map[string]any{"name": name, "table": table, "options": []any{}}, nil)
	if err != nil {
		return false, err
	}
	if !r.ok() {
		msg := ""
		if r.Error != nil {
			msg = fmt.Sprintf(" (Code %d %s)", r.Error.Code, strings.TrimSpace(r.Error.Message))
		}
		return false, provider.NewError(provider.KindProtocol, nil, "Das Gerät lehnt die Einstellung %s ab%s", name, msg)
	}
	var p struct {
		Options []string `json:"options"`
	}
	_ = decodeParams(r, &p)
	for _, o := range p.Options {
		if strings.Contains(strings.ToLower(o), "reboot") || strings.Contains(strings.ToLower(o), "restart") {
			restart = true
		}
	}
	return restart, nil
}

// ---- JSON helpers --------------------------------------------------------------------------

func jmap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func jarr(v any) []any {
	a, _ := v.([]any)
	return a
}

func jint(v any) int {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		f, _ := x.Float64()
		return int(f + 0.5)
	case float64:
		return int(x + 0.5)
	case bool:
		if x {
			return 1
		}
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}

func jbool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	}
	return jint(v) != 0
}

func jstr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return ""
}

// setBoolLike keeps the type the device used for a flag (bool or number).
func setBoolLike(m map[string]any, key string, v bool) {
	switch m[key].(type) {
	case json.Number, float64:
		n := json.Number("0")
		if v {
			n = "1"
		}
		m[key] = n
	default:
		m[key] = v
	}
}

func at(a []any, i int) any {
	if i >= 0 && i < len(a) {
		return a[i]
	}
	return nil
}

// ---- reading -------------------------------------------------------------------------------

var dahuaResolutions = []provider.Choice{
	{Value: "352x288", Label: "CIF (352×288)"},
	{Value: "704x576", Label: "D1 (704×576)"},
	{Value: "960x576", Label: "960H (960×576)"},
	{Value: "1280x720", Label: "720p (1280×720)"},
	{Value: "1280x960", Label: "960p (1280×960)"},
	{Value: "960x1080", Label: "1080N (960×1080)"},
	{Value: "1920x1080", Label: "1080p (1920×1080)"},
	{Value: "2560x1440", Label: "4 MP (2560×1440)"},
	{Value: "2592x1944", Label: "5 MP (2592×1944)"},
	{Value: "3840x2160", Label: "4K (3840×2160)"},
}

var dahuaSubResolutions = []provider.Choice{
	{Value: "176x144", Label: "QCIF (176×144)"},
	{Value: "352x288", Label: "CIF (352×288)"},
	{Value: "704x576", Label: "D1 (704×576)"},
}

func codecOf(s string) string {
	if strings.HasPrefix(strings.ToUpper(s), "H.265") {
		return "H.265"
	}
	return "H.264"
}

func dahuaStream(entry any) *provider.StreamSettings {
	e := jmap(entry)
	v := jmap(e["Video"])
	if v == nil {
		return nil
	}
	return &provider.StreamSettings{
		Enabled: jbool(e["VideoEnable"]),
		Codec:   codecOf(jstr(v["Compression"])),
		Res:     fmt.Sprintf("%dx%d", jint(v["Width"]), jint(v["Height"])),
		FPS:     jint(v["FPS"]),
		Control: strings.ToUpper(jstr(v["BitRateControl"])),
		Bitrate: jint(v["BitRate"]),
		Quality: jint(v["Quality"]),
		GOP:     jint(v["GOP"]),
	}
}

func withCurrent(list []provider.Choice, cur string) []provider.Choice {
	for _, c := range list {
		if c.Value == cur {
			return list
		}
	}
	return append([]provider.Choice{{Value: cur, Label: strings.Replace(cur, "x", "×", 1) + " (aktuell)"}}, list...)
}

// dahuaRecord turns RecordMode and the Record schedule of a channel into
// the simple form.
func dahuaRecord(mode any, rec any) *provider.RecordSettings {
	if mode == nil {
		return nil
	}
	switch jint(jmap(mode)["Mode"]) {
	case 1:
		return &provider.RecordSettings{Mode: provider.RecordContinuous}
	case 2:
		return &provider.RecordSettings{Mode: provider.RecordOff}
	}
	days := jarr(jmap(rec)["TimeSection"])
	if len(days) < 7 {
		return &provider.RecordSettings{Mode: provider.RecordCustom}
	}
	return scheduleMode(func(day int) []string { return stringsOf(jarr(days[day])) }, func(s string) (int, string) {
		mask, span, _ := strings.Cut(strings.TrimSpace(s), " ")
		n, _ := strconv.Atoi(mask)
		return n, strings.TrimSpace(span)
	})
}

func stringsOf(a []any) []string {
	out := make([]string, 0, len(a))
	for _, v := range a {
		out = append(out, jstr(v))
	}
	return out
}

// scheduleMode recognises "the same window every day with one mask".
func scheduleMode(day func(int) []string, parse func(string) (mask int, span string)) *provider.RecordSettings {
	var first []string
	mask := -1
	for d := 0; d < 7; d++ {
		var spans []string
		for _, s := range day(d) {
			m, span := parse(s)
			if m == 0 {
				continue
			}
			if mask >= 0 && m != mask {
				return &provider.RecordSettings{Mode: provider.RecordCustom}
			}
			mask = m
			spans = append(spans, span)
		}
		if d == 0 {
			first = spans
		} else if strings.Join(spans, ",") != strings.Join(first, ",") {
			return &provider.RecordSettings{Mode: provider.RecordCustom}
		}
	}
	if mask <= 0 {
		return &provider.RecordSettings{Mode: provider.RecordOff}
	}
	from, to, ok := provider.WindowFromSlots(first)
	if !ok {
		return &provider.RecordSettings{Mode: provider.RecordCustom}
	}
	switch {
	case mask&1 != 0:
		return &provider.RecordSettings{Mode: provider.RecordContinuous, From: from, To: to}
	case mask&2 != 0:
		return &provider.RecordSettings{Mode: provider.RecordMotion, From: from, To: to}
	}
	return &provider.RecordSettings{Mode: provider.RecordCustom}
}

// dahuaMotion: firmware with detection windows (V3.0) uses the window's
// sensitivity 0–100, older firmware the level 1–6.
func dahuaMotion(md any) *provider.MotionSettings {
	m := jmap(md)
	if m == nil {
		return nil
	}
	ms := &provider.MotionSettings{Enabled: jbool(m["Enable"]), Sensitivity: jint(m["Level"]), SensitivityMax: 6}
	if w := jmap(at(jarr(m["MotionDetectWindow"]), 0)); w != nil {
		if _, ok := w["Sensitive"]; ok {
			ms.Sensitivity, ms.SensitivityMax = jint(w["Sensitive"]), 100
		}
	}
	return ms
}

// ReadSettings reads the device configuration.
func (d *rpcDriver) ReadSettings(ctx context.Context) (*provider.DeviceSettings, error) {
	enc, err := d.getConfig(ctx, "Encode")
	if err != nil {
		return nil, err
	}
	titles, _ := d.getConfig(ctx, "ChannelTitle")
	modes, _ := d.getConfig(ctx, "RecordMode")
	recs, _ := d.getConfig(ctx, "Record")
	mds, _ := d.getConfig(ctx, "MotionDetect")
	losses, _ := d.getConfig(ctx, "LossDetect")
	blinds, _ := d.getConfig(ctx, "BlindDetect")
	encs := jarr(enc)
	n := len(encs)
	if d.channels > 0 && d.channels < n {
		n = d.channels
	}
	h265 := strings.HasPrefix(d.firmware, "4.") || strings.HasPrefix(d.firmware, "5.")
	for _, e := range encs {
		for _, f := range append(jarr(jmap(e)["MainFormat"]), jarr(jmap(e)["ExtraFormat"])...) {
			if codecOf(jstr(jmap(jmap(f)["Video"])["Compression"])) == "H.265" {
				h265 = true
			}
		}
	}
	codecs := []string{"H.264"}
	if h265 {
		codecs = append(codecs, "H.265")
	}
	s := &provider.DeviceSettings{CanReboot: true, Notes: []string{
		"Hinweis: Dieses Gerät meldet nicht, welche Werte es erlaubt. CNM bietet übliche Werte an und zeigt nach dem Speichern, falls das Gerät einen Wert anders übernimmt.",
		"Hinweis: Die Hauptstream-Einstellungen gelten bei diesem Gerät gleichermaßen für Dauer-, Bewegungs- und Alarmaufnahme.",
	}}
	for ch := 0; ch < n; ch++ {
		e := jmap(encs[ch])
		c := provider.ChannelSettings{Channel: ch}
		if t := jmap(at(jarr(titles), ch)); t != nil {
			name := jstr(t["Name"])
			c.Name = &name
		}
		if m := dahuaStream(at(jarr(e["MainFormat"]), 0)); m != nil {
			fpsMax := max(25, m.FPS)
			m.Options = &provider.StreamOptions{Codecs: codecs, Resolutions: withCurrent(dahuaResolutions, m.Res), FPSMax: fpsMax,
				BitrateMin: 64, BitrateMax: 16384, GOPUnit: "frames", GOPMax: 250}
			c.Main = m
		}
		if sub := dahuaStream(at(jarr(e["ExtraFormat"]), 0)); sub != nil {
			sub.Options = &provider.StreamOptions{Codecs: codecs, Resolutions: withCurrent(dahuaSubResolutions, sub.Res), FPSMax: max(25, sub.FPS),
				BitrateMin: 32, BitrateMax: 4096, GOPUnit: "frames", GOPMax: 250}
			c.Sub = sub
		}
		c.Record = dahuaRecord(at(jarr(modes), ch), at(jarr(recs), ch))
		c.Motion = dahuaMotion(at(jarr(mds), ch))
		if c.Motion != nil {
			if ev := dahuaEvent(at(jarr(mds), ch), ch); ev != nil {
				c.Motion.Beep = ev.Beep
			}
		}
		c.VideoLoss = dahuaEvent(at(jarr(losses), ch), ch)
		c.Tamper = dahuaEvent(at(jarr(blinds), ch), ch)
		s.Channels = append(s.Channels, c)
	}
	s.Time = d.readTime(ctx)
	s.Disks = d.readDisks(ctx)
	s.DiskAlarms = d.readDiskAlarms(ctx)
	if sg := jmap(at(jarr(d.mustConfig(ctx, "StorageGroup")), 0)); sg != nil {
		if v, ok := sg["OverWrite"]; ok {
			b := jbool(v)
			s.Overwrite = &b
		}
	}
	sys := &provider.SystemSettings{}
	if g := firstObject(d.mustConfig(ctx, "General")); g != nil {
		if v, ok := g["MachineName"]; ok {
			name := jstr(v)
			sys.Name = &name
		}
	}
	if l := firstObject(d.mustConfig(ctx, "Locales")); l != nil {
		sys.VideoStandard = strings.ToUpper(jstr(l["VideoStandard"]))
	}
	if sys.Name != nil || sys.VideoStandard != "" {
		s.System = sys
	}
	s.Network = d.readNetwork(ctx)
	s.Users = d.canChangePassword()
	return s, nil
}

// readNetwork reads the network settings for display.
func (d *rpcDriver) readNetwork(ctx context.Context) *provider.NetworkInfo {
	nw := jmap(d.mustConfig(ctx, "Network"))
	if nw == nil {
		return nil
	}
	ifName := jstr(nw["DefaultInterface"])
	if ifName == "" {
		ifName = "eth0"
	}
	n := &provider.NetworkInfo{Hostname: jstr(nw["Hostname"]), Ports: map[string]int{}}
	if e := jmap(nw[ifName]); e != nil {
		n.IP, n.Mask, n.Gateway, n.MAC = jstr(e["IPAddress"]), jstr(e["SubnetMask"]), jstr(e["DefaultGateway"]), jstr(e["PhysicalAddress"])
		for _, x := range jarr(e["DnsServers"]) {
			if s := jstr(x); s != "" {
				n.DNS = append(n.DNS, s)
			}
		}
		if v, ok := e["DhcpEnable"]; ok {
			b := jbool(v)
			n.DHCP = &b
		}
	}
	for _, p := range []struct{ table, field, label string }{{"Web", "Port", "HTTP"}, {"DVRIP", "TCPPort", "TCP"}, {"RTSP", "Port", "RTSP"}} {
		if t := firstObject(d.mustConfig(ctx, p.table)); t != nil {
			if v := jint(t[p.field]); v > 0 {
				n.Ports[p.label] = v
			}
		}
	}
	return n
}

func (d *rpcDriver) mustConfig(ctx context.Context, name string) any {
	t, _ := d.getConfig(ctx, name)
	return t
}

func (d *rpcDriver) readTime(ctx context.Context) *provider.TimeSettings {
	r, err := d.rpc.call(ctx, "global.getCurrentTime", nil, nil)
	if err != nil || !r.ok() {
		return nil
	}
	var p struct {
		Time string `json:"time"`
	}
	if decodeParams(r, &p) != nil {
		return nil
	}
	wall, err := parseDahuaTime(p.Time)
	if err != nil {
		return nil
	}
	ts := &provider.TimeSettings{DeviceTime: wall.Format(dahuaTime), OffsetSeconds: int(wall.Sub(provider.WallNow()).Seconds())}
	if ntp := jmap(d.mustConfig(ctx, "NTP")); ntp != nil {
		ts.NTP = &provider.NTPSettings{Enabled: jbool(ntp["Enable"]), Server: jstr(ntp["Address"]), Port: jint(ntp["Port"]), IntervalMin: jint(ntp["UpdatePeriod"])}
	}
	return ts
}

func (d *rpcDriver) readDisks(ctx context.Context) []provider.DiskInfo {
	r, err := d.rpc.call(ctx, "storage.getDeviceAllInfo", nil, nil)
	if err != nil || !r.ok() {
		return d.recordingUsage(ctx)
	}
	var p struct {
		Info []struct {
			Name   string `json:"Name"`
			State  string `json:"State"`
			Detail []struct {
				TotalBytes json.Number `json:"TotalBytes"`
				UsedBytes  json.Number `json:"UsedBytes"`
			} `json:"Detail"`
		} `json:"info"`
	}
	if decodeParams(r, &p) != nil {
		return nil
	}
	out := []provider.DiskInfo{}
	for _, i := range p.Info {
		var total, used float64
		for _, det := range i.Detail {
			t, _ := det.TotalBytes.Float64()
			u, _ := det.UsedBytes.Float64()
			total, used = total+t, used+u
		}
		out = append(out, provider.DiskInfo{Name: i.Name, State: i.State, TotalBytes: int64(total), FreeBytes: int64(total - used),
			Error: i.State != "" && !strings.EqualFold(i.State, "Success")})
	}
	return out
}

// dahuaEvent reads an event entry: Enable and its EventHandler's buzzer and
// recording of channel ch (ch < 0: device event without recording).
func dahuaEvent(v any, ch int) *provider.EventSettings {
	m := jmap(v)
	if m == nil {
		return nil
	}
	ev := &provider.EventSettings{Enabled: jbool(m["Enable"])}
	if eh := jmap(m["EventHandler"]); eh != nil {
		if b, ok := eh["BeepEnable"]; ok {
			x := jbool(b)
			ev.Beep = &x
		}
		if r, ok := eh["RecordEnable"]; ok && ch >= 0 {
			x := false
			if jbool(r) {
				for _, c := range jarr(eh["RecordChannels"]) {
					x = x || jint(c) == ch
				}
			}
			ev.Record = &x
		}
	}
	return ev
}

// applyDahuaEvent writes an event's switches into its entry; an enabled
// event gets an active schedule (without one it never fires).
func applyDahuaEvent(m map[string]any, want *provider.EventSettings, ch int) {
	setBoolLike(m, "Enable", want.Enabled)
	eh := jmap(m["EventHandler"])
	if eh == nil {
		return
	}
	if want.Beep != nil {
		if _, ok := eh["BeepEnable"]; ok {
			setBoolLike(eh, "BeepEnable", *want.Beep)
		}
	}
	if want.Record != nil && ch >= 0 {
		if _, ok := eh["RecordEnable"]; ok {
			var chans []any
			for _, c := range jarr(eh["RecordChannels"]) {
				if jint(c) != ch {
					chans = append(chans, c)
				}
			}
			if *want.Record {
				chans = append(chans, json.Number(strconv.Itoa(ch)))
			}
			if chans == nil {
				chans = []any{}
			}
			eh["RecordChannels"] = chans
			setBoolLike(eh, "RecordEnable", len(chans) > 0)
		}
	}
	if days := jarr(eh["TimeSection"]); want.Enabled && len(days) >= 7 && !hasActiveSection(days) {
		for d := 0; d < 7; d++ {
			days[d] = dahuaSections(1, "", "")
		}
	}
}

// firstObject is a table object or the first object of a list.
func firstObject(v any) map[string]any {
	if m := jmap(v); m != nil {
		return m
	}
	return jmap(at(jarr(v), 0))
}

// readDiskAlarms reads the reactions to disk problems (nil when the device
// offers none of them).
func (d *rpcDriver) readDiskAlarms(ctx context.Context) *provider.DiskAlarms {
	a := &provider.DiskAlarms{}
	found := false
	for _, ev := range []struct {
		table string
		dst   **provider.EventSettings
	}{{"StorageNotExist", &a.NoDisk}, {"StorageFailure", &a.Failure}, {"StorageLowSpace", &a.LowSpace}} {
		t, err := d.getConfig(ctx, ev.table)
		if err != nil {
			continue
		}
		m := firstObject(t)
		if m == nil {
			continue
		}
		*ev.dst = dahuaEvent(m, -1)
		found = true
		if ev.table == "StorageLowSpace" {
			a.LowSpacePercent = jint(m["LowerLimit"])
		}
	}
	if !found {
		return nil
	}
	return a
}

// recordingUsage sums the recording files of all channels, for firmware
// that does not report its disk: once the disk is full and overwritten this
// is about its size, and the files give the period the recordings cover.
// Listing a few thousand files takes seconds, so the result is kept for a
// while (saving reads the settings back after every channel).
func (d *rpcDriver) recordingUsage(ctx context.Context) []provider.DiskInfo {
	d.diskMu.Lock()
	defer d.diskMu.Unlock()
	if d.diskEst != nil && time.Since(d.diskAt) < 10*time.Minute {
		return d.diskEst
	}
	n := d.channels
	if n <= 0 {
		n = d.cfg.Channels
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	// searched backwards in 30-day windows: firmware refuses long ranges
	const window = 30 * 24 * time.Hour
	end := d.clock.ToDevice(time.Now()).Add(time.Hour)
	var total int64
	var oldest, newest time.Time
	for ch := 0; ch < n; ch++ {
		seen := map[time.Time]bool{} // files spanning two windows
		found := false
		for k := 0; k < 14; k++ {
			wEnd := end.Add(-time.Duration(k) * window)
			files, err := d.listFiles(ctx, ch, wEnd.Add(-window), wEnd, 20000)
			if err != nil {
				return nil
			}
			if len(files) == 0 {
				if found || k >= 2 {
					break
				}
				continue
			}
			found = true
			for _, f := range files {
				if seen[f.start] {
					continue
				}
				seen[f.start] = true
				total += f.length
				if oldest.IsZero() || f.start.Before(oldest) {
					oldest = f.start
				}
				if f.end.After(newest) {
					newest = f.end
				}
			}
		}
	}
	if total == 0 {
		return nil
	}
	d.diskEst = []provider.DiskInfo{{Name: "Aufnahmen", TotalBytes: total, Estimated: true,
		Oldest: oldest.Format(dahuaTime), Newest: newest.Format(dahuaTime)}}
	d.diskAt = time.Now()
	return d.diskEst
}

// ---- writing -------------------------------------------------------------------------------

// applyStream changes the video fields of a format entry that differ from
// the current values.
func applyStream(entry map[string]any, want *provider.StreamSettings) error {
	v := jmap(entry["Video"])
	if v == nil {
		return errors.New("Stream ohne Video-Einstellungen")
	}
	cur := dahuaStream(entry)
	if cur.Enabled != want.Enabled {
		setBoolLike(entry, "VideoEnable", want.Enabled)
	}
	if cur.Codec != want.Codec {
		v["Compression"] = want.Codec
	}
	if cur.Res != want.Res {
		w, h, ok := strings.Cut(want.Res, "x")
		wi, e1 := strconv.Atoi(w)
		hi, e2 := strconv.Atoi(h)
		if !ok || e1 != nil || e2 != nil {
			return fmt.Errorf("Auflösung %q ungültig", want.Res)
		}
		v["Width"], v["Height"] = json.Number(strconv.Itoa(wi)), json.Number(strconv.Itoa(hi))
	}
	if cur.FPS != want.FPS {
		v["FPS"] = json.Number(strconv.Itoa(want.FPS))
	}
	if cur.Control != want.Control {
		v["BitRateControl"] = want.Control
	}
	if cur.Bitrate != want.Bitrate {
		v["BitRate"] = json.Number(strconv.Itoa(want.Bitrate))
	}
	if cur.Quality != want.Quality {
		v["Quality"] = json.Number(strconv.Itoa(want.Quality))
	}
	if cur.GOP != want.GOP {
		v["GOP"] = json.Number(strconv.Itoa(want.GOP))
	}
	return nil
}

// dahuaSections builds the 6 time sections of a day for a mask.
func dahuaSections(mask int, from, to string) []any {
	out := []any{}
	for _, s := range provider.DaySlots(from, to) {
		out = append(out, fmt.Sprintf("%d %s", mask, s))
	}
	for len(out) < 6 {
		out = append(out, "0 00:00:00-24:00:00")
	}
	return out
}

func hasActiveSection(days []any) bool {
	for d := 0; d < len(days) && d < 7; d++ {
		for _, s := range jarr(days[d]) {
			if mask, _, _ := strings.Cut(strings.TrimSpace(jstr(s)), " "); mask != "0" && mask != "" {
				return true
			}
		}
	}
	return false
}

// ApplySettings writes a change and reports what the device took over.
func (d *rpcDriver) ApplySettings(ctx context.Context, change provider.SettingsChange) (*provider.SettingsResult, error) {
	if err := change.Validate(); err != nil {
		return nil, err
	}
	tables := map[string]any{}
	dirty := map[string]bool{}
	load := func(name string) (any, error) {
		if t, ok := tables[name]; ok {
			return t, nil
		}
		t, err := d.getConfig(ctx, name)
		if err != nil {
			return nil, err
		}
		tables[name] = t
		return t, nil
	}
	entry := func(name string, ch int) (map[string]any, error) {
		t, err := load(name)
		if err != nil {
			return nil, err
		}
		m := jmap(at(jarr(t), ch))
		if m == nil {
			return nil, provider.NewError(provider.KindConfig, nil, "Kanal %d gibt es in %s nicht", ch+1, name)
		}
		return m, nil
	}
	for _, c := range change.Channels {
		if c.Name != nil {
			t, err := entry("ChannelTitle", c.Channel)
			if err != nil {
				return nil, err
			}
			if jstr(t["Name"]) != *c.Name {
				t["Name"] = *c.Name
				dirty["ChannelTitle"] = true
			}
		}
		if c.Main != nil || c.Sub != nil {
			e, err := entry("Encode", c.Channel)
			if err != nil {
				return nil, err
			}
			if c.Main != nil {
				for _, f := range jarr(e["MainFormat"]) {
					if err := applyStream(jmap(f), c.Main); err != nil {
						return nil, provider.NewError(provider.KindConfig, err, "Kanal %d Hauptstream: %v", c.Channel+1, err)
					}
				}
			}
			if c.Sub != nil {
				if f := jmap(at(jarr(e["ExtraFormat"]), 0)); f != nil {
					if err := applyStream(f, c.Sub); err != nil {
						return nil, provider.NewError(provider.KindConfig, err, "Kanal %d Substream: %v", c.Channel+1, err)
					}
				}
			}
			dirty["Encode"] = true
		}
		motionNeeded := c.Record != nil && c.Record.Mode == provider.RecordMotion
		if c.Motion != nil || motionNeeded {
			m, err := entry("MotionDetect", c.Channel)
			if err != nil {
				return nil, err
			}
			if c.Motion != nil {
				if eh := jmap(m["EventHandler"]); eh != nil && c.Motion.Beep != nil {
					if _, ok := eh["BeepEnable"]; ok {
						setBoolLike(eh, "BeepEnable", *c.Motion.Beep)
					}
				}
				setBoolLike(m, "Enable", c.Motion.Enabled || motionNeeded)
				if w := jmap(at(jarr(m["MotionDetectWindow"]), 0)); w != nil && w["Sensitive"] != nil {
					w["Sensitive"] = json.Number(strconv.Itoa(min(max(c.Motion.Sensitivity, 0), 100)))
				} else if c.Motion.Sensitivity >= 1 {
					m["Level"] = json.Number(strconv.Itoa(min(c.Motion.Sensitivity, 6)))
				}
			}
			if motionNeeded {
				setBoolLike(m, "Enable", true)
				if eh := jmap(m["EventHandler"]); eh != nil {
					setBoolLike(eh, "RecordEnable", true)
					chans := jarr(eh["RecordChannels"])
					found := false
					for _, x := range chans {
						found = found || jint(x) == c.Channel
					}
					if !found {
						eh["RecordChannels"] = append(chans, json.Number(strconv.Itoa(c.Channel)))
					}
					if days := jarr(eh["TimeSection"]); len(days) >= 7 && !hasActiveSection(days) {
						for d := 0; d < 7; d++ {
							days[d] = dahuaSections(1, "", "")
						}
					}
				}
			}
			dirty["MotionDetect"] = true
		}
		for _, ev := range []struct {
			table string
			want  *provider.EventSettings
		}{{"LossDetect", c.VideoLoss}, {"BlindDetect", c.Tamper}} {
			if ev.want == nil {
				continue
			}
			m, err := entry(ev.table, c.Channel)
			if err != nil {
				return nil, err
			}
			applyDahuaEvent(m, ev.want, c.Channel)
			dirty[ev.table] = true
		}
		if r := c.Record; r != nil {
			mode, err := entry("RecordMode", c.Channel)
			if err != nil {
				return nil, err
			}
			if r.Mode == provider.RecordOff {
				mode["Mode"] = json.Number("2")
			} else {
				rec, err := entry("Record", c.Channel)
				if err != nil {
					return nil, err
				}
				days := jarr(rec["TimeSection"])
				if len(days) < 7 {
					return nil, provider.NewError(provider.KindProtocol, nil, "Kanal %d: Aufnahme-Zeitplan unbekannt", c.Channel+1)
				}
				mask := 1
				if r.Mode == provider.RecordMotion {
					mask = 6 // motion and alarm
				}
				for d := 0; d < 7; d++ {
					days[d] = dahuaSections(mask, r.From, r.To)
				}
				mode["Mode"] = json.Number("0")
				dirty["Record"] = true
			}
			dirty["RecordMode"] = true
		}
	}
	if n := change.NTP; n != nil {
		t, err := load("NTP")
		if err != nil {
			return nil, err
		}
		m := jmap(t)
		if m == nil {
			return nil, provider.NewError(provider.KindUnsupported, nil, "NTP nicht verfügbar")
		}
		setBoolLike(m, "Enable", n.Enabled)
		if n.Server != "" {
			m["Address"] = n.Server
		}
		if n.Port > 0 {
			m["Port"] = json.Number(strconv.Itoa(n.Port))
		}
		if n.IntervalMin > 0 {
			m["UpdatePeriod"] = json.Number(strconv.Itoa(n.IntervalMin))
		}
		dirty["NTP"] = true
	}
	if a := change.DiskAlarms; a != nil {
		for _, ev := range []struct {
			table string
			want  *provider.EventSettings
		}{{"StorageNotExist", a.NoDisk}, {"StorageFailure", a.Failure}, {"StorageLowSpace", a.LowSpace}} {
			if ev.want == nil {
				continue
			}
			t, err := load(ev.table)
			if err != nil {
				return nil, err
			}
			m := firstObject(t)
			if m == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "%s nicht verfügbar", ev.table)
			}
			applyDahuaEvent(m, ev.want, -1)
			if ev.table == "StorageLowSpace" && a.LowSpacePercent > 0 {
				m["LowerLimit"] = json.Number(strconv.Itoa(a.LowSpacePercent))
			}
			dirty[ev.table] = true
		}
	}
	if sy := change.System; sy != nil {
		if sy.Name != nil {
			t, err := load("General")
			if err != nil {
				return nil, err
			}
			g := firstObject(t)
			if g == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "Gerätename nicht verfügbar")
			}
			g["MachineName"] = *sy.Name
			dirty["General"] = true
		}
		if sy.VideoStandard != "" {
			t, err := load("Locales")
			if err != nil {
				return nil, err
			}
			l := firstObject(t)
			if l == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "Videonorm nicht verfügbar")
			}
			if !strings.EqualFold(jstr(l["VideoStandard"]), sy.VideoStandard) {
				l["VideoStandard"] = sy.VideoStandard
				dirty["Locales"] = true
			}
		}
	}
	if change.Overwrite != nil {
		sg, err := entry("StorageGroup", 0)
		if err != nil {
			return nil, err
		}
		setBoolLike(sg, "OverWrite", *change.Overwrite)
		dirty["StorageGroup"] = true
	}
	res := &provider.SettingsResult{}
	// encoding first, the record mode after its schedule
	for _, name := range []string{"Encode", "ChannelTitle", "MotionDetect", "LossDetect", "BlindDetect", "Record", "RecordMode", "NTP",
		"StorageGroup", "StorageNotExist", "StorageFailure", "StorageLowSpace", "General", "Locales"} {
		if !dirty[name] {
			continue
		}
		restart, err := d.setConfig(ctx, name, tables[name])
		if err != nil {
			return nil, err
		}
		res.Restart = res.Restart || restart
	}
	after, err := d.ReadSettings(ctx)
	if err != nil {
		return nil, err
	}
	res.Settings = after
	res.Warnings = provider.CompareSettings(change, after)
	return res, nil
}

// SetDeviceTime sets the device clock.
func (d *rpcDriver) SetDeviceTime(ctx context.Context, wall time.Time) error {
	r, err := d.rpc.call(ctx, "global.setCurrentTime", map[string]any{"time": wall.Format(dahuaTime), "tolerance": 5}, nil)
	if err != nil {
		return err
	}
	if !r.ok() {
		return provider.NewError(provider.KindProtocol, nil, "Das Gerät lehnt das Stellen der Uhr ab")
	}
	d.syncTime(ctx)
	return nil
}

// Reboot restarts the device. A dropped connection counts as success: the
// device is restarting.
func (d *rpcDriver) Reboot(ctx context.Context) error {
	r, err := d.rpc.call(ctx, "magicBox.reboot", nil, nil)
	if err != nil {
		if provider.KindOf(err) == provider.KindNetwork {
			return nil
		}
		return err
	}
	if !r.ok() {
		return provider.NewError(provider.KindProtocol, nil, "Das Gerät lehnt den Neustart ab")
	}
	return nil
}
