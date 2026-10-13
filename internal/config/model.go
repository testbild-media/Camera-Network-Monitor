// Package config defines CNM's persistent data model (config.json), its
// tolerant loader for configurations written by earlier versions, validation,
// and a copy-on-write store with atomic persistence.
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SchemaVersion is the config format written by this implementation.
// 4: devices carry a protocol (+ detection) instead of manufacturer/model.
const SchemaVersion = 4

// Presentation / policy values.
const (
	AspectAuto = "auto"
	Aspect16x9 = "16:9"
	Aspect4x3  = "4:3"
	Aspect1x1  = "1:1"

	FitFill    = "fill"
	FitContain = "contain"
	FitCover   = "cover"

	RoleAdmin  = "admin"
	RoleViewer = "viewer"

	StorageAuto    = "auto"
	StoragePresent = "present"
	StorageAbsent  = "absent"

	LiveAuto   = "auto"
	LiveModern = "modern"
	LiveLegacy = "legacy"

	// Device option keys.
	OptRecordingStorage = "recordingStorage"
	OptLiveProtocol     = "liveProtocol" // obsolete, removed on load (DVRIP transport is auto-detected)
)

// Config is the root of config.json. Values obtained from a Store are
// immutable snapshots; mutate only through Store.Update.
type Config struct {
	Version  int       `json:"version"`
	System   System    `json:"system"`
	Devices  []Device  `json:"devices"`
	Cameras  []Camera  `json:"cameras"`
	Groups   []Group   `json:"groups"`
	Monitors []Monitor `json:"monitors"`
	Users    []User    `json:"users"`
	Displays []Display `json:"displays"`
}

// System holds global settings.
type System struct {
	Name                string          `json:"name"`
	ListenHost          string          `json:"listenHost"`
	ListenPort          int             `json:"listenPort"`
	HealthIntervalMs    int             `json:"healthIntervalMs"`
	ReconnectMinMs      int             `json:"reconnectMinMs"`
	ReconnectMaxMs      int             `json:"reconnectMaxMs"`
	SessionTTLHours     int             `json:"sessionTtlHours"`
	KioskSessionTTLDays int             `json:"kioskSessionTtlDays"`
	Media               Media           `json:"media"`
	Display             DisplaySettings `json:"display"`
}

// DisplaySettings configure the local display output (server GPU outputs).
type DisplaySettings struct {
	Browser      string `json:"browser"`      // auto | chromium | firefox
	DailyRestart string `json:"dailyRestart"` // "HH:MM" local time, "" = off
}

// Browser choices for local displays.
const (
	BrowserAuto     = "auto"
	BrowserChromium = "chromium"
	BrowserFirefox  = "firefox"
)

// DisplayKeyPrefix marks kiosk sessions of local displays (Principal.KeyID).
const DisplayKeyPrefix = "display:"

// Display is a local output of the server's GPU (DRM connector, e.g.
// "HDMI-A-1") that shows one multiviewer as a kiosk. The display agent signs
// in with the display's own kiosk credential (TokenHash/TokenEnc).
type Display struct {
	ID        string          `json:"id"`
	Connector string          `json:"connector"`
	Label     string          `json:"label,omitempty"`
	Enabled   bool            `json:"enabled"`
	MonitorID string          `json:"monitorId"`
	Mode      string          `json:"mode"`     // "" = preferred (EDID), else "1920x1080@60.000"
	Rotation  int             `json:"rotation"` // 0, 90, 180, 270
	TokenHash string          `json:"tokenHash"`
	TokenEnc  json.RawMessage `json:"tokenEnc,omitempty"`
}

// Media holds go2rtc / FFmpeg settings.
type Media struct {
	Enabled             bool   `json:"enabled"`
	PreloadMultiview    bool   `json:"preloadMultiview"`
	PreloadMain         bool   `json:"preloadMain"`
	AllowWebRTC         bool   `json:"allowWebRtc"`
	ReconcileIntervalMs int    `json:"reconcileIntervalMs"`
	Go2rtcURL           string `json:"go2rtcUrl"`
	ManageGo2rtc        bool   `json:"manageGo2rtc"`
	Go2rtcBin           string `json:"go2rtcBin"`
	FFmpegBin           string `json:"ffmpegBin"`
	// Go2rtcConfigPath: only its directory is used (go2rtc working directory);
	// go2rtc gets its configuration on the command line since 1.1.0.
	Go2rtcConfigPath     string `json:"go2rtcConfigPath"`
	Go2rtcAPIListen      string `json:"go2rtcApiListen"`
	Go2rtcRTSPListen     string `json:"go2rtcRtspListen"`
	Go2rtcWebRTCListen   string `json:"go2rtcWebrtcListen"`
	Go2rtcRestartDelayMs int    `json:"go2rtcRestartDelayMs"`
}

