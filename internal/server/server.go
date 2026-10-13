// Package server wires the HTTP API, the control WebSocket, the media proxy
// and the embedded web UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"cnm/internal/auth"
	"cnm/internal/config"
	"cnm/internal/connreg"
	"cnm/internal/device"
	"cnm/internal/display"
	"cnm/internal/hub"
	"cnm/internal/media"
	"cnm/internal/playback"
	"cnm/internal/provider"
	"cnm/internal/secret"
)

// Deps are the collaborators of the server.
type Deps struct {
	Store    *config.Store
	Keys     *secret.Keyring
	Auth     *auth.Service
	Devices  *device.Manager
	Recon    *media.Reconciler
	Process  *media.Process
	Playback *playback.Service
	Displays *display.Host     // local GPU outputs (nil = not available)
	Conns    *connreg.Registry // per session (logout / revoke)
	Log      *slog.Logger
	Version  string
	Static   http.Handler
}

// Server is the HTTP layer.
type Server struct {
	Deps
	Hub       *hub.Hub
	all       *connreg.Registry // every long-lived connection (shutdown)
	streams   *connreg.Registry // media sessions by scope + stream name
	playbacks *connreg.Registry // playback/download sessions by camera id
	started   time.Time
}

// New creates the server and its hub.
func New(d Deps) *Server {
	s := &Server{Deps: d, all: connreg.New(), streams: connreg.New(), playbacks: connreg.New(), started: time.Now()}
	s.Hub = hub.New(s.renderState, d.Conns, d.Log.With("component", "hub"))
	if d.Store != nil {
		d.Store.Subscribe(func(_, next *config.Config) { s.revalidateSessions(next) })
	}
	return s
}

// CloseConnections hard-closes all proxied media sockets and playback streams.
func (s *Server) CloseConnections() int { return s.all.CloseAll() }

// ---- helpers ------------------------------------------------------------------------------

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": apiError{Code: code, Message: msg}})
}

// writeAnyErr maps internal errors onto user facing responses.
func writeAnyErr(w http.ResponseWriter, err error) {
	var ve *config.ValidationError
	switch {
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, "VALIDATION", ve.Msg)
	default:
		writeErr(w, http.StatusInternalServerError, "INTERNAL", provider.Redact(err.Error()))
	}
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return config.Invalid("Ungültige Anfrage: %v", err)
	}
	return nil
}

type ctxKey int

const principalKey ctxKey = 1

func principal(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(principalKey).(*auth.Principal)
	return p
}

// access levels
type access int

const (
	public access = iota
	viewer
	admin
)

// handle wraps a handler with authentication, authorisation and CSRF checks.
func (s *Server) handle(level access, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if level == public {
			fn(w, r)
			return
		}
		p, err := s.Auth.Resolve(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "Anmeldung erforderlich")
			return
		}
		if level == admin && !p.IsAdmin() {
			writeErr(w, http.StatusForbidden, "FORBIDDEN", "Keine Berechtigung")
			return
		}
		if isMutating(r.Method) {
			if !sameOrigin(r) || !auth.CheckCSRF(p, r) {
				writeErr(w, http.StatusForbidden, "CSRF", "Sicherheitsprüfung fehlgeschlagen – bitte Seite neu laden")
				return
			}
		}
		fn(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	}
}

func isMutating(m string) bool {
	return m == http.MethodPost || m == http.MethodPut || m == http.MethodPatch || m == http.MethodDelete
}

// sameOrigin rejects requests whose Origin header names a different host.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	o = strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://")
	return strings.EqualFold(o, r.Host)
}

// ---- middleware ----------------------------------------------------------------------------

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.Log.Error("handler panic", "path", r.URL.Path, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				writeErr(w, http.StatusInternalServerError, "INTERNAL", "Interner Fehler")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		if trustworthyOrigin(r) {
			// browsers ignore COOP on plain-HTTP LAN addresses and log an error for it
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
		}
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self' ws: wss:; frame-ancestors 'self'; base-uri 'none'; form-action 'self'; object-src 'none'")
		next.ServeHTTP(w, r)
	})
}

