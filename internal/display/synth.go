package display

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SynthMonitor describes a display for SyntheticEDID.
type SynthMonitor struct {
	Vendor  string // 3 letters
	Product uint16
	Serial  uint32
	Name    string
	// Preferred timing (clock in kHz, totals in pixels/lines).
	W, H, Clock, HTotal, VTotal int
	VICs                        []int // CTA-861 video codes (adds an extension block)
}

// SyntheticEDID builds a valid EDID 1.4 (plus CTA-861 extension when VICs
// are given). Used by tests and the device simulator's demo sysfs tree.
func SyntheticEDID(m SynthMonitor) []byte {
	ext := 0
	if len(m.VICs) > 0 {
		ext = 1
	}
	b := make([]byte, 128*(1+ext))
	copy(b, edidHeader)
	v := strings.ToUpper(m.Vendor + "AAA")[:3]
	id := uint16(v[0]-'A'+1)<<10 | uint16(v[1]-'A'+1)<<5 | uint16(v[2]-'A'+1)
	b[8], b[9] = byte(id>>8), byte(id)
	b[10], b[11] = byte(m.Product), byte(m.Product>>8)
	b[12], b[13], b[14], b[15] = byte(m.Serial), byte(m.Serial>>8), byte(m.Serial>>16), byte(m.Serial>>24)
	b[18], b[19] = 1, 4
	b[35], b[36] = 0x21, 0x08 // 640x480@60, 800x600@60, 1024x768@60
	for i := 38; i < 54; i++ {
		b[i] = 0x01
	}
	b[38], b[39] = byte(1280/8-31), 0x80 // 1280x1024@60 (5:4)
	// descriptor 1: preferred detailed timing
	d := b[54:72]
	pc := m.Clock / 10
	d[0], d[1] = byte(pc), byte(pc>>8)
	hb, vb := m.HTotal-m.W, m.VTotal-m.H
	d[2], d[3], d[4] = byte(m.W), byte(hb), byte((m.W>>8)<<4|(hb>>8)&0x0f)
	d[5], d[6], d[7] = byte(m.H), byte(vb), byte((m.H>>8)<<4|(vb>>8)&0x0f)
	d[17] = 0x1e
	// descriptor 2: monitor name, 3: serial text, 4: dummy
	text := func(off int, tag byte, s string) {
		x := b[off : off+18]
		x[3] = tag
		t := []byte(s)
		if len(t) > 13 {
			t = t[:13]
		}
		copy(x[5:], t)
		if len(t) < 13 {
			x[5+len(t)] = 0x0a
			for i := 6 + len(t); i < 18; i++ {
				x[i] = 0x20
			}
		}
	}
	text(72, 0xfc, m.Name)
	text(90, 0xff, fmt.Sprintf("%08d", m.Serial))
	b[108+3] = 0x10
	b[126] = byte(ext)
	b[127] = checksum(b[:127])
	if ext == 1 {
		e := b[128:256]
		e[0], e[1] = 0x02, 0x03
		e[4] = 2<<5 | byte(len(m.VICs))
		for i, vic := range m.VICs {
			e[5+i] = byte(vic)
		}
		e[2] = byte(5 + len(m.VICs))
		e[127] = checksum(e[:127])
	}
	return b
}

func checksum(b []byte) byte {
	var s byte
	for _, x := range b {
		s += x
	}
	return -s
}

// WriteSysfsFixture creates a fake /sys/class/drm tree (demo and tests):
// connected monitors get status, edid and modes files.
func WriteSysfsFixture(root string, card string, connectors map[string]*SynthMonitor) error {
	for name, mon := range connectors {
		dir := filepath.Join(root, card+"-"+name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		status, edid, modes := "disconnected\n", []byte{}, ""
		if mon != nil {
			status = "connected\n"
			edid = SyntheticEDID(*mon)
			if e, err := ParseEDID(edid); err == nil {
				for _, m := range e.Modes {
					modes += fmt.Sprintf("%dx%d\n", m.Width, m.Height)
				}
			}
		}
		for f, data := range map[string][]byte{"status": []byte(status), "edid": edid, "modes": []byte(modes), "enabled": []byte("disabled\n")} {
			if err := os.WriteFile(filepath.Join(dir, f), data, 0o644); err != nil {
				return err
			}
		}
	}
	return os.MkdirAll(filepath.Join(root, card), 0o755)
}
