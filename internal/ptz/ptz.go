// Package ptz manages manual PTZ control of motorised cameras.
//
// A user takes over a camera ("Steuerung übernehmen"); CNM then pauses the
// camera's own automatic behaviour – tracking is switched off, the return
// to the guard position is postponed to CNM's return time (or switched off
// when the camera does not accept that time) – and only that user may
// steer. When the user ends the control, or after the return time without
// input, the camera moves to its guard position and CNM restores exactly
// what it paused. Sessions are stored in the data directory, so a restart
// of CNM restores the camera, too; a restore that fails (camera offline) is
// retried until it succeeds or the device is removed.
//
// Both lenses of a dual-lens camera share one head: a session belongs to
// the head, not to the camera channel. All commands to one head run one
// after another, so a stop can never overtake the move it ends.
package ptz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"cnm/internal/config"
	"cnm/internal/device"
	"cnm/internal/provider"
)

// FileName of the persisted sessions in the data directory.
const FileName = "ptz-sessions.json"

// Devices is what the manager needs from the device manager.
type Devices interface {
	Driver(deviceID string) (provider.Driver, device.Status, error)
	Status(id string) (device.Status, bool)
}

// User identifies who steers.
type User struct {
	ID    string
	Name  string
	Admin bool
}

// Error is an API error with HTTP status and code.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func errf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Error codes.
const (
	CodeNotFound    = "NOT_FOUND"
	CodeUnsupported = "PTZ_UNSUPPORTED"
	CodeOffline     = "DEVICE_OFFLINE"
	CodeBusy        = "PTZ_BUSY"
	CodeNotOwner    = "PTZ_NOT_CONTROLLING"
	CodeDevice      = "PTZ_DEVICE_ERROR"
	CodeForbidden   = "FORBIDDEN"
)

// deviceTimeout bounds one series of camera commands. They run detached
// from the browser request: a closed tab must not cut a pause or restore
// in half.
const deviceTimeout = 30 * time.Second

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), deviceTimeout)
}

// session is one manual control of a head.
type session struct {
	Key      string    `json:"key"`
	DeviceID string    `json:"deviceId"`
	Channel  int       `json:"channel"`
	UserID   string    `json:"userId"`
	Username string    `json:"username"`
	Since    time.Time `json:"since"`
	Active   time.Time `json:"active"`
	// ReturnAfter: the control ends after this time without input.
	ReturnAfter time.Duration `json:"returnAfter"`
	// Saved is the automatic behaviour before the control; Paused is what
	// CNM changed (only that is restored).
	Saved    provider.PTZAuto     `json:"saved"`
	Paused   provider.PTZAuto     `json:"paused"`
	Features provider.PTZFeatures `json:"features"`
	// Releasing: the camera still has to be restored (retried while the
	// device is offline).
	Releasing bool `json:"releasing"`

	busy     bool      // a pause or restore for this session is running
	lastTry  time.Time // last restore attempt
	attempts int       // failed restore attempts (log throttling)
	moveAt   time.Time // a continuous move runs without a stop since then
	stopping bool      // the move watchdog is stopping the camera
}

// Controller is shown to the users.
type Controller struct {
	Username  string    `json:"username"`
	Self      bool      `json:"self"`
	Since     time.Time `json:"since"`
	ReturnAt  time.Time `json:"returnAt"`
	Releasing bool      `json:"releasing"`
}

// View of a camera for the UI.
type View struct {
	Features      provider.PTZFeatures `json:"features"`
	Presets       []provider.PTZPreset `json:"presets"`
	GuardSet      bool                 `json:"guardSet"`
	Tracking      *bool                `json:"tracking,omitempty"`
	Guard         *bool                `json:"guard,omitempty"`
	ReturnMinutes int                  `json:"returnMinutes"`
	Controller    *Controller          `json:"controller,omitempty"`
}

// Manager keeps the sessions.
type Manager struct {
	devices Devices
	cfg     func() *config.Config
	path    string
	log     *slog.Logger
	now     func() time.Time
	// RetryEvery is the pause between restore attempts.
	RetryEvery time.Duration
	// MoveTimeout stops a continuous move that is not renewed (the browser
	// renews it while a button is held).
	MoveTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*session
	heads    map[string]*sync.Mutex // serialises the commands per head
	closed   bool
	started  bool
	stop     chan struct{}
	done     chan struct{}
	wg       sync.WaitGroup
}

