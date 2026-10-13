package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Defaults.
const (
	DefaultListenHost          = "0.0.0.0"
	DefaultListenPort          = 8090
	DefaultHealthIntervalMs    = 30000
	DefaultReconnectMinMs      = 5000
	DefaultReconnectMaxMs      = 60000
	DefaultSessionTTLHours     = 12
	DefaultKioskSessionTTLDays = 30
	DefaultGo2rtcURL           = "http://127.0.0.1:1984"
	DefaultGo2rtcAPIListen     = "127.0.0.1:1984"
	DefaultGo2rtcRTSPListen    = "127.0.0.1:8554"
	DefaultGo2rtcWebRTCListen  = ":8555"
	DefaultGo2rtcRestartDelay  = 3000
	DefaultReconcileIntervalMs = 15000
	DefaultLayoutGap           = 4
	DefaultSystemName          = "Camera Network Monitor"
)

// Layout limits.
const (
	MinGap       = 0
	MaxGap       = 24
	MinRowHeight = 0.25
	MaxRowHeight = 6
	MinSlotWidth = 1
	MaxSlotWidth = 12
	MaxRows      = 16
	MaxSlotsRow  = 16
)

// NewID returns a short random id with the given prefix.
func NewID(prefix string) string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// DefaultSystem returns the default system settings for a data directory.
func DefaultSystem(dataDir string) System {
	return System{
		Name:                DefaultSystemName,
		ListenHost:          DefaultListenHost,
		ListenPort:          DefaultListenPort,
		HealthIntervalMs:    DefaultHealthIntervalMs,
		ReconnectMinMs:      DefaultReconnectMinMs,
		ReconnectMaxMs:      DefaultReconnectMaxMs,
		SessionTTLHours:     DefaultSessionTTLHours,
		KioskSessionTTLDays: DefaultKioskSessionTTLDays,
		Media: Media{
			Enabled:              true,
			PreloadMultiview:     true,
			PreloadMain:          false,
			AllowWebRTC:          false,
			ReconcileIntervalMs:  DefaultReconcileIntervalMs,
			Go2rtcURL:            DefaultGo2rtcURL,
			ManageGo2rtc:         false,
			Go2rtcBin:            "",
			FFmpegBin:            "",
			Go2rtcConfigPath:     filepath.Join(dataDir, "go2rtc.yaml"),
			Go2rtcAPIListen:      DefaultGo2rtcAPIListen,
			Go2rtcRTSPListen:     DefaultGo2rtcRTSPListen,
			Go2rtcWebRTCListen:   DefaultGo2rtcWebRTCListen,
			Go2rtcRestartDelayMs: DefaultGo2rtcRestartDelay,
		},
	}
}

// Default returns a fresh configuration.
func Default(dataDir string) *Config {
	c := &Config{
		Version:  SchemaVersion,
		System:   DefaultSystem(dataDir),
		Devices:  []Device{},
		Cameras:  []Camera{},
		Groups:   []Group{},
		Users:    []User{},
		Monitors: []Monitor{DefaultMonitor()},
		Displays: []Display{},
	}
	return c
}

// DefaultMonitor returns "Monitor 1" with a 2x2 layout.
func DefaultMonitor() Monitor {
	return Monitor{ID: "monitor-1", Label: "Monitor 1", Enabled: true, Layout: TemplateLayout("2x2", nil)}
}

// DefaultCamera returns a new camera for a device channel.
func DefaultCamera(deviceID string, channel int) Camera {
	return Camera{
		ID:       NewUUID(),
		DeviceID: deviceID,
		Channel:  channel,
		Enabled:  true,
		Streams:  ChannelStreams{Main: true, Sub: true},
		View:     ChannelView{AspectRatio: Aspect16x9, ObjectFit: FitFill},
	}
}

// ---- layout templates ----------------------------------------------------------

// LayoutTemplate describes a starting point for a layout.
type LayoutTemplate struct {
	ID    string    `json:"id"`
	Label string    `json:"label"`
	Rows  []int     `json:"rows"`    // slots per row
	Hghts []float64 `json:"heights"` // row weights
}

