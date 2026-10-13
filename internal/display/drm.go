package display

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultSysfs is where the kernel exposes DRM connectors.
const DefaultSysfs = "/sys/class/drm"

// Connector is a physical output of a GPU as seen by the kernel.
type Connector struct {
	Name      string `json:"name"`          // e.g. "HDMI-A-1" (same naming as the compositor)
	Card      string `json:"card"`          // e.g. "card0" (numbering may change between boots)
	PCI       string `json:"pci,omitempty"` // stable PCI address of the GPU, e.g. "0000:01:00.0"
	Connected bool   `json:"connected"`
	EDID      *EDID  `json:"edid,omitempty"`
	Modes     []Mode `json:"modes"`
}

var connRe = regexp.MustCompile(`^(card\d+)-(.+)$`)

// Scan lists the DRM connectors below root (DefaultSysfs when empty).
// Writeback connectors are skipped. A missing directory (no GPU, not Linux)
// yields an empty list.
func Scan(root string) ([]Connector, error) {
	if root == "" {
		root = DefaultSysfs
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Connector{}, nil
		}
		return nil, err
	}
	out := []Connector{}
	for _, e := range entries {
		m := connRe.FindStringSubmatch(e.Name())
		if m == nil || strings.HasPrefix(m[2], "Writeback") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		c := Connector{Card: m[1], Name: m[2], Modes: []Mode{}, PCI: cardPCI(filepath.Join(root, m[1]))}
		c.Connected = strings.TrimSpace(readSmall(filepath.Join(dir, "status"))) == "connected"
		if raw, err := os.ReadFile(filepath.Join(dir, "edid")); err == nil && len(raw) >= 128 {
			if ed, err := ParseEDID(raw); err == nil {
				c.EDID = ed
				c.Modes = append(c.Modes, ed.Modes...)
			}
		}
		// the kernel's mode list may contain modes the EDID parser does not
		// know (e.g. CVT timings); they are offered without refresh rate
		for _, line := range strings.Split(readSmall(filepath.Join(dir, "modes")), "\n") {
			mode, ok := ParseMode(strings.TrimSpace(line))
			if !ok || mode.Width == 0 {
				continue
			}
			if _, have := Best(c.Modes, mode); !have {
				c.Modes = append(c.Modes, mode)
			}
		}
		if c.EDID != nil {
			c.EDID.Modes = nil // exposed once, via c.Modes
		}
		c.Modes = SortModes(c.Modes)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Card != out[j].Card {
			return cardNum(out[i].Card) < cardNum(out[j].Card)
		}
		return naturalLess(out[i].Name, out[j].Name)
	})
	return out, nil
}

// cardPCI returns the PCI address of a DRM card ("" if not a PCI device).
func cardPCI(cardDir string) string {
	real, err := filepath.EvalSymlinks(filepath.Join(cardDir, "device"))
	if err != nil {
		return ""
	}
	return filepath.Base(real)
}

// NormalizePCI accepts "0000:01:00.0", "01:00.0" or "pci-0000:01:00.0".
func NormalizePCI(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(strings.ToLower(s)), "pci-")
	if strings.Count(s, ":") == 1 {
		s = "0000:" + s
	}
	return s
}

func readSmall(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n, _ := f.Read(buf)
	return string(buf[:n])
}

func cardNum(card string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(card, "card"))
	return n
}

var numRe = regexp.MustCompile(`\d+|\D+`)

// naturalLess compares "HDMI-A-2" < "HDMI-A-10".
func naturalLess(a, b string) bool {
	pa, pb := numRe.FindAllString(a, -1), numRe.FindAllString(b, -1)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea == nil && eb == nil {
			return na < nb
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}

// ValidConnectorName reports whether s is a plausible DRM connector name.
func ValidConnectorName(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
