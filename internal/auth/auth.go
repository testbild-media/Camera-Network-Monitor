// Package auth implements CNM's identity model:
//
//   - user sessions (cookie "cnm_session", 12 h sliding TTL, CSRF token)
//   - kiosk sessions bound to exactly one monitor and always viewer-only
//     (cookie "cnm_k_<hash(monitor)>", selected explicitly per request via
//     the X-CNM-Kiosk header or the kiosk query parameter, so an admin tab and
//     a kiosk tab in the same browser never mix identities)
//   - no anonymous access at all (localhost only matters for the first-admin bootstrap)
//
// Sessions live in memory, are swept periodically and persisted (token hashes
// only) so that a service restart does not log everybody out (persist.go).
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cnm/internal/config"
	"cnm/internal/connreg"
	"cnm/internal/secret"
)

// Kind of principal.
type Kind string

const (
	KindUser  Kind = "user"
	KindKiosk Kind = "kiosk"
)

// Cookie and header names.
const (
	SessionCookie  = "cnm_session"
	KioskCookiePfx = "cnm_k_"
	KioskHeader    = "X-CNM-Kiosk"
	KioskQuery     = "kiosk"
	CSRFHeader     = "X-CSRF-Token"
)

// Principal is the authenticated caller of a request.
type Principal struct {
	Kind      Kind   `json:"kind"`
	UserID    string `json:"userId,omitempty"`
	Username  string `json:"username,omitempty"`
	Role      string `json:"role"`
	SessionID string `json:"-"`
	KeyID     string `json:"-"`
	MonitorID string `json:"monitorId,omitempty"`
	CSRF      string `json:"-"`
}

// IsAdmin reports admin rights. Kiosk and local principals are never admin.
func (p *Principal) IsAdmin() bool {
	return p != nil && p.Kind == KindUser && p.Role == config.RoleAdmin
}

// ScopeMonitor returns the monitor a kiosk is bound to ("" = unrestricted viewer).
func (p *Principal) ScopeMonitor() string {
	if p != nil && p.Kind == KindKiosk {
		return p.MonitorID
	}
	return ""
}

// ConnKey is the key used in the connection registry ("" for local anonymous).
func (p *Principal) ConnKey() string {
	if p == nil || p.SessionID == "" {
		return "local"
	}
	return p.SessionID
}

// Errors.
var (
	ErrUnauthorized   = errors.New("nicht angemeldet")
	ErrInvalidLogin   = errors.New("Benutzername oder Passwort falsch")
	ErrRateLimited    = errors.New("Zu viele Fehlversuche – bitte später erneut versuchen")
	ErrKioskDenied    = errors.New("ZUGRIFF GESPERRT")
	ErrBootstrapDone  = errors.New("Bootstrap nicht mehr verfügbar")
	ErrBootstrapLocal = errors.New("Bootstrap ist nur über localhost möglich")
)

type session struct {
	id        string // registry key (not the cookie secret)
	kind      Kind
	userID    string
	keyID     string
	monitorID string
	csrf      string
	ttl       time.Duration
	expires   time.Time
	touched   time.Time
}

// Service manages sessions.
type Service struct {
	store      *config.Store
	keys       *secret.Keyring
	conns      *connreg.Registry
	limit      *RateLimiter
	kioskLimit *RateLimiter
	now        func() time.Time

	mu       sync.Mutex
	sessions map[string]*session // key: sha256(cookie token)

	saveMu sync.Mutex
	dirty  atomic.Bool
}

// NewService creates the auth service and follows config changes to
// invalidate sessions of removed/disabled users and revoked kiosk keys.
func NewService(store *config.Store, keys *secret.Keyring, conns *connreg.Registry) *Service {
	s := &Service{
		store:      store,
		keys:       keys,
		conns:      conns,
		limit:      NewRateLimiter(5, 10*time.Minute),
		kioskLimit: NewRateLimiter(20, 10*time.Minute),
		now:        time.Now,
		sessions:   map[string]*session{},
	}
	s.loadSessions()
	store.Subscribe(func(_, next *config.Config) { s.revalidate(next) })
	return s
}