// LayoutTemplates are the templates offered in the UI. Row weights
// of the mixed templates are chosen so that every tile keeps the same aspect.
var LayoutTemplates = []LayoutTemplate{
	{ID: "2x2", Label: "2 × 2", Rows: []int{2, 2}, Hghts: []float64{1, 1}},
	{ID: "3x3", Label: "3 × 3", Rows: []int{3, 3, 3}, Hghts: []float64{1, 1, 1}},
	{ID: "4x4", Label: "4 × 4", Rows: []int{4, 4, 4, 4}, Hghts: []float64{1, 1, 1, 1}},
	{ID: "5x5", Label: "5 × 5", Rows: []int{5, 5, 5, 5, 5}, Hghts: []float64{1, 1, 1, 1, 1}},
	{ID: "6x6", Label: "6 × 6", Rows: []int{6, 6, 6, 6, 6, 6}, Hghts: []float64{1, 1, 1, 1, 1, 1}},
	{ID: "3+3+2", Label: "3 + 3 + 2", Rows: []int{3, 3, 2}, Hghts: []float64{1, 1, 1.5}},
	{ID: "4+4+2", Label: "4 + 4 + 2", Rows: []int{4, 4, 2}, Hghts: []float64{1, 1, 2}},
	{ID: "6+6+3", Label: "6 + 6 + 3", Rows: []int{6, 6, 3}, Hghts: []float64{1, 1, 2}},
}

// FindTemplate returns the template with the given id.
func FindTemplate(id string) (LayoutTemplate, bool) {
	for _, t := range LayoutTemplates {
		if t.ID == id {
			return t, true
		}
	}
	return LayoutTemplate{}, false
}

// TemplateLayout builds a layout from a template, filling slots in order with
// the given camera ids (used to keep assignments when switching templates).
func TemplateLayout(id string, cameras []string) Layout {
	t, ok := FindTemplate(id)
	if !ok {
		t = LayoutTemplates[0]
	}
	l := Layout{Template: t.ID, Gap: DefaultLayoutGap}
	n := 0
	for i, count := range t.Rows {
		row := Row{ID: NewID("r_"), Height: t.Hghts[i]}
		for j := 0; j < count; j++ {
			s := Slot{ID: NewID("s_"), Width: 1}
			if n < len(cameras) {
				s.CameraID = cameras[n]
			}
			n++
			row.Slots = append(row.Slots, s)
		}
		l.Rows = append(l.Rows, row)
	}
	return l
}

// ---- normalisation ---------------------------------------------------------------

func clampInt(v, lo, hi, def int) int {
	if v == 0 && def != 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if math.IsNaN(v) || v == 0 {
		return 1
	}
	return math.Max(lo, math.Min(hi, v))
}

// NormalizeAspect maps legacy spellings onto the canonical values.
func NormalizeAspect(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	s = strings.ReplaceAll(s, "/", ":")
	switch s {
	case "16:9", "169", "16x9":
		return Aspect16x9
	case "4:3", "43", "4x3":
		return Aspect4x3
	case "1:1", "11", "1x1", "square":
		return Aspect1x1
	case "auto", "source", "quelle", "native", "original":
		return AspectAuto
	}
	return Aspect16x9
}

// NormalizeFit maps legacy spellings onto the canonical values.
func NormalizeFit(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "contain":
		return FitContain
	case "cover", "crop":
		return FitCover
	}
	return FitFill
}

// NormalizeStorage maps the recording storage override.
func NormalizeStorage(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "present", "yes", "true", "hdd", "disk", "available", "1":
		return StoragePresent
	case "absent", "no", "false", "none", "nohdd", "no-hdd", "missing", "0":
		return StorageAbsent
	}
	return StorageAuto
}

