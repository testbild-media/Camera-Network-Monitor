package sofia

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cnm/internal/provider"
)

// Device settings over DVRIP (read with CONFIG_GET, written with
// CONFIG_SET; entries are changed in place so unknown fields stay):
//
//	Simplify.Encode           [ch].MainFormat / .ExtraFormat: VideoEnable, Video{Compression,
//	                          Resolution (name, e.g. "D1"), FPS, BitRateControl, BitRate, Quality, GOP (seconds)}
//	EncodeCapability (1360)   EncodeInfo[0|1].ResolutionMask/CompressionMask, ImageSizePerChannel,
//	                          ExImageSizePerChannel (bit n = resolution n of resolutionNames)
//	ChannelTitle (1048/1046)  names
//	Record.[ch]               RecordMode ConfigRecord|ManualRecord|ClosedRecord, TimeSection[7][6]
//	                          "1 HH:MM:SS-HH:MM:SS", Mask[7][6] "0x7" (1 regular, 2 motion, 4 alarm)
//	Detect.MotionDetect.[ch]  Enable, Level 1–6, EventHandler{RecordEnable, RecordMask, BeepEnable, TimeSection}
//	Detect.LossDetect.[ch]    video loss: Enable, EventHandler (as above)
//	Detect.BlindDetect.[ch]   camera covered: Enable, Level, EventHandler
//	Storage.StorageNotExist, Storage.StorageFailure, Storage.StorageLowSpace (LowerLimit %)
//	                          Enable, EventHandler{BeepEnable, …}
//	NetWork.NetNTP            Enable, Server{Name, Port}, UpdatePeriod
//	General.General           OverWrite "OverWrite" | "StopRecord", MachineName
//	General.Location          VideoFormat "PAL" | "NTSC" (change: restart)
//	General.AutoMaintain      AutoRebootDay "Never" | "EveryDay" | "Sunday" … , AutoRebootHour
//	NetWork.NetCommon         HostIP, Submask, GateWay (hex, first octet in the low byte), MAC,
//	                          HostName, HttpPort, TCPPort; NetWork.NetDNS Address, SpareAddress;
//	                          NetWork.NetDHCP [0].Enable
//	OPTimeSetting, OPMachine  (1450) set the clock, reboot
const (
	MsgConfigSet     = 1040
	MsgConfigSetRsp  = 1041
	MsgTitleSet      = 1046
	MsgTitleSetRsp   = 1047
	MsgAbility       = 1360
	MsgAbilityRsp    = 1361
	MsgSysManager    = 1450
	MsgSysManagerRsp = 1451
)

// retText explains DVRIP answers to configuration requests. 150, 602 and
// 603 mean "taken over, restart needed".
var retText = map[int]string{
	604: "Fehler beim Schreiben im Gerät",
	605: "Das Gerät unterstützt diese Einstellung nicht",
	606: "Das Gerät hat die Werte nicht akzeptiert (meist ein Wert außerhalb des erlaubten Bereichs)",
	607: "Diese Einstellung gibt es im Gerät nicht",
	608: "Das Gerät konnte die Einstellung nicht lesen",
	103: "Das Gerät erlaubt diese Anfrage nicht",
	107: "Dem Benutzer fehlen im Gerät die Rechte für Einstellungen",
}

// resolutionSize is the size of a resolution name for PAL (NTSC: 480/240/120
// lines); it ranks resolutions.
var resolutionSize = map[string][2]int64{
	"D1": {704, 576}, "HD1": {352, 576}, "BCIF": {704, 288}, "CIF": {352, 288}, "QCIF": {176, 144},
	"VGA": {640, 480}, "QVGA": {320, 240}, "SVCD": {480, 480}, "QQVGA": {160, 128}, "ND1": {240, 192},
	"960H": {928, 576}, "720P": {1280, 720}, "960": {1280, 960}, "UXGA": {1600, 1200}, "1080P": {1920, 1080},
	"WUXGA": {1920, 1200}, "2_5M": {1872, 1408}, "3M": {2048, 1536}, "5M": {3744, 1408}, "1080N": {960, 1080}, "4M": {2592, 1520},
}

// resolutionNames is the DVRIP resolution enumeration (bit n of a
// resolution mask).
var resolutionNames = []provider.Choice{
	{Value: "D1", Label: "D1 (704×576)"}, {Value: "HD1", Label: "HD1 (352×576)"}, {Value: "BCIF", Label: "BCIF (704×288)"},
	{Value: "CIF", Label: "CIF (352×288)"}, {Value: "QCIF", Label: "QCIF (176×144)"}, {Value: "VGA", Label: "VGA (640×480)"},
	{Value: "QVGA", Label: "QVGA (320×240)"}, {Value: "SVCD", Label: "SVCD (480×480)"}, {Value: "QQVGA", Label: "QQVGA (160×128)"},
	{Value: "ND1", Label: "ND1 (240×192)"}, {Value: "960H", Label: "960H (928×576)"}, {Value: "720P", Label: "720p (1280×720)"},
	{Value: "960", Label: "960p (1280×960)"}, {Value: "UXGA", Label: "UXGA (1600×1200)"}, {Value: "1080P", Label: "1080p (1920×1080)"},
	{Value: "WUXGA", Label: "WUXGA (1920×1200)"}, {Value: "2_5M", Label: "2,5 MP (1872×1408)"}, {Value: "3M", Label: "3 MP (2048×1536)"},
	{Value: "5M", Label: "5 MP"}, {Value: "1080N", Label: "1080N (960×1080)"}, {Value: "4M", Label: "4 MP (2592×1520)"},
}

