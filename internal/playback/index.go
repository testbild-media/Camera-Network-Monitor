package playback

import (
	"context"
	"sync"
	"time"

	"cnm/internal/provider"
)

// Availability of recordings for a camera.
type Availability string

const (
	AvailYes     Availability = "yes"
	AvailNo      Availability = "no"
	AvailUnknown Availability = "unknown"
)

// RecheckInterval is how often a camera's recording availability is refreshed.
const RecheckInterval = 10 * time.Minute

// listWindow is the look-back for devices with a recording list ("are there
// any recordings at all?"); probeWindow is used by probe-only devices.
const listWindow = 30 * 24 * time.Hour

type availEntry struct {
	state   Availability
	checked time.Time
}

// index tracks per camera whether the device holds recordings, so the UI only
// offers playback where there is something to play (no manual storage setting).
type index struct {
	mu       sync.Mutex
	m        map[string]availEntry
	force    map[string]bool // device ids to recheck immediately
	kick     chan struct{}
	onChange func()
}

func (x *index) init() {
	x.m = map[string]availEntry{}
	x.force = map[string]bool{}
	x.kick = make(chan struct{}, 1)
}

// OnAvailabilityChange registers a callback (e.g. state broadcast).
func (s *Service) OnAvailabilityChange(fn func()) { s.idx.onChange = fn }

// Availability returns whether recordings exist for a camera.
func (s *Service) Availability(cameraID string) Availability {
	s.idx.mu.Lock()
	defer s.idx.mu.Unlock()
	if e, ok := s.idx.m[cameraID]; ok {
		return e.state
	}
	return AvailUnknown
}

// RecheckDevice schedules an immediate availability check for a device
// (called when it (re)connects or its configuration changes).
func (s *Service) RecheckDevice(deviceID string) {
	s.idx.mu.Lock()
	s.idx.force[deviceID] = true
	s.idx.mu.Unlock()
	select {
	case s.idx.kick <- struct{}{}:
	default:
	}
}

// RunIndex keeps the availability index fresh until ctx is cancelled.
func (s *Service) RunIndex(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		s.indexPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.idx.kick:
			// coalesce bursts (startup: several devices connect at once)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (s *Service) setAvail(id string, a Availability) bool {
	s.idx.mu.Lock()
	defer s.idx.mu.Unlock()
	prev, ok := s.idx.m[id]
	s.idx.m[id] = availEntry{state: a, checked: time.Now()}
	return !ok || prev.state != a
}

// indexPass checks due cameras: devices in parallel (max 2), cameras of one
// device sequentially to spare old DVRs.
func (s *Service) indexPass(ctx context.Context) {
	if s.devices == nil {
		return
	}
	c := s.cfg()
	s.idx.mu.Lock()
	force := s.idx.force
	s.idx.force = map[string]bool{}
	s.idx.mu.Unlock()

	type job struct {
		t  *target
		id string
	}
	byDev := map[string][]job{}
	changed := false
	live := map[string]bool{}
	for _, cam := range c.Cameras {
		live[cam.ID] = true
		t, e := s.resolve(cam.ID)
		if e != nil && e.Code == CodeCameraNotFound {
			continue // disabled: no device queries, keep the last state for re-enabling
		}
		if e != nil || !t.caps.Playback.Enabled() {
			if s.setAvail(cam.ID, AvailNo) {
				changed = true
			}
			continue
		}
		st, ok := s.devices.Status(t.dev.ID)
		if !ok || !st.Online() {
			continue // keep the last known state while the device is offline
		}
		if st.Storage == string(provider.StorageAbsent) {
			if s.setAvail(cam.ID, AvailNo) {
				changed = true
			}
			continue
		}
		s.idx.mu.Lock()
		e2, known := s.idx.m[cam.ID]
		s.idx.mu.Unlock()
		if known && e2.state != AvailUnknown && !force[t.dev.ID] && time.Since(e2.checked) < RecheckInterval {
			continue
		}
		byDev[t.dev.ID] = append(byDev[t.dev.ID], job{t: t, id: cam.ID})
	}
	// forget removed cameras
	s.idx.mu.Lock()
	for id := range s.idx.m {
		if !live[id] {
			delete(s.idx.m, id)
		}
	}
	s.idx.mu.Unlock()

	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for devID, jobs := range byDev {
		wg.Add(1)
		go func(devID string, jobs []job) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			for _, j := range jobs {
				if ctx.Err() != nil {
					return
				}
				a := s.probeCamera(ctx, j.t)
				if a == AvailUnknown {
					continue
				}
				if s.setAvail(j.id, a) {
					mu.Lock()
					changed = true
					mu.Unlock()
				}
			}
		}(devID, jobs)
	}
	wg.Wait()
	if changed && s.idx.onChange != nil {
		s.idx.onChange()
	}
}

func (s *Service) probeCamera(ctx context.Context, t *target) Availability {
	drv, _, err := s.devices.Driver(t.dev.ID)
	if err != nil {
		return AvailUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now := time.Now()
	from, to := now.Add(-listWindow), now
	if t.caps.SearchKind == "probe" {
		from, to = now.Add(-15*time.Minute), now.Add(-10*time.Minute)
	}
	if p, ok := drv.(provider.RecordingProber); ok {
		has, err := p.HasRecordings(ctx, t.cam.Channel, from, to)
		if err != nil {
			s.log.Debug("recording probe failed", "camera", t.cam.ID, "error", provider.Redact(err.Error()))
			return AvailUnknown
		}
		return boolAvail(has)
	}
	if sr, ok := drv.(provider.RecordingSearcher); ok && t.caps.RecordingSearch.Enabled() {
		recs, err := sr.SearchRecordings(ctx, t.cam.Channel, from, to)
		if err != nil {
			return AvailUnknown
		}
		return boolAvail(len(recs) > 0)
	}
	// playback without any search: availability cannot be determined
	return AvailUnknown
}

func boolAvail(b bool) Availability {
	if b {
		return AvailYes
	}
	return AvailNo
}
