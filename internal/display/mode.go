// Package display covers CNM's local display output: the server's own GPU
// outputs (DRM connectors) show multiviewers as kiosks.
//
//   - drm.go / edid.go: connector and EDID mode detection from sysfs (CNM side,
//     read only, works inside the hardened service)
//   - protocol.go:      the JSON contract between CNM and the display agent
//   - host.go:          CNM side of the agent socket (desired state, status)
//   - agent/:           the display agent (compositor + kiosk browsers)
package display

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

// Mode is a video mode. RefreshMHz is the vertical refresh in mHz as computed
// by the kernel/wlroots (0 = unknown, "highest available").
type Mode struct {
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	RefreshMHz int  `json:"refresh"`
	Preferred  bool `json:"preferred,omitempty"`
}

// String renders the canonical config form "1920x1080@60.000" ("1920x1080"
// without known refresh).
func (m Mode) String() string {
	if m.Width <= 0 || m.Height <= 0 {
		return ""
	}
	if m.RefreshMHz <= 0 {
		return fmt.Sprintf("%dx%d", m.Width, m.Height)
	}
	return fmt.Sprintf("%dx%d@%d.%03d", m.Width, m.Height, m.RefreshMHz/1000, m.RefreshMHz%1000)
}

// Label is the human readable form ("1920 × 1080 @ 60 Hz").
func (m Mode) Label() string {
	if m.RefreshMHz <= 0 {
		return fmt.Sprintf("%d × %d", m.Width, m.Height)
	}
	hz := strconv.FormatFloat(float64(m.RefreshMHz)/1000, 'f', 2, 64)
	for len(hz) > 0 && hz[len(hz)-1] == '0' {
		hz = hz[:len(hz)-1]
	}
	if hz[len(hz)-1] == '.' {
		hz = hz[:len(hz)-1]
	}
	return fmt.Sprintf("%d × %d @ %s Hz", m.Width, m.Height, hz)
}

var modeRe = regexp.MustCompile(`^(\d{2,5})x(\d{2,5})(?:@(\d{1,3})(?:\.(\d{1,3}))?(?:Hz)?)?$`)

// ParseMode parses "1920x1080", "1920x1080@60", "1920x1080@59.940" or
// "1920x1080@60Hz". The empty string is valid and means "preferred".
func ParseMode(s string) (Mode, bool) {
	if s == "" {
		return Mode{}, true
	}
	m := modeRe.FindStringSubmatch(s)
	if m == nil {
		return Mode{}, false
	}
	w, _ := strconv.Atoi(m[1])
	h, _ := strconv.Atoi(m[2])
	if w < 64 || h < 64 || w > 16384 || h > 16384 {
		return Mode{}, false
	}
	mode := Mode{Width: w, Height: h}
	if m[3] != "" {
		hz, _ := strconv.Atoi(m[3])
		frac := m[4]
		for len(frac) < 3 {
			frac += "0"
		}
		f, _ := strconv.Atoi(frac)
		mode.RefreshMHz = hz*1000 + f
		if mode.RefreshMHz <= 0 || mode.RefreshMHz > 1000000 {
			return Mode{}, false
		}
	}
	return mode, true
}

// refreshMHz computes the refresh rate exactly like the kernel/wlroots
// (clock in kHz) so that modes derived from EDID match compositor modes.
func refreshMHz(clockKHz, htotal, vtotal int) int {
	if htotal <= 0 || vtotal <= 0 {
		return 0
	}
	return int((int64(clockKHz)*1000000/int64(htotal) + int64(vtotal)/2) / int64(vtotal))
}

// Best returns the mode of list that matches want: same size and nearest
// refresh (want.RefreshMHz == 0 → highest refresh). ok is false when no mode
// has the requested size.
func Best(list []Mode, want Mode) (Mode, bool) {
	var best Mode
	found := false
	for _, m := range list {
		if m.Width != want.Width || m.Height != want.Height {
			continue
		}
		if !found {
			best, found = m, true
			continue
		}
		if want.RefreshMHz <= 0 {
			if m.RefreshMHz > best.RefreshMHz {
				best = m
			}
			continue
		}
		if abs(m.RefreshMHz-want.RefreshMHz) < abs(best.RefreshMHz-want.RefreshMHz) {
			best = m
		}
	}
	return best, found
}

// Largest returns the preferred mode if flagged, else the biggest mode
// (area, then refresh).
func Largest(list []Mode) (Mode, bool) {
	for _, m := range list {
		if m.Preferred {
			return m, true
		}
	}
	if len(list) == 0 {
		return Mode{}, false
	}
	best := list[0]
	for _, m := range list[1:] {
		a, b := m.Width*m.Height, best.Width*best.Height
		if a > b || a == b && m.RefreshMHz > best.RefreshMHz {
			best = m
		}
	}
	return best, true
}

// SortModes orders modes by size (largest first), then refresh (highest
// first) and removes duplicates; a preferred flag survives deduplication.
func SortModes(list []Mode) []Mode {
	type key struct{ w, h, r int }
	seen := map[key]int{}
	out := make([]Mode, 0, len(list))
	for _, m := range list {
		if m.Width <= 0 || m.Height <= 0 {
			continue
		}
		k := key{m.Width, m.Height, m.RefreshMHz}
		if i, ok := seen[k]; ok {
			out[i].Preferred = out[i].Preferred || m.Preferred
			continue
		}
		seen[k] = len(out)
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Width*a.Height != b.Width*b.Height {
			return a.Width*a.Height > b.Width*b.Height
		}
		if a.Width != b.Width {
			return a.Width > b.Width
		}
		return a.RefreshMHz > b.RefreshMHz
	})
	return out
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
