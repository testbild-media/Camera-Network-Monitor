package server

import (
	"encoding/json"
	"fmt"

	"cnm/internal/config"
	"cnm/internal/media"
	"cnm/internal/playback"
	"cnm/internal/provider"
)

// PublicCamera is the browser view of a camera (no host, no credentials).
type PublicCamera struct {
	ID          string `json:"id"`
	DeviceID    string `json:"deviceId"`
	DeviceLabel string `json:"deviceLabel"`
	Channel     int    `json:"channel"`
	Label       string `json:"label,omitempty"`      // configured camera name
	DeviceName  string `json:"deviceName,omitempty"` // name reported by the device
	Enabled     bool   `json:"enabled"`
	Multiview   string `json:"multiview,omitempty"`
	Focus       string `json:"focus,omitempty"`
	// Alternate streams the player switches to when the primary one delivers
	// no picture (multiview: main, single view: sub).
	MultiviewAlt string `json:"multiviewAlt,omitempty"`
	FocusAlt     string `json:"focusAlt,omitempty"`
	Aspect       string `json:"aspectRatio"`
	Fit          string `json:"objectFit"`
	// Playback is only offered when the device supports it AND holds
	// recordings for this camera – and never in kiosk sessions.
	Playback bool `json:"playback"`
}

// PublicState is the structural state pushed to browsers.
type PublicState struct {
	Rev      uint64           `json:"rev"`
	System   map[string]any   `json:"system"`
	Media    map[string]any   `json:"media"`
	Monitors []config.Monitor `json:"monitors"`
	Cameras  []PublicCamera   `json:"cameras"`
}

func cameraStreams(c *config.Config) map[string]*media.CameraStreams { return media.Cameras(c) }

func (s *Server) cameraList(c *config.Config, scope string) []PublicCamera {
	streams := cameraStreams(c)
	var allowed map[string]bool
	if scope != "" {
		allowed = map[string]bool{}
		if m, ok := c.Monitor(scope); ok && m.Enabled {
			for _, id := range m.CameraIDs() {
				allowed[id] = true
			}
		}
	}
	type devInfo struct {
		label string
		names map[int]string
		caps  provider.Capabilities
	}
	devs := map[string]*devInfo{}
	for _, d := range c.Devices {
		di := &devInfo{label: d.Label}
		if st, ok := s.Devices.Status(d.ID); ok && st.Info != nil {
			di.names = st.Info.ChannelNames
		}
		if p, model, err := provider.Resolve(d.Protocol); err == nil {
			di.caps = provider.DeviceCapabilities(p, model, d.Variant())
		}
		devs[d.ID] = di
	}
	out := []PublicCamera{}
	for _, cam := range c.Cameras {
		if allowed != nil && !allowed[cam.ID] {
			continue
		}
		di := devs[cam.DeviceID]
		if di == nil {
			continue
		}
		pc := PublicCamera{
			ID: cam.ID, DeviceID: cam.DeviceID, DeviceLabel: di.label, Channel: cam.Channel,
			Label: cam.Label, DeviceName: di.names[cam.Channel], Enabled: cam.Enabled,
			Aspect: cam.View.AspectRatio, Fit: cam.View.ObjectFit,
		}
		if scope == "" && cam.Enabled && di.caps.Playback.Enabled() && s.Playback != nil {
			pc.Playback = s.Playback.Availability(cam.ID) == playback.AvailYes
		}
		if cs := streams[cam.ID]; cs != nil && cam.Enabled {
			pc.Multiview, pc.Focus = cs.Multiview, cs.Focus
			if cs.MultiviewAlt == provider.Main && cs.Focus != cs.Multiview {
				pc.MultiviewAlt = cs.Focus
			}
			if cs.FocusAlt == provider.Sub && cs.Focus != cs.Multiview && cs.MultiviewKind == provider.Sub {
				pc.FocusAlt = cs.Multiview
			} else if cs.FocusAlt == provider.Sub && scope == "" && cs.Preview != "" && cs.Preview != cs.Focus {
				pc.FocusAlt = cs.Preview // sub-only stream (users only)
			}
		}
		out = append(out, pc)
	}
	return out
}

// monitorList returns the enabled monitors visible in a scope.
func monitorList(c *config.Config, scope string) []config.Monitor {
	out := []config.Monitor{}
	for _, m := range c.Monitors {
		if !m.Enabled || (scope != "" && m.ID != scope) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) publicState(scope string) PublicState {
	c := s.Store.Get()
	return PublicState{
		Rev:      s.Store.Revision(),
		System:   map[string]any{"name": c.System.Name, "version": s.Version},
		Media:    map[string]any{"enabled": c.System.Media.Enabled, "allowWebRtc": c.System.Media.AllowWebRTC && scope == ""},
		Monitors: monitorList(c, scope),
		Cameras:  s.cameraList(c, scope),
	}
}

func (s *Server) renderState(scope string) []byte {
	b, err := json.Marshal(s.publicState(scope))
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	return b
}

// mediaSummary returns the media status for a principal scope. Kiosks only
// get the coarse state, never error details.
func (s *Server) mediaSummary(kiosk bool) media.Status {
	st := s.Recon.Status()
	if kiosk {
		st.Errors = nil
		st.Version = ""
	}
	return st
}