// configGet returns the value of a configuration ("" Name = the request name).
func configGet(ctx context.Context, ctl *Control, name string) (any, error) {
	r, err := ctl.Request(ctx, MsgConfigGet, MsgConfigGetRsp, map[string]any{"Name": name, "SessionID": SessionString(ctl.Session())})
	if err != nil {
		return nil, err
	}
	if !r.OK() {
		return nil, provider.NewError(provider.KindUnsupported, nil, "Konfiguration %s nicht verfügbar (Ret %d)", name, r.Ret())
	}
	return r[name], nil
}

// request sends a command with the session and checks the answer; Ret 603
// means "taken over, restart required".
func request(ctx context.Context, ctl *Control, msg, rsp uint16, name string, value any) (restart bool, err error) {
	r, err := ctl.Request(ctx, msg, rsp, map[string]any{"Name": name, name: value, "SessionID": SessionString(ctl.Session())})
	if err != nil {
		return false, err
	}
	switch ret := r.Ret(); {
	case r.OK():
		return false, nil
	case ret == 150 || ret == 602 || ret == 603:
		return true, nil
	case retText[ret] != "":
		return false, provider.NewError(provider.KindProtocol, nil, "%s: %s (Ret %d)", name, retText[ret], ret)
	default:
		return false, provider.NewError(provider.KindProtocol, nil, "Das Gerät lehnt %s ab (Ret %d)", name, ret)
	}
}

func cfgMap(v any) map[string]any { x, _ := v.(map[string]any); return x }
func cfgArr(v any) []any          { x, _ := v.([]any); return x }

func num(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x + 0.5)
	case int:
		return x
	case int64:
		return int(x)
	case bool:
		if x {
			return 1
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimPrefix(strings.ToLower(x), "0x"), 16, 64); err == nil && strings.HasPrefix(strings.ToLower(x), "0x") {
			return int(n)
		}
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func truthy(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return num(v) != 0
}

func hex32(v int) string { return fmt.Sprintf("0x%08X", uint32(v)) }

func sofiaStream(v any) *provider.StreamSettings {
	f := cfgMap(v)
	vid := cfgMap(f["Video"])
	if vid == nil {
		return nil
	}
	codec := "H.264"
	if strings.Contains(strings.ToUpper(str(vid["Compression"])), "265") {
		codec = "H.265"
	}
	return &provider.StreamSettings{
		Enabled: truthy(f["VideoEnable"]), Codec: codec, Res: str(vid["Resolution"]), FPS: num(vid["FPS"]),
		Control: strings.ToUpper(str(vid["BitRateControl"])), Bitrate: num(vid["BitRate"]), Quality: num(vid["Quality"]), GOP: num(vid["GOP"]),
	}
}

func resolutionChoices(mask int, cur string) ([]provider.Choice, bool) {
	var out []provider.Choice
	for i, c := range resolutionNames {
		if mask&(1<<i) != 0 {
			out = append(out, c)
		}
	}
	fromDevice := len(out) > 0
	if !fromDevice {
		out = append(out, resolutionNames[0], resolutionNames[3], resolutionNames[4])
	}
	for _, c := range out {
		if c.Value == cur {
			return out, fromDevice
		}
	}
	return append([]provider.Choice{{Value: cur, Label: cur + " (aktuell)"}}, out...), fromDevice
}

func codecChoices(mask int) []string {
	out := []string{"H.264"}
	if mask&(1<<8) != 0 {
		out = append(out, "H.265")
	}
	return out
}

// encodeCaps returns the main/extra resolution and codec masks of a channel.
func encodeCaps(caps map[string]any, ch int) (mainRes, extraRes, mainCodec, extraCodec int) {
	info := cfgArr(caps["EncodeInfo"])
	mainRes, mainCodec = num(cfgMap(cfgAt(info, 0))["ResolutionMask"]), num(cfgMap(cfgAt(info, 0))["CompressionMask"])
	extraRes, extraCodec = num(cfgMap(cfgAt(info, 1))["ResolutionMask"]), num(cfgMap(cfgAt(info, 1))["CompressionMask"])
	if v := cfgAt(cfgArr(caps["ImageSizePerChannel"]), ch); v != nil && num(v) != 0 {
		mainRes = num(v)
	}
	if v := cfgAt(cfgArr(caps["ExImageSizePerChannel"]), ch); v != nil && num(v) != 0 {
		extraRes = num(v)
	}
	return
}

func cfgAt(x []any, i int) any {
	if i >= 0 && i < len(x) {
		return x[i]
	}
	return nil
}

