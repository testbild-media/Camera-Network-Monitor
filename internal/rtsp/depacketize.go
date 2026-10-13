package rtsp

import (
	"encoding/binary"
	"time"
)

// AccessUnit is one video frame in Annex-B form (4 byte start codes).
type AccessUnit struct {
	Data []byte
	// Key is set for IDR (H.264) and IRAP (H.265) access units. Parameter
	// sets are always present in front of a keyframe.
	Key bool
	// PTS90k is the unwrapped RTP timestamp in 90 kHz units; the first
	// access unit has 0 (later ones may be negative with B-frames).
	PTS90k int64
	// NTP is the recording time from the ONVIF replay header extension
	// (0xABAC); zero when the server does not send it.
	NTP   time.Time
	Codec Codec
}

// ONVIF replay RTP header extension (ONVIF Streaming Specification §6.3).
const onvifExtProfile = 0xABAC

// NTP epoch (1900) to Unix epoch (1970) in seconds.
const ntpUnixOffset = 2208988800

// maxAUSize protects against unbounded growth on broken streams.
const maxAUSize = 16 << 20

type rtpHeader struct {
	marker bool
	pt     int
	seq    uint16
	ts     uint32
	ntp    time.Time
}

// parseRTP splits an RTP packet into header fields and payload.
func parseRTP(pkt []byte) (rtpHeader, []byte, bool) {
	var h rtpHeader
	if len(pkt) < 12 || pkt[0]>>6 != 2 {
		return h, nil, false
	}
	h.marker = pkt[1]&0x80 != 0
	h.pt = int(pkt[1] & 0x7F)
	h.seq = binary.BigEndian.Uint16(pkt[2:])
	h.ts = binary.BigEndian.Uint32(pkt[4:])
	off := 12 + 4*int(pkt[0]&0x0F)
	end := len(pkt)
	if pkt[0]&0x20 != 0 { // padding
		if end == 0 || int(pkt[end-1]) > end-off {
			return h, nil, false
		}
		end -= int(pkt[end-1])
	}
	if pkt[0]&0x10 != 0 { // header extension
		if off+4 > end {
			return h, nil, false
		}
		profile := binary.BigEndian.Uint16(pkt[off:])
		n := 4 * int(binary.BigEndian.Uint16(pkt[off+2:]))
		ext := pkt[off+4:]
		off += 4 + n
		if off > end {
			return h, nil, false
		}
		if profile == onvifExtProfile && n >= 8 {
			h.ntp = ntpTime(binary.BigEndian.Uint32(ext), binary.BigEndian.Uint32(ext[4:]))
		}
	}
	if off > end {
		return h, nil, false
	}
	return h, pkt[off:end], true
}

// ntpTime converts a 64 bit NTP timestamp. Values with the most significant
// bit clear belong to era 1 (after February 2036, RFC 4330 §3).
func ntpTime(sec, frac uint32) time.Time {
	if sec == 0 && frac == 0 {
		return time.Time{}
	}
	s := int64(sec)
	if sec&0x80000000 == 0 {
		s += 1 << 32
	}
	ns := int64((uint64(frac) * 1e9) >> 32)
	return time.Unix(s-ntpUnixOffset, ns).UTC()
}

// depacketizer turns RTP packets (RFC 6184 / RFC 7798) into access units.
type depacketizer struct {
	codec Codec
	pt    int
	clock int64

	vps, sps, pps []byte // latest parameter sets (SDP or in-band)

	// access unit in progress
	have   bool
	ts     uint32
	ntp    time.Time
	nals   [][]byte
	size   int
	broken bool

	// fragmentation unit in progress
	fu   []byte
	fuOn bool

	seq   uint16
	seqOK bool

	// timestamp unwrapping
	lastTS uint32
	ext    int64
	tsOK   bool
}

