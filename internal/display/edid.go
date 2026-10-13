package display

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// EDID describes a connected display as far as CNM needs it.
type EDID struct {
	Vendor  string `json:"vendor,omitempty"`  // PNP id (three letters)
	Product uint16 `json:"product,omitempty"` // product code
	Serial  string `json:"serial,omitempty"`  // serial string descriptor or number
	Name    string `json:"name,omitempty"`    // monitor name descriptor
	Modes   []Mode `json:"modes"`             // sorted, preferred flagged
}

var edidHeader = []byte{0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00}

// timing is a raw video timing (clock in kHz).
type timing struct {
	w, h, clock, htotal, vtotal int
}

func (t timing) mode() Mode {
	return Mode{Width: t.w, Height: t.h, RefreshMHz: refreshMHz(t.clock, t.htotal, t.vtotal)}
}

// dmt holds VESA DMT timings for established/standard timings (by nominal Hz).
var dmt = map[[3]int]timing{
	{640, 480, 60}:   {640, 480, 25175, 800, 525},
	{640, 480, 67}:   {640, 480, 30240, 864, 525},
	{640, 480, 72}:   {640, 480, 31500, 832, 520},
	{640, 480, 75}:   {640, 480, 31500, 840, 500},
	{720, 400, 70}:   {720, 400, 28322, 900, 449},
	{720, 400, 88}:   {720, 400, 35500, 900, 449},
	{800, 600, 56}:   {800, 600, 36000, 1024, 625},
	{800, 600, 60}:   {800, 600, 40000, 1056, 628},
	{800, 600, 72}:   {800, 600, 50000, 1040, 666},
	{800, 600, 75}:   {800, 600, 49500, 1056, 625},
	{832, 624, 75}:   {832, 624, 57284, 1152, 667},
	{1024, 768, 60}:  {1024, 768, 65000, 1344, 806},
	{1024, 768, 70}:  {1024, 768, 75000, 1328, 806},
	{1024, 768, 75}:  {1024, 768, 78750, 1312, 800},
	{1152, 864, 75}:  {1152, 864, 108000, 1600, 900},
	{1152, 870, 75}:  {1152, 870, 100000, 1456, 915},
	{1280, 720, 60}:  {1280, 720, 74250, 1650, 750},
	{1280, 800, 60}:  {1280, 800, 83500, 1680, 831},
	{1280, 960, 60}:  {1280, 960, 108000, 1800, 1000},
	{1280, 1024, 60}: {1280, 1024, 108000, 1688, 1066},
	{1280, 1024, 75}: {1280, 1024, 135000, 1688, 1066},
	{1366, 768, 60}:  {1366, 768, 85500, 1792, 798},
	{1400, 1050, 60}: {1400, 1050, 121750, 1864, 1089},
	{1440, 900, 60}:  {1440, 900, 106500, 1904, 934},
	{1600, 900, 60}:  {1600, 900, 108000, 1800, 1000},
	{1600, 1200, 60}: {1600, 1200, 162000, 2160, 1250},
	{1680, 1050, 60}: {1680, 1050, 146250, 2240, 1089},
	{1920, 1080, 60}: {1920, 1080, 148500, 2200, 1125},
	{1920, 1200, 60}: {1920, 1200, 154000, 2080, 1235},
}

// established timings (bytes 35–37), bit 7 of byte 35 first; 0 = interlaced/unused
var established = [17][3]int{
	{720, 400, 70}, {720, 400, 88}, {640, 480, 60}, {640, 480, 67},
	{640, 480, 72}, {640, 480, 75}, {800, 600, 56}, {800, 600, 60},
	{800, 600, 72}, {800, 600, 75}, {832, 624, 75}, {0, 0, 0}, // 1024x768@87i
	{1024, 768, 60}, {1024, 768, 70}, {1024, 768, 75}, {1280, 1024, 75},
	{1152, 870, 75},
}