// sofiaRecord turns a Record entry into the simple form.
func sofiaRecord(v any) *provider.RecordSettings {
	r := cfgMap(v)
	if r == nil {
		return nil
	}
	switch str(r["RecordMode"]) {
	case "ManualRecord":
		return &provider.RecordSettings{Mode: provider.RecordContinuous}
	case "ClosedRecord":
		return &provider.RecordSettings{Mode: provider.RecordOff}
	}
	sections, masks := cfgArr(r["TimeSection"]), cfgArr(r["Mask"])
	if len(sections) < 7 {
		return &provider.RecordSettings{Mode: provider.RecordCustom}
	}
	day := func(d int) []string {
		var out []string
		ms := cfgArr(cfgAt(masks, d))
		for i, s := range cfgArr(sections[d]) {
			on, span, _ := strings.Cut(strings.TrimSpace(str(s)), " ")
			mask := 0
			if on == "1" {
				mask = num(cfgAt(ms, i))
			}
			out = append(out, fmt.Sprintf("%d %s", mask, span))
		}
		return out
	}
	return scheduleMode(day)
}

// scheduleMode recognises "the same window every day with one mask" in
// sections "mask HH:MM:SS-HH:MM:SS".
func scheduleMode(day func(int) []string) *provider.RecordSettings {
	var first []string
	mask := -1
	for d := 0; d < 7; d++ {
		var spans []string
		for _, s := range day(d) {
			ms, span, _ := strings.Cut(s, " ")
			n, _ := strconv.Atoi(ms)
			if n == 0 {
				continue
			}
			if mask >= 0 && n != mask {
				return &provider.RecordSettings{Mode: provider.RecordCustom}
			}
			mask = n
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
	switch {
	case !ok:
		return &provider.RecordSettings{Mode: provider.RecordCustom}
	case mask&1 != 0:
		return &provider.RecordSettings{Mode: provider.RecordContinuous, From: from, To: to}
	case mask&2 != 0:
		return &provider.RecordSettings{Mode: provider.RecordMotion, From: from, To: to}
	}
	return &provider.RecordSettings{Mode: provider.RecordCustom}
}

// ReadSettings reads the device configuration.
func (d *driver) ReadSettings(ctx context.Context) (*provider.DeviceSettings, error) {
	ctl, err := d.control()
	if err != nil {
		return nil, err
	}
	enc, err := configGet(ctx, ctl, "Simplify.Encode")
	if err != nil {
		return nil, err
	}
	var caps map[string]any
	if r, err := ctl.Request(ctx, MsgAbility, MsgAbilityRsp, map[string]any{"Name": "EncodeCapability", "SessionID": SessionString(ctl.Session())}); err == nil && r.OK() {
		caps = cfgMap(r["EncodeCapability"])
	}
	fpsMax := 25
	if loc, err := configGet(ctx, ctl, "General.Location"); err == nil && strings.EqualFold(str(cfgMap(loc)["VideoFormat"]), "NTSC") {
		fpsMax = 30
	}
	titles := ctl.ChannelTitles(ctx)
	recs, _ := configGet(ctx, ctl, "Record")
	mds, _ := configGet(ctx, ctl, "Detect.MotionDetect")
	losses, _ := configGet(ctx, ctl, "Detect.LossDetect")
	blinds, _ := configGet(ctx, ctl, "Detect.BlindDetect")
	encs := cfgArr(enc)
	s := &provider.DeviceSettings{CanReboot: true, Log: true, CanFormat: true, Users: true}
	allFromDevice := caps != nil
	for ch := range encs {
		e := cfgMap(encs[ch])
		c := provider.ChannelSettings{Channel: ch}
		if titles != nil {
			name := titles[ch]
			c.Name = &name
		}
		mainRes, extraRes, mainCodec, extraCodec := encodeCaps(caps, ch)
		if st := sofiaStream(e["MainFormat"]); st != nil {
			res, ok := resolutionChoices(mainRes, st.Res)
			allFromDevice = allFromDevice && ok
			st.Options = &provider.StreamOptions{Codecs: codecChoices(mainCodec), Resolutions: res, FPSMax: max(fpsMax, st.FPS),
				BitrateMin: 32, BitrateMax: 8192, GOPUnit: "seconds", GOPMax: 12, FromDevice: ok}
			c.Main = st
		}
		if st := sofiaStream(e["ExtraFormat"]); st != nil {
			res, ok := resolutionChoices(extraRes, st.Res)
			st.Options = &provider.StreamOptions{Codecs: codecChoices(extraCodec), Resolutions: res, FPSMax: max(fpsMax, st.FPS),
				BitrateMin: 16, BitrateMax: 2048, GOPUnit: "seconds", GOPMax: 12, FromDevice: ok}
			c.Sub = st
		}
		c.Record = sofiaRecord(cfgAt(cfgArr(recs), ch))
		if md := cfgMap(cfgAt(cfgArr(mds), ch)); md != nil {
			c.Motion = &provider.MotionSettings{Enabled: truthy(md["Enable"]), Sensitivity: num(md["Level"]), SensitivityMax: 6}
			if ev := sofiaEvent(md, ch); ev != nil {
				c.Motion.Beep = ev.Beep
			}
		}
		c.VideoLoss = sofiaEvent(cfgAt(cfgArr(losses), ch), ch)
		c.Tamper = sofiaEvent(cfgAt(cfgArr(blinds), ch), ch)
		s.Channels = append(s.Channels, c)
	}
	if !allFromDevice {
		s.Notes = append(s.Notes, "Hinweis: Dieses Gerät meldet nicht alle erlaubten Werte. CNM bietet übliche Werte an und zeigt nach dem Speichern, falls das Gerät einen Wert anders übernimmt.")
	}
	if wall, err := ctl.DeviceTime(ctx); err == nil {
		s.Time = &provider.TimeSettings{DeviceTime: wall.Format(dvripTime), OffsetSeconds: int(wall.Sub(provider.WallNow()).Seconds())}
		if ntp, err := configGet(ctx, ctl, "NetWork.NetNTP"); err == nil && cfgMap(ntp) != nil {
			n := cfgMap(ntp)
			srv := cfgMap(n["Server"])
			s.Time.NTP = &provider.NTPSettings{Enabled: truthy(n["Enable"]), Server: str(srv["Name"]), Port: num(srv["Port"]), IntervalMin: num(n["UpdatePeriod"])}
		}
	}
	s.Disks = d.readDisks(ctx, ctl)
	s.DiskAlarms = readSofiaDiskAlarms(ctx, ctl)
	sys := &provider.SystemSettings{}
	if g, err := configGet(ctx, ctl, "General.General"); err == nil {
		if ow, ok := cfgMap(g)["OverWrite"]; ok {
			b := strings.EqualFold(str(ow), "OverWrite")
			s.Overwrite = &b
		}
		if n, ok := cfgMap(g)["MachineName"]; ok {
			name := str(n)
			sys.Name = &name
		}
	}
	if loc, err := configGet(ctx, ctl, "General.Location"); err == nil {
		sys.VideoStandard = strings.ToUpper(str(cfgMap(loc)["VideoFormat"]))
	}
	if am, err := configGet(ctx, ctl, "General.AutoMaintain"); err == nil && cfgMap(am) != nil {
		if day := sofiaRebootDay(str(cfgMap(am)["AutoRebootDay"])); day != "" {
			sys.AutoReboot = &provider.AutoReboot{Day: day, Hour: num(cfgMap(am)["AutoRebootHour"])}
		}
	}
	if sys.Name != nil || sys.VideoStandard != "" || sys.AutoReboot != nil {
		s.System = sys
	}
	s.Network = readSofiaNetwork(ctx, ctl)
	return s, nil
}

var sofiaDays = map[string]string{"never": "Never", "daily": "EveryDay", "sun": "Sunday", "mon": "Monday", "tue": "Tuesday",
	"wed": "Wednesday", "thu": "Thursday", "fri": "Friday", "sat": "Saturday"}

func sofiaRebootDay(v string) string {
	for k, x := range sofiaDays {
		if strings.EqualFold(x, v) {
			return k
		}
	}
	return ""
}

// sofiaIP decodes an address like "0x0B0200C0" = 192.0.2.11 (first octet in the low byte).
func sofiaIP(v any) string {
	n := num(v)
	if n == 0 {
		return ""
	}
	u := uint32(n)
	return fmt.Sprintf("%d.%d.%d.%d", u&0xff, u>>8&0xff, u>>16&0xff, u>>24)
}

func readSofiaNetwork(ctx context.Context, ctl *Control) *provider.NetworkInfo {
	v, err := configGet(ctx, ctl, "NetWork.NetCommon")
	nc := cfgMap(v)
	if err != nil || nc == nil {
		return nil
	}
	n := &provider.NetworkInfo{Hostname: str(nc["HostName"]), IP: sofiaIP(nc["HostIP"]), Mask: sofiaIP(nc["Submask"]),
		Gateway: sofiaIP(nc["GateWay"]), MAC: str(nc["MAC"]), Ports: map[string]int{}}
	if p := num(nc["HttpPort"]); p > 0 {
		n.Ports["HTTP"] = p
	}
	if p := num(nc["TCPPort"]); p > 0 {
		n.Ports["TCP"] = p
	}
	if dns, err := configGet(ctx, ctl, "NetWork.NetDNS"); err == nil {
		for _, k := range []string{"Address", "SpareAddress"} {
			if ip := sofiaIP(cfgMap(dns)[k]); ip != "" {
				n.DNS = append(n.DNS, ip)
			}
		}
	}
	if dhcp, err := configGet(ctx, ctl, "NetWork.NetDHCP"); err == nil {
		if m := firstMap(dhcp); m != nil {
			b := truthy(m["Enable"])
			n.DHCP = &b
		}
	}
	return n
}

func (d *driver) readDisks(ctx context.Context, ctl *Control) []provider.DiskInfo {
	r, err := ctl.Request(ctx, MsgSysInfo, MsgSysInfoRsp, map[string]any{"Name": "StorageInfo", "SessionID": SessionString(ctl.Session())})
	if err != nil || !r.OK() {
		return nil
	}
	out := []provider.DiskInfo{}
	for _, dv := range cfgArr(r["StorageInfo"]) {
		disk := cfgMap(dv)
		for i, pv := range cfgArr(disk["Partition"]) {
			p := cfgMap(pv)
			total := int64(num(p["TotalSpace"])) << 20 // MB
			if total == 0 {
				continue
			}
			info := provider.DiskInfo{Name: fmt.Sprintf("Festplatte %d, Partition %d", num(disk["PlysicalNo"])+1, i+1),
				TotalBytes: total, FreeBytes: int64(num(p["RemainSpace"])) << 20, Disk: num(disk["PlysicalNo"]), Part: i}
			// an empty disk reports placeholders such as "2000-00-00 00:00:00"
			if old := str(p["OldStartTime"]); validDiskTime(old) {
				info.Oldest = old
			}
			if nw := str(p["NewEndTime"]); validDiskTime(nw) {
				info.Newest = nw
			}
			// DirverType (sic): 0 read/write, 1 read only, 2 redundant, 3 snapshots
			switch num(p["DirverType"]) {
			case 1:
				info.State = "nur lesen – das Gerät nimmt darauf nicht auf"
			case 2:
				info.State = "Redundanz"
			case 3:
				info.State = "Schnappschüsse"
			}
			if st := num(p["Status"]); st != 0 {
				info.State = strings.TrimPrefix(info.State+" · ", " · ") + fmt.Sprintf("Fehlerstatus %d", st)
				info.Error = true
			}
			out = append(out, info)
		}
	}
	return out
}

// sofiaEvent reads a detection entry: Enable and its EventHandler's buzzer
// and recording of channel ch (ch < 0: device event without recording).
func sofiaEvent(v any, ch int) *provider.EventSettings {
	m := cfgMap(v)
	if m == nil {
		return nil
	}
	ev := &provider.EventSettings{Enabled: truthy(m["Enable"])}
	if eh := cfgMap(m["EventHandler"]); eh != nil {
		if b, ok := eh["BeepEnable"]; ok {
			x := truthy(b)
			ev.Beep = &x
		}
		if r, ok := eh["RecordEnable"]; ok && ch >= 0 {
			x := truthy(r) && num(eh["RecordMask"])&(1<<ch) != 0
			ev.Record = &x
		}
	}
	return ev
}

// applySofiaEvent writes an event's switches into its entry; an enabled
// event gets an active schedule (without one it never fires).
func applySofiaEvent(m map[string]any, want *provider.EventSettings, ch int) {
	m["Enable"] = want.Enabled
	eh := cfgMap(m["EventHandler"])
	if eh == nil {
		return
	}
	if want.Beep != nil {
		if _, ok := eh["BeepEnable"]; ok {
			eh["BeepEnable"] = *want.Beep
		}
	}
	if want.Record != nil && ch >= 0 {
		if _, ok := eh["RecordEnable"]; ok {
			mask := num(eh["RecordMask"])
			if *want.Record {
				mask |= 1 << ch
			} else {
				mask &^= 1 << ch
			}
			eh["RecordMask"] = hex32(mask)
			eh["RecordEnable"] = mask != 0
		}
	}
	if days := cfgArr(eh["TimeSection"]); want.Enabled && len(days) >= 7 && !activeSections(days) {
		for i := 0; i < 7; i++ {
			days[i] = sofiaSections("", "")
		}
	}
}

// firstMap is an object or the first object of a list (firmware differs).
func firstMap(v any) map[string]any {
	if m := cfgMap(v); m != nil {
		return m
	}
	return cfgMap(cfgAt(cfgArr(v), 0))
}

// readSofiaDiskAlarms reads the reactions to disk problems (nil when the
// device offers none of them).
func readSofiaDiskAlarms(ctx context.Context, ctl *Control) *provider.DiskAlarms {
	a := &provider.DiskAlarms{}
	found := false
	for _, ev := range []struct {
		name string
		dst  **provider.EventSettings
	}{{"Storage.StorageNotExist", &a.NoDisk}, {"Storage.StorageFailure", &a.Failure}, {"Storage.StorageLowSpace", &a.LowSpace}} {
		v, err := configGet(ctx, ctl, ev.name)
		if err != nil {
			continue
		}
		m := firstMap(v)
		if m == nil {
			continue
		}
		*ev.dst = sofiaEvent(m, -1)
		found = true
		if ev.name == "Storage.StorageLowSpace" {
			a.LowSpacePercent = num(m["LowerLimit"])
		}
	}
	if !found {
		return nil
	}
	return a
}

// validDiskTime reports a real recording time (not a placeholder).
func validDiskTime(s string) bool {
	t, err := time.Parse(dvripTime, s)
	return err == nil && t.Year() > 2000
}

// applyStream changes the fields of a format entry that differ.
func applyStream(f map[string]any, want *provider.StreamSettings) {
	vid := cfgMap(f["Video"])
	cur := sofiaStream(f)
	if vid == nil || cur == nil {
		return
	}
	if cur.Enabled != want.Enabled {
		f["VideoEnable"] = want.Enabled
	}
	if cur.Codec != want.Codec {
		vid["Compression"] = want.Codec
	}
	if cur.Res != want.Res {
		vid["Resolution"] = want.Res
	}
	if cur.FPS != want.FPS {
		vid["FPS"] = want.FPS
	}
	if cur.Control != want.Control {
		vid["BitRateControl"] = want.Control
	}
	if cur.Bitrate != want.Bitrate {
		vid["BitRate"] = want.Bitrate
	}
	if cur.Quality != want.Quality {
		vid["Quality"] = want.Quality
	}
	if cur.GOP != want.GOP {
		vid["GOP"] = want.GOP
	}
}

func sofiaSections(from, to string) []any {
	out := []any{}
	for _, s := range provider.DaySlots(from, to) {
		out = append(out, "1 "+s)
	}
	for len(out) < 6 {
		out = append(out, "0 00:00:00-24:00:00")
	}
	return out
}

func sofiaMasks(n, mask int) []any {
	out := []any{}
	for i := 0; i < 6; i++ {
		if i < n {
			out = append(out, hex32(mask))
		} else {
			out = append(out, hex32(0))
		}
	}
	return out
}

func activeSections(days []any) bool {
	for d := 0; d < len(days) && d < 7; d++ {
		for _, s := range cfgArr(days[d]) {
			if strings.HasPrefix(strings.TrimSpace(str(s)), "1 ") {
				return true
			}
		}
	}
	return false
}

// ApplySettings writes a change and reports what the device took over.
func (d *driver) ApplySettings(ctx context.Context, change provider.SettingsChange) (*provider.SettingsResult, error) {
	if err := change.Validate(); err != nil {
		return nil, err
	}
	ctl, err := d.control()
	if err != nil {
		return nil, err
	}
	res := &provider.SettingsResult{}
	set := func(msg, rsp uint16, name string, value any) error {
		restart, err := request(ctx, ctl, msg, rsp, name, value)
		res.Restart = res.Restart || restart
		return err
	}
	// encoding first; the device decides what it accepts. A refusal (Ret 606)
	// is explained from its capability tables.
	var enc []any
	var adjusted []string // sub streams CNM had to adapt
	encDirty := false
	for _, c := range change.Channels {
		if c.Main == nil && c.Sub == nil {
			continue
		}
		if enc == nil {
			v, err := configGet(ctx, ctl, "Simplify.Encode")
			if err != nil {
				return nil, err
			}
			enc = cfgArr(v)
		}
		e := cfgMap(cfgAt(enc, c.Channel))
		if e == nil {
			return nil, provider.NewError(provider.KindConfig, nil, "Kanal %d gibt es nicht", c.Channel+1)
		}
		if c.Main != nil {
			applyStream(cfgMap(e["MainFormat"]), c.Main)
		}
		if c.Sub != nil {
			applyStream(cfgMap(e["ExtraFormat"]), c.Sub)
		}
		encDirty = true
		if note := d.fitSubStream(ctx, ctl, e, c.Channel); note != "" {
			adjusted = append(adjusted, note)
		}
	}
	var titles []any
	for _, c := range change.Channels {
		if c.Name != nil {
			if titles == nil {
				cur := ctl.ChannelTitles(ctx)
				n := max(len(cur), ctl.Channels())
				for i := 0; i < n; i++ {
					titles = append(titles, cur[i])
				}
			}
			if c.Channel >= len(titles) {
				return nil, provider.NewError(provider.KindConfig, nil, "Kanal %d gibt es nicht", c.Channel+1)
			}
			titles[c.Channel] = *c.Name
		}
		motionNeeded := c.Record != nil && c.Record.Mode == provider.RecordMotion
		if c.Motion != nil || motionNeeded {
			name := fmt.Sprintf("Detect.MotionDetect.[%d]", c.Channel)
			v, err := configGet(ctx, ctl, name)
			if err != nil {
				return nil, err
			}
			md := cfgMap(v)
			if md == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "Kanal %d: Bewegungserkennung nicht verfügbar", c.Channel+1)
			}
			if c.Motion != nil {
				md["Enable"] = c.Motion.Enabled || motionNeeded
				if c.Motion.Sensitivity >= 1 {
					md["Level"] = min(c.Motion.Sensitivity, 6)
				}
				if eh := cfgMap(md["EventHandler"]); eh != nil && c.Motion.Beep != nil {
					if _, ok := eh["BeepEnable"]; ok {
						eh["BeepEnable"] = *c.Motion.Beep
					}
				}
			}
			if motionNeeded {
				md["Enable"] = true
				if eh := cfgMap(md["EventHandler"]); eh != nil {
					eh["RecordEnable"] = true
					eh["RecordMask"] = hex32(num(eh["RecordMask"]) | 1<<c.Channel)
					if days := cfgArr(eh["TimeSection"]); len(days) >= 7 && !activeSections(days) {
						for i := 0; i < 7; i++ {
							days[i] = sofiaSections("", "")
						}
					}
				}
			}
			if err := set(MsgConfigSet, MsgConfigSetRsp, name, md); err != nil {
				return nil, err
			}
		}
		for _, ev := range []struct {
			table string
			want  *provider.EventSettings
		}{{"Detect.LossDetect", c.VideoLoss}, {"Detect.BlindDetect", c.Tamper}} {
			if ev.want == nil {
				continue
			}
			name := fmt.Sprintf("%s.[%d]", ev.table, c.Channel)
			v, err := configGet(ctx, ctl, name)
			if err != nil {
				return nil, err
			}
			m := cfgMap(v)
			if m == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "Kanal %d: %s nicht verfügbar", c.Channel+1, ev.table)
			}
			applySofiaEvent(m, ev.want, c.Channel)
			if err := set(MsgConfigSet, MsgConfigSetRsp, name, m); err != nil {
				return nil, err
			}
		}
		if r := c.Record; r != nil {
			name := fmt.Sprintf("Record.[%d]", c.Channel)
			v, err := configGet(ctx, ctl, name)
			if err != nil {
				return nil, err
			}
			rec := cfgMap(v)
			if rec == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "Kanal %d: Aufnahme-Einstellungen nicht verfügbar", c.Channel+1)
			}
			if r.Mode == provider.RecordOff {
				rec["RecordMode"] = "ClosedRecord"
			} else {
				mask := 7
				if r.Mode == provider.RecordMotion {
					mask = 6 // motion and alarm
				}
				slots := len(provider.DaySlots(r.From, r.To))
				var sections, masks []any
				for i := 0; i < 7; i++ {
					sections = append(sections, sofiaSections(r.From, r.To))
					masks = append(masks, sofiaMasks(slots, mask))
				}
				rec["RecordMode"], rec["TimeSection"], rec["Mask"] = "ConfigRecord", sections, masks
			}
			if err := set(MsgConfigSet, MsgConfigSetRsp, name, rec); err != nil {
				return nil, err
			}
		}
	}
	if encDirty {
		if err := set(MsgConfigSet, MsgConfigSetRsp, "Simplify.Encode", enc); err != nil {
			if why := d.explainEncode(ctx, ctl, enc, change); why != "" {
				err = provider.NewError(provider.KindProtocol, err, "%s – %s", err.Error(), why)
			}
			return nil, err
		}
	}
	if titles != nil {
		if err := set(MsgTitleSet, MsgTitleSetRsp, "ChannelTitle", titles); err != nil {
			return nil, err
		}
	}
	if n := change.NTP; n != nil {
		v, err := configGet(ctx, ctl, "NetWork.NetNTP")
		if err != nil {
			return nil, err
		}
		ntp := cfgMap(v)
		if ntp == nil {
			return nil, provider.NewError(provider.KindUnsupported, nil, "NTP nicht verfügbar")
		}
		ntp["Enable"] = n.Enabled
		if srv := cfgMap(ntp["Server"]); srv != nil {
			if n.Server != "" {
				srv["Name"] = n.Server
			}
			if n.Port > 0 {
				srv["Port"] = n.Port
			}
		}
		if n.IntervalMin > 0 {
			ntp["UpdatePeriod"] = n.IntervalMin
		}
		if err := set(MsgConfigSet, MsgConfigSetRsp, "NetWork.NetNTP", ntp); err != nil {
			return nil, err
		}
	}
	if a := change.DiskAlarms; a != nil {
		for _, ev := range []struct {
			name string
			want *provider.EventSettings
		}{{"Storage.StorageNotExist", a.NoDisk}, {"Storage.StorageFailure", a.Failure}, {"Storage.StorageLowSpace", a.LowSpace}} {
			if ev.want == nil {
				continue
			}
			v, err := configGet(ctx, ctl, ev.name)
			if err != nil {
				return nil, err
			}
			m := firstMap(v)
			if m == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "%s nicht verfügbar", ev.name)
			}
			applySofiaEvent(m, ev.want, -1)
			if ev.name == "Storage.StorageLowSpace" && a.LowSpacePercent > 0 {
				m["LowerLimit"] = a.LowSpacePercent
			}
			if err := set(MsgConfigSet, MsgConfigSetRsp, ev.name, v); err != nil {
				return nil, err
			}
		}
	}
	if sy := change.System; sy != nil {
		if sy.Name != nil {
			v, err := configGet(ctx, ctl, "General.General")
			if err != nil {
				return nil, err
			}
			g := cfgMap(v)
			if g == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "Gerätename nicht verfügbar")
			}
			g["MachineName"] = *sy.Name
			if err := set(MsgConfigSet, MsgConfigSetRsp, "General.General", g); err != nil {
				return nil, err
			}
		}
		if r := sy.AutoReboot; r != nil {
			v, err := configGet(ctx, ctl, "General.AutoMaintain")
			if err != nil {
				return nil, err
			}
			am := cfgMap(v)
			if am == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "Automatischer Neustart nicht verfügbar")
			}
			am["AutoRebootDay"], am["AutoRebootHour"] = sofiaDays[r.Day], r.Hour
			if err := set(MsgConfigSet, MsgConfigSetRsp, "General.AutoMaintain", am); err != nil {
				return nil, err
			}
		}
		// the video standard last: the recorder restarts for it
		if sy.VideoStandard != "" {
			v, err := configGet(ctx, ctl, "General.Location")
			if err != nil {
				return nil, err
			}
			loc := cfgMap(v)
			if loc == nil {
				return nil, provider.NewError(provider.KindUnsupported, nil, "Videonorm nicht verfügbar")
			}
			if !strings.EqualFold(str(loc["VideoFormat"]), sy.VideoStandard) {
				loc["VideoFormat"] = sy.VideoStandard
				if err := set(MsgConfigSet, MsgConfigSetRsp, "General.Location", loc); err != nil {
					return nil, err
				}
			}
		}
	}
	if change.Overwrite != nil {
		v, err := configGet(ctx, ctl, "General.General")
		if err != nil {
			return nil, err
		}
		g := cfgMap(v)
		if g == nil {
			return nil, provider.NewError(provider.KindUnsupported, nil, "Überschreiben nicht verfügbar")
		}
		g["OverWrite"] = "StopRecord"
		if *change.Overwrite {
			g["OverWrite"] = "OverWrite"
		}
		if err := set(MsgConfigSet, MsgConfigSetRsp, "General.General", g); err != nil {
			return nil, err
		}
	}
	after, err := d.ReadSettings(ctx)
	if err != nil {
		return nil, err
	}
	res.Settings = after
	res.Warnings = provider.CompareSettings(change, after)
	if len(adjusted) > 0 {
		// the adapted sub stream is intended: drop its comparison warnings
		var keep []string
		for _, w := range res.Warnings {
			if !strings.Contains(w, "Substream Auflösung") {
				keep = append(keep, w)
			}
		}
		res.Warnings = append(adjusted, keep...)
	}
	return res, nil
}