func newDepacketizer(t Track) *depacketizer {
	d := &depacketizer{codec: t.Codec, pt: t.PayloadType, clock: int64(t.ClockRate)}
	if d.clock <= 0 {
		d.clock = 90000
	}
	for _, p := range t.ParamSets {
		d.learn(p)
	}
	return d
}

func (d *depacketizer) nalType(n []byte) byte {
	if d.codec == H265 {
		return n[0] >> 1 & 0x3F
	}
	return n[0] & 0x1F
}

// learn remembers parameter sets.
func (d *depacketizer) learn(n []byte) {
	if len(n) == 0 {
		return
	}
	c := append([]byte(nil), n...)
	switch t := d.nalType(n); {
	case d.codec == H264 && t == 7, d.codec == H265 && t == 33:
		d.sps = c
	case d.codec == H264 && t == 8, d.codec == H265 && t == 34:
		d.pps = c
	case d.codec == H265 && t == 32:
		d.vps = c
	}
}

// push processes one RTP packet; emit is called for every completed access unit.
func (d *depacketizer) push(pkt []byte, emit func(AccessUnit) error) error {
	h, payload, ok := parseRTP(pkt)
	if !ok || (d.pt >= 0 && h.pt != d.pt) {
		return nil
	}
	if d.seqOK && h.seq != d.seq+1 {
		// packet loss (cannot happen over TCP unless the server drops):
		// the frame in progress is damaged
		d.fuOn, d.fu = false, nil
		d.broken = d.have
	}
	d.seq, d.seqOK = h.seq, true
	if d.have && h.ts != d.ts {
		if err := d.flush(emit); err != nil {
			return err
		}
	}
	if !d.have {
		d.have, d.ts, d.ntp = true, h.ts, time.Time{}
	}
	if d.ntp.IsZero() && !h.ntp.IsZero() {
		d.ntp = h.ntp
	}
	if len(payload) > 0 {
		if d.codec == H265 {
			d.h265(payload)
		} else {
			d.h264(payload)
		}
	}
	if h.marker {
		return d.flush(emit)
	}
	return nil
}

func (d *depacketizer) add(n []byte) {
	if len(n) == 0 {
		return
	}
	d.size += len(n) + 4
	if d.size > maxAUSize {
		d.broken = true
		return
	}
	d.nals = append(d.nals, n)
}

// h264 handles single NAL units, STAP-A and FU-A (RFC 6184).
func (d *depacketizer) h264(p []byte) {
	switch t := p[0] & 0x1F; {
	case t >= 1 && t <= 23:
		d.add(append([]byte(nil), p...))
	case t == 24: // STAP-A
		d.aggregate(p[1:])
	case t == 28: // FU-A
		if len(p) < 2 {
			return
		}
		hdr := p[1]
		if hdr&0x80 != 0 {
			d.fu = append(d.fu[:0:0], p[0]&0xE0|hdr&0x1F)
			d.fuOn = true
		}
		if !d.fuOn {
			d.broken = true // continuation without start: NAL unit lost
			return
		}
		d.fu = append(d.fu, p[2:]...)
		if len(d.fu) > maxAUSize {
			d.fuOn, d.fu, d.broken = false, nil, true
			return
		}
		if hdr&0x40 != 0 {
			d.add(d.fu)
			d.fu, d.fuOn = nil, false
		}
	}
	// STAP-B, MTAP and FU-B (interleaved mode) are not used by cameras
}

// h265 handles single NAL units, aggregation packets and fragmentation
// units (RFC 7798, without DONL fields).
func (d *depacketizer) h265(p []byte) {
	if len(p) < 2 {
		return
	}
	switch t := p[0] >> 1 & 0x3F; {
	case t < 48:
		d.add(append([]byte(nil), p...))
	case t == 48: // AP
		d.aggregate(p[2:])
	case t == 49: // FU
		if len(p) < 3 {
			return
		}
		hdr := p[2]
		if hdr&0x80 != 0 {
			d.fu = append(d.fu[:0:0], p[0]&0x81|(hdr&0x3F)<<1, p[1])
			d.fuOn = true
		}
		if !d.fuOn {
			d.broken = true // continuation without start: NAL unit lost
			return
		}
		d.fu = append(d.fu, p[3:]...)
		if len(d.fu) > maxAUSize {
			d.fuOn, d.fu, d.broken = false, nil, true
			return
		}
		if hdr&0x40 != 0 {
			d.add(d.fu)
			d.fu, d.fuOn = nil, false
		}
	}
}