// Normalize fills defaults and clamps values so that the config is always
// usable. It never fails: invalid values are repaired, not rejected.
func Normalize(c *Config, dataDir string) {
	def := DefaultSystem(dataDir)
	s := &c.System
	if strings.TrimSpace(s.Name) == "" {
		s.Name = def.Name
	}
	if s.ListenHost == "" {
		s.ListenHost = def.ListenHost
	}
	s.ListenPort = clampInt(s.ListenPort, 1, 65535, def.ListenPort)
	s.HealthIntervalMs = clampInt(s.HealthIntervalMs, 5000, 3600000, def.HealthIntervalMs)
	s.ReconnectMinMs = clampInt(s.ReconnectMinMs, 1000, 600000, def.ReconnectMinMs)
	s.ReconnectMaxMs = clampInt(s.ReconnectMaxMs, 1000, 3600000, def.ReconnectMaxMs)
	if s.ReconnectMaxMs < s.ReconnectMinMs {
		s.ReconnectMaxMs = s.ReconnectMinMs
	}
	s.SessionTTLHours = clampInt(s.SessionTTLHours, 1, 24*365, def.SessionTTLHours)
	s.KioskSessionTTLDays = clampInt(s.KioskSessionTTLDays, 1, 3650, def.KioskSessionTTLDays)
	m := &s.Media
	m.ReconcileIntervalMs = clampInt(m.ReconcileIntervalMs, 2000, 600000, def.Media.ReconcileIntervalMs)
	m.Go2rtcRestartDelayMs = clampInt(m.Go2rtcRestartDelayMs, 500, 600000, def.Media.Go2rtcRestartDelayMs)
	if m.Go2rtcURL == "" {
		m.Go2rtcURL = def.Media.Go2rtcURL
	}
	m.Go2rtcURL = strings.TrimRight(m.Go2rtcURL, "/")
	if m.Go2rtcConfigPath == "" {
		m.Go2rtcConfigPath = def.Media.Go2rtcConfigPath
	}
	if m.Go2rtcAPIListen == "" {
		m.Go2rtcAPIListen = def.Media.Go2rtcAPIListen
	}
	if m.Go2rtcRTSPListen == "" {
		m.Go2rtcRTSPListen = def.Media.Go2rtcRTSPListen
	}
	if m.Go2rtcWebRTCListen == "" {
		m.Go2rtcWebRTCListen = def.Media.Go2rtcWebRTCListen
	}

	if c.Devices == nil {
		c.Devices = []Device{}
	}
	for i := range c.Devices {
		NormalizeDevice(&c.Devices[i])
	}
	syncCameras(c)
	normalizeGroups(c)
	if c.Users == nil {
		c.Users = []User{}
	}
	for i := range c.Users {
		u := &c.Users[i]
		if u.ID == "" {
			u.ID = NewID("u_")
		}
		if u.Role != RoleAdmin {
			u.Role = RoleViewer
		}
		if u.APIKeys == nil {
			u.APIKeys = []APIKey{}
		}
		normalizePreferences(&u.Preferences)
		for j := range u.APIKeys {
			k := &u.APIKeys[j]
			if k.ID == "" {
				k.ID = NewID("k_")
			}
			if k.Label == "" {
				k.Label = "Kiosk"
			}
		}
	}
	if c.Monitors == nil {
		c.Monitors = []Monitor{}
	}
	for i := range c.Monitors {
		NormalizeMonitor(&c.Monitors[i])
	}
	if len(c.Monitors) == 0 {
		c.Monitors = append(c.Monitors, DefaultMonitor())
	}
	normalizeDisplays(c)
	// slots referencing removed cameras become empty ("Nicht belegt")
	for mi := range c.Monitors {
		for ri := range c.Monitors[mi].Layout.Rows {
			for si := range c.Monitors[mi].Layout.Rows[ri].Slots {
				sl := &c.Monitors[mi].Layout.Rows[ri].Slots[si]
				if sl.CameraID != "" {
					if _, ok := c.Camera(sl.CameraID); !ok {
						sl.CameraID = ""
					}
				}
			}
		}
	}
	c.Version = SchemaVersion
}

var (
	displayModeRe = regexp.MustCompile(`^\d{2,5}x\d{2,5}(@\d{1,3}\.\d{3})?$`)
	clockRe       = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
	connectorRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
)

// normalizeDisplays drops invalid/duplicate connectors, clears references to
// removed monitors (such a display is switched off) and repairs values.
func normalizeDisplays(c *Config) {
	ds := &c.System.Display
	switch strings.ToLower(strings.TrimSpace(ds.Browser)) {
	case BrowserChromium, "chrome", "chromium-browser":
		ds.Browser = BrowserChromium
	case BrowserFirefox, "firefox-esr":
		ds.Browser = BrowserFirefox
	default:
		ds.Browser = BrowserAuto
	}
	ds.DailyRestart = strings.TrimSpace(ds.DailyRestart)
	if !clockRe.MatchString(ds.DailyRestart) {
		ds.DailyRestart = ""
	}
	if c.Displays == nil {
		c.Displays = []Display{}
	}
	seenID := map[string]bool{}
	seenConn := map[string]bool{}
	kept := c.Displays[:0]
	for _, d := range c.Displays {
		d.Connector = strings.TrimSpace(d.Connector)
		if !connectorRe.MatchString(d.Connector) || seenConn[d.Connector] {
			continue
		}
		seenConn[d.Connector] = true
		if !IsUUID(d.ID) || seenID[d.ID] {
			d.ID = NewUUID()
		}
		seenID[d.ID] = true
		d.Label = strings.TrimSpace(d.Label)
		if len(d.Label) > 80 {
			d.Label = d.Label[:80]
		}
		if _, ok := c.Monitor(d.MonitorID); !ok {
			d.MonitorID = ""
		}
		if d.MonitorID == "" || d.TokenHash == "" {
			d.Enabled = false
		}
		if !displayModeRe.MatchString(d.Mode) {
			d.Mode = ""
		}
		switch d.Rotation {
		case 0, 90, 180, 270:
		default:
			d.Rotation = 0
		}
		kept = append(kept, d)
	}
	c.Displays = kept
}