// cea holds progressive CTA-861 VIC timings.
var cea = map[int]timing{
	1:   {640, 480, 25175, 800, 525},
	2:   {720, 480, 27000, 858, 525},
	3:   {720, 480, 27000, 858, 525},
	4:   {1280, 720, 74250, 1650, 750},
	16:  {1920, 1080, 148500, 2200, 1125},
	17:  {720, 576, 27000, 864, 625},
	18:  {720, 576, 27000, 864, 625},
	19:  {1280, 720, 74250, 1980, 750},
	31:  {1920, 1080, 148500, 2640, 1125},
	32:  {1920, 1080, 74250, 2750, 1125},
	33:  {1920, 1080, 74250, 2640, 1125},
	34:  {1920, 1080, 74250, 2200, 1125},
	60:  {1280, 720, 59400, 3300, 750},
	61:  {1280, 720, 74250, 3960, 750},
	62:  {1280, 720, 74250, 3300, 750},
	63:  {1920, 1080, 297000, 2200, 1125},
	64:  {1920, 1080, 297000, 2640, 1125},
	93:  {3840, 2160, 297000, 5500, 2250},
	94:  {3840, 2160, 297000, 5280, 2250},
	95:  {3840, 2160, 297000, 4400, 2250},
	96:  {3840, 2160, 594000, 5280, 2250},
	97:  {3840, 2160, 594000, 4400, 2250},
	98:  {4096, 2160, 297000, 5500, 2250},
	99:  {4096, 2160, 297000, 5280, 2250},
	100: {4096, 2160, 297000, 4400, 2250},
	101: {4096, 2160, 594000, 5280, 2250},
	102: {4096, 2160, 594000, 4400, 2250},
}

// ParseEDID decodes an EDID blob (base block plus CTA-861 and DisplayID
// extensions). Checksum errors are tolerated – the kernel already validated
// what it exposes – but the header must be present.
func ParseEDID(b []byte) (*EDID, error) {
	if len(b) < 128 || !bytes.Equal(b[:8], edidHeader) {
		return nil, errors.New("keine gültige EDID")
	}
	e := &EDID{}
	v := uint16(b[8])<<8 | uint16(b[9])
	e.Vendor = string([]byte{byte('A' - 1 + (v>>10)&0x1f), byte('A' - 1 + (v>>5)&0x1f), byte('A' - 1 + v&0x1f)})
	e.Product = uint16(b[10]) | uint16(b[11])<<8
	if sn := uint32(b[12]) | uint32(b[13])<<8 | uint32(b[14])<<16 | uint32(b[15])<<24; sn != 0 {
		e.Serial = fmt.Sprint(sn)
	}
	var modes []Mode
	hasPreferred := false
	add := func(m Mode, preferred bool) {
		m.Preferred = preferred && !hasPreferred
		hasPreferred = hasPreferred || m.Preferred
		modes = append(modes, m)
	}
	// detailed timings / descriptors – the first detailed timing is preferred
	first := true
	for i := 0; i < 4; i++ {
		d := b[54+18*i : 54+18*(i+1)]
		if d[0] != 0 || d[1] != 0 {
			if t, ok := detailedTiming(d); ok {
				add(t.mode(), first)
			}
			first = false
			continue
		}
		text := descriptorText(d[5:18])
		switch d[3] {
		case 0xfc:
			e.Name = text
		case 0xff:
			if text != "" {
				e.Serial = text
			}
		}
	}
	// established timings
	bits := uint32(b[35])<<16 | uint32(b[36])<<8 | uint32(b[37])
	for i, et := range established {
		if et[0] != 0 && bits&(1<<(23-uint(i))) != 0 {
			add(nominal(et), false)
		}
	}
	// standard timings
	ver := int(b[18])*10 + int(b[19])
	for i := 0; i < 8; i++ {
		x, y := b[38+2*i], b[39+2*i]
		if x == 0x01 && y == 0x01 || x == 0 {
			continue
		}
		w := (int(x) + 31) * 8
		var h int
		switch y >> 6 {
		case 0:
			if ver < 13 {
				h = w
			} else {
				h = w * 10 / 16
			}
		case 1:
			h = w * 3 / 4
		case 2:
			h = w * 4 / 5
		default:
			h = w * 9 / 16
		}
		add(nominal([3]int{w, h, int(y&0x3f) + 60}), false)
	}
	// extensions
	for n := 1; n <= int(b[126]) && len(b) >= 128*(n+1); n++ {
		ext := b[128*n : 128*(n+1)]
		switch ext[0] {
		case 0x02:
			for _, m := range parseCTA(ext) {
				add(m, false)
			}
		case 0x70:
			for _, m := range parseDisplayID(ext) {
				add(m, m.Preferred)
			}
		}
	}
	e.Modes = SortModes(modes)
	return e, nil
}

