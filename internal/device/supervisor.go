package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"cnm/internal/config"
	"cnm/internal/provider"
	"cnm/internal/secret"
)

type supervisor struct {
	m   *Manager
	id  string
	fp  string
	dev config.Device
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	kick   chan struct{} // manual reconnect

	drvMu sync.Mutex
	drv   provider.Driver

	authLocked atomic.Bool
	authErr    atomic.Value // string

	stMu   sync.Mutex
	status Status
}

func (m *Manager) newSupervisor(d config.Device) *supervisor {
	ctx, cancel := context.WithCancel(context.Background())
	s := &supervisor{
		m: m, id: d.ID, fp: fingerprint(&d), dev: d,
		log:    m.log.With("device", d.ID, "label", d.Label),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		kick:   make(chan struct{}, 1),
	}
	s.status = Status{ID: d.ID, State: Disconnected, Since: time.Now(), Storage: string(provider.StorageUnknown)}
	return s
}

func (s *supervisor) Status() Status {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	st := s.status
	st.AuthLocked = s.authLocked.Load()
	return st
}

func (s *supervisor) update(fn func(st *Status)) {
	s.stMu.Lock()
	prev := s.status
	fn(&s.status)
	if s.status.State != prev.State {
		s.status.Since = time.Now()
	}
	st := s.status
	st.AuthLocked = s.authLocked.Load()
	s.stMu.Unlock()
	s.m.emitStatus(st)
	if prev.State != st.State || prev.LiveMode != st.LiveMode {
		s.m.emitSources()
	}
}

func (s *supervisor) driver() provider.Driver {
	s.drvMu.Lock()
	defer s.drvMu.Unlock()
	return s.drv
}

func (s *supervisor) reconnect() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// stop cancels the supervisor and closes the driver without waiting for the device.
func (s *supervisor) stop() {
	s.cancel()
	if d := s.driver(); d != nil {
		d.Close()
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		s.log.Warn("supervisor did not stop in time")
	}
}

// ---- provider.Env -----------------------------------------------------------------------

type env struct{ s *supervisor }

func (e env) AllowLogin() bool { return !e.s.authLocked.Load() && e.s.ctx.Err() == nil }

func (e env) ReportAuthFailure(err error) {
	if e.s.authLocked.Swap(true) {
		return
	}
	msg := "Authentifizierung fehlgeschlagen"
	if err != nil {
		msg = provider.Redact(err.Error())
	}
	e.s.authErr.Store(msg)
	e.s.log.Warn("authentication failed – automatic logins stopped until credentials change or manual reconnect", "error", msg)
	e.s.update(func(st *Status) {
		st.State = Error
		st.Error = msg
		st.ErrorKind = string(provider.KindAuth)
		st.NextRetry = nil
	})
	e.s.m.emitSources()
}

func (e env) LegacyLiveURL(channel int, kind provider.StreamKind) string {
	if e.s.m.legacyURL == nil {
		return ""
	}
	return e.s.m.legacyURL(e.s.id, channel, kind)
}

// ---- main loop ------------------------------------------------------------------------------

func (s *supervisor) settings() (health, minB, maxB time.Duration) {
	sys := s.m.store.Get().System
	return time.Duration(sys.HealthIntervalMs) * time.Millisecond,
		time.Duration(sys.ReconnectMinMs) * time.Millisecond,
		time.Duration(sys.ReconnectMaxMs) * time.Millisecond
}

func (s *supervisor) buildDriver() (provider.Driver, error) {
	p, model, err := provider.Resolve(s.dev.Protocol)
	if err != nil {
		return nil, provider.NewError(provider.KindConfig, err, "%s", err.Error())
	}
	password := ""
	if s.dev.Credentials.HasPassword() {
		pw, err := s.m.keys.Decrypt(s.dev.Credentials.Password)
		if err != nil {
			if secret.LooksEncrypted(s.dev.Credentials.Password) {
				return nil, provider.NewError(provider.KindConfig, err, "DVR-Passwort kann nicht entschlüsselt werden (master.key passt nicht zu config.json) – Passwort neu eingeben")
			}
			// legacy plaintext value
			var plain string
			if jsonErr := jsonUnmarshalString(s.dev.Credentials.Password, &plain); jsonErr != nil {
				return nil, provider.NewError(provider.KindConfig, jsonErr, "DVR-Passwort unlesbar – Passwort neu eingeben")
			}
			pw = plain
		}
		password = pw
	}
	return p.NewDriver(driverConfig(&s.dev, model, password), env{s})
}