// syncCameras keeps exactly one camera per device channel: cameras of
// removed devices/channels are dropped, missing ones are created.
func syncCameras(c *Config) {
	if c.Cameras == nil {
		c.Cameras = []Camera{}
	}
	devs := map[string]*Device{}
	for i := range c.Devices {
		devs[c.Devices[i].ID] = &c.Devices[i]
	}
	seenID := map[string]bool{}
	seenSrc := map[string]bool{}
	kept := c.Cameras[:0]
	for _, cam := range c.Cameras {
		d := devs[cam.DeviceID]
		src := cam.DeviceID + "|" + strconv.Itoa(cam.Channel)
		if d == nil || cam.Channel < 0 || cam.Channel >= d.ChannelCount || seenSrc[src] {
			continue
		}
		if cam.ID == "" || seenID[cam.ID] {
			cam.ID = NewUUID()
		}
		seenID[cam.ID] = true
		seenSrc[src] = true
		cam.Label = strings.TrimSpace(cam.Label)
		cam.View.AspectRatio = NormalizeAspect(cam.View.AspectRatio)
		cam.View.ObjectFit = NormalizeFit(cam.View.ObjectFit)
		kept = append(kept, cam)
	}
	c.Cameras = kept
	for i := range c.Devices {
		d := &c.Devices[i]
		for ch := 0; ch < d.ChannelCount; ch++ {
			if !seenSrc[d.ID+"|"+strconv.Itoa(ch)] {
				c.Cameras = append(c.Cameras, DefaultCamera(d.ID, ch))
			}
		}
	}
	// stable order: device order, then channel
	order := map[string]int{}
	for i, d := range c.Devices {
		order[d.ID] = i
	}
	sort.SliceStable(c.Cameras, func(i, j int) bool {
		a, b := c.Cameras[i], c.Cameras[j]
		if order[a.DeviceID] != order[b.DeviceID] {
			return order[a.DeviceID] < order[b.DeviceID]
		}
		return a.Channel < b.Channel
	})
}

func normalizeGroups(c *Config) {
	if c.Groups == nil {
		c.Groups = []Group{}
	}
	seen := map[string]bool{}
	out := c.Groups[:0]
	for _, g := range c.Groups {
		if g.ID == "" || seen[g.ID] {
			g.ID = NewID("g_")
		}
		seen[g.ID] = true
		g.Label = strings.TrimSpace(g.Label)
		if g.Label == "" {
			g.Label = "Gruppe"
		}
		member := map[string]bool{}
		ids := []string{}
		for _, id := range g.CameraIDs {
			if _, ok := c.Camera(id); ok && !member[id] {
				member[id] = true
				ids = append(ids, id)
			}
		}
		g.CameraIDs = ids
		out = append(out, g)
	}
	c.Groups = out
}

// NormalizeDevice fills option defaults. Recording storage is detected
// automatically, a legacy override option is dropped.
func NormalizeDevice(d *Device) {
	d.Label = strings.TrimSpace(d.Label)
	if d.Label == "" {
		d.Label = d.ID
	}
	if d.Ports == nil {
		d.Ports = map[string]int{}
	}
	if d.Options == nil {
		d.Options = map[string]string{}
	}
	delete(d.Options, OptRecordingStorage)
	// obsolete: the DVRIP live transport is always detected automatically,
	// native Dahua playback is no longer opt-in
	delete(d.Options, OptLiveProtocol)
	delete(d.Options, "nativePlayback")
	if d.ChannelCount < 0 {
		d.ChannelCount = 0
	}
	if d.ChannelCount > 256 {
		d.ChannelCount = 256
	}
}

// NormalizeMonitor repairs a monitor layout.
func NormalizeMonitor(m *Monitor) {
	if m.ID == "" {
		m.ID = NewID("m_")
	}
	if strings.TrimSpace(m.Label) == "" {
		m.Label = m.ID
	}
	l := &m.Layout
	if l.Gap < MinGap {
		l.Gap = MinGap
	}
	if l.Gap > MaxGap {
		l.Gap = MaxGap
	}
	if len(l.Rows) > MaxRows {
		l.Rows = l.Rows[:MaxRows]
	}
	for i := range l.Rows {
		r := &l.Rows[i]
		if r.ID == "" {
			r.ID = NewID("r_")
		}
		r.Height = clampFloat(r.Height, MinRowHeight, MaxRowHeight)
		if len(r.Slots) > MaxSlotsRow {
			r.Slots = r.Slots[:MaxSlotsRow]
		}
		for j := range r.Slots {
			s := &r.Slots[j]
			if s.ID == "" {
				s.ID = NewID("s_")
			}
			s.Width = clampInt(s.Width, MinSlotWidth, MaxSlotWidth, 1)
			s.Label = strings.TrimSpace(s.Label)
		}
	}
	// A layout always has at least one slot.
	if len(l.Rows) == 0 {
		*l = TemplateLayout("2x2", nil)
	}
	for i := range l.Rows {
		if len(l.Rows[i].Slots) == 0 {
			l.Rows[i].Slots = []Slot{{ID: NewID("s_"), Width: 1}}
		}
	}
}