// fitSubStream adapts the sub stream resolution of a channel to what the
// device allows with its main resolution (ExImageSizePerChannelEx); old
// DVRs refuse the whole encoding otherwise, even for a disabled sub stream.
func (d *driver) fitSubStream(ctx context.Context, ctl *Control, e map[string]any, ch int) string {
	m, s := sofiaStream(e["MainFormat"]), sofiaStream(e["ExtraFormat"])
	if m == nil || s == nil {
		return ""
	}
	if d.caps == nil {
		r, err := ctl.Request(ctx, MsgAbility, MsgAbilityRsp, map[string]any{"Name": "EncodeCapability", "SessionID": SessionString(ctl.Session())})
		if err != nil || !r.OK() {
			return ""
		}
		d.caps = cfgMap(r["EncodeCapability"])
	}
	ex := cfgArr(cfgAt(cfgArr(d.caps["ExImageSizePerChannelEx"]), ch))
	mi := resolutionIndex(m.Res)
	if mi < 0 || mi >= len(ex) {
		return ""
	}
	allowed := num(ex[mi])
	si := resolutionIndex(s.Res)
	if allowed == 0 || (si >= 0 && allowed&(1<<si) != 0) {
		return ""
	}
	// the largest allowed resolution
	best, bestPx := "", int64(-1)
	for i, c := range resolutionNames {
		if allowed&(1<<i) == 0 {
			continue
		}
		if px := resolutionSize[c.Value][0] * resolutionSize[c.Value][1]; px > bestPx {
			best, bestPx = c.Value, px
		}
	}
	if best == "" {
		return ""
	}
	cfgMap(cfgMap(e["ExtraFormat"])["Video"])["Resolution"] = best
	return fmt.Sprintf("Kanal %d: Substream-Auflösung von %s auf %s angepasst – bei Hauptstream %s erlaubt das Gerät nur %s", ch+1, s.Res, best, m.Res, maskNames(allowed))
}