// New creates the manager and loads persisted sessions (they are restored
// once their device is connected).
func New(devices Devices, cfg func() *config.Config, dataDir string, log *slog.Logger) *Manager {
	m := &Manager{devices: devices, cfg: cfg, log: log, now: time.Now, RetryEvery: 30 * time.Second, MoveTimeout: 8 * time.Second,
		sessions: map[string]*session{}, heads: map[string]*sync.Mutex{}, stop: make(chan struct{}), done: make(chan struct{})}
	if dataDir != "" {
		m.path = filepath.Join(dataDir, FileName)
		m.load()
	}
	return m
}

func (m *Manager) load() {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var list []*session
	if json.Unmarshal(data, &list) != nil {
		m.log.Warn("ptz sessions unreadable, ignored", "file", m.path)
		return
	}
	for _, s := range list {
		if s.Key == "" {
			continue
		}
		s.Releasing = true // the user is gone: restore the camera
		m.sessions[s.Key] = s
		m.log.Info("ptz control from before the restart will be ended", "device", s.DeviceID, "user", s.Username)
	}
}

// saveLocked writes the sessions (m.mu held).
func (m *Manager) saveLocked() {
	if m.path == "" {
		return
	}
	list := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	if len(list) == 0 {
		if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("cannot remove ptz sessions", "error", err)
		}
		return
	}
	data, _ := json.MarshalIndent(list, "", "  ")
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		m.log.Warn("cannot store ptz sessions", "error", err)
		return
	}
	if err := os.Rename(tmp, m.path); err != nil {
		m.log.Warn("cannot store ptz sessions", "error", err)
	}
}

// head returns the command lock of a head.
func (m *Manager) head(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.heads[key]
	if h == nil {
		h = &sync.Mutex{}
		m.heads[key] = h
	}
	return h
}

// Start runs the timer loop (ends idle controls, retries restores, stops
// moves that were not renewed).
func (m *Manager) Start() {
	m.mu.Lock()
	if m.started || m.closed {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	go func() {
		defer close(m.done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-t.C:
				m.Tick()
			}
		}
	}()
}

// Close stops the loop and waits for running restores (sessions stay
// stored and are restored after the next start).
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	started := m.started
	m.mu.Unlock()
	close(m.stop)
	if started {
		<-m.done
	}
	m.wg.Wait()
}

// goLocked starts background work unless the manager is closed (m.mu held).
func (m *Manager) goLocked(fn func()) {
	if m.closed {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		fn()
	}()
}

// Tick runs one round of the timer loop (exported for tests).
func (m *Manager) Tick() {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	changed := false
	for k, s := range m.sessions {
		if s.busy {
			continue
		}
		key := k
		switch {
		case s.Releasing && now.Sub(s.lastTry) >= m.RetryEvery:
			s.lastTry = now
			m.goLocked(func() { m.restore(key) })
		case !s.Releasing && now.Sub(s.Active) >= s.ReturnAfter:
			s.Releasing, s.lastTry, changed = true, now, true
			m.log.Info("ptz control ended without input", "device", s.DeviceID, "user", s.Username)
			m.goLocked(func() { m.restore(key) })
		case !s.Releasing && !s.stopping && !s.moveAt.IsZero() && now.Sub(s.moveAt) >= m.MoveTimeout:
			s.stopping = true
			m.goLocked(func() { m.stopMove(key) })
		}
	}
	if changed {
		m.saveLocked()
	}
}

// stopMove stops a move that was not renewed; it is repeated after
// MoveTimeout when the camera does not answer.
func (m *Manager) stopMove(key string) {
	m.mu.Lock()
	s := m.sessions[key]
	if s == nil {
		m.mu.Unlock()
		return
	}
	dev, ch, since := s.DeviceID, s.Channel, s.moveAt
	m.mu.Unlock()
	hl := m.head(key)
	hl.Lock()
	err := errors.New("not connected")
	if ctl, _, cerr := m.controller(dev); cerr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = ctl.PTZMove(ctx, ch, 0, 0, 0)
		cancel()
	}
	hl.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	s.stopping = false
	if !s.moveAt.Equal(since) {
		return // a new move or stop meanwhile
	}
	if err == nil {
		s.moveAt = time.Time{}
	} else {
		s.moveAt = m.now()
	}
}