// trustworthyOrigin reports whether the browser treats the page origin as
// potentially trustworthy (HTTPS or a loopback host).
func trustworthyOrigin(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Handler builds the HTTP handler tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)

	// auth
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/me", s.handleMe)
	mux.HandleFunc("POST /api/auth/bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /api/auth/kiosk", s.handleKioskLogin)

	// runtime
	mux.HandleFunc("GET /api/state", s.handle(viewer, s.handleState))
	mux.HandleFunc("PUT /api/me/preferences", s.handle(viewer, s.handleSavePreferences))
	mux.HandleFunc("GET /api/monitors", s.handle(viewer, s.handleMonitors))
	mux.HandleFunc("GET /api/cameras", s.handle(viewer, s.handleCameras))
	mux.HandleFunc("GET /api/media/status", s.handle(viewer, s.handleMediaStatus))
	mux.HandleFunc("POST /api/devices/reconnect", s.handle(admin, s.handleReconnect))

	// playback
	mux.HandleFunc("GET /api/playback/info", s.handle(viewer, s.handlePlaybackInfo))
	mux.HandleFunc("POST /api/playback/search", s.handle(viewer, s.handlePlaybackSearch))
	mux.HandleFunc("GET /api/playback/stream", s.handle(viewer, s.handlePlaybackStream))
	mux.HandleFunc("GET /api/playback/download", s.handle(viewer, s.handlePlaybackDownload))

	// metadata
	mux.HandleFunc("GET /api/protocols", s.handle(viewer, s.handleProtocols))
	mux.HandleFunc("GET /api/layout-templates", s.handle(viewer, s.handleTemplates))

	// settings (admin)
	mux.HandleFunc("GET /api/settings", s.handle(admin, s.handleSettings))
	mux.HandleFunc("PUT /api/settings/system", s.handle(admin, s.handleSaveSystem))
	mux.HandleFunc("POST /api/settings/devices", s.handle(admin, s.handleSaveDevice))
	mux.HandleFunc("POST /api/settings/devices/test", s.handle(admin, s.handleTestDevice))
	mux.HandleFunc("POST /api/settings/discovery", s.handle(admin, s.handleDiscovery))
	mux.HandleFunc("DELETE /api/settings/devices/{id}", s.handle(admin, s.handleDeleteDevice))
	mux.HandleFunc("POST /api/settings/monitors", s.handle(admin, s.handleSaveMonitor))
	mux.HandleFunc("DELETE /api/settings/monitors/{id}", s.handle(admin, s.handleDeleteMonitor))
	mux.HandleFunc("POST /api/settings/cameras", s.handle(admin, s.handleSaveCamera))
	mux.HandleFunc("POST /api/settings/cameras/bulk", s.handle(admin, s.handleBulkCameras))
	mux.HandleFunc("POST /api/settings/groups", s.handle(admin, s.handleSaveGroup))
	mux.HandleFunc("DELETE /api/settings/groups/{id}", s.handle(admin, s.handleDeleteGroup))
	mux.HandleFunc("GET /api/settings/displays", s.handle(admin, s.handleDisplays))
	mux.HandleFunc("POST /api/settings/displays", s.handle(admin, s.handleSaveDisplay))
	mux.HandleFunc("POST /api/settings/displays/options", s.handle(admin, s.handleDisplayOptions))
	mux.HandleFunc("DELETE /api/settings/displays/{id}", s.handle(admin, s.handleDeleteDisplay))
	mux.HandleFunc("POST /api/settings/displays/{id}/identify", s.handle(admin, s.handleIdentifyDisplay))
	mux.HandleFunc("POST /api/settings/displays/{id}/reload", s.handle(admin, s.handleReloadDisplay))
	mux.HandleFunc("GET /api/settings/users", s.handle(admin, s.handleUsers))
	mux.HandleFunc("POST /api/settings/users", s.handle(admin, s.handleCreateUser))
	mux.HandleFunc("PUT /api/settings/users/{id}", s.handle(admin, s.handleUpdateUser))
	mux.HandleFunc("DELETE /api/settings/users/{id}", s.handle(admin, s.handleDeleteUser))
	mux.HandleFunc("POST /api/settings/users/{id}/api-keys", s.handle(admin, s.handleCreateKey))
	mux.HandleFunc("GET /api/settings/users/{userId}/api-keys/{keyId}/reveal", s.handle(admin, s.handleRevealKey))
	mux.HandleFunc("DELETE /api/settings/users/{userId}/api-keys/{keyId}", s.handle(admin, s.handleDeleteKey))

	// live
	mux.HandleFunc("GET /ws", s.handle(viewer, func(w http.ResponseWriter, r *http.Request) { s.Hub.Serve(w, r, principal(r)) }))
	mux.HandleFunc("GET /media/", s.handle(viewer, s.handleMedia))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "Unbekannter Endpunkt")
	})
	if s.Static != nil {
		mux.Handle("/", s.Static)
	}
	return s.recoverer(securityHeaders(mux))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version})
}
