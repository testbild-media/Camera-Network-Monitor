// Package provider defines the device abstraction.
//
// The core application (device supervision, media reconciliation, playback,
// UI) only talks to these interfaces. The admin only chooses a protocol
// family (e.g. "Dahua", "Sofia") and enters the address; everything else –
// sub-protocol and login variant, ports, URLs, channel count, recording
// search – is detected and handled by the provider implementation
// registered with Register. Adding a protocol never requires UI changes:
// the admin UI is generated from Info().
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Support is the verification level of a capability.
type Support string

const (
	// Unsupported: not available for this model.
	Unsupported Support = "unsupported"
	// Verified: implemented and enabled.
	Verified Support = "verified"
	// Experimental: implemented from public documentation and tested against
	// the simulator, but not yet confirmed on real hardware. Enabled and
	// marked as such in the UI.
	Experimental Support = "experimental"
	// Disabled: partially known but deliberately not enabled.
	Disabled Support = "disabled"
)

// Enabled reports whether the capability may be used.
func (s Support) Enabled() bool { return s == Verified || s == Experimental }

// PortSpec describes a configurable port.
type PortSpec struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Default int    `json:"default"`
}

// OptionChoice is one value of a select option.
type OptionChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Option types and sections (drive the generated device dialog).
const (
	OptionSelect = "select"
	OptionText   = "text"
	OptionNumber = "number"

	SectionDevice  = "device"
	SectionNetwork = "network"
	// SectionAdvanced options are only needed when the detection is wrong
	// (shown collapsed under "Erweitert").
	SectionAdvanced = "advanced"
)

// OptionSpec describes a provider specific device option.
type OptionSpec struct {
	Key         string         `json:"key"`
	Label       string         `json:"label"`
	Help        string         `json:"help,omitempty"`
	Default     string         `json:"default"`
	Type        string         `json:"type"`    // select (default) | text | number
	Section     string         `json:"section"` // device (default) | network
	Placeholder string         `json:"placeholder,omitempty"`
	Min         int            `json:"min,omitempty"`
	Max         int            `json:"max,omitempty"`
	Choices     []OptionChoice `json:"choices,omitempty"`
}

// OptChannels is the option key of a configured channel count.
const OptChannels = "channels"

// ChannelOverride is the optional channel count of auto-detecting protocols
// (empty = detected).
func ChannelOverride() OptionSpec {
	return OptionSpec{Key: OptChannels, Label: "Kanäle", Type: OptionNumber, Section: SectionAdvanced, Min: 1, Max: 256,
		Placeholder: "automatisch", Help: "Nur angeben, wenn CNM die Kanalzahl nicht richtig erkennt"}
}

// ChannelCount returns the number of cameras a configured model provides.
func ChannelCount(m Model, options map[string]string) int {
	if m.Channels > 0 {
		return m.Channels
	}
	if n, err := strconv.Atoi(options[OptChannels]); err == nil && n > 0 {
		if n > 256 {
			n = 256
		}
		return n
	}
	for _, o := range m.Options {
		if o.Key == OptChannels {
			if n, err := strconv.Atoi(o.Default); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}

// Capabilities of a model. Every adapter reports each function separately
// (discovery, authentication, channel list, live, recording search,
// replay/seek, download, PTZ, configuration, events); an empty value means
// unsupported.
type Capabilities struct {
	Discovery       Support `json:"discovery"`   // found by the network scan
	ChannelList     Support `json:"channelList"` // channel count/names read from the device
	LiveMain        Support `json:"liveMain"`
	LiveSub         Support `json:"liveSub"`
	RecordingSearch Support `json:"recordingSearch"`
	// SearchKind: "list" (real recording list), "probe" (time based probe
	// only) or "span" (first/last recorded instant only, e.g. ONVIF Profile G).
	SearchKind  string  `json:"searchKind,omitempty"`
	Playback    Support `json:"playback"` // replay with seek (reopen at an absolute time)
	Download    Support `json:"download"` // export of a recorded range as MP4
	DeviceTime  Support `json:"deviceTime"`
	StorageInfo Support `json:"storageInfo"`
	PTZ         Support `json:"ptz"`
	Config      Support `json:"config"`
	Events      Support `json:"events"`
}

// Normalized fills unset fields with Unsupported and derives Download from
// Playback (the export uses the playback pipeline).
func (c Capabilities) Normalized() Capabilities {
	for _, s := range []*Support{&c.Discovery, &c.ChannelList, &c.LiveMain, &c.LiveSub, &c.RecordingSearch, &c.Playback, &c.Download, &c.DeviceTime, &c.StorageInfo, &c.PTZ, &c.Config, &c.Events} {
		if *s == "" {
			*s = Unsupported
		}
	}
	if c.Download == Unsupported && c.Playback.Enabled() {
		c.Download = c.Playback
	}
	return c
}

// Model describes one protocol family the admin can choose (the type keeps
// its historical name; there is exactly one per protocol, never per device
// model). Its ID is global over all providers and stored in the config.
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Channels int    `json:"channels"` // fixed channel count (1 for single streams), 0 = detected or option
	// AutoDetect: the driver detects variant and channel count at Connect
	// (DeviceInfo.Variant/Channels); the "channels" option only overrides.
	AutoDetect   bool         `json:"autoDetect"`
	Ports        []PortSpec   `json:"ports"`
	Options      []OptionSpec `json:"options"`
	Capabilities Capabilities `json:"capabilities"`
	// Auth names the authentication method (shown with the capabilities).
	Auth  string `json:"auth,omitempty"`
	Notes string `json:"notes,omitempty"`
}

// Info describes a provider (one implementation, one or more protocols).
type Info struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Order  int     `json:"-"` // position in the device dialog (lower first)
	Models []Model `json:"models"`
}

