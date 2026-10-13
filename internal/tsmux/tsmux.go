// Package tsmux is a minimal MPEG transport stream muxer for one H.264 or
// H.265 video elementary stream. CNM uses it for bridged live streams:
// unlike a raw Annex-B byte stream it carries explicit frame boundaries and
// timestamps (one PES packet per access unit), so consumers such as go2rtc
// never have to guess where a frame ends (multi-slice frames stay intact).
package tsmux

import (
	"bytes"
	"io"
)

// Stream types.
const (
	StreamTypeH264 = 0x1B
	StreamTypeH265 = 0x24
)

const (
	packetSize = 188
	pidPMT     = 0x1000
	pidVideo   = 0x0100
)

// Muxer writes TS packets to w.
type Muxer struct {
	w          io.Writer
	streamType byte
	cc         map[uint16]byte
	buf        bytes.Buffer
}

// New creates a muxer for the given stream type.
func New(w io.Writer, streamType byte) *Muxer {
	return &Muxer{w: w, streamType: streamType, cc: map[uint16]byte{}}
}

var crcTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

func crc32mpeg(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, v := range b {
		crc = crc<<8 ^ crcTable[byte(crc>>24)^v]
	}
	return crc
}

func (m *Muxer) nextCC(pid uint16) byte {
	c := m.cc[pid]
	m.cc[pid] = (c + 1) & 0x0F
	return c
}

func (m *Muxer) writeSection(pid uint16, section []byte) {
	crc := crc32mpeg(section)
	section = append(section, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	pkt := make([]byte, packetSize)
	pkt[0] = 0x47
	pkt[1] = 0x40 | byte(pid>>8)
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | m.nextCC(pid)
	pkt[4] = 0 // pointer field
	n := copy(pkt[5:], section)
	for i := 5 + n; i < packetSize; i++ {
		pkt[i] = 0xFF
	}
	m.buf.Write(pkt)
}

func (m *Muxer) writeTables() {
	// PAT: program 1 → PMT PID
	m.writeSection(0, []byte{
		0x00, 0xB0, 13, // table id, section length
		0x00, 0x01, 0xC1, 0x00, 0x00, // ts id, version/current, section numbers
		0x00, 0x01, 0xE0 | byte(pidPMT>>8), byte(pidPMT & 0xFF),
	})
	// PMT: one video stream, PCR on the video PID
	m.writeSection(pidPMT, []byte{
		0x02, 0xB0, 18,
		0x00, 0x01, 0xC1, 0x00, 0x00,
		0xE0 | byte(pidVideo>>8), byte(pidVideo & 0xFF), // PCR PID
		0xF0, 0x00, // program info length
		m.streamType, 0xE0 | byte(pidVideo>>8), byte(pidVideo & 0xFF), 0xF0, 0x00,
	})
}

func putTS(b []byte, prefix byte, ts uint64) {
	b[0] = prefix<<4 | byte(ts>>29)&0x0E | 1
	b[1] = byte(ts >> 22)
	b[2] = byte(ts>>14) | 1
	b[3] = byte(ts >> 7)
	b[4] = byte(ts<<1) | 1
}

var (
	audH264 = []byte{0, 0, 0, 1, 0x09, 0xF0}
	audH265 = []byte{0, 0, 0, 1, 0x46, 0x01, 0x50}
)

// WriteFrame writes one access unit (Annex-B) with its presentation time in
// 90 kHz units. Tables are repeated before every keyframe.
func (m *Muxer) WriteFrame(au []byte, pts uint64, key bool) error {
	m.buf.Reset()
	if key {
		m.writeTables()
	}
	aud := audH264
	if m.streamType == StreamTypeH265 {
		aud = audH265
	}
	if bytes.HasPrefix(au, aud[:5]) {
		aud = nil
	}
	pts &= 0x1FFFFFFFF
	hdr := make([]byte, 14)
	copy(hdr, []byte{0, 0, 1, 0xE0})
	payloadLen := 3 + 5 + len(aud) + len(au)
	if payloadLen <= 0xFFFF {
		hdr[4], hdr[5] = byte(payloadLen>>8), byte(payloadLen)
	} // else 0 = unbounded (allowed for video)
	hdr[6] = 0x80 // marker bits
	hdr[7] = 0x80 // PTS only
	hdr[8] = 5
	putTS(hdr[9:], 0x2, pts)
	pes := make([]byte, 0, len(hdr)+len(aud)+len(au))
	pes = append(pes, hdr...)
	pes = append(pes, aud...)
	pes = append(pes, au...)

	first := true
	for len(pes) > 0 {
		pkt := make([]byte, packetSize)
		pkt[0] = 0x47
		pkt[1] = byte(pidVideo >> 8)
		if first {
			pkt[1] |= 0x40
		}
		pkt[2] = byte(pidVideo & 0xFF)
		// adaptation field: PCR + random access on the first packet of a keyframe,
		// stuffing on the last packet
		var af []byte
		if first && key {
			pcr := pts - 9000 // 100 ms ahead of presentation
			if pts < 9000 {
				pcr = 0
			}
			af = []byte{0x50, byte(pcr >> 25), byte(pcr >> 17), byte(pcr >> 9), byte(pcr >> 1), byte(pcr<<7) | 0x7E, 0x00}
		}
		room := packetSize - 4
		if af != nil {
			room -= 1 + len(af)
		}
		if len(pes) < room {
			stuff := room - len(pes)
			if af == nil {
				if stuff == 1 {
					af = []byte{} // length byte only
				} else {
					af = append([]byte{0x00}, bytes.Repeat([]byte{0xFF}, stuff-2)...)
				}
			} else {
				af = append(af, bytes.Repeat([]byte{0xFF}, stuff)...)
			}
			room = len(pes)
		}
		if af != nil {
			pkt[3] = 0x30 | m.nextCC(pidVideo)
			pkt[4] = byte(len(af))
			copy(pkt[5:], af)
			copy(pkt[5+len(af):], pes[:room])
		} else {
			pkt[3] = 0x10 | m.nextCC(pidVideo)
			copy(pkt[4:], pes[:room])
		}
		pes = pes[room:]
		m.buf.Write(pkt)
		first = false
	}
	_, err := m.w.Write(m.buf.Bytes())
	return err
}
