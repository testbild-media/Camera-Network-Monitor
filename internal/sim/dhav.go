package sim

import (
	"bytes"
	"encoding/binary"
	"time"
)

// dhTime packs wall clock fields into the 32 bit DHTIME format used by DHAV.
func dhTime(t time.Time) uint32 {
	return uint32(t.Second()&0x3F) | uint32(t.Minute()&0x3F)<<6 | uint32(t.Hour()&0x1F)<<12 |
		uint32(t.Day()&0x1F)<<17 | uint32(int(t.Month())&0x0F)<<22 | uint32((t.Year()-2000)&0x3F)<<26
}

// DHAV wraps H.264 access units into a Dahua DHAV stream as found in DVR
// recordings (layout as parsed by FFmpeg's dhav demuxer):
//
//	"DHAV" type(1) pad(3) seq(4) frameLen(4) date(4) ms(2) extLen(1) sum(1) ext… payload "dhav" frameLen(4)
func DHAV(clip *Clip, start time.Time, frames int) []byte {
	var out bytes.Buffer
	frameDur := time.Second / time.Duration(clip.FPS)
	for i := 0; i < frames; i++ {
		au := clip.AUs[i%len(clip.AUs)]
		ts := start.Add(time.Duration(i) * frameDur)
		typ := byte(0xFD)
		if au.Key {
			typ = 0xFC
		}
		// extension: 0x80 size (w/8, h/8), 0x81 codec 0x02 (H.264) + frame rate
		ext := []byte{0x80, 0, byte(clip.W / 8), byte(clip.H / 8), 0x81, 0, 0x02, byte(clip.FPS)}
		total := 24 + len(ext) + len(au.Data) + 8
		h := make([]byte, 24)
		copy(h, "DHAV")
		h[4] = typ
		binary.LittleEndian.PutUint32(h[8:], uint32(i))
		binary.LittleEndian.PutUint32(h[12:], uint32(total))
		binary.LittleEndian.PutUint32(h[16:], dhTime(ts))
		binary.LittleEndian.PutUint16(h[20:], uint16(ts.UnixMilli()))
		h[22] = byte(len(ext))
		var sum byte
		for _, b := range h[:23] {
			sum += b
		}
		h[23] = sum
		out.Write(h)
		out.Write(ext)
		out.Write(au.Data)
		tr := make([]byte, 8)
		copy(tr, "dhav")
		binary.LittleEndian.PutUint32(tr[4:], uint32(total))
		out.Write(tr)
	}
	return out.Bytes()
}