// FindModel returns a model by id (case-insensitive).
func (i Info) FindModel(id string) (Model, bool) {
	for _, m := range i.Models {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}
	return Model{}, false
}

// StreamKind is main or sub.
type StreamKind string

const (
	Main StreamKind = "main"
	Sub  StreamKind = "sub"
)

// DeviceConfig is the provider's view of a configured device (decrypted).
type DeviceConfig struct {
	ID    string
	Model Model
	// Variant is the result of an earlier detection ("" = detect at Connect).
	Variant  string
	Host     string
	Username string
	Password string
	Ports    map[string]int
	Options  map[string]string
	Channels int
}

// Port returns the configured port or the model default.
func (d DeviceConfig) Port(key string) int {
	if p, ok := d.Ports[key]; ok && p > 0 {
		return p
	}
	for _, ps := range d.Model.Ports {
		if ps.Key == key {
			return ps.Default
		}
	}
	return 0
}

// Option returns an option value or the model default.
func (d DeviceConfig) Option(key string) string {
	if v := d.Options[key]; v != "" {
		return v
	}
	for _, o := range d.Model.Options {
		if o.Key == key {
			return o.Default
		}
	}
	return ""
}

// Env is what the core offers to drivers.
type Env interface {
	// AllowLogin must be consulted before every login attempt. It returns false
	// while the device is locked after an authentication failure.
	AllowLogin() bool
	// ReportAuthFailure locks the device against further automatic logins.
	ReportAuthFailure(err error)
	// LegacyLiveURL returns the internal (loopback, capability-token protected)
	// URL under which go2rtc can pull a CNM-bridged live stream.
	LegacyLiveURL(channel int, kind StreamKind) string
}

// DeviceInfo is reported after login.
type DeviceInfo struct {
	// Variant identifies the detected sub-protocol/login variant (stored and
	// handed back as DeviceConfig.Variant); Summary describes it for humans
	// ("RPC2 über HTTP 80"). Both are empty for protocols without variants.
	Variant string `json:"variant,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Model is the type the device reports about itself (logs only, never
	// shown: CNM works with protocols, not device models).
	Model        string            `json:"-"`
	Serial       string            `json:"serial,omitempty"`
	Firmware     string            `json:"firmware,omitempty"`
	Channels     int               `json:"channels,omitempty"`
	ChannelNames map[int]string    `json:"channelNames,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}

// StorageState of recording storage.
type StorageState string

const (
	StorageUnknown StorageState = "unknown"
	StoragePresent StorageState = "present"
	StorageAbsent  StorageState = "absent"
)

// Health is the result of a periodic check.
type Health struct {
	Storage StorageState
	// ClockOffset = device wall clock (interpreted as UTC) - real UTC time.
	// Nil when the device time is not supported/unknown.
	ClockOffset *time.Duration
	// Degraded explains a partial problem (control works, something else not).
	Degraded string
	// LiveMode is reported by providers with auto-detected live transports.
	LiveMode string
}

// Driver is a per-device control connection created by a provider. All
// methods must be safe for concurrent use and honour ctx deadlines.
type Driver interface {
	// Connect performs the device login. It must call Env.AllowLogin first.
	Connect(ctx context.Context) (*DeviceInfo, error)
	// Check runs the periodic health check on an established connection.
	Check(ctx context.Context) (*Health, error)
	// LiveSources returns go2rtc source URLs for a channel stream. It does
	// no I/O. An empty result means the stream cannot be provided right now.
	LiveSources(channel int, kind StreamKind) []string
	// Close releases the connection. It must not block on unreachable devices.
	Close()
}

// Recording is one recorded time range (absolute time).
type Recording struct {
	Start time.Time
	End   time.Time
}

// RecordingSearcher is implemented by drivers with recording search.
type RecordingSearcher interface {
	SearchRecordings(ctx context.Context, channel int, from, to time.Time) ([]Recording, error)
}

// RecordingProber answers cheaply whether any recordings exist in a range
// (used to decide whether playback is offered for a camera).
type RecordingProber interface {
	HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error)
}