// aggregate splits STAP-A / AP payloads: 16 bit size + NAL unit, repeated.
func (d *depacketizer) aggregate(p []byte) {
	for len(p) >= 2 {
		n := int(binary.BigEndian.Uint16(p))
		p = p[2:]
		if n == 0 || n > len(p) {
			return
		}
		d.add(append([]byte(nil), p[:n]...))
		p = p[n:]
	}
}

// flush completes the access unit in progress.
func (d *depacketizer) flush(emit func(AccessUnit) error) error {
	if !d.have {
		return nil
	}
	nals, ts, ntp, broken := d.nals, d.ts, d.ntp, d.broken
	d.have, d.nals, d.size, d.broken = false, nil, 0, false
	d.fu, d.fuOn = nil, false
	pts := d.unwrap(ts)
	if broken || len(nals) == 0 {
		return nil
	}
	au, ok := d.build(nals)
	if !ok {
		return nil
	}
	au.PTS90k, au.NTP = pts, ntp
	return emit(au)
}

func (d *depacketizer) unwrap(ts uint32) int64 {
	if !d.tsOK {
		d.tsOK, d.lastTS = true, ts
		return 0
	}
	d.ext += int64(int32(ts - d.lastTS))
	d.lastTS = ts
	if d.clock == 90000 {
		return d.ext
	}
	return d.ext * 90000 / d.clock
}

// build assembles an Annex-B access unit. Units without a coded slice (only
// parameter sets or SEI) are not emitted; their parameter sets are kept.
func (d *depacketizer) build(nals [][]byte) (AccessUnit, bool) {
	au := AccessUnit{Codec: d.codec}
	var hasVPS, hasSPS, hasPPS, vcl bool
	for _, n := range nals {
		t := d.nalType(n)
		if d.codec == H265 {
			switch {
			case t == 32:
				hasVPS = true
			case t == 33:
				hasSPS = true
			case t == 34:
				hasPPS = true
			case t <= 31:
				vcl = true
				if t >= 16 && t <= 21 {
					au.Key = true
				}
			}
		} else {
			switch {
			case t == 7:
				hasSPS = true
			case t == 8:
				hasPPS = true
			case t >= 1 && t <= 5:
				vcl = true
				if t == 5 {
					au.Key = true
				}
			}
		}
		d.learn(n)
	}
	if !vcl {
		return au, false
	}
	var prefix [][]byte
	if au.Key {
		if d.codec == H265 && !hasVPS && d.vps != nil {
			prefix = append(prefix, d.vps)
		}
		if !hasSPS && d.sps != nil {
			prefix = append(prefix, d.sps)
		}
		if !hasPPS && d.pps != nil {
			prefix = append(prefix, d.pps)
		}
	}
	size := 0
	for _, n := range nals {
		size += 4 + len(n)
	}
	for _, n := range prefix {
		size += 4 + len(n)
	}
	out := make([]byte, 0, size)
	put := func(n []byte) { out = append(append(out, 0, 0, 0, 1), n...) }
	rest := nals
	// an access unit delimiter must stay the first NAL unit
	if t := d.nalType(nals[0]); (d.codec == H264 && t == 9) || (d.codec == H265 && t == 35) {
		put(nals[0])
		rest = nals[1:]
	}
	for _, n := range prefix {
		put(n)
	}
	for _, n := range rest {
		put(n)
	}
	au.Data = out
	return au, true
}
