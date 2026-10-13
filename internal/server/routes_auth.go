package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"cnm/internal/auth"
	"cnm/internal/config"
)

func setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(maxAge.Seconds()),
	})
}

func clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1})
}

type meResponse struct {
	Authenticated  bool            `json:"authenticated"`
	Principal      *auth.Principal `json:"principal,omitempty"`
	CSRF           string          `json:"csrf,omitempty"`
	NeedsBootstrap bool            `json:"needsBootstrap"`
	Local          bool            `json:"local"`
	SystemName     string          `json:"systemName"`
	Version        string          `json:"version"`
	// Preferences of the logged in user; StartupScreen is the effective
	// target (empty when the stored one is no longer valid).
	Preferences *config.UserPreferences `json:"preferences,omitempty"`
}

func (s *Server) me(r *http.Request, p *auth.Principal) meResponse {
	resp := meResponse{
		NeedsBootstrap: s.Auth.NeedsBootstrap(),
		Local:          auth.IsLocal(r),
		SystemName:     s.Store.Get().System.Name,
		Version:        s.Version,
	}
	if p != nil {
		resp.Authenticated = true
		resp.Principal = p
		resp.CSRF = p.CSRF
		if p.Kind == auth.KindUser {
			c := s.Store.Get()
			if u, ok := c.User(p.UserID); ok {
				prefs := u.Preferences
				prefs.StartupScreen = c.EffectiveStartupScreen(u)
				resp.Preferences = &prefs
			}
		}
	}
	return resp
}

// handleSavePreferences stores personal preferences of the logged in user
// (never of kiosks). Admin-only targets are rejected for viewers.
func (s *Server) handleSavePreferences(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p == nil || p.Kind != auth.KindUser {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "Nur für Benutzerkonten")
		return
	}
	var in struct {
		StartupScreen *string `json:"startupScreen"`
		CameraPicker  *string `json:"cameraPicker"`
	}
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	var saved config.UserPreferences
	_, err := s.Store.Update(func(c *config.Config) error {
		u, ok := c.User(p.UserID)
		if !ok {
			return config.Invalid("Benutzer nicht gefunden")
		}
		if in.StartupScreen != nil {
			v := strings.TrimSpace(*in.StartupScreen)
			if err := c.ValidateStartupScreen(u, v); err != nil {
				return err
			}
			u.Preferences.StartupScreen = v
		}
		if in.CameraPicker != nil {
			switch *in.CameraPicker {
			case config.PickerDevice, config.PickerGroup:
				u.Preferences.CameraPicker = *in.CameraPicker
			default:
				return config.Invalid("Unbekannte Sortierung")
			}
		}
		saved = u.Preferences
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, err := s.Auth.Resolve(r)
	if err != nil {
		resp := s.me(r, nil)
		if auth.KioskSelector(r) != "" {
			// neutral answer for kiosks – no hints about missing parameters
			writeJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
			return
		}
		writeJSON(w, http.StatusUnauthorized, resp)
		return
	}
	writeJSON(w, http.StatusOK, s.me(r, p))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden, "CSRF", "Ungültige Herkunft")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeAnyErr(w, err)
		return
	}
	p, tok, err := s.Auth.Login(body.Username, body.Password, auth.ClientIP(r))
	if err != nil {
		status := http.StatusUnauthorized
		code := "INVALID_LOGIN"
		if errors.Is(err, auth.ErrRateLimited) {
			status, code = http.StatusTooManyRequests, "RATE_LIMITED"
		}
		s.Log.Info("login failed", "user", body.Username, "ip", auth.ClientIP(r), "reason", code)
		writeErr(w, status, code, err.Error())
		return
	}
	s.Log.Info("login", "user", p.Username, "ip", auth.ClientIP(r))
	setCookie(w, r, auth.SessionCookie, tok, s.Auth.SessionTTL())
	writeJSON(w, http.StatusOK, s.me(r, p))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if mon := auth.KioskSelector(r); mon != "" {
		if ck, err := r.Cookie(auth.KioskCookieName(mon)); err == nil {
			s.Auth.Logout(ck.Value)
		}
		clearCookie(w, r, auth.KioskCookieName(mon))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if ck, err := r.Cookie(auth.SessionCookie); err == nil {
		// CSRF protection for logout of a real session
		if p, err := s.Auth.Resolve(r); err == nil && p.SessionID != "" && !auth.CheckCSRF(p, r) {
			writeErr(w, http.StatusForbidden, "CSRF", "Sicherheitsprüfung fehlgeschlagen")
			return
		}
		s.Auth.Logout(ck.Value)
	}
	clearCookie(w, r, auth.SessionCookie)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden, "CSRF", "Ungültige Herkunft")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeAnyErr(w, err)
		return
	}
	p, tok, err := s.Auth.Bootstrap(body.Username, body.Password, auth.IsLocal(r))
	switch {
	case errors.Is(err, auth.ErrBootstrapLocal), errors.Is(err, auth.ErrBootstrapDone):
		writeErr(w, http.StatusForbidden, "BOOTSTRAP_UNAVAILABLE", err.Error())
		return
	case err != nil:
		writeAnyErr(w, err)
		return
	}
	s.Log.Info("initial admin created", "user", p.Username)
	setCookie(w, r, auth.SessionCookie, tok, s.Auth.SessionTTL())
	writeJSON(w, http.StatusOK, s.me(r, p))
}

func (s *Server) handleKioskLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Monitor   string `json:"mon"`
		MonitorID string `json:"monitorId"` // v0.3.x API
		Key       string `json:"key"`
	}
	if !sameOrigin(r) || readJSON(r, &body) != nil {
		writeErr(w, http.StatusForbidden, "ACCESS_DENIED", auth.ErrKioskDenied.Error())
		return
	}
	if body.Monitor == "" {
		body.Monitor = body.MonitorID
	}
	p, tok, err := s.Auth.KioskLogin(body.Monitor, body.Key, auth.ClientIP(r))
	if err != nil {
		s.Log.Info("kiosk access denied", "ip", auth.ClientIP(r))
		writeErr(w, http.StatusForbidden, "ACCESS_DENIED", auth.ErrKioskDenied.Error())
		return
	}
	setCookie(w, r, auth.KioskCookieName(body.Monitor), tok, s.Auth.KioskTTL())
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "principal": p, "csrf": p.CSRF, "systemName": s.Store.Get().System.Name, "version": s.Version})
}
