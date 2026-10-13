package provider

import (
	"sort"
	"sync"
	"time"
)

// Clock maps absolute instants to a device's local wall clock. DVRs store
// recordings in naive local time; the offset (device wall clock interpreted
// as UTC minus real UTC) captures both the time zone and clock drift. Without
// a measured offset the server's local time zone is assumed.
type Clock struct {
	mu     sync.RWMutex
	offset *time.Duration
}

// Set records a measured offset.
func (c *Clock) Set(d time.Duration) {
	c.mu.Lock()
	c.offset = &d
	c.mu.Unlock()
}

// Offset returns the measured offset, if any.
func (c *Clock) Offset() (time.Duration, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.offset == nil {
		return 0, false
	}
	return *c.offset, true
}

// MeasureOffset computes the offset from a device wall clock reading. Device
// clocks report whole seconds (truncated), so the true reading lies half a
// second later on average.
func MeasureOffset(deviceWall time.Time, now time.Time) time.Duration {
	wall := time.Date(deviceWall.Year(), deviceWall.Month(), deviceWall.Day(), deviceWall.Hour(), deviceWall.Minute(), deviceWall.Second(), 0, time.UTC)
	return wall.Add(500 * time.Millisecond).Sub(now.UTC()).Round(time.Second)
}

// ToDevice returns the device wall clock for an instant. The returned time's
// fields (year … second) are the device's local fields; its location is UTC.
func (c *Clock) ToDevice(t time.Time) time.Time {
	if off, ok := c.Offset(); ok {
		return t.UTC().Add(off)
	}
	l := t.In(time.Local)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), time.UTC)
}

// FromDevice converts device wall clock fields back to an absolute instant.
func (c *Clock) FromDevice(wall time.Time) time.Time {
	w := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), time.UTC)
	if off, ok := c.Offset(); ok {
		return w.Add(-off)
	}
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.Local)
}

// MergeRecordings sorts, clips to [from,to] and merges ranges whose gap is
// at most maxGap.
func MergeRecordings(in []Recording, from, to time.Time, maxGap time.Duration) []Recording {
	var rs []Recording
	for _, r := range in {
		if !r.End.After(r.Start) {
			continue
		}
		if r.Start.Before(from) {
			r.Start = from
		}
		if r.End.After(to) {
			r.End = to
		}
		if r.End.After(r.Start) {
			rs = append(rs, r)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Start.Before(rs[j].Start) })
	var out []Recording
	for _, r := range rs {
		if n := len(out); n > 0 && !r.Start.After(out[n-1].End.Add(maxGap)) {
			if r.End.After(out[n-1].End) {
				out[n-1].End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out
}