// SetDeviceTime sets the device clock.
func (d *driver) SetDeviceTime(ctx context.Context, wall time.Time) error {
	ctl, err := d.control()
	if err != nil {
		return err
	}
	if _, err := request(ctx, ctl, MsgSysManager, MsgSysManagerRsp, "OPTimeSetting", wall.Format(dvripTime)); err != nil {
		return err
	}
	d.syncTime(ctx, ctl)
	return nil
}

// Reboot restarts the device; a dropped connection counts as success.
func (d *driver) Reboot(ctx context.Context) error {
	ctl, err := d.control()
	if err != nil {
		return err
	}
	_, err = request(ctx, ctl, MsgSysManager, MsgSysManagerRsp, "OPMachine", map[string]any{"Action": "Reboot"})
	if err != nil && provider.KindOf(err) == provider.KindNetwork {
		return nil
	}
	return err
}

// resolutionIndex is the bit of a resolution name in the capability masks.
func resolutionIndex(name string) int {
	for i, c := range resolutionNames {
		if c.Value == name {
			return i
		}
	}
	return -1
}

func maskNames(mask int) string {
	var out []string
	for i, c := range resolutionNames {
		if mask&(1<<i) != 0 {
			out = append(out, c.Value)
		}
	}
	if len(out) == 0 {
		return "keine"
	}
	return strings.Join(out, ", ")
}

