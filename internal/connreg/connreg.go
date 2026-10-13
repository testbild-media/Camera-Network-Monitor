// Package connreg tracks long-lived connections (media WebSockets, playback
// streams, control WebSockets) per session so they can be terminated
// immediately on logout, kiosk key revocation or shutdown.
package connreg

import "sync"

// Registry maps a session key to close functions.
type Registry struct {
	mu   sync.Mutex
	next uint64
	m    map[string]map[uint64]func()
}

// New creates an empty registry.
func New() *Registry { return &Registry{m: map[string]map[uint64]func(){}} }

// Add registers closeFn under key and returns a function that unregisters it.
// closeFn must be idempotent and non-blocking.
func (r *Registry) Add(key string, closeFn func()) (remove func()) {
	r.mu.Lock()
	r.next++
	id := r.next
	if r.m[key] == nil {
		r.m[key] = map[uint64]func(){}
	}
	r.m[key][id] = closeFn
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if set := r.m[key]; set != nil {
			delete(set, id)
			if len(set) == 0 {
				delete(r.m, key)
			}
		}
		r.mu.Unlock()
	}
}

// Close terminates every connection registered under key.
func (r *Registry) Close(key string) int {
	r.mu.Lock()
	set := r.m[key]
	delete(r.m, key)
	r.mu.Unlock()
	for _, fn := range set {
		fn()
	}
	return len(set)
}

// CloseAll terminates every registered connection.
func (r *Registry) CloseAll() int {
	r.mu.Lock()
	all := r.m
	r.m = map[string]map[uint64]func(){}
	r.mu.Unlock()
	n := 0
	for _, set := range all {
		for _, fn := range set {
			fn()
			n++
		}
	}
	return n
}

// Count returns the number of registered connections.
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, set := range r.m {
		n += len(set)
	}
	return n
}

// Keys returns the keys that currently have registered connections.
func (r *Registry) Keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	return out
}
