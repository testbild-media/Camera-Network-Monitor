// Package media manages the live media path: it derives the desired go2rtc
// stream set from the configuration, reconciles go2rtc towards it, keeps
// multiview producers warm (preload) and reports verified media health.
//
// DVR credentials only ever appear in go2rtc source URLs inside this package
// and the go2rtc API calls; browsers only learn the stable stream names.
package media

import (
	"fmt"
	"sort"
	"strings"

	"cnm/internal/config"
	"cnm/internal/provider"
)

// Prefix of all streams owned by CNM.
const Prefix = "cnm_"

// DeviceKeys returns the sanitised media key for every device id. Collisions
// (e.g. "DVR1" and "dvr1") are resolved deterministically with a suffix.
func DeviceKeys(c *config.Config) map[string]string {
	ids := make([]string, 0, len(c.Devices))
	for _, d := range c.Devices {
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	used := map[string]bool{}
	out := map[string]string{}
	for _, id := range ids {
		k := config.SanitizeID(id)
		if config.IsUUID(id) {
			// keep stream names short and readable; collisions get a suffix
			k = strings.ToLower(id[:8])
		}
		base := k
		for n := 2; used[k]; n++ {
			k = fmt.Sprintf("%s_%d", base, n)
		}
		used[k] = true
		out[id] = k
	}
	return out
}

// StreamName builds "cnm_<key>_chNN_<main|sub>" (NN = one based channel).
func StreamName(deviceKey string, channel int, kind provider.StreamKind) string {
	return fmt.Sprintf("%s%s_ch%02d_%s", Prefix, deviceKey, channel+1, kind)
}

// CameraStreams describes which streams a camera uses.
type CameraStreams struct {
	CameraID      string
	DeviceID      string
	Channel       int
	Enabled       bool
	Multiview     string              // stream name used in multiview tiles ("" = none)
	MultiviewKind provider.StreamKind //
	Focus         string              // stream name used in the focus view ("" = none)
	FocusKind     provider.StreamKind
	Fallback      bool // multiview stream has main as second source
	// MultiviewAlt / FocusAlt: the other stream kind registered as second
	// source of the stream (go2rtc uses it when the first one cannot be
	// opened or has no codec the browser plays); "" = none.
	MultiviewAlt provider.StreamKind
	FocusAlt     provider.StreamKind
	// Preview is a sub-only stream for the camera settings (never falls
	// back to the main stream, never preloaded; "" = camera has no sub).
	Preview string
}

// PreviewName is the sub-only settings preview stream of a camera whose
// multiview stream is not a pure sub stream.
func PreviewName(deviceKey string, channel int) string {
	return fmt.Sprintf("%s%s_ch%02d_preview", Prefix, deviceKey, channel+1)
}

// Cameras derives stream names for every camera from the configuration only.
func Cameras(c *config.Config) map[string]*CameraStreams {
	keys := DeviceKeys(c)
	out := map[string]*CameraStreams{}
	for _, cam := range c.Cameras {
		if _, ok := c.Device(cam.DeviceID); !ok {
			continue
		}
		cs := &CameraStreams{
			CameraID: cam.ID,
			DeviceID: cam.DeviceID,
			Channel:  cam.Channel,
			Enabled:  cam.Enabled,
		}
		out[cs.CameraID] = cs
		if !cam.Enabled {
			continue
		}
		mainOK, subOK := cam.Streams.Main, cam.Streams.Sub
		// automatic: multiview = sub (main when there is no sub or it delivers
		// nothing), single view = main (sub when there is no main or it
		// delivers nothing)
		var mv, fk provider.StreamKind
		switch {
		case subOK:
			mv = provider.Sub
		case mainOK:
			mv = provider.Main
		}
		switch {
		case mainOK:
			fk = provider.Main
		case subOK:
			fk = provider.Sub
		}
		if mainOK && subOK {
			cs.Fallback = true
			cs.MultiviewAlt = provider.Main
			cs.FocusAlt = provider.Sub
		}
		if mv != "" {
			cs.MultiviewKind = mv
			cs.Multiview = StreamName(keys[cam.DeviceID], cam.Channel, mv)
		}
		if fk != "" {
			cs.FocusKind = fk
			cs.Focus = StreamName(keys[cam.DeviceID], cam.Channel, fk)
		}
		if subOK {
			if mv == provider.Sub && cs.MultiviewAlt == "" {
				cs.Preview = cs.Multiview
			} else {
				cs.Preview = PreviewName(keys[cam.DeviceID], cam.Channel)
			}
		}
	}
	return out
}

// MonitorCameras returns the set of camera ids assigned to enabled monitors.
func MonitorCameras(c *config.Config) map[string]bool {
	out := map[string]bool{}
	for _, m := range c.Monitors {
		if !m.Enabled {
			continue
		}
		for _, id := range m.CameraIDs() {
			out[id] = true
		}
	}
	return out
}

// AllowedStreams returns the stream names a scope may open. monitorID ""
// means unrestricted (all enabled cameras).
func AllowedStreams(c *config.Config, monitorID string) map[string]bool {
	cams := Cameras(c)
	out := map[string]bool{}
	add := func(cs *CameraStreams) {
		if cs == nil || !cs.Enabled {
			return
		}
		if cs.Multiview != "" {
			out[cs.Multiview] = true
		}
		if cs.Focus != "" {
			out[cs.Focus] = true
		}
	}
	if monitorID == "" {
		for _, cs := range cams {
			add(cs)
			if cs.Enabled && cs.Preview != "" {
				out[cs.Preview] = true // settings preview (users only, never kiosks)
			}
		}
		return out
	}
	m, ok := c.Monitor(monitorID)
	if !ok || !m.Enabled {
		return out
	}
	for _, id := range m.CameraIDs() {
		add(cams[id])
	}
	return out
}

// DesiredStream is a go2rtc stream CNM wants to exist.
type DesiredStream struct {
	Name     string
	DeviceID string
	Channel  int
	Kind     provider.StreamKind
	Sources  []string // contain credentials – never log or expose
	Preload  bool
}

// fingerprint of the sources (normalised, credentials included – internal only).
func (d *DesiredStream) fingerprint() string {
	n := make([]string, len(d.Sources))
	for i, s := range d.Sources {
		n[i] = normalizeSource(s)
	}
	return strings.Join(n, "\n")
}

// normalizeSource drops the "#..." go2rtc option fragment, which some
// producers do not echo back.
func normalizeSource(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i]
	}
	return s
}