// Device is a camera provider (DVR/NVR, IP camera, stream source). Its ID is
// an internal, immutable identifier (UUID for new devices). The admin sets
// only the protocol and the address; Detected is filled in by CNM.
type Device struct {
	ID           string            `json:"id"`
	Label        string            `json:"label"`
	Protocol     string            `json:"protocol"`
	Detected     *Detection        `json:"detected,omitempty"`
	Host         string            `json:"host"`
	Credentials  Credentials       `json:"credentials"`
	Ports        map[string]int    `json:"ports"`
	ChannelCount int               `json:"channelCount"`
	Options      map[string]string `json:"options"`
	Metadata     map[string]any    `json:"metadata,omitempty"`
}

// Credentials hold the DVR login. Password is the encrypted stored form and is
// never sent to browsers.
type Credentials struct {
	Username string          `json:"username"`
	Password json.RawMessage `json:"password,omitempty"`
}

// HasPassword reports whether a password value is stored.
func (c Credentials) HasPassword() bool {
	return len(c.Password) > 0 && string(c.Password) != "null" && string(c.Password) != `""`
}

// Camera is a first-class camera. Devices only provide its video: the
// camera is bound to one channel of one device and keeps its own identity,
// name, presentation and group memberships.
type Camera struct {
	ID       string         `json:"id"`
	DeviceID string         `json:"deviceId"`
	Channel  int            `json:"channel"` // zero based
	Label    string         `json:"label"`
	Enabled  bool           `json:"enabled"`
	Streams  ChannelStreams `json:"streams"`
	View     ChannelView    `json:"view"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Group is a named set of cameras (a camera may be in several groups).
type Group struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	CameraIDs []string `json:"cameraIds"`
}

// ChannelStreams flags which live streams the channel offers.
type ChannelStreams struct {
	Main bool `json:"main"`
	Sub  bool `json:"sub"`
}

// ChannelView holds presentation settings shared by multiview, focus and playback.
// Which stream a view shows is not configurable: the multiview uses the sub
// stream and the single view the main stream, each falling back to the other.
type ChannelView struct {
	AspectRatio string `json:"aspectRatio"`
	ObjectFit   string `json:"objectFit"`
}

// Monitor is a named multiview.
type Monitor struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Layout  Layout `json:"layout"`
}

// Layout is row based: each row has a vertical weight, each slot a horizontal weight.
type Layout struct {
	Template string `json:"template"`
	Gap      int    `json:"gap"`
	Rows     []Row  `json:"rows"`
}

// Row of a layout.
type Row struct {
	ID     string  `json:"id"`
	Height float64 `json:"height"`
	Slots  []Slot  `json:"slots"`
}

// Slot of a row.
type Slot struct {
	ID       string `json:"id"`
	CameraID string `json:"cameraId"`
	Width    int    `json:"width"`
	Label    string `json:"label"`
}

// User is a CNM account.
type User struct {
	ID           string          `json:"id"`
	Username     string          `json:"username"`
	Role         string          `json:"role"`
	Enabled      bool            `json:"enabled"`
	PasswordHash json.RawMessage `json:"passwordHash,omitempty"`
	APIKeys      []APIKey        `json:"apiKeys"`
	CreatedAt    string          `json:"createdAt"`
	Preferences  UserPreferences `json:"preferences"`
}

// UserPreferences are personal settings of a user account.
type UserPreferences struct {
	// StartupScreen: "" (standard), "monitor:<id>" or – admins only –
	// "settings:<tab>". Invalid targets fall back to the standard view.
	StartupScreen string `json:"startupScreen,omitempty"`
	// CameraPicker organises the camera list of the layout editor:
	// "device" (default) or "group".
	CameraPicker string `json:"cameraPicker,omitempty"`
}

// Picker modes.
const (
	PickerDevice = "device"
	PickerGroup  = "group"
)

// SettingsTabs are the admin settings tabs a startup screen may name.
var SettingsTabs = []string{"monitors", "cameras", "groups", "devices", "displays", "users", "system"}

// APIKey is a kiosk key bound to exactly one monitor.
type APIKey struct {
	ID        string          `json:"id"`
	Label     string          `json:"label"`
	MonitorID string          `json:"monitorId"`
	CreatedAt string          `json:"createdAt"`
	Enabled   bool            `json:"enabled"`
	RevokedAt string          `json:"revokedAt,omitempty"`
	TokenHash string          `json:"tokenHash"`
	TokenEnc  json.RawMessage `json:"tokenEnc,omitempty"`
}

// Recoverable reports whether the raw token can be revealed again.
func (k APIKey) Recoverable() bool {
	return len(k.TokenEnc) > 0 && string(k.TokenEnc) != "null"
}

// ---- legacy camera references -----------------------------------------------

// ParseLegacyCameraRef splits a pre-v3 camera reference "<deviceId>:<index>".
// The device id may itself contain ':' – the index is the part after the last ':'.
func ParseLegacyCameraRef(id string) (deviceID string, index int, err error) {
	i := strings.LastIndexByte(id, ':')
	if i <= 0 || i == len(id)-1 {
		return "", 0, fmt.Errorf("invalid camera reference %q", id)
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil || n < 0 || n > 255 {
		return "", 0, fmt.Errorf("invalid camera reference %q", id)
	}
	return id[:i], n, nil
}

// NewUUID returns a random RFC 4122 version 4 UUID.
func NewUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// IsUUID reports whether s has the canonical 8-4-4-4-12 hex form.
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// ---- lookups --------------------------------------------------------------

// Device returns the device with the given id.
func (c *Config) Device(id string) (*Device, bool) {
	for i := range c.Devices {
		if c.Devices[i].ID == id {
			return &c.Devices[i], true
		}
	}
	return nil, false
}

// Monitor returns the monitor with the given id.
func (c *Config) Monitor(id string) (*Monitor, bool) {
	for i := range c.Monitors {
		if c.Monitors[i].ID == id {
			return &c.Monitors[i], true
		}
	}
	return nil, false
}

// Display returns the local display with the given id.
func (c *Config) Display(id string) (*Display, bool) {
	for i := range c.Displays {
		if c.Displays[i].ID == id {
			return &c.Displays[i], true
		}
	}
	return nil, false
}

// DisplayByConnector returns the local display configured for a connector.
func (c *Config) DisplayByConnector(name string) (*Display, bool) {
	for i := range c.Displays {
		if c.Displays[i].Connector == name {
			return &c.Displays[i], true
		}
	}
	return nil, false
}

// User returns the user with the given id.
func (c *Config) User(id string) (*User, bool) {
	for i := range c.Users {
		if c.Users[i].ID == id {
			return &c.Users[i], true
		}
	}
	return nil, false
}

// UserByName returns the user with the given username (case-insensitive).
func (c *Config) UserByName(name string) (*User, bool) {
	for i := range c.Users {
		if strings.EqualFold(c.Users[i].Username, name) {
			return &c.Users[i], true
		}
	}
	return nil, false
}

// Camera returns the camera with the given id.
func (c *Config) Camera(id string) (*Camera, bool) {
	for i := range c.Cameras {
		if c.Cameras[i].ID == id {
			return &c.Cameras[i], true
		}
	}
	return nil, false
}

// CameraSource returns a camera together with its providing device.
func (c *Config) CameraSource(id string) (*Device, *Camera, bool) {
	cam, ok := c.Camera(id)
	if !ok {
		return nil, nil, false
	}
	d, ok := c.Device(cam.DeviceID)
	if !ok {
		return nil, nil, false
	}
	return d, cam, true
}

// DeviceCameras returns the cameras provided by a device, ordered by channel.
func (c *Config) DeviceCameras(deviceID string) []*Camera {
	var out []*Camera
	for i := range c.Cameras {
		if c.Cameras[i].DeviceID == deviceID {
			out = append(out, &c.Cameras[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	return out
}

// Group returns the group with the given id.
func (c *Config) Group(id string) (*Group, bool) {
	for i := range c.Groups {
		if c.Groups[i].ID == id {
			return &c.Groups[i], true
		}
	}
	return nil, false
}

// CameraGroups returns the ids of the groups a camera belongs to.
func (c *Config) CameraGroups(cameraID string) []string {
	out := []string{}
	for _, g := range c.Groups {
		for _, id := range g.CameraIDs {
			if id == cameraID {
				out = append(out, g.ID)
				break
			}
		}
	}
	return out
}

// Option returns a device option or def.
func (d *Device) Option(key, def string) string {
	if v, ok := d.Options[key]; ok && v != "" {
		return v
	}
	return def
}

// CameraIDs returns the distinct camera ids assigned to the monitor's slots.
func (m *Monitor) CameraIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range m.Layout.Rows {
		for _, s := range r.Slots {
			if s.CameraID != "" && !seen[s.CameraID] {
				seen[s.CameraID] = true
				out = append(out, s.CameraID)
			}
		}
	}
	return out
}

// ActiveAdmins counts enabled admin accounts.
func (c *Config) ActiveAdmins() int {
	n := 0
	for _, u := range c.Users {
		if u.Enabled && u.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	b, err := json.Marshal(c)
	if err != nil {
		panic("config: clone marshal: " + err.Error())
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		panic("config: clone unmarshal: " + err.Error())
	}
	return &out
}

var streamNameSanitizer = regexp.MustCompile(`[^a-z0-9_-]+`)

// SanitizeID converts a device id into the form used inside media names.
func SanitizeID(id string) string {
	s := streamNameSanitizer.ReplaceAllString(strings.ToLower(id), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		s = "dev"
	}
	return s
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidID reports whether id is acceptable for newly created objects.
func ValidID(id string) bool { return idPattern.MatchString(id) }
