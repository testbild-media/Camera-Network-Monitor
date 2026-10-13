package playback

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// readInitSegment reads top level boxes until ftyp and moov have been seen
// and returns their bytes. FFmpeg's fragmented MP4 output starts with them.
func readInitSegment(r *bufio.Reader) ([]byte, error) {
	var out []byte
	seenMoov := false
	for !seenMoov {
		hdr, err := r.Peek(8)
		if err != nil {
			return nil, err
		}
		size := uint64(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])
		hlen := uint64(8)
		if size == 1 {
			big, err := r.Peek(16)
			if err != nil {
				return nil, err
			}
			size = binary.BigEndian.Uint64(big[8:16])
			hlen = 16
		}
		if size < hlen || size > 32<<20 {
			return nil, fmt.Errorf("mp4: invalid box %q size %d", typ, size)
		}
		box := make([]byte, size)
		if _, err := io.ReadFull(r, box); err != nil {
			return nil, err
		}
		out = append(out, box...)
		if typ == "moov" {
			seenMoov = true
		}
		if len(out) > 32<<20 {
			return nil, errors.New("mp4: init segment too large")
		}
	}
	return out, nil
}

// findBox searches a box by path (e.g. "moov/trak/mdia/minf/stbl/stsd") and
// returns its payload. stsd children are handled by codecString.
func findBox(data []byte, path ...string) []byte {
	if len(path) == 0 {
		return data
	}
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		hlen := 8
		if size == 1 && off+16 <= len(data) {
			size = int(binary.BigEndian.Uint64(data[off+8:]))
			hlen = 16
		}
		if size == 0 {
			size = len(data) - off
		}
		if size < hlen || off+size > len(data) {
			return nil
		}
		if typ == path[0] {
			return findBox(data[off+hlen:off+size], path[1:]...)
		}
		off += size
	}
	return nil
}

// CodecString derives the RFC 6381 codec parameter from an init segment,
// e.g. "avc1.64001f" or "hvc1.1.6.L93.B0". It inspects the first video track.
func CodecString(init []byte) (string, error) {
	moov := findBox(init, "moov")
	if moov == nil {
		return "", errors.New("mp4: no moov")
	}
	for off := 0; off+8 <= len(moov); {
		size := int(binary.BigEndian.Uint32(moov[off:]))
		if size < 8 || off+size > len(moov) {
			break
		}
		if string(moov[off+4:off+8]) == "trak" {
			stsd := findBox(moov[off+8:off+size], "mdia", "minf", "stbl", "stsd")
			if c, ok := sampleEntryCodec(stsd); ok {
				return c, nil
			}
		}
		off += size
	}
	return "", errors.New("mp4: no supported video sample entry")
}

func sampleEntryCodec(stsd []byte) (string, bool) {
	// full box header (4) + entry count (4)
	if len(stsd) < 16 {
		return "", false
	}
	entry := stsd[8:]
	size := int(binary.BigEndian.Uint32(entry))
	if size < 8 || size > len(entry) {
		return "", false
	}
	typ := string(entry[4:8])
	// VisualSampleEntry: 8 header + 78 bytes fields
	if size < 86 {
		return "", false
	}
	children := entry[86:size]
	switch typ {
	case "avc1", "avc3":
		avcC := findBox(children, "avcC")
		if len(avcC) < 4 {
			return "", false
		}
		return fmt.Sprintf("%s.%02x%02x%02x", typ, avcC[1], avcC[2], avcC[3]), true
	case "hvc1", "hev1":
		hvcC := findBox(children, "hvcC")
		if len(hvcC) < 13 {
			return "", false
		}
		return hevcCodec(typ, hvcC), true
	}
	return "", false
}

// hevcCodec builds the codec string per ISO/IEC 14496-15 Annex E.
func hevcCodec(typ string, c []byte) string {
	profileSpace := c[1] >> 6
	tier := (c[1] >> 5) & 1
	profileIdc := c[1] & 0x1F
	compat := binary.BigEndian.Uint32(c[2:6])
	// reverse bit order of the compatibility flags
	var rev uint32
	for i := 0; i < 32; i++ {
		if compat&(1<<uint(i)) != 0 {
			rev |= 1 << uint(31-i)
		}
	}
	constraint := c[6:12]
	level := c[12]
	var b strings.Builder
	b.WriteString(typ)
	b.WriteByte('.')
	if profileSpace > 0 {
		b.WriteByte(byte('A' + profileSpace - 1))
	}
	fmt.Fprintf(&b, "%d.%X.", profileIdc, rev)
	if tier == 1 {
		b.WriteByte('H')
	} else {
		b.WriteByte('L')
	}
	fmt.Fprintf(&b, "%d", level)
	// trailing zero constraint bytes are omitted
	last := -1
	for i, v := range constraint {
		if v != 0 {
			last = i
		}
	}
	for i := 0; i <= last; i++ {
		fmt.Fprintf(&b, ".%X", constraint[i])
	}
	if last < 0 {
		b.WriteString(".B0")
	}
	return b.String()
}
