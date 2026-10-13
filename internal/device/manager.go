// Package device supervises one control connection per configured device.
//
// Every device runs in its own goroutine with its own context; a hanging or
// failing device can never affect other devices. Network errors are retried
// with exponential backoff, authentication errors are never retried
// automatically (DVR account lockout protection).
package device

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"cnm/internal/config"
	"cnm/internal/provider"
	"cnm/internal/secret"
)

// State of a device connection.
type State string

const (
	Disconnected State = "disconnected"
	Connecting   State = "connecting"
	Connected    State = "connected"
	Degraded     State = "degraded"
	Error        State = "error"
)

// Status is the observable runtime state of a device (admin only).
type Status struct {
	ID            string               `json:"id"`
	State         State                `json:"state"`
	Since         time.Time            `json:"since"`
	Error         string               `json:"error,omitempty"`
	ErrorKind     string               `json:"errorKind,omitempty"`
	AuthLocked    bool                 `json:"authLocked"`
	LastOK        *time.Time           `json:"lastOk,omitempty"`
	NextRetry     *time.Time           `json:"nextRetry,omitempty"`
	Info          *provider.DeviceInfo `json:"info,omitempty"`
	Storage       string               `json:"storage"`
	ClockOffsetMs *int64               `json:"clockOffsetMs,omitempty"`
	LiveMode      string               `json:"liveMode,omitempty"`
	Degraded      string               `json:"degraded,omitempty"`
}

// Online reports whether the control connection is usable.
func (s Status) Online() bool { return s.State == Connected || s.State == Degraded }

// LegacyURLFunc builds the internal bridge URL for a device stream.
type LegacyURLFunc func(deviceID string, channel int, kind provider.StreamKind) string

// Manager owns all supervisors.
type Manager struct {
	store     *config.Store
	keys      *secret.Keyring
	legacyURL LegacyURLFunc
	log       *slog.Logger

	mu     sync.Mutex
	sups   map[string]*supervisor
	closed bool

	lmu       sync.Mutex
	onStatus  []func(Status)
	onSources []func()
}

// NewManager creates a manager. Call Start to launch supervisors.
func NewManager(store *config.Store, keys *secret.Keyring, legacyURL LegacyURLFunc, log *slog.Logger) *Manager {
	return &Manager{store: store, keys: keys, legacyURL: legacyURL, log: log, sups: map[string]*supervisor{}}
}

// OnStatus registers a callback for status changes (called from supervisor goroutines).
func (m *Manager) OnStatus(fn func(Status)) {
	m.lmu.Lock()
	m.onStatus = append(m.onStatus, fn)
	m.lmu.Unlock()
}

// OnSourcesChanged registers a callback for changes that affect media sources
// (auth lock, live mode detection, device (re)connected).
func (m *Manager) OnSourcesChanged(fn func()) {
	m.lmu.Lock()
	m.onSources = append(m.onSources, fn)
	m.lmu.Unlock()
}

func (m *Manager) emitStatus(s Status) {
	m.lmu.Lock()
	fns := append([]func(Status){}, m.onStatus...)
	m.lmu.Unlock()
	for _, fn := range fns {
		fn(s)
	}
}

func (m *Manager) emitSources() {
	m.lmu.Lock()
	fns := append([]func(){}, m.onSources...)
	m.lmu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Start launches supervisors for the current config and follows changes.
func (m *Manager) Start() {
	m.store.Subscribe(func(_, next *config.Config) { m.apply(next) })
	m.apply(m.store.Get())
}

// fingerprint covers every field whose change requires a new connection.
func fingerprint(d *config.Device) string {
	// the detection result is not part of it: storing it must not reconnect
	type fp struct {
		Proto, H string
		P        map[string]int
		U        string
		Pw       json.RawMessage
	}
	b, _ := json.Marshal(fp{d.Protocol, d.Host, d.Ports, d.Credentials.Username, d.Credentials.Password})
	return string(b)
}

func (m *Manager) apply(c *config.Config) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	want := map[string]*config.Device{}
	for i := range c.Devices {
		want[c.Devices[i].ID] = &c.Devices[i]
	}
	var stop []*supervisor
	for id, s := range m.sups {
		d, ok := want[id]
		if !ok || fingerprint(d) != s.fp {
			stop = append(stop, s)
			delete(m.sups, id)
		}
	}
	var started []*supervisor
	for id, d := range want {
		if _, ok := m.sups[id]; ok {
			continue
		}
		s := m.newSupervisor(*d)
		m.sups[id] = s
		started = append(started, s)
	}
	m.mu.Unlock()
	for _, s := range stop {
		go s.stop() // never block the config writer on a hanging device
	}
	for _, s := range started {
		go s.run()
	}
	if len(stop) > 0 || len(started) > 0 {
		m.emitSources()
	}
}