// restore moves the camera to its guard position and restores what CNM
// paused; the session ends when that worked.
func (m *Manager) restore(key string) {
	m.mu.Lock()
	s := m.sessions[key]
	if s == nil || s.busy || !s.Releasing {
		m.mu.Unlock()
		return
	}
	s.busy = true
	sess := *s
	m.mu.Unlock()

	err := m.restoreDevice(key, sess)

	m.mu.Lock()
	defer m.mu.Unlock()
	s.busy = false
	if m.sessions[key] != s || !s.Releasing {
		return // taken over again meanwhile
	}
	if err != nil {
		if _, ok := m.cfg().Device(s.DeviceID); ok {
			s.attempts++
			if s.attempts == 1 || s.attempts%20 == 0 {
				m.log.Info("ptz restore pending", "device", s.DeviceID, "attempts", s.attempts, "error", provider.Redact(err.Error()))
			}
			return
		}
		m.log.Info("ptz restore dropped: device removed", "device", s.DeviceID)
	} else {
		m.log.Info("ptz camera returned and automatic behaviour restored", "device", s.DeviceID)
	}
	delete(m.sessions, key)
	m.saveLocked()
}

// restoreDevice stops the camera, moves it to its guard position (best
// effort) and restores tracking and the guard return independently.
func (m *Manager) restoreDevice(key string, s session) error {
	ctl, _, err := m.controller(s.DeviceID)
	if err != nil {
		return err
	}
	hl := m.head(key)
	hl.Lock()
	defer hl.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), deviceTimeout)
	defer cancel()
	_ = ctl.PTZMove(ctx, s.Channel, 0, 0, 0)
	if s.Features.Guard && s.Saved.GuardSet {
		if gerr := ctl.PTZGotoGuard(ctx, s.Channel); gerr != nil {
			m.log.Info("ptz return to the guard position failed", "device", s.DeviceID, "error", provider.Redact(gerr.Error()))
		}
	}
	var errs []error
	if s.Paused.Tracking != nil && s.Saved.Tracking != nil {
		if err := ctl.PTZSetAuto(ctx, s.Channel, provider.PTZAuto{Tracking: s.Saved.Tracking, TrackingMode: s.Saved.TrackingMode}); err != nil {
			errs = append(errs, err)
		}
	}
	if (s.Paused.Guard != nil || s.Paused.GuardSeconds > 0) && s.Saved.Guard != nil {
		g := provider.PTZAuto{Guard: s.Saved.Guard}
		if s.Paused.GuardSeconds > 0 {
			g.GuardSeconds = s.Saved.GuardSeconds
		}
		if err := ctl.PTZSetAuto(ctx, s.Channel, g); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// controller returns the PTZ interface of a connected device.
func (m *Manager) controller(deviceID string) (provider.PTZController, device.Status, error) {
	drv, st, err := m.devices.Driver(deviceID)
	if err != nil {
		return nil, st, provider.NewError(provider.KindNetwork, err, "Gerät nicht verbunden")
	}
	ctl, ok := drv.(provider.PTZController)
	if !ok {
		return nil, st, provider.NewError(provider.KindUnsupported, nil, "Dieses Gerät hat keine PTZ-Steuerung")
	}
	return ctl, st, nil
}

// target is a resolved camera.
type target struct {
	dev     *config.Device
	channel int
	key     string
	minutes int
}

func (m *Manager) resolve(cameraID string) (*target, *Error) {
	d, cam, ok := m.cfg().CameraSource(cameraID)
	if !ok || !cam.Enabled {
		return nil, errf(404, CodeNotFound, "Kamera nicht gefunden oder deaktiviert")
	}
	st, ok := m.devices.Status(d.ID)
	if !ok || st.Info == nil {
		return nil, errf(503, CodeOffline, "Das Gerät ist derzeit nicht verbunden.")
	}
	head, ok := st.Info.PTZ[cam.Channel]
	if !ok {
		return nil, errf(409, CodeUnsupported, "Diese Kamera lässt sich nicht schwenken oder zoomen.")
	}
	return &target{dev: d, channel: cam.Channel, key: fmt.Sprintf("%s/%d", d.ID, head), minutes: provider.PTZReturnMinutes(d.Options)}, nil
}

func (m *Manager) deviceErr(err error) *Error {
	switch provider.KindOf(err) {
	case provider.KindUnsupported:
		return errf(409, CodeUnsupported, "%s", provider.Redact(err.Error()))
	case provider.KindNetwork:
		return errf(503, CodeOffline, "Die Kamera ist derzeit nicht erreichbar.")
	}
	return errf(502, CodeDevice, "Die Kamera lehnt den Befehl ab: %s", provider.Redact(err.Error()))
}

func (m *Manager) controllerView(s *session, u User) *Controller {
	if s == nil {
		return nil
	}
	return &Controller{Username: s.Username, Self: s.UserID == u.ID && !s.Releasing, Since: s.Since, ReturnAt: s.Active.Add(s.ReturnAfter), Releasing: s.Releasing}
}

// Status returns who controls a camera (no device I/O).
func (m *Manager) Status(cameraID string, u User) (*Controller, *Error) {
	t, e := m.resolve(cameraID)
	if e != nil {
		return nil, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.controllerView(m.sessions[t.key], u), nil
}

// View reads the camera's PTZ state.
func (m *Manager) View(ctx context.Context, cameraID string, u User) (*View, *Error) {
	t, e := m.resolve(cameraID)
	if e != nil {
		return nil, e
	}
	ctl, _, err := m.controller(t.dev.ID)
	if err != nil {
		return nil, m.deviceErr(err)
	}
	st, err := ctl.PTZState(ctx, t.channel)
	if err != nil {
		return nil, m.deviceErr(err)
	}
	v := &View{Features: st.Features, Presets: st.Presets, GuardSet: st.Auto.GuardSet, ReturnMinutes: t.minutes}
	m.mu.Lock()
	s := m.sessions[t.key]
	v.Controller = m.controllerView(s, u)
	if s != nil && s.Features != (provider.PTZFeatures{}) {
		// show the behaviour the camera returns to, not the paused one
		v.Tracking, v.Guard = s.Saved.Tracking, s.Saved.Guard
	} else {
		v.Tracking, v.Guard = st.Auto.Tracking, st.Auto.Guard
	}
	m.mu.Unlock()
	if v.Presets == nil {
		v.Presets = []provider.PTZPreset{}
	}
	return v, nil
}

// Take gives the control of a camera to u. A control of another user is
// only taken over with force.
func (m *Manager) Take(ctx context.Context, cameraID string, u User, force bool) (*Controller, *Error) {
	t, e := m.resolve(cameraID)
	if e != nil {
		return nil, e
	}
	ctl, _, err := m.controller(t.dev.ID)
	if err != nil {
		return nil, m.deviceErr(err)
	}
	now := m.now()
	returnAfter := time.Duration(t.minutes) * time.Minute
	m.mu.Lock()
	s := m.sessions[t.key]
	switch {
	case s != nil && s.busy:
		m.mu.Unlock()
		return nil, errf(409, CodeBusy, "Die Kamera wird gerade vorbereitet oder zurückgestellt – bitte kurz warten.")
	case s != nil && !s.Releasing:
		defer m.mu.Unlock()
		if s.UserID != u.ID {
			if !force {
				return nil, errf(409, CodeBusy, "Die Kamera wird gerade von %s gesteuert.", s.Username)
			}
			m.log.Info("ptz control taken over", "device", s.DeviceID, "from", s.Username, "to", u.Name)
			s.UserID, s.Username, s.Since = u.ID, u.Name, now
		}
		s.Active, s.Channel, s.ReturnAfter = now, t.channel, returnAfter
		m.saveLocked()
		return m.controllerView(s, u), nil
	case s != nil:
		// a restore is pending (camera was offline): take it over; the
		// behaviour saved at the first takeover stays
		s.UserID, s.Username, s.Since, s.Active, s.Channel, s.ReturnAfter = u.ID, u.Name, now, now, t.channel, returnAfter
		s.Releasing, s.busy = false, true
		m.saveLocked()
	default:
		s = &session{Key: t.key, DeviceID: t.dev.ID, Channel: t.channel, UserID: u.ID, Username: u.Name, Since: now, Active: now,
			ReturnAfter: returnAfter, busy: true}
		m.sessions[t.key] = s
	}
	pending := s.Features != (provider.PTZFeatures{})
	m.mu.Unlock()

	dctx, cancel := detached(ctx)
	defer cancel()
	hl := m.head(t.key)
	hl.Lock()
	changed, err := m.pause(dctx, ctl, s, t, pending)
	hl.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	s.busy = false
	if err != nil {
		if pending || changed {
			s.Releasing, s.lastTry = true, time.Time{} // restore what was changed
		} else {
			delete(m.sessions, t.key) // nothing changed on the camera
		}
		m.saveLocked()
		return nil, m.deviceErr(err)
	}
	s.Active = m.now()
	m.saveLocked()
	m.log.Info("ptz control taken", "device", t.dev.ID, "channel", t.channel+1, "user", u.Name)
	return m.controllerView(s, u), nil
}

// pause reads and stores the automatic behaviour (unless a pending session
// already holds it), then switches tracking off and postpones the guard
// return to CNM's return time – or switches the return off when the camera
// refuses that time or its own time is unknown. Every step is stored before
// the next one, so a restore knows exactly what to undo. changed reports
// whether anything was changed on the camera.
func (m *Manager) pause(ctx context.Context, ctl provider.PTZController, s *session, t *target, pending bool) (changed bool, err error) {
	m.mu.Lock()
	saved, f, paused := s.Saved, s.Features, s.Paused
	m.mu.Unlock()
	if !pending {
		st, err := ctl.PTZState(ctx, t.channel)
		if err != nil {
			return false, err
		}
		saved, f = st.Auto, st.Features
		m.mu.Lock()
		s.Saved, s.Features, s.Paused = saved, f, provider.PTZAuto{}
		m.saveLocked() // stored before anything changes on the camera
		m.mu.Unlock()
	}
	record := func(p provider.PTZAuto) {
		m.mu.Lock()
		if p.Tracking != nil {
			s.Paused.Tracking = p.Tracking
		}
		if p.Guard != nil {
			s.Paused.Guard = p.Guard
		}
		if p.GuardSeconds > 0 {
			s.Paused.GuardSeconds = p.GuardSeconds
		}
		m.saveLocked()
		m.mu.Unlock()
	}
	off := false
	if f.Tracking && saved.Tracking != nil && *saved.Tracking {
		p := provider.PTZAuto{Tracking: &off}
		if err := ctl.PTZSetAuto(ctx, t.channel, p); err != nil {
			return paused.Tracking != nil, err
		}
		record(p)
		changed = true
	}
	if f.Guard && saved.Guard != nil && *saved.Guard {
		var gerr error
		if saved.GuardSeconds > 0 {
			p := provider.PTZAuto{GuardSeconds: t.minutes * 60}
			if gerr = ctl.PTZSetAuto(ctx, t.channel, p); gerr == nil {
				record(p)
				changed = true
			}
		}
		if saved.GuardSeconds <= 0 || (gerr != nil && provider.KindOf(gerr) != provider.KindNetwork) {
			// the camera refuses CNM's time (or its own is unknown): switch the return off
			p := provider.PTZAuto{Guard: &off}
			if gerr = ctl.PTZSetAuto(ctx, t.channel, p); gerr == nil {
				record(p)
				changed = true
			}
		}
		if gerr != nil {
			// steering still works; the camera may just return by itself
			m.log.Warn("ptz guard return not paused", "device", s.DeviceID, "error", provider.Redact(gerr.Error()))
		}
	}
	return changed, nil
}

// own returns the session of a camera if u controls it (m.mu held).
func (m *Manager) own(t *target, u User) (*session, *Error) {
	s := m.sessions[t.key]
	if s == nil || s.Releasing {
		return nil, errf(409, CodeNotOwner, "Zuerst die Steuerung übernehmen.")
	}
	if s.UserID != u.ID {
		return nil, errf(409, CodeBusy, "Die Kamera wird gerade von %s gesteuert.", s.Username)
	}
	if s.busy {
		return nil, errf(409, CodeBusy, "Die Kamera wird gerade vorbereitet – bitte kurz warten.")
	}
	return s, nil
}

// command runs a device command for the controlling user. Commands to one
// head run one after another, and ownership is checked again once it is
// this command's turn (a release in between wins).
func (m *Manager) command(cameraID string, u User, fn func(ctl provider.PTZController, s *session, ch int) error) *Error {
	t, e := m.resolve(cameraID)
	if e != nil {
		return e
	}
	ctl, _, err := m.controller(t.dev.ID)
	if err != nil {
		return m.deviceErr(err)
	}
	hl := m.head(t.key)
	hl.Lock()
	defer hl.Unlock()
	m.mu.Lock()
	s, e := m.own(t, u)
	if e != nil {
		m.mu.Unlock()
		return e
	}
	s.Active = m.now()
	ch := s.Channel
	m.mu.Unlock()
	if err := fn(ctl, s, ch); err != nil {
		return m.deviceErr(err)
	}
	return nil
}

// Move starts (or with all zero: stops) a continuous movement. The browser
// renews it while a button is held; otherwise it stops after MoveTimeout.
func (m *Manager) Move(ctx context.Context, cameraID string, u User, pan, tilt, zoom float64) *Error {
	clamp := func(v float64) float64 { return min(max(v, -1), 1) }
	pan, tilt, zoom = clamp(pan), clamp(tilt), clamp(zoom)
	stop := pan == 0 && tilt == 0 && zoom == 0
	return m.command(cameraID, u, func(ctl provider.PTZController, s *session, ch int) error {
		var cctx context.Context
		var cancel context.CancelFunc
		if stop {
			cctx, cancel = detached(ctx) // a stop must reach the camera even if the tab closes
		} else {
			cctx, cancel = context.WithTimeout(ctx, deviceTimeout)
		}
		defer cancel()
		err := ctl.PTZMove(cctx, ch, pan, tilt, zoom)
		m.mu.Lock()
		switch {
		case stop && err == nil:
			s.moveAt = time.Time{}
		case stop: // the watchdog repeats the stop
			s.moveAt = m.now().Add(-m.MoveTimeout)
		case err == nil:
			s.moveAt = m.now()
		}
		m.mu.Unlock()
		return err
	})
}

// GotoPreset moves to a stored position.
func (m *Manager) GotoPreset(ctx context.Context, cameraID string, u User, id int) *Error {
	return m.command(cameraID, u, func(ctl provider.PTZController, _ *session, ch int) error {
		return ctl.PTZGotoPreset(ctx, ch, id)
	})
}

// GotoGuard moves to the guard position (the control stays with u).
func (m *Manager) GotoGuard(ctx context.Context, cameraID string, u User) *Error {
	return m.command(cameraID, u, func(ctl provider.PTZController, _ *session, ch int) error {
		return ctl.PTZGotoGuard(ctx, ch)
	})
}

// SaveGuard stores the current position as guard position (admins only).
func (m *Manager) SaveGuard(ctx context.Context, cameraID string, u User) *Error {
	if !u.Admin {
		return errf(403, CodeForbidden, "Nur Administratoren können die feste Position ändern.")
	}
	return m.command(cameraID, u, func(ctl provider.PTZController, s *session, ch int) error {
		if err := ctl.PTZSaveGuard(ctx, ch); err != nil {
			return err
		}
		m.mu.Lock()
		s.Saved.GuardSet = true
		m.saveLocked()
		m.mu.Unlock()
		return nil
	})
}

// Release ends a control: the camera returns and its automatic behaviour is
// restored. Admins may end the control of other users.
func (m *Manager) Release(_ context.Context, cameraID string, u User) *Error {
	t, e := m.resolve(cameraID)
	if e != nil {
		return e
	}
	m.mu.Lock()
	s := m.sessions[t.key]
	if s == nil || s.Releasing {
		m.mu.Unlock()
		return nil
	}
	if s.UserID != u.ID && !u.Admin {
		m.mu.Unlock()
		return errf(409, CodeBusy, "Die Kamera wird gerade von %s gesteuert.", s.Username)
	}
	if s.busy {
		m.mu.Unlock()
		return errf(409, CodeBusy, "Die Kamera wird gerade vorbereitet – bitte kurz warten.")
	}
	// lastTry keeps the timer loop from starting a second restore meanwhile
	s.Releasing, s.lastTry = true, m.now()
	m.saveLocked()
	m.mu.Unlock()
	m.log.Info("ptz control ended", "device", t.dev.ID, "by", u.Name)
	m.restore(t.key)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[t.key] == s {
		return errf(202, CodeOffline, "Die Kamera ist gerade nicht erreichbar – CNM stellt sie wieder her, sobald sie erreichbar ist.")
	}
	return nil
}

// Pending reports the number of sessions (tests, status).
func (m *Manager) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}