func (s *supervisor) run() {
	defer close(s.done)
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("supervisor panic", "panic", r, "stack", string(debug.Stack()))
			s.update(func(st *Status) { st.State = Error; st.Error = "interner Fehler"; st.ErrorKind = "internal" })
		}
	}()
	drv, err := s.buildDriver()
	if err != nil {
		s.update(func(st *Status) {
			st.State = Error
			st.Error = provider.Redact(err.Error())
			st.ErrorKind = string(provider.KindConfig)
		})
		<-s.ctx.Done() // wait for a config change (which replaces this supervisor)
		return
	}
	s.drvMu.Lock()
	s.drv = drv
	s.drvMu.Unlock()
	defer drv.Close()

	_, minB, _ := s.settings()
	backoff := minB
	for s.ctx.Err() == nil {
		if s.authLocked.Load() {
			msg, _ := s.authErr.Load().(string)
			s.update(func(st *Status) {
				st.State = Error
				st.Error = msg
				st.ErrorKind = string(provider.KindAuth)
				st.NextRetry = nil
			})
			if !s.waitKick(0) {
				return
			}
			s.authLocked.Store(false)
			s.m.emitSources()
			backoff = minB
			continue
		}
		s.update(func(st *Status) { st.State = Connecting; st.NextRetry = nil })
		cctx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
		info, err := drv.Connect(cctx)
		cancel()
		if s.ctx.Err() != nil {
			return
		}
		if err != nil {
			kind := provider.KindOf(err)
			if kind == provider.KindAuth || errors.Is(err, provider.ErrLoginBlocked) {
				env{s}.ReportAuthFailure(err)
				continue
			}
			_, minB, maxB := s.settings()
			if backoff < minB {
				backoff = minB
			}
			next := time.Now().Add(backoff)
			msg := provider.Redact(err.Error())
			s.log.Info("connect failed", "error", msg, "retry_in", backoff.String())
			s.update(func(st *Status) {
				st.State = Disconnected
				st.Error = msg
				st.ErrorKind = string(kind)
				st.NextRetry = &next
			})
			if kind == provider.KindConfig {
				if !s.waitKick(0) {
					return
				}
				continue
			}
			if !s.waitKick(backoff) {
				return
			}
			backoff *= 2
			if backoff > maxB {
				backoff = maxB
			}
			continue
		}
		_, minB, _ = s.settings()
		backoff = minB
		live := ""
		if info != nil && info.Extra != nil {
			live = info.Extra["liveMode"]
		}
		s.m.storeDetection(s.dev.ID, info)
		now := time.Now()
		s.log.Info("connected")
		s.update(func(st *Status) {
			st.State = Connected
			st.Error, st.ErrorKind, st.Degraded = "", "", ""
			st.Info = info
			st.LastOK = &now
			st.NextRetry = nil
			if live != "" {
				st.LiveMode = live
			}
		})
		manual := s.healthLoop(drv)
		if s.ctx.Err() != nil {
			return
		}
		if manual {
			backoff = 0
			continue
		}
		// connection lost: first retry after the minimum backoff
		_, minB, _ = s.settings()
		if !s.waitKick(minB) {
			return
		}
		backoff = minB
	}
}

// healthLoop runs periodic checks until the connection fails. It returns true
// when a manual reconnect was requested.
func (s *supervisor) healthLoop(drv provider.Driver) bool {
	for {
		cctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		h, err := drv.Check(cctx)
		cancel()
		if s.ctx.Err() != nil {
			return false
		}
		if err != nil {
			kind := provider.KindOf(err)
			msg := provider.Redact(err.Error())
			if kind == provider.KindAuth || errors.Is(err, provider.ErrLoginBlocked) {
				env{s}.ReportAuthFailure(err)
				return false
			}
			s.log.Info("health check failed", "error", msg)
			s.update(func(st *Status) {
				st.State = Disconnected
				st.Error = msg
				st.ErrorKind = string(kind)
			})
			return false
		}
		now := time.Now()
		s.update(func(st *Status) {
			st.LastOK = &now
			st.Storage = string(h.Storage)
			if h.ClockOffset != nil {
				ms := h.ClockOffset.Milliseconds()
				st.ClockOffsetMs = &ms
			}
			if h.LiveMode != "" {
				st.LiveMode = h.LiveMode
			}
			st.Degraded = h.Degraded
			if h.Degraded != "" {
				st.State = Degraded
			} else {
				st.State = Connected
			}
			st.Error, st.ErrorKind = "", ""
		})
		interval, _, _ := s.settings()
		select {
		case <-s.ctx.Done():
			return false
		case <-s.kick:
			s.log.Info("manual reconnect")
			return true
		case <-time.After(interval):
		}
	}
}

// waitKick waits for d (0 = forever), a manual reconnect or cancellation.
// It returns false when the supervisor is stopping.
func (s *supervisor) waitKick(d time.Duration) bool {
	var timer <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-s.ctx.Done():
		return false
	case <-s.kick:
		return true
	case <-timer:
		return true
	}
}

func jsonUnmarshalString(raw []byte, out *string) error {
	if len(raw) < 2 || raw[0] != '"' {
		return fmt.Errorf("not a string")
	}
	return jsonUnmarshal(raw, out)
}

var jsonUnmarshal = func(b []byte, v any) error { return json.Unmarshal(b, v) }