// Close stops all supervisors, waiting at most until ctx is done.
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	sups := make([]*supervisor, 0, len(m.sups))
	for _, s := range m.sups {
		sups = append(sups, s)
	}
	m.sups = map[string]*supervisor{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range sups {
		wg.Add(1)
		go func(s *supervisor) { defer wg.Done(); s.stop() }(s)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (m *Manager) get(id string) *supervisor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sups[id]
}

// Statuses returns all device statuses sorted by id.
func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	out := make([]Status, 0, len(m.sups))
	for _, s := range m.sups {
		out = append(out, s.Status())
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Status returns the status of one device.
func (m *Manager) Status(id string) (Status, bool) {
	s := m.get(id)
	if s == nil {
		return Status{}, false
	}
	return s.Status(), true
}

// Reconnect forces an immediate reconnect and clears an authentication lock.
func (m *Manager) Reconnect(id string) bool {
	s := m.get(id)
	if s == nil {
		return false
	}
	s.reconnect()
	return true
}

// Sources returns go2rtc sources for a channel stream; nil when the device is
// unknown, locked after an auth failure or not able to provide the stream.
func (m *Manager) Sources(deviceID string, channel int, kind provider.StreamKind) []string {
	s := m.get(deviceID)
	if s == nil || s.authLocked.Load() {
		return nil
	}
	drv := s.driver()
	if drv == nil {
		return nil
	}
	return drv.LiveSources(channel, kind)
}

// Online reports whether the device control connection is up.
func (m *Manager) Online(deviceID string) bool {
	s := m.get(deviceID)
	return s != nil && s.Status().Online()
}

// SourceKey returns a value that changes whenever source-affecting state of
// the device changes (used to detect when streams must be re-registered).
func (m *Manager) SourceKey(deviceID string) string {
	s := m.get(deviceID)
	if s == nil {
		return "absent"
	}
	st := s.Status()
	return s.fp + "|" + st.LiveMode + "|" + boolStr(s.authLocked.Load())
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// OpenLegacyLive opens a CNM-bridged live stream (DVRIP two-socket firmware).
func (m *Manager) OpenLegacyLive(ctx context.Context, deviceID string, channel int, kind provider.StreamKind) (io.ReadCloser, error) {
	s := m.get(deviceID)
	if s == nil {
		return nil, errors.New("Gerät unbekannt")
	}
	if s.authLocked.Load() {
		return nil, provider.ErrLoginBlocked
	}
	drv := s.driver()
	op, ok := drv.(provider.LegacyLiveOpener)
	if drv == nil || !ok {
		return nil, errors.New("Gerät unterstützt keinen Legacy-Livepfad")
	}
	return op.OpenLegacyLive(ctx, channel, kind)
}

// ErrNotConnected is returned when an operation needs an online device.
var ErrNotConnected = errors.New("Gerät ist nicht verbunden")

// Driver returns the driver of an online device.
func (m *Manager) Driver(deviceID string) (provider.Driver, Status, error) {
	s := m.get(deviceID)
	if s == nil {
		return nil, Status{}, errors.New("Gerät unbekannt")
	}
	st := s.Status()
	drv := s.driver()
	if drv == nil || !st.Online() {
		return nil, st, ErrNotConnected
	}
	return drv, st, nil
}

// storeDetection persists what a connection found out (variant, summary,
// channel count) so the next connection skips the detection and cameras
// exist for every channel. The camera count is only taken over at the first
// detection: later the device may report something else, but cameras never
// appear or disappear behind the admin's back.
func (m *Manager) storeDetection(id string, info *provider.DeviceInfo) {
	if info == nil || info.Variant == "" {
		return
	}
	cur, ok := m.store.Get().Device(id)
	if !ok {
		return
	}
	_, model, err := provider.Resolve(cur.Protocol)
	if err != nil || !model.AutoDetect {
		return
	}
	want := config.Detection{Variant: info.Variant, Summary: info.Summary, Channels: info.Channels}
	if cur.Detected != nil && cur.Detected.Channels > 0 {
		want.Channels = cur.Detected.Channels
	}
	if cur.Detected != nil && *cur.Detected == want && (want.Channels == 0 || cur.ChannelCount > 0) {
		return
	}
	_, err = m.store.Update(func(c *config.Config) error {
		d, ok := c.Device(id)
		if !ok || d.Protocol != cur.Protocol || d.Host != cur.Host {
			return nil // changed meanwhile: the new supervisor detects again
		}
		det := want
		d.Detected = &det
		if d.ChannelCount == 0 && det.Channels > 0 {
			d.ChannelCount = det.Channels
		}
		return nil
	})
	if err != nil {
		m.log.Warn("detection not stored", "device", id, "error", err)
		return
	}
	m.log.Info("detected", "device", id, "variant", want.Variant, "summary", want.Summary, "channels", want.Channels)
}

// TestConnection runs a one-shot login test for an (unsaved) device config.
func TestConnection(ctx context.Context, d *config.Device, password string) (*provider.TestResult, error) {
	p, model, err := provider.Resolve(d.Protocol)
	if err != nil {
		return nil, provider.NewError(provider.KindConfig, err, "%s", err.Error())
	}
	cfg := driverConfig(d, model, password)
	cfg.Variant = "" // a test always detects afresh
	drv, err := p.NewDriver(cfg, testEnv{})
	if err != nil {
		return nil, err
	}
	if t, ok := drv.(provider.Tester); ok {
		return t.Test(ctx)
	}
	info, err := drv.Connect(ctx)
	drv.Close()
	if err != nil {
		return nil, err
	}
	return &provider.TestResult{Info: info, Message: "Login erfolgreich"}, nil
}

type testEnv struct{}

func (testEnv) AllowLogin() bool                              { return true }
func (testEnv) ReportAuthFailure(error)                       {}
func (testEnv) LegacyLiveURL(int, provider.StreamKind) string { return "" }

func driverConfig(d *config.Device, model provider.Model, password string) provider.DeviceConfig {
	return provider.DeviceConfig{
		ID:       d.ID,
		Model:    model,
		Variant:  d.Variant(),
		Host:     d.Host,
		Username: d.Credentials.Username,
		Password: password,
		Ports:    d.Ports,
		Options:  d.Options,
		Channels: d.ChannelCount,
	}
}
