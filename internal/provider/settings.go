package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Device settings: the part of a device's own configuration CNM shows and
// changes – encoding, channel names, time, recording, motion detection and
// disk information. Drivers that support it implement Configurator. All
// values are protocol neutral; the drivers translate them and only write
// what changed.

// DeviceSettings is a snapshot of a device's configuration.
type DeviceSettings struct {
	Channels []ChannelSettings `json:"channels"`
	Time     *TimeSettings     `json:"time,omitempty"`
	// Disks is nil when the device does not report its disks.
	Disks []DiskInfo `json:"disks"`
	// Overwrite: the oldest recordings are overwritten when the disk is full
	// (nil = not reported).
	Overwrite *bool `json:"overwrite,omitempty"`
	CanReboot bool  `json:"canReboot"`
	// Log: the device's own event log can be read (LogReader).
	Log bool `json:"log,omitempty"`
	// CanFormat: disks can be formatted (DiskFormatter).
	CanFormat bool `json:"canFormat,omitempty"`
	// DiskAlarms: reactions to disk problems; nil = not offered.
	DiskAlarms *DiskAlarms `json:"diskAlarms,omitempty"`
	// System: name, video standard, maintenance restart; nil = not offered.
	System *SystemSettings `json:"system,omitempty"`
	// Network is shown only: a wrong value would cut the recorder off.
	Network *NetworkInfo `json:"network,omitempty"`
	// Users: the recorder's users can be listed and passwords changed
	// (UserManager).
	Users bool `json:"users,omitempty"`
	// Notes explain limitations, e.g. that the allowed values are CNM's
	// defaults because the device does not report them.
	Notes []string `json:"notes,omitempty"`
}

// ChannelSettings of one channel (0 based). Nil sections are not supported
// (in a snapshot) or not changed (in a SettingsChange).
type ChannelSettings struct {
	Channel int             `json:"channel"`
	Name    *string         `json:"name,omitempty"`
	Main    *StreamSettings `json:"main,omitempty"`
	Sub     *StreamSettings `json:"sub,omitempty"`
	Record  *RecordSettings `json:"record,omitempty"`
	Motion  *MotionSettings `json:"motion,omitempty"`
	// VideoLoss: the camera signal is lost; Tamper: the camera is covered
	// (blind detection). Nil = not offered.
	VideoLoss *EventSettings `json:"videoLoss,omitempty"`
	Tamper    *EventSettings `json:"tamper,omitempty"`
}

// EventSettings is an event the recorder detects and how it reacts.
type EventSettings struct {
	Enabled bool  `json:"enabled"`
	Beep    *bool `json:"beep,omitempty"`   // the recorder's buzzer; nil = not offered
	Record  *bool `json:"record,omitempty"` // record the channel; nil = not offered
}

// SystemSettings: general settings of the recorder. In a change only the
// set fields are written.
type SystemSettings struct {
	Name          *string     `json:"name,omitempty"`          // device name; nil = not offered
	VideoStandard string      `json:"videoStandard,omitempty"` // "PAL" | "NTSC"; "" = not offered
	AutoReboot    *AutoReboot `json:"autoReboot,omitempty"`    // nil = not offered
}

// AutoReboot is the recorder's maintenance restart.
type AutoReboot struct {
	Day  string `json:"day"`  // never, daily, sun, mon, tue, wed, thu, fri, sat
	Hour int    `json:"hour"` // 0–23
}

