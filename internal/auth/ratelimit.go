package auth

import (
	"sync"
	"time"
)

// RateLimiter blocks a key after max failures within window.
type RateLimiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]*rlEntry
}

type rlEntry struct {
	fails   int
	first   time.Time
	blocked time.Time // zero = not blocked
}

// NewRateLimiter creates a limiter.
func NewRateLimiter(max int, window time.Duration) *RateLimiter {
	return &RateLimiter{max: max, window: window, now: time.Now, entries: map[string]*rlEntry{}}
}

// Allow reports whether an attempt for key may proceed.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return true
	}
	now := l.now()
	if !e.blocked.IsZero() {
		if now.Sub(e.blocked) < l.window {
			return false
		}
		delete(l.entries, key)
		return true
	}
	if now.Sub(e.first) > l.window {
		delete(l.entries, key)
	}
	return true
}

// Fail records a failed attempt.
func (l *RateLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil || now.Sub(e.first) > l.window {
		e = &rlEntry{first: now}
		l.entries[key] = e
	}
	e.fails++
	if e.fails >= l.max && e.blocked.IsZero() {
		e.blocked = now
	}
}

// Reset clears a key after a successful login.
func (l *RateLimiter) Reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// Sweep removes expired entries so memory does not grow.
func (l *RateLimiter) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, e := range l.entries {
		ref := e.first
		if !e.blocked.IsZero() {
			ref = e.blocked
		}
		if now.Sub(ref) > l.window {
			delete(l.entries, k)
		}
	}
}

// Len returns the number of tracked keys.
func (l *RateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
