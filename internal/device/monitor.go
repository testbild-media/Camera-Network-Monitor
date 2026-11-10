package device

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"cnm/internal/provider"
)

// Recorder monitoring: problems a recorder has although its connection
// works – no or a faulty disk, recording stopped, a wrong clock. Recorders
// that offer device settings are checked an hour after another while they
// are connected; a recorder that silently stopped recording would otherwise
// go unnoticed until someone needs a recording.

const (
	monitorFirst    = 2 * time.Minute
	monitorInterval = time.Hour
	// continuous recording: the newest data is at most this old
	recordingGap = 3 * time.Hour
	clockLimit   = 2 * time.Minute
	wallLayout   = "2006-01-02 15:04:05"
)

// MonitorResult is the outcome of the last check.
type MonitorResult struct {
	CheckedAt time.Time `json:"checkedAt"`
	Problems  []string  `json:"problems,omitempty"`
}

// ErrNoMonitor: the device's protocol offers nothing to check.
var ErrNoMonitor = errors.New("Für dieses Protokoll kann CNM den Rekorder nicht prüfen")

// monitorLoop checks the recorder until ctx (the connection) ends.
func (s *supervisor) monitorLoop(ctx context.Context, drv provider.Driver) {
	if _, ok := drv.(provider.Configurator); !ok {
		return
	}
	wait := monitorFirst
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = monitorInterval
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, err := s.runMonitor(cctx, drv)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.log.Info("recorder check failed", "error", provider.Redact(err.Error()))
		}
	}
}

// Monitor checks a connected recorder now.
func (m *Manager) Monitor(ctx context.Context, deviceID string) (*MonitorResult, error) {
	drv, _, err := m.Driver(deviceID)
	if err != nil {
		return nil, err
	}
	s := m.get(deviceID)
	if s == nil {
		return nil, errors.New("Gerät unbekannt")
	}
	return s.runMonitor(ctx, drv)
}

// runMonitor checks once, stores the result in the status and logs changes.
func (s *supervisor) runMonitor(ctx context.Context, drv provider.Driver) (*MonitorResult, error) {
	s.monMu.Lock()
	defer s.monMu.Unlock()
	res, err := s.check(ctx, drv)
	if err != nil {
		return nil, err
	}
	prev := s.Status().Monitor
	s.update(func(st *Status) { st.Monitor = res })
	switch {
	case len(res.Problems) > 0 && (prev == nil || !slices.Equal(prev.Problems, res.Problems)):
		s.log.Warn("recorder problems", "problems", strings.Join(res.Problems, " | "))
	case len(res.Problems) == 0 && prev != nil && len(prev.Problems) > 0:
		s.log.Info("recorder problems resolved")
	}
	return res, nil
}

// check reads the recorder's settings (disks, clock, recording modes) and
// probes the channels that should record continuously.
func (s *supervisor) check(ctx context.Context, drv provider.Driver) (*MonitorResult, error) {
	c, ok := drv.(provider.Configurator)
	if !ok {
		return nil, ErrNoMonitor
	}
	set, err := c.ReadSettings(ctx)
	if err != nil {
		return nil, err
	}
	res := &MonitorResult{CheckedAt: time.Now()}
	problems, probe := settingsProblems(set)
	res.Problems = problems
	pr, ok := drv.(provider.RecordingProber)
	if !ok || len(probe) == 0 {
		return res, nil
	}
	labels := map[int]string{}
	enabled := map[int]bool{}
	for _, cam := range s.m.store.Get().Cameras {
		if cam.DeviceID == s.id {
			labels[cam.Channel], enabled[cam.Channel] = cam.Label, cam.Enabled
		}
	}
	now := time.Now()
	for _, ch := range probe {
		if !enabled[ch.Channel] {
			continue
		}
		has, err := pr.HasRecordings(ctx, ch.Channel, now.Add(-recordingGap), now)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			continue // unknown, not a finding
		}
		if !has {
			name := labels[ch.Channel]
			if name == "" && ch.Name != nil {
				name = *ch.Name
			}
			if name != "" {
				name = " (" + name + ")"
			}
			res.Problems = append(res.Problems, fmt.Sprintf("Kanal %d%s: keine Aufnahme in den letzten %d Stunden", ch.Channel+1, name, int(recordingGap.Hours())))
		}
	}
	return res, nil
}

// settingsProblems evaluates read settings: clock, disks and whether the
// recorder still records. It also returns the channels whose recordings are
// to be probed – those recording continuously all day while the recorder as
// a whole still records.
func settingsProblems(set *provider.DeviceSettings) (problems []string, probe []provider.ChannelSettings) {
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }
	recording := false
	var continuous []provider.ChannelSettings
	for _, ch := range set.Channels {
		if ch.Record == nil {
			continue
		}
		if ch.Record.Mode != provider.RecordOff {
			recording = true
		}
		if ch.Record.Mode == provider.RecordContinuous && ch.Record.From == "" && (ch.Main == nil || ch.Main.Enabled) {
			continuous = append(continuous, ch)
		}
	}

	var devNow time.Time
	if t := set.Time; t != nil {
		devNow, _ = time.Parse(wallLayout, t.DeviceTime)
		off := time.Duration(t.OffsetSeconds) * time.Second
		switch {
		case off.Abs() > 48*time.Hour:
			add("Die Uhr des Rekorders ist falsch (zeigt %s) – Aufnahmen bekommen ein falsches Datum", t.DeviceTime)
		case off.Abs() > clockLimit:
			dir := "vor"
			if off < 0 {
				dir = "nach"
			}
			add("Die Uhr des Rekorders geht %s %s", fmtSpan(off.Abs()), dir)
		}
	}

	stopped := false
	if set.Disks != nil {
		var total, free int64
		var newest time.Time
		var newestStr string
		for _, d := range set.Disks {
			total += d.TotalBytes
			free += d.FreeBytes
			if d.Error {
				state := d.State
				if state == "" {
					state = "Fehler"
				}
				add("Festplatte „%s“ meldet einen Fehler (%s)", d.Name, state)
			}
			if t, err := time.Parse(wallLayout, d.Newest); err == nil && t.After(newest) {
				newest, newestStr = t, d.Newest
			}
		}
		switch {
		case len(set.Disks) == 0 && recording:
			add("Keine Festplatte erkannt – der Rekorder nimmt nichts auf")
			stopped = true
		case recording && set.Overwrite != nil && !*set.Overwrite && total > 0 && free < total/100 && !anyEstimated(set.Disks):
			add("Die Festplatte ist voll und Überschreiben ist ausgeschaltet – der Rekorder nimmt nichts mehr auf")
			stopped = true
		}
		if len(continuous) > 0 && !newest.IsZero() && !devNow.IsZero() && devNow.Sub(newest) > recordingGap {
			add("Keine neuen Aufnahmen seit %s (neueste Aufnahme %s)", fmtSpan(devNow.Sub(newest)), newestStr)
			stopped = true
		}
	}
	if !stopped {
		probe = continuous
	}
	return problems, probe
}

func anyEstimated(disks []provider.DiskInfo) bool {
	for _, d := range disks {
		if d.Estimated {
			return true
		}
	}
	return false
}

// fmtSpan writes a duration the way people say it.
func fmtSpan(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d Tagen", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d Stunden", int(d.Hours()))
	default:
		return fmt.Sprintf("%d Minuten", int(d.Minutes()))
	}
}
