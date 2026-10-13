package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/connreg"
	"cnm/internal/secret"
)

func newSvc(t *testing.T) (*Service, *config.Store, *connreg.Registry) {
	c := config.Default(t.TempDir())
	h, _ := secret.HashPassword("test-admin-pw")
	c.Users = []config.User{{ID: "u1", Username: "admin", Role: config.RoleAdmin, Enabled: true, PasswordHash: []byte(`"` + h + `"`), APIKeys: []config.APIKey{}}}
	store := config.NewMemoryStore(c)
	keys, _ := secret.NewKeyring(secret.GenerateKey())
	conns := connreg.New()
	return NewService(store, keys, conns), store, conns
}

func TestSessionExpiryAndSweep(t *testing.T) {
	s, _, conns := newSvc(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	p, tok, err := s.Login("admin", "test-admin-pw", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	conns.Add(p.SessionID, func() { closed = true })
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("Cookie", SessionCookie+"="+tok)
	if got, err := s.Resolve(r); err != nil || got.Role != config.RoleAdmin {
		t.Fatalf("resolve: %v %v", got, err)
	}
	// sliding expiry: activity within the TTL keeps the session alive
	now = now.Add(11 * time.Hour)
	if _, err := s.Resolve(r); err != nil {
		t.Fatal("session expired too early")
	}
	now = now.Add(13 * time.Hour)
	s.Sweep()
	if s.SessionCount() != 0 {
		t.Fatal("expired session not swept from memory")
	}
	if !closed {
		t.Fatal("connections of an expired session must be closed")
	}
	if _, err := s.Resolve(r); err == nil {
		t.Fatal("expired session still valid")
	}
}

func TestRateLimiterWindowAndSweep(t *testing.T) {
	l := NewRateLimiter(5, 10*time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if !l.Allow("k") {
			t.Fatalf("attempt %d blocked too early", i)
		}
		l.Fail("k")
	}
	if l.Allow("k") {
		t.Fatal("6th attempt must be blocked")
	}
	now = now.Add(11 * time.Minute)
	if !l.Allow("k") {
		t.Fatal("block must expire after the window")
	}
	l.Fail("other")
	now = now.Add(11 * time.Minute)
	l.Sweep()
	if l.Len() != 0 {
		t.Fatalf("stale entries not removed: %d", l.Len())
	}
	// success resets the counter
	l.Fail("x")
	l.Reset("x")
	if l.Len() != 0 {
		t.Fatal("reset did not clear the key")
	}
}

func TestDisablingUserEndsSessionsImmediately(t *testing.T) {
	s, store, conns := newSvc(t)
	store.Update(func(c *config.Config) error {
		h, _ := secret.HashPassword("viewer-pass")
		c.Users = append(c.Users, config.User{ID: "u2", Username: "v", Role: config.RoleViewer, Enabled: true, PasswordHash: []byte(`"` + h + `"`), APIKeys: []config.APIKey{}})
		return nil
	})
	p, _, err := s.Login("v", "viewer-pass", "192.0.2.2")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	conns.Add(p.SessionID, func() { close(closed) })
	store.Update(func(c *config.Config) error { c.Users[1].Enabled = false; return nil })
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("connections of a disabled user were not closed")
	}
	if n := s.SessionCount(); n != 0 {
		t.Fatalf("session of the disabled user still stored (%d sessions)", n)
	}
}

func TestIsLocalIgnoresProxiedRequests(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	if !IsLocal(r) {
		t.Fatal("loopback must be local")
	}
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	if IsLocal(r) {
		t.Fatal("proxied request must not count as local")
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "[::1]:5555"
	if !IsLocal(r2) {
		t.Fatal("IPv6 loopback must be local")
	}
}

func TestSessionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	store, _, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := secret.HashPassword("test-admin-pw")
	if _, err := store.Update(func(c *config.Config) error {
		c.Users = []config.User{{ID: "u1", Username: "admin", Role: config.RoleAdmin, Enabled: true, PasswordHash: []byte(`"` + h + `"`), APIKeys: []config.APIKey{}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	keys, _ := secret.NewKeyring(secret.GenerateKey())
	s1 := NewService(store, keys, connreg.New())
	p, tok, err := s1.Login("admin", "test-admin-pw", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SaveSessions(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, SessionsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), tok) {
		t.Fatal("cookie token stored in clear text")
	}
	if fi, _ := os.Stat(filepath.Join(dir, SessionsFile)); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("sessions file mode %v", fi.Mode().Perm())
	}
	req := func() *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("Cookie", SessionCookie+"="+tok)
		return r
	}
	// "restart": a new service on the same data directory
	s2 := NewService(store, keys, connreg.New())
	got, err := s2.Resolve(req())
	if err != nil || got.UserID != "u1" || got.CSRF != p.CSRF || got.SessionID != p.SessionID {
		t.Fatalf("session not restored: %+v %v", got, err)
	}
	s2.Logout(tok)
	if err := s2.SaveSessions(); err != nil {
		t.Fatal(err)
	}
	s3 := NewService(store, keys, connreg.New())
	if _, err := s3.Resolve(req()); err == nil {
		t.Fatal("logged out session resurrected after restart")
	}
	// a corrupt file is ignored (users simply log in again)
	if err := os.WriteFile(filepath.Join(dir, SessionsFile), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s4 := NewService(store, keys, connreg.New()); s4.SessionCount() != 0 {
		t.Fatal("corrupt session file must be ignored")
	}
}
