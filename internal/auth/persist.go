package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Session persistence: user and kiosk sessions survive a service restart
// (update, watchdog restart), so operators watching a monitor are not thrown
// back to the login form. Only the SHA-256 of the cookie token is stored – the
// file cannot be used to log in. It is written atomically with mode 0600,
// right after logins/logouts/revocations and otherwise at most once per sweep.
// A missing or unreadable file only means that users have to log in again.

// SessionsFile is the file name inside the data directory.
const SessionsFile = "sessions.json"

type storedSession struct {
	Key       string    `json:"key"` // sha256(cookie token)
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	UserID    string    `json:"userId"`
	KeyID     string    `json:"keyId,omitempty"`
	MonitorID string    `json:"monitorId,omitempty"`
	CSRF      string    `json:"csrf"`
	TTL       int64     `json:"ttlSeconds"`
	Expires   time.Time `json:"expires"`
}

type sessionFile struct {
	Version  int             `json:"version"`
	Sessions []storedSession `json:"sessions"`
}

func (s *Service) sessionsPath() string {
	if s.store.Dir() == "" {
		return "" // in-memory store (tests)
	}
	return filepath.Join(s.store.Dir(), SessionsFile)
}

// loadSessions restores persisted sessions that are still valid for the
// current configuration.
func (s *Service) loadSessions() {
	p := s.sessionsPath()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var f sessionFile
	if json.Unmarshal(data, &f) != nil {
		return
	}
	now := s.now()
	c := s.store.Get()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range f.Sessions {
		if len(st.Key) != 64 || st.ID == "" || st.CSRF == "" || st.TTL <= 0 || !now.Before(st.Expires) {
			continue
		}
		if st.Kind != KindUser && st.Kind != KindKiosk {
			continue
		}
		sess := &session{
			id: st.ID, kind: st.Kind, userID: st.UserID, keyID: st.KeyID, monitorID: st.MonitorID,
			csrf: st.CSRF, ttl: time.Duration(st.TTL) * time.Second, expires: st.Expires, touched: now,
		}
		if _, ok := s.principalFor(sess, c); !ok {
			continue
		}
		s.sessions[st.Key] = sess
	}
}

// SaveSessions writes the session file if sessions changed since the last save.
func (s *Service) SaveSessions() error {
	p := s.sessionsPath()
	if p == "" {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if !s.dirty.Swap(false) {
		return nil
	}
	s.mu.Lock()
	f := sessionFile{Version: 1, Sessions: make([]storedSession, 0, len(s.sessions))}
	for k, sess := range s.sessions {
		f.Sessions = append(f.Sessions, storedSession{
			Key: k, ID: sess.id, Kind: sess.kind, UserID: sess.userID, KeyID: sess.keyID, MonitorID: sess.monitorID,
			CSRF: sess.csrf, TTL: int64(sess.ttl / time.Second), Expires: sess.expires,
		})
	}
	s.mu.Unlock()
	data, err := json.Marshal(f)
	if err == nil {
		tmp := p + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			if err = os.Rename(tmp, p); err != nil {
				_ = os.Remove(tmp)
			}
		}
	}
	if err != nil {
		s.dirty.Store(true) // retry with the next sweep
	}
	return err
}

// changed marks the session set as modified; important changes (login,
// logout, revocation) are written right away in the background.
func (s *Service) changed(now bool) {
	s.dirty.Store(true)
	if now && s.sessionsPath() != "" {
		go s.SaveSessions()
	}
}