// SourceFunc yields go2rtc sources for a device channel stream.
type SourceFunc func(deviceID string, channel int, kind provider.StreamKind) []string

// Desired computes the desired stream set. Streams without sources (device
// locked after an auth failure, detection pending) are omitted on purpose so
// go2rtc does not keep hammering a DVR with bad credentials.
func Desired(c *config.Config, sources SourceFunc, online func(deviceID string) bool) (map[string]*DesiredStream, int) {
	out := map[string]*DesiredStream{}
	expected := 0
	if !c.System.Media.Enabled {
		return out, 0
	}
	inMon := MonitorCameras(c)
	for _, cs := range Cameras(c) {
		if !cs.Enabled {
			continue
		}
		d, _ := c.Device(cs.DeviceID)
		if d == nil {
			continue
		}
		add := func(name string, kind, alt provider.StreamKind, preload bool) {
			if name == "" {
				return
			}
			if ex, ok := out[name]; ok {
				ex.Preload = ex.Preload || preload
				return
			}
			expected++
			src := sources(cs.DeviceID, cs.Channel, kind)
			if alt != "" && alt != kind {
				src = append(append([]string{}, src...), sources(cs.DeviceID, cs.Channel, alt)...)
			}
			if len(src) == 0 {
				return
			}
			out[name] = &DesiredStream{Name: name, DeviceID: cs.DeviceID, Channel: cs.Channel, Kind: kind, Sources: src, Preload: preload}
		}
		warm := inMon[cs.CameraID] && (online == nil || online(cs.DeviceID))
		add(cs.Multiview, cs.MultiviewKind, cs.MultiviewAlt, warm && c.System.Media.PreloadMultiview)
		add(cs.Focus, cs.FocusKind, cs.FocusAlt, warm && c.System.Media.PreloadMain && cs.Focus != cs.Multiview)
		if cs.Preview != "" && cs.Preview != cs.Multiview {
			// only a definition: go2rtc connects when the settings preview opens
			add(cs.Preview, provider.Sub, "", false)
		}
	}
	return out, expected
}