// explainEncode describes the changed channels of a refused encoding
// against the device's capability tables: allowed main resolutions and the
// sub stream resolutions allowed with the chosen main resolution. (The
// encoder capacity the device reports is not used: real recorders run well
// above it.)
func (d *driver) explainEncode(ctx context.Context, ctl *Control, enc []any, change provider.SettingsChange) string {
	r, err := ctl.Request(ctx, MsgAbility, MsgAbilityRsp, map[string]any{"Name": "EncodeCapability", "SessionID": SessionString(ctl.Session())})
	if err != nil || !r.OK() {
		return ""
	}
	caps := cfgMap(r["EncodeCapability"])
	var parts []string
	for _, c := range change.Channels {
		if c.Main == nil && c.Sub == nil {
			continue
		}
		e := cfgMap(cfgAt(enc, c.Channel))
		m, s := sofiaStream(e["MainFormat"]), sofiaStream(e["ExtraFormat"])
		if m == nil || s == nil {
			continue
		}
		mainMask, _, _, _ := encodeCaps(caps, c.Channel)
		part := fmt.Sprintf("Kanal %d: Haupt %s@%d (erlaubt: %s)", c.Channel+1, m.Res, m.FPS, maskNames(mainMask))
		subSt := ""
		if !s.Enabled {
			subSt = " aus"
		}
		part += fmt.Sprintf(", Sub %s@%d%s", s.Res, s.FPS, subSt)
		if ex := cfgArr(cfgAt(cfgArr(caps["ExImageSizePerChannelEx"]), c.Channel)); ex != nil {
			if i := resolutionIndex(m.Res); i >= 0 && i < len(ex) {
				part += fmt.Sprintf(" (bei Haupt %s erlaubt: %s)", m.Res, maskNames(num(ex[i])))
			}
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}