// PlaybackSource is what a driver returns to start historical playback.
// Exactly one of URL (FFmpeg input, e.g. rtsp) or Reader (raw byte stream in
// Format understood by FFmpeg, e.g. "dhav") is set.
type PlaybackSource struct {
	URL       string
	InputArgs []string // extra FFmpeg input options (e.g. -rtsp_transport tcp)
	Reader    io.ReadCloser
	Format    string
}

// PlaybackOpener is implemented by drivers with historical playback.
type PlaybackOpener interface {
	OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*PlaybackSource, error)
}

// LegacyLiveOpener is implemented by drivers that need CNM to bridge live
// video (e.g. DVRIP two-socket firmware). The reader yields an MPEG-TS
// stream (H.264/H.265, one PES per frame) starting at a keyframe.
type LegacyLiveOpener interface {
	OpenLegacyLive(ctx context.Context, channel int, kind StreamKind) (io.ReadCloser, error)
}

// Tester performs a one-shot connection test with a fresh driver.
type Tester interface {
	Test(ctx context.Context) (*TestResult, error)
}

// TestResult of a connection test.
type TestResult struct {
	Info     *DeviceInfo `json:"info,omitempty"`
	LiveMode string      `json:"liveMode,omitempty"`
	Message  string      `json:"message"`
}

// Provider creates drivers for its models.
type Provider interface {
	Info() Info
	NewDriver(cfg DeviceConfig, env Env) (Driver, error)
}

// VariantCapabilities is implemented by providers whose detected variants
// differ in what they offer (e.g. Dahua RPC2 vs. the native protocol).
type VariantCapabilities interface {
	VariantCapabilities(m Model, variant string) (Capabilities, bool)
}

// DeviceCapabilities returns the effective capabilities of a configured
// device with the given detected variant ("" = not detected yet: the
// protocol's capabilities).
func DeviceCapabilities(p Provider, m Model, variant string) Capabilities {
	if a, ok := p.(VariantCapabilities); ok && variant != "" {
		if c, ok := a.VariantCapabilities(m, variant); ok {
			return c.Normalized()
		}
	}
	return m.Capabilities.Normalized()
}

// ---- error classification ------------------------------------------------------

// ErrorKind classifies driver errors for the supervisor.
type ErrorKind string

const (
	KindAuth        ErrorKind = "auth"        // wrong credentials / locked account: never auto-retry
	KindNetwork     ErrorKind = "network"     // unreachable, timeout, reset: retry with backoff
	KindProtocol    ErrorKind = "protocol"    // unexpected answer: retry with backoff
	KindUnsupported ErrorKind = "unsupported" // feature not available
	KindConfig      ErrorKind = "config"      // invalid configuration: wait for change
	KindNotFound    ErrorKind = "notfound"    // e.g. no recordings
)

// Error is a classified provider error. Message must never contain secrets.
type Error struct {
	Kind ErrorKind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil && e.Msg == "" {
		return e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// NewError builds a classified error.
func NewError(kind ErrorKind, err error, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...), Err: err}
}

// KindOf returns the classification of err (network by default).
func KindOf(err error) ErrorKind {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Kind
	}
	if errors.Is(err, ErrLoginBlocked) {
		return KindAuth
	}
	return KindNetwork
}

// ErrLoginBlocked is returned when a login is suppressed because the device
// is locked after an authentication failure.
var ErrLoginBlocked = errors.New("Login gesperrt nach Authentifizierungsfehler – Zugangsdaten prüfen oder manuell neu verbinden")

// ---- registry --------------------------------------------------------------------------------

var (
	regMu     sync.RWMutex
	providers = map[string]Provider{}
)

// Register adds a provider. Called from provider packages' init().
func Register(p Provider) {
	regMu.Lock()
	defer regMu.Unlock()
	providers[strings.ToLower(p.Info().ID)] = p
}

// Get returns a provider by id.
func Get(id string) (Provider, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	p, ok := providers[strings.ToLower(id)]
	return p, ok
}

// All returns all providers in dialog order (Info.Order, then name).
func All() []Provider {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Info(), out[j].Info()
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.Name < b.Name
	})
	return out
}

// Resolve finds provider and protocol by the protocol id (case-insensitive).
func Resolve(protocol string) (Provider, Model, error) {
	id := strings.TrimSpace(protocol)
	for _, p := range All() {
		if m, ok := p.Info().FindModel(id); ok {
			return p, m, nil
		}
	}
	if id == "" {
		return nil, Model{}, fmt.Errorf("kein Protokoll gewählt")
	}
	return nil, Model{}, fmt.Errorf("unbekanntes Protokoll %q", protocol)
}

// Protocols lists all protocols in the order the device dialog offers them
// (provider order, the standard protocols last).
func Protocols() []Model {
	var out []Model
	for _, p := range All() {
		out = append(out, p.Info().Models...)
	}
	return out
}