// nominal maps a (w, h, Hz) triple onto exact DMT timing when known.
func nominal(k [3]int) Mode {
	if t, ok := dmt[k]; ok {
		return t.mode()
	}
	return Mode{Width: k[0], Height: k[1], RefreshMHz: k[2] * 1000}
}

func detailedTiming(d []byte) (timing, bool) {
	clock := (int(d[0]) | int(d[1])<<8) * 10
	ha := int(d[2]) | int(d[4]&0xf0)<<4
	hb := int(d[3]) | int(d[4]&0x0f)<<8
	va := int(d[5]) | int(d[7]&0xf0)<<4
	vb := int(d[6]) | int(d[7]&0x0f)<<8
	if d[17]&0x80 != 0 || ha == 0 || va == 0 {
		return timing{}, false // interlaced modes are not offered
	}
	return timing{ha, va, clock, ha + hb, va + vb}, true
}

func descriptorText(b []byte) string {
	if i := bytes.IndexByte(b, 0x0a); i >= 0 {
		b = b[:i]
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, string(b))
	return strings.TrimSpace(s)
}

func parseCTA(ext []byte) []Mode {
	var out []Mode
	d := int(ext[2])
	if d < 4 || d > 127 {
		d = 4
	}
	for i := 4; i < d; {
		tag, l := ext[i]>>5, int(ext[i]&0x1f)
		if i+1+l > d {
			break
		}
		if tag == 2 { // video data block
			for _, svd := range ext[i+1 : i+1+l] {
				vic := int(svd)
				if vic >= 129 && vic <= 192 {
					vic &= 0x7f
				}
				t, ok := cea[vic]
				if !ok {
					continue
				}
				out = append(out, t.mode())
				// the kernel also offers the 1000/1001 variant of 24/30/60/120 Hz modes
				if hz := (t.mode().RefreshMHz + 500) / 1000; hz%6 == 0 {
					alt := t
					alt.clock = (t.clock*1000 + 500) / 1001
					out = append(out, alt.mode())
				}
			}
		}
		i += 1 + l
	}
	for i := d; i+18 <= 127; i += 18 {
		if ext[i] == 0 && ext[i+1] == 0 {
			break
		}
		if t, ok := detailedTiming(ext[i : i+18]); ok {
			out = append(out, t.mode())
		}
	}
	return out
}

// parseDisplayID reads type I (0x03, 10 kHz) and type VII (0x22, 1 kHz)
// detailed timings from a DisplayID extension block.
func parseDisplayID(ext []byte) []Mode {
	var out []Mode
	if len(ext) < 5 {
		return nil
	}
	end := 5 + int(ext[2])
	if end > len(ext)-1 {
		end = len(ext) - 1
	}
	for i := 5; i+3 <= end; {
		tag, l := ext[i], int(ext[i+2])
		if l == 0 && tag == 0 {
			break
		}
		payload := ext[i+3 : min(i+3+l, end)]
		if tag == 0x03 || tag == 0x22 {
			unit := 10
			if tag == 0x22 {
				unit = 1
			}
			for j := 0; j+20 <= len(payload); j += 20 {
				t := payload[j : j+20]
				clock := (int(t[0]) | int(t[1])<<8 | int(t[2])<<16 + 1) * unit
				if t[3]&0x10 != 0 {
					continue // interlaced
				}
				ha := int(t[4]) | int(t[5])<<8 + 1
				hb := int(t[6]) | int(t[7])<<8 + 1
				va := int(t[12]) | int(t[13])<<8 + 1
				vb := int(t[14]) | int(t[15])<<8 + 1
				m := timing{ha, va, clock, ha + hb, va + vb}.mode()
				m.Preferred = t[3]&0x80 != 0
				out = append(out, m)
			}
		}
		i += 3 + l
	}
	return out
}