func tokenKey(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// KioskCookieName returns the per-monitor kiosk cookie name.
func KioskCookieName(monitorID string) string {
	h := sha256.Sum256([]byte(monitorID))
	return KioskCookiePfx + hex.EncodeToString(h[:8])
}

// ---- request helpers ---------------------------------------------------------------

// IsLocal reports whether the request comes directly from the loopback
// interface (used for the first-admin bootstrap). Requests that carry proxy
// headers are never considered local.
func IsLocal(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ClientIP returns the remote IP (no proxy headers are trusted).
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// KioskSelector returns the monitor id a request explicitly selects as kiosk identity.
func KioskSelector(r *http.Request) string {
	if v := r.Header.Get(KioskHeader); v != "" {
		return v
	}
	return r.URL.Query().Get(KioskQuery)
}

// ---- session lifecycle --------------------------------------------------------------

func (s *Service) newSession(kind Kind, userID, keyID, monitorID string, ttl time.Duration) (token string, sess *session) {
	token = secret.GenerateToken()
	now := s.now()
	sess = &session{
		id:        secret.RandomID(12),
		kind:      kind,
		userID:    userID,
		keyID:     keyID,
		monitorID: monitorID,
		csrf:      secret.GenerateToken(),
		ttl:       ttl,
		expires:   now.Add(ttl),
		touched:   now,
	}
	s.mu.Lock()
	s.sessions[tokenKey(token)] = sess
	s.mu.Unlock()
	s.changed(true)
	return token, sess
}

func (s *Service) sessionTTL() time.Duration {
	return time.Duration(s.store.Get().System.SessionTTLHours) * time.Hour
}

func (s *Service) kioskTTL() time.Duration {
	return time.Duration(s.store.Get().System.KioskSessionTTLDays) * 24 * time.Hour
}

// lookup returns a live session and slides its expiry.
func (s *Service) lookup(token string) *session {
	if token == "" {
		return nil
	}
	k := tokenKey(token)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[k]
	if sess == nil {
		return nil
	}
	if now.After(sess.expires) {
		delete(s.sessions, k)
		go s.conns.Close(sess.id)
		s.changed(false)
		return nil
	}
	if now.Sub(sess.touched) > time.Minute {
		sess.touched = now
		sess.expires = now.Add(sess.ttl)
		s.changed(false)
	}
	return sess
}

// Sweep removes expired sessions and stale rate limiter entries.
func (s *Service) Sweep() {
	now := s.now()
	var expired []string
	s.mu.Lock()
	for k, sess := range s.sessions {
		if now.After(sess.expires) {
			delete(s.sessions, k)
			expired = append(expired, sess.id)
		}
	}
	s.mu.Unlock()
	for _, id := range expired {
		s.conns.Close(id)
	}
	if len(expired) > 0 {
		s.changed(false)
	}
	s.limit.Sweep()
	s.kioskLimit.Sweep()
	_ = s.SaveSessions()
}

// SessionCount returns the number of live sessions.
func (s *Service) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *Service) principalFor(sess *session, c *config.Config) (*Principal, bool) {
	if sess.kind == KindKiosk && strings.HasPrefix(sess.keyID, config.DisplayKeyPrefix) {
		return displayPrincipal(sess, c)
	}
	u, ok := c.User(sess.userID)
	if !ok || !u.Enabled {
		return nil, false
	}
	switch sess.kind {
	case KindUser:
		return &Principal{Kind: KindUser, UserID: u.ID, Username: u.Username, Role: u.Role, SessionID: sess.id, CSRF: sess.csrf}, true
	case KindKiosk:
		var key *config.APIKey
		for i := range u.APIKeys {
			if u.APIKeys[i].ID == sess.keyID {
				key = &u.APIKeys[i]
			}
		}
		if key == nil || !key.Enabled || key.MonitorID != sess.monitorID {
			return nil, false
		}
		if m, ok := c.Monitor(sess.monitorID); !ok || !m.Enabled {
			return nil, false
		}
		return &Principal{Kind: KindKiosk, UserID: u.ID, Username: u.Username, Role: config.RoleViewer, SessionID: sess.id, KeyID: key.ID, MonitorID: sess.monitorID, CSRF: sess.csrf}, true
	}
	return nil, false
}

// displayPrincipal resolves the kiosk session of a local display: valid while
// the display is enabled and still shows the same, enabled monitor.
func displayPrincipal(sess *session, c *config.Config) (*Principal, bool) {
	d, ok := c.Display(strings.TrimPrefix(sess.keyID, config.DisplayKeyPrefix))
	if !ok || !d.Enabled || d.MonitorID != sess.monitorID {
		return nil, false
	}
	if m, ok := c.Monitor(sess.monitorID); !ok || !m.Enabled {
		return nil, false
	}
	return &Principal{Kind: KindKiosk, Username: "Display " + d.Connector, Role: config.RoleViewer, SessionID: sess.id, KeyID: sess.keyID, MonitorID: sess.monitorID, CSRF: sess.csrf}, true
}

// revalidate drops sessions that became invalid after a config change and
// immediately closes their connections (kiosk revoke, user disabled/deleted).
func (s *Service) revalidate(c *config.Config) {
	var dead []string
	s.mu.Lock()
	for k, sess := range s.sessions {
		if _, ok := s.principalFor(sess, c); !ok {
			delete(s.sessions, k)
			dead = append(dead, sess.id)
		}
	}
	s.mu.Unlock()
	for _, id := range dead {
		s.conns.Close(id)
	}
	if len(dead) > 0 {
		s.changed(true)
	}
}

// Resolve authenticates a request. A request selecting a kiosk identity is
// only ever authenticated as that kiosk – it never falls back to a user
// session or to the localhost viewer.
func (s *Service) Resolve(r *http.Request) (*Principal, error) {
	c := s.store.Get()
	if mon := KioskSelector(r); mon != "" {
		ck, err := r.Cookie(KioskCookieName(mon))
		if err != nil {
			return nil, ErrUnauthorized
		}
		sess := s.lookup(ck.Value)
		if sess == nil || sess.kind != KindKiosk || sess.monitorID != mon {
			return nil, ErrUnauthorized
		}
		p, ok := s.principalFor(sess, c)
		if !ok {
			return nil, ErrUnauthorized
		}
		return p, nil
	}
	if ck, err := r.Cookie(SessionCookie); err == nil && ck.Value != "" {
		if sess := s.lookup(ck.Value); sess != nil && sess.kind == KindUser {
			if p, ok := s.principalFor(sess, c); ok {
				return p, nil
			}
		}
	}
	// No anonymous access – not even from localhost. Kiosk URLs are the only
	// way to watch without a user login.
	return nil, ErrUnauthorized
}

// ---- login flows ------------------------------------------------------------------------

// Login verifies credentials (rate limited per client IP + username).
func (s *Service) Login(username, password, clientIP string) (*Principal, string, error) {
	key := clientIP + "|" + strings.ToLower(strings.TrimSpace(username))
	if !s.limit.Allow(key) {
		return nil, "", ErrRateLimited
	}
	c := s.store.Get()
	u, ok := c.UserByName(strings.TrimSpace(username))
	if !ok || !u.Enabled {
		secret.VerifyPassword(nil, password) // constant-ish timing for unknown users
		s.limit.Fail(key)
		return nil, "", ErrInvalidLogin
	}
	okPw, rehash := secret.VerifyPassword(u.PasswordHash, password)
	if !okPw {
		s.limit.Fail(key)
		return nil, "", ErrInvalidLogin
	}
	s.limit.Reset(key)
	if rehash {
		if h, err := secret.HashPassword(password); err == nil {
			uid := u.ID
			go s.store.Update(func(c *config.Config) error {
				if u, ok := c.User(uid); ok {
					u.PasswordHash = jsonString(h)
				}
				return nil
			})
		}
	}
	tok, sess := s.newSession(KindUser, u.ID, "", "", s.sessionTTL())
	p, _ := s.principalFor(sess, c)
	return p, tok, nil
}

// Logout ends the session identified by the cookie token.
func (s *Service) Logout(token string) {
	k := tokenKey(token)
	s.mu.Lock()
	sess := s.sessions[k]
	delete(s.sessions, k)
	s.mu.Unlock()
	if sess != nil {
		s.conns.Close(sess.id)
		s.changed(true)
	}
}

// Bootstrap creates the first admin. Only possible while no users exist and
// only from localhost.
func (s *Service) Bootstrap(username, password string, local bool) (*Principal, string, error) {
	if !local {
		return nil, "", ErrBootstrapLocal
	}
	if !s.NeedsBootstrap() {
		return nil, "", ErrBootstrapDone
	}
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return nil, "", err
	}
	if err := secret.ValidatePasswordPolicy(password); err != nil {
		return nil, "", config.Invalid("%s", err.Error())
	}
	h, err := secret.HashPassword(password)
	if err != nil {
		return nil, "", err
	}
	var uid string
	_, err = s.store.Update(func(c *config.Config) error {
		if len(c.Users) > 0 {
			return ErrBootstrapDone
		}
		uid = config.NewID("u_")
		c.Users = append(c.Users, config.User{ID: uid, Username: username, Role: config.RoleAdmin, Enabled: true, PasswordHash: jsonString(h), APIKeys: []config.APIKey{}, CreatedAt: s.now().UTC().Format(time.RFC3339)})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	tok, sess := s.newSession(KindUser, uid, "", "", s.sessionTTL())
	p, _ := s.principalFor(sess, s.store.Get())
	return p, tok, nil
}

// NeedsBootstrap reports whether no users exist.
func (s *Service) NeedsBootstrap() bool { return len(s.store.Get().Users) == 0 }

// KioskLogin exchanges a kiosk URL key for a kiosk session bound to monitorID.
// All failures return the same neutral error.
func (s *Service) KioskLogin(monitorID, key, clientIP string) (*Principal, string, error) {
	if !s.kioskLimit.Allow(clientIP) {
		return nil, "", ErrKioskDenied
	}
	if monitorID == "" || key == "" || len(key) > 256 {
		s.kioskLimit.Fail(clientIP)
		return nil, "", ErrKioskDenied
	}
	c := s.store.Get()
	var user *config.User
	var match *config.APIKey
	for i := range c.Users {
		u := &c.Users[i]
		for j := range u.APIKeys {
			k := &u.APIKeys[j]
			if secret.VerifyTokenHash(k.TokenHash, key) {
				user, match = u, k
			}
		}
	}
	if match == nil {
		// local displays sign in with their own credential
		for i := range c.Displays {
			d := &c.Displays[i]
			if d.TokenHash == "" || !secret.VerifyTokenHash(d.TokenHash, key) {
				continue
			}
			if !d.Enabled || subtle.ConstantTimeCompare([]byte(d.MonitorID), []byte(monitorID)) != 1 {
				break
			}
			if m, ok := c.Monitor(monitorID); !ok || !m.Enabled {
				break
			}
			tok, sess := s.newSession(KindKiosk, "", config.DisplayKeyPrefix+d.ID, monitorID, s.kioskTTL())
			if p, ok := s.principalFor(sess, c); ok {
				return p, tok, nil
			}
			break
		}
	}
	if match == nil || !match.Enabled || !user.Enabled || subtle.ConstantTimeCompare([]byte(match.MonitorID), []byte(monitorID)) != 1 {
		s.kioskLimit.Fail(clientIP)
		return nil, "", ErrKioskDenied
	}
	if m, ok := c.Monitor(monitorID); !ok || !m.Enabled {
		s.kioskLimit.Fail(clientIP)
		return nil, "", ErrKioskDenied
	}
	tok, sess := s.newSession(KindKiosk, user.ID, match.ID, monitorID, s.kioskTTL())
	p, ok := s.principalFor(sess, c)
	if !ok {
		return nil, "", ErrKioskDenied
	}
	return p, tok, nil
}

// SessionTTL exposes the user session TTL (cookie Max-Age).
func (s *Service) SessionTTL() time.Duration { return s.sessionTTL() }

// KioskTTL exposes the kiosk session TTL (cookie Max-Age).
func (s *Service) KioskTTL() time.Duration { return s.kioskTTL() }

// CheckCSRF validates the CSRF header for session principals.
func CheckCSRF(p *Principal, r *http.Request) bool {
	if p == nil || p.SessionID == "" {
		return true // local anonymous viewer: no cookie authority to abuse
	}
	got := r.Header.Get(CSRFHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(p.CSRF)) == 1
}

// ValidateUsername enforces simple username rules.
func ValidateUsername(u string) error {
	if len(u) < 2 || len(u) > 64 {
		return config.Invalid("Benutzername muss 2–64 Zeichen lang sein")
	}
	for _, r := range u {
		if r < 0x20 || r == '"' || r == '\\' {
			return config.Invalid("Benutzername enthält ungültige Zeichen")
		}
	}
	return nil
}

func jsonString(s string) []byte {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	b = append(b, s...)
	b = append(b, '"')
	return b
}