// ---- validation for admin mutations ---------------------------------------------------

// ValidationError is a user facing validation problem.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Invalid creates a ValidationError.
func Invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// ValidateMonitorInput checks a monitor sent by the admin UI (strict, no clamping).
func ValidateMonitorInput(m *Monitor) error {
	if strings.TrimSpace(m.Label) == "" {
		return Invalid("Monitorname fehlt")
	}
	if len([]rune(m.Label)) > 64 {
		return Invalid("Monitorname ist zu lang")
	}
	l := m.Layout
	if l.Gap < MinGap || l.Gap > MaxGap {
		return Invalid("Gap muss zwischen %d und %d px liegen", MinGap, MaxGap)
	}
	if len(l.Rows) == 0 {
		return Invalid("Layout benötigt mindestens eine Zeile")
	}
	if len(l.Rows) > MaxRows {
		return Invalid("Maximal %d Zeilen", MaxRows)
	}
	for i, r := range l.Rows {
		if math.IsNaN(r.Height) || r.Height < MinRowHeight || r.Height > MaxRowHeight {
			return Invalid("Zeile %d: Höhe muss zwischen %.2f und %.0f liegen", i+1, MinRowHeight, float64(MaxRowHeight))
		}
		if len(r.Slots) == 0 {
			return Invalid("Zeile %d benötigt mindestens einen Slot", i+1)
		}
		if len(r.Slots) > MaxSlotsRow {
			return Invalid("Zeile %d: maximal %d Slots", i+1, MaxSlotsRow)
		}
		for j, s := range r.Slots {
			if s.Width < MinSlotWidth || s.Width > MaxSlotWidth {
				return Invalid("Zeile %d, Slot %d: Breite muss zwischen %d und %d liegen", i+1, j+1, MinSlotWidth, MaxSlotWidth)
			}
			if len([]rune(s.Label)) > 64 {
				return Invalid("Zeile %d, Slot %d: Label ist zu lang", i+1, j+1)
			}
			if len(s.CameraID) > 128 {
				return Invalid("Zeile %d, Slot %d: ungültige Kamera", i+1, j+1)
			}
		}
	}
	return nil
}

// normalizePreferences drops unknown values (syntax only – whether a target
// is still valid is decided when it is used, see EffectiveStartupScreen).
func normalizePreferences(p *UserPreferences) {
	if p.CameraPicker != PickerDevice && p.CameraPicker != PickerGroup {
		p.CameraPicker = ""
	}
	kind, target, _ := strings.Cut(p.StartupScreen, ":")
	switch {
	case kind == "monitor" && ValidID(target):
	case kind == "settings" && validTab(target):
	default:
		p.StartupScreen = ""
	}
}

func validTab(t string) bool {
	for _, x := range SettingsTabs {
		if x == t {
			return true
		}
	}
	return false
}

// ValidateStartupScreen checks a startup screen a user wants to save.
func (c *Config) ValidateStartupScreen(u *User, v string) error {
	if v == "" {
		return nil
	}
	kind, target, _ := strings.Cut(v, ":")
	switch kind {
	case "monitor":
		if m, ok := c.Monitor(target); ok && m.Enabled {
			return nil
		}
		return Invalid("Dieser Multiviewer existiert nicht oder ist deaktiviert")
	case "settings":
		if u.Role != RoleAdmin {
			return Invalid("Einstellungen sind nur für Administratoren verfügbar")
		}
		if validTab(target) {
			return nil
		}
	}
	return Invalid("Unbekannter Startbildschirm")
}

// EffectiveStartupScreen returns the user's startup screen if it is still
// valid (monitor exists and is enabled, settings only for admins), else "".
func (c *Config) EffectiveStartupScreen(u *User) string {
	if u == nil || c.ValidateStartupScreen(u, u.Preferences.StartupScreen) != nil {
		return ""
	}
	return u.Preferences.StartupScreen
}