// RebootDays are the values of AutoReboot.Day.
var RebootDays = []string{"never", "daily", "sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// NetworkInfo: the recorder's network settings (read only).
type NetworkInfo struct {
	Hostname string         `json:"hostname,omitempty"`
	IP       string         `json:"ip,omitempty"`
	Mask     string         `json:"mask,omitempty"`
	Gateway  string         `json:"gateway,omitempty"`
	DNS      []string       `json:"dns,omitempty"`
	DHCP     *bool          `json:"dhcp,omitempty"`
	MAC      string         `json:"mac,omitempty"`
	Ports    map[string]int `json:"ports,omitempty"` // "HTTP", "TCP", "RTSP"
}

// DiskAlarms: how the recorder reacts to disk problems (nil = not offered).
type DiskAlarms struct {
	NoDisk   *EventSettings `json:"noDisk,omitempty"`
	Failure  *EventSettings `json:"failure,omitempty"`
	LowSpace *EventSettings `json:"lowSpace,omitempty"`
	// LowSpacePercent: free space (percent) below which LowSpace triggers.
	LowSpacePercent int `json:"lowSpacePercent,omitempty"`
}

// StreamSettings is the encoding of one stream.
type StreamSettings struct {
	Enabled bool   `json:"enabled"`
	Codec   string `json:"codec"`      // "H.264", "H.265"
	Res     string `json:"resolution"` // the device's own value, e.g. "1920x1080" or "D1"
	FPS     int    `json:"fps"`
	Control string `json:"control"` // "CBR" or "VBR"
	Bitrate int    `json:"bitrate"` // kbit/s; with VBR the upper limit
	Quality int    `json:"quality"` // 1 (lowest) … 6 (best), used with VBR
	GOP     int    `json:"gop"`     // keyframe interval in Options.GOPUnit
	// Options are the allowed values (snapshot only).
	Options *StreamOptions `json:"options,omitempty"`
}

// StreamOptions lists what a stream accepts.
type StreamOptions struct {
	Codecs      []string `json:"codecs"`
	Resolutions []Choice `json:"resolutions"`
	FPSMax      int      `json:"fpsMax"`
	BitrateMin  int      `json:"bitrateMin"`
	BitrateMax  int      `json:"bitrateMax"`
	GOPUnit     string   `json:"gopUnit"` // "frames" or "seconds"
	GOPMax      int      `json:"gopMax"`
	// FromDevice: the device reported these options. Otherwise they are
	// CNM's defaults and the device may refuse some of them.
	FromDevice bool `json:"fromDevice"`
}

// Choice is one selectable value.
type Choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Recording modes.
const (
	RecordContinuous = "continuous"
	RecordMotion     = "motion"
	RecordOff        = "off"
	// RecordCustom is a schedule set up on the device that CNM cannot show
	// in its simple form (read only; choosing another mode replaces it).
	RecordCustom = "custom"
)

// RecordSettings: what a channel records, optionally only within a daily
// window ("HH:MM"; empty = all day; To before From = across midnight).
type RecordSettings struct {
	Mode string `json:"mode"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// MotionSettings of a channel.
type MotionSettings struct {
	Enabled        bool `json:"enabled"`
	Sensitivity    int  `json:"sensitivity"`
	SensitivityMax int  `json:"sensitivityMax"` // 6 (levels) or 100
	// Beep: the recorder's buzzer on motion; nil = not offered.
	Beep *bool `json:"beep,omitempty"`
}

// TimeSettings: the device clock and its time server.
type TimeSettings struct {
	DeviceTime    string       `json:"deviceTime"`    // device wall clock "2006-01-02 15:04:05"
	OffsetSeconds int          `json:"offsetSeconds"` // device minus server wall clock
	NTP           *NTPSettings `json:"ntp,omitempty"`
}

// NTPSettings of a device.
type NTPSettings struct {
	Enabled     bool   `json:"enabled"`
	Server      string `json:"server"`
	Port        int    `json:"port"`
	IntervalMin int    `json:"intervalMin"`
}

// DiskInfo describes one disk or partition.
type DiskInfo struct {
	Name       string `json:"name"`
	TotalBytes int64  `json:"totalBytes"`
	FreeBytes  int64  `json:"freeBytes"`
	State      string `json:"state,omitempty"`
	Oldest     string `json:"oldest,omitempty"` // oldest recording (device wall clock), if reported
	Newest     string `json:"newest,omitempty"` // end of the newest recording, if reported
	// Estimated: the device does not report its disk; TotalBytes is the sum
	// of its recording files (about the disk size once the disk is full and
	// overwritten), FreeBytes is unknown.
	Estimated bool `json:"estimated,omitempty"`
	// Error: the device reports a fault of this disk (State explains it).
	Error bool `json:"error,omitempty"`
	// Disk and Part address the partition for formatting.
	Disk int `json:"disk"`
	Part int `json:"part"`
}

// LogEntry is one entry of a device's own event log (device wall clock).
type LogEntry struct {
	Time string `json:"time"`
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	User string `json:"user,omitempty"`
}

// LogReader is implemented by drivers that read the device's event log:
// system starts, disk errors, logins, changed settings.
type LogReader interface {
	// ReadLog returns at most limit entries of [from, to], newest first.
	// kind narrows the log: all, system, storage, config, record, alarm,
	// account.
	ReadLog(ctx context.Context, from, to time.Time, kind string, limit int) ([]LogEntry, error)
}

// DeviceUser is a user account of the recorder (never its password).
type DeviceUser struct {
	Name     string `json:"name"`
	Group    string `json:"group,omitempty"`
	Reserved bool   `json:"reserved,omitempty"` // built in
	Self     bool   `json:"self,omitempty"`     // the login CNM uses
}

// UserManager is implemented by drivers that manage the recorder's users.
type UserManager interface {
	Users(ctx context.Context) ([]DeviceUser, error)
	// ChangePassword sets a user's password; oldPass "" = the login CNM
	// uses. The new password is checked with a separate login; when that
	// fails, the old one is restored and an error returned.
	ChangePassword(ctx context.Context, user, oldPass, newPass string) error
}

// PasswordRestore is the error after a password change whose check login
// failed; restoreErr tells whether the old password could be restored.
func PasswordRestore(loginErr, restoreErr error) error {
	if restoreErr != nil {
		return NewError(KindProtocol, loginErr, "Das neue Passwort wurde gesetzt, die Anmeldung damit klappt aber nicht (%s) – und das alte ließ sich nicht wiederherstellen (%s). Den Rekorder bitte nicht neu starten und das Passwort vor Ort prüfen.",
			Redact(loginErr.Error()), Redact(restoreErr.Error()))
	}
	return NewError(KindProtocol, loginErr, "Die Anmeldung mit dem neuen Passwort hat nicht geklappt (%s) – CNM hat das alte Passwort wiederhergestellt, es ist nichts geändert.", Redact(loginErr.Error()))
}

// DiskFormatter is implemented by drivers that can format a disk
// partition of the device. All recordings on it are deleted.
type DiskFormatter interface {
	FormatDisk(ctx context.Context, disk, part int) error
}

// SettingsChange lists what to change. Only listed channels and non-nil
// sections are written.
type SettingsChange struct {
	Channels   []ChannelSettings `json:"channels"`
	NTP        *NTPSettings      `json:"ntp,omitempty"`
	Overwrite  *bool             `json:"overwrite,omitempty"`
	DiskAlarms *DiskAlarms       `json:"diskAlarms,omitempty"`
	System     *SystemSettings   `json:"system,omitempty"`
}

// SettingsResult is the state after a change.
type SettingsResult struct {
	Settings *DeviceSettings `json:"settings"`
	// Warnings name values the device did not take over as requested.
	Warnings []string `json:"warnings,omitempty"`
	// Restart: the device asks for a restart before changes take effect.
	Restart bool `json:"restart,omitempty"`
	// Failures name the channels the device refused (when channels are
	// saved one after another); the other channels were saved.
	Failures []string `json:"failures,omitempty"`
}

// Configurator is implemented by drivers that can read and change device
// settings. Every method needs a connected driver.
type Configurator interface {
	ReadSettings(ctx context.Context) (*DeviceSettings, error)
	ApplySettings(ctx context.Context, change SettingsChange) (*SettingsResult, error)
	// SetDeviceTime sets the device clock to a wall clock time.
	SetDeviceTime(ctx context.Context, wall time.Time) error
	Reboot(ctx context.Context) error
}

var reHHMM = regexp.MustCompile(`^([01]\d|2[0-4]):([0-5]\d)$`)

// Validate checks a change before it is sent to a device.
func (c SettingsChange) Validate() error {
	seen := map[int]bool{}
	for _, ch := range c.Channels {
		if ch.Channel < 0 || ch.Channel > 255 || seen[ch.Channel] {
			return NewError(KindConfig, nil, "Ungültiger Kanal %d", ch.Channel+1)
		}
		seen[ch.Channel] = true
		if ch.Name != nil && len(*ch.Name) > 63 {
			return NewError(KindConfig, nil, "Kanal %d: Name zu lang (höchstens 63 Zeichen)", ch.Channel+1)
		}
		for _, s := range []*StreamSettings{ch.Main, ch.Sub} {
			if s == nil {
				continue
			}
			switch {
			case s.Codec != "H.264" && s.Codec != "H.265":
				return NewError(KindConfig, nil, "Kanal %d: unbekannter Codec %q", ch.Channel+1, s.Codec)
			case s.Control != "CBR" && s.Control != "VBR":
				return NewError(KindConfig, nil, "Kanal %d: Bitratentyp muss CBR oder VBR sein", ch.Channel+1)
			case s.FPS < 1 || s.FPS > 60:
				return NewError(KindConfig, nil, "Kanal %d: Bildrate %d außerhalb 1–60", ch.Channel+1, s.FPS)
			case s.Bitrate < 16 || s.Bitrate > 32768:
				return NewError(KindConfig, nil, "Kanal %d: Bitrate %d kbit/s außerhalb 16–32768", ch.Channel+1, s.Bitrate)
			case s.Quality < 1 || s.Quality > 6:
				return NewError(KindConfig, nil, "Kanal %d: Qualität %d außerhalb 1–6", ch.Channel+1, s.Quality)
			case s.GOP < 1 || s.GOP > 300:
				return NewError(KindConfig, nil, "Kanal %d: Schlüsselbild-Intervall %d außerhalb 1–300", ch.Channel+1, s.GOP)
			case s.Res == "" || len(s.Res) > 20:
				return NewError(KindConfig, nil, "Kanal %d: Auflösung fehlt", ch.Channel+1)
			}
		}
		if r := ch.Record; r != nil {
			switch r.Mode {
			case RecordContinuous, RecordMotion, RecordOff:
			default:
				return NewError(KindConfig, nil, "Kanal %d: unbekannter Aufnahmemodus %q", ch.Channel+1, r.Mode)
			}
			if (r.From == "") != (r.To == "") || (r.From != "" && (!reHHMM.MatchString(r.From) || !reHHMM.MatchString(r.To) || r.From == r.To)) {
				return NewError(KindConfig, nil, "Kanal %d: Zeitfenster bitte als „HH:MM“ von–bis angeben", ch.Channel+1)
			}
		}
		if m := ch.Motion; m != nil && (m.Sensitivity < 0 || m.Sensitivity > 100) {
			return NewError(KindConfig, nil, "Kanal %d: Empfindlichkeit außerhalb des gültigen Bereichs", ch.Channel+1)
		}
	}
	if s := c.System; s != nil {
		if s.Name != nil && (strings.TrimSpace(*s.Name) == "" || len(*s.Name) > 63) {
			return NewError(KindConfig, nil, "Gerätename: 1–63 Zeichen")
		}
		if s.VideoStandard != "" && s.VideoStandard != "PAL" && s.VideoStandard != "NTSC" {
			return NewError(KindConfig, nil, "Videonorm muss PAL oder NTSC sein")
		}
		if r := s.AutoReboot; r != nil && (!slices.Contains(RebootDays, r.Day) || r.Hour < 0 || r.Hour > 23) {
			return NewError(KindConfig, nil, "Automatischer Neustart: ungültiger Tag oder ungültige Stunde")
		}
	}
	if a := c.DiskAlarms; a != nil && a.LowSpace != nil && (a.LowSpacePercent < 1 || a.LowSpacePercent > 99) {
		return NewError(KindConfig, nil, "Festplatten-Alarm: Grenze für wenig Speicherplatz 1–99 %%")
	}
	if n := c.NTP; n != nil {
		if n.Enabled && (n.Server == "" || len(n.Server) > 128) {
			return NewError(KindConfig, nil, "NTP-Server fehlt")
		}
		if n.Port < 0 || n.Port > 65535 || n.IntervalMin < 0 || n.IntervalMin > 100000 {
			return NewError(KindConfig, nil, "NTP: ungültiger Port oder ungültiges Intervall")
		}
	}
	return nil
}

// DaySlots turns a daily window into time sections "HH:MM:SS-HH:MM:SS" of
// one day: all day for an empty window, two sections across midnight.
func DaySlots(from, to string) []string {
	if from == "" {
		return []string{"00:00:00-24:00:00"}
	}
	if from < to {
		return []string{from + ":00-" + to + ":00"}
	}
	out := []string{}
	if to != "00:00" {
		out = append(out, "00:00:00-"+to+":00")
	}
	return append(out, from+":00-24:00:00")
}

// WindowFromSlots is the reverse of DaySlots: it recognises an all-day
// section, one window or a window across midnight. ok is false for anything
// else (several windows).
func WindowFromSlots(slots []string) (from, to string, ok bool) {
	type span struct{ a, b string }
	var spans []span
	for _, s := range slots {
		if len(s) != 17 || s[8] != '-' {
			return "", "", false
		}
		spans = append(spans, span{s[:5], s[9:14]})
	}
	switch len(spans) {
	case 1:
		if spans[0].a == "00:00" && spans[0].b == "24:00" {
			return "", "", true
		}
		return spans[0].a, spans[0].b, true
	case 2:
		x, y := spans[0], spans[1]
		if x.a > y.a {
			x, y = y, x
		}
		if x.a == "00:00" && y.b == "24:00" && x.b < y.a {
			return y.a, x.b, true
		}
	}
	return "", "", false
}

// SettingsWarning formats a value the device did not take over.
func onOff(b bool) string {
	if b {
		return "an"
	}
	return "aus"
}

func SettingsWarning(channel int, what string, want, got any) string {
	return fmt.Sprintf("Kanal %d: %s – gewünscht %v, das Gerät meldet %v", channel+1, what, want, got)
}

// CompareSettings lists the values of a change the device did not take over.
func CompareSettings(change SettingsChange, after *DeviceSettings) []string {
	var out []string
	byCh := map[int]ChannelSettings{}
	for _, c := range after.Channels {
		byCh[c.Channel] = c
	}
	event := func(ch int, label string, want, got *EventSettings) {
		if want == nil || got == nil {
			return
		}
		if want.Enabled != got.Enabled {
			out = append(out, SettingsWarning(ch, label, onOff(want.Enabled), onOff(got.Enabled)))
		}
		if want.Beep != nil && got.Beep != nil && *want.Beep != *got.Beep {
			out = append(out, SettingsWarning(ch, label+" Summer", onOff(*want.Beep), onOff(*got.Beep)))
		}
		if want.Record != nil && got.Record != nil && *want.Record != *got.Record {
			out = append(out, SettingsWarning(ch, label+" Aufnahme", onOff(*want.Record), onOff(*got.Record)))
		}
	}
	stream := func(ch int, label string, want, got *StreamSettings) {
		if want == nil {
			return
		}
		if got == nil {
			out = append(out, SettingsWarning(ch, label, "geändert", "nicht lesbar"))
			return
		}
		pairs := []struct {
			what      string
			want, got any
		}{
			{"aktiv", want.Enabled, got.Enabled}, {"Codec", want.Codec, got.Codec}, {"Auflösung", want.Res, got.Res},
			{"Bildrate", want.FPS, got.FPS}, {"Bitratentyp", want.Control, got.Control}, {"Bitrate", want.Bitrate, got.Bitrate},
			{"Qualität", want.Quality, got.Quality}, {"Schlüsselbild-Intervall", want.GOP, got.GOP},
		}
		for _, p := range pairs {
			if p.want != p.got {
				out = append(out, SettingsWarning(ch, label+" "+p.what, p.want, p.got))
			}
		}
	}
	for _, c := range change.Channels {
		a, ok := byCh[c.Channel]
		if !ok {
			out = append(out, fmt.Sprintf("Kanal %d: nach dem Speichern nicht mehr lesbar", c.Channel+1))
			continue
		}
		if c.Name != nil && a.Name != nil && *a.Name != *c.Name {
			out = append(out, SettingsWarning(c.Channel, "Name", *c.Name, *a.Name))
		}
		stream(c.Channel, "Hauptstream", c.Main, a.Main)
		stream(c.Channel, "Substream", c.Sub, a.Sub)
		if r := c.Record; r != nil && a.Record != nil && (r.Mode != a.Record.Mode || r.From != a.Record.From || r.To != a.Record.To) {
			out = append(out, SettingsWarning(c.Channel, "Aufnahme", r.Mode+" "+r.From+"–"+r.To, a.Record.Mode+" "+a.Record.From+"–"+a.Record.To))
		}
		if m := c.Motion; m != nil && a.Motion != nil {
			if m.Enabled != a.Motion.Enabled && !(c.Record != nil && c.Record.Mode == RecordMotion) {
				out = append(out, SettingsWarning(c.Channel, "Bewegungserkennung", m.Enabled, a.Motion.Enabled))
			}
			if m.Sensitivity != a.Motion.Sensitivity {
				out = append(out, SettingsWarning(c.Channel, "Empfindlichkeit", m.Sensitivity, a.Motion.Sensitivity))
			}
			if m.Beep != nil && a.Motion.Beep != nil && *m.Beep != *a.Motion.Beep {
				out = append(out, SettingsWarning(c.Channel, "Summer bei Bewegung", onOff(*m.Beep), onOff(*a.Motion.Beep)))
			}
		}
		event(c.Channel, "Videoverlust", c.VideoLoss, a.VideoLoss)
		event(c.Channel, "Sabotage", c.Tamper, a.Tamper)
	}
	if w, g := change.System, after.System; w != nil && g != nil {
		if w.Name != nil && g.Name != nil && *w.Name != *g.Name {
			out = append(out, fmt.Sprintf("Gerätename: gewünscht %q, das Gerät meldet %q", *w.Name, *g.Name))
		}
		if w.VideoStandard != "" && g.VideoStandard != "" && w.VideoStandard != g.VideoStandard {
			out = append(out, fmt.Sprintf("Videonorm: gewünscht %s, das Gerät meldet %s (wird meist erst nach dem Neustart wirksam)", w.VideoStandard, g.VideoStandard))
		}
		if w.AutoReboot != nil && g.AutoReboot != nil && *w.AutoReboot != *g.AutoReboot {
			out = append(out, "Automatischer Neustart wurde nicht wie gewünscht übernommen")
		}
	}
	if w, g := change.DiskAlarms, after.DiskAlarms; w != nil && g != nil {
		for _, p := range []struct {
			label    string
			want, is *EventSettings
		}{{"keine Festplatte", w.NoDisk, g.NoDisk}, {"Festplattenfehler", w.Failure, g.Failure}, {"wenig Speicherplatz", w.LowSpace, g.LowSpace}} {
			if p.want != nil && p.is != nil && (p.want.Enabled != p.is.Enabled || (p.want.Beep != nil && p.is.Beep != nil && *p.want.Beep != *p.is.Beep)) {
				out = append(out, fmt.Sprintf("Festplatten-Alarm „%s“ wurde nicht wie gewünscht übernommen", p.label))
			}
		}
		if w.LowSpace != nil && g.LowSpace != nil && w.LowSpacePercent != g.LowSpacePercent {
			out = append(out, fmt.Sprintf("Festplatten-Alarm: Grenze gewünscht %d %%, das Gerät meldet %d %%", w.LowSpacePercent, g.LowSpacePercent))
		}
	}
	if n := change.NTP; n != nil && after.Time != nil && after.Time.NTP != nil {
		g := after.Time.NTP
		if g.Enabled != n.Enabled || (n.Server != "" && g.Server != n.Server) {
			out = append(out, fmt.Sprintf("NTP: gewünscht %v/%s, das Gerät meldet %v/%s", n.Enabled, n.Server, g.Enabled, g.Server))
		}
	}
	if change.Overwrite != nil && after.Overwrite != nil && *change.Overwrite != *after.Overwrite {
		out = append(out, "Überschreiben bei voller Festplatte wurde nicht übernommen")
	}
	return out
}

// CheckOptions verifies the streams of a change against the options the
// device offers (the current snapshot): frame rate, keyframe interval,
// bitrate, codec and – where the device reports them – resolutions.
func CheckOptions(change SettingsChange, cur *DeviceSettings) error {
	byCh := map[int]ChannelSettings{}
	for _, c := range cur.Channels {
		byCh[c.Channel] = c
	}
	for _, c := range change.Channels {
		cc, ok := byCh[c.Channel]
		if !ok {
			return NewError(KindConfig, nil, "Kanal %d gibt es auf diesem Gerät nicht", c.Channel+1)
		}
		for _, p := range []struct {
			label      string
			want, have *StreamSettings
		}{{"Hauptstream", c.Main, cc.Main}, {"Substream", c.Sub, cc.Sub}} {
			if p.want == nil || p.have == nil || p.have.Options == nil {
				continue
			}
			o, w := p.have.Options, p.want
			unit := "Bilder"
			if o.GOPUnit == "seconds" {
				unit = "Sekunden"
			}
			where := fmt.Sprintf("Kanal %d %s", c.Channel+1, p.label)
			switch {
			case o.FPSMax > 0 && w.FPS > o.FPSMax:
				return NewError(KindConfig, nil, "%s: höchstens %d Bilder pro Sekunde (eingetragen: %d)", where, o.FPSMax, w.FPS)
			case o.GOPMax > 0 && w.GOP > o.GOPMax:
				return NewError(KindConfig, nil, "%s: Schlüsselbild höchstens alle %d %s (eingetragen: %d)", where, o.GOPMax, unit, w.GOP)
			case o.BitrateMax > 0 && (w.Bitrate < o.BitrateMin || w.Bitrate > o.BitrateMax):
				return NewError(KindConfig, nil, "%s: Bitrate muss zwischen %d und %d kbit/s liegen (eingetragen: %d)", where, o.BitrateMin, o.BitrateMax, w.Bitrate)
			case len(o.Codecs) > 0 && !slices.Contains(o.Codecs, w.Codec):
				return NewError(KindConfig, nil, "%s: Codec %s wird nicht unterstützt", where, w.Codec)
			case o.FromDevice && !slices.ContainsFunc(o.Resolutions, func(r Choice) bool { return r.Value == w.Res }):
				return NewError(KindConfig, nil, "%s: Auflösung %s wird nicht unterstützt", where, w.Res)
			}
		}
	}
	return nil
}

// ChangeDetails describes the stream values of a change for the log (a
// change contains no credentials).
func ChangeDetails(c SettingsChange) string {
	var out []string
	for _, ch := range c.Channels {
		for _, p := range []struct {
			l string
			s *StreamSettings
		}{{"main", ch.Main}, {"sub", ch.Sub}} {
			if s := p.s; s != nil {
				out = append(out, fmt.Sprintf("K%d %s %v %s %s %dfps %s %dk q%d gop%d", ch.Channel+1, p.l, s.Enabled, s.Codec, s.Res, s.FPS, s.Control, s.Bitrate, s.Quality, s.GOP))
			}
		}
		if r := ch.Record; r != nil {
			out = append(out, fmt.Sprintf("K%d record %s %s-%s", ch.Channel+1, r.Mode, r.From, r.To))
		}
	}
	return strings.Join(out, "; ")
}
