package reolink

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// flvFrame is one video access unit in Annex-B form.
type flvFrame struct {
	data []byte
	ms   int64 // presentation time in milliseconds (FLV timestamp + composition offset)
	key  bool
	hevc bool
}

// flvReader demuxes the video of an FLV stream: H.264 (codec 7), H.265 as
// codec 12 (the common extension) or as "enhanced FLV" (FourCC hvc1/avc1).
// Parameter sets from the sequence header are put in front of every
// keyframe that does not carry its own.
type flvReader struct {
	r       *bufio.Reader
	started bool
	nalLen  int
	params  [][]byte // VPS/SPS/PPS without start codes
	hevc    bool
	tsHigh  int64 // extension of 32 bit timestamps
	lastTS  uint32
}

func newFLVReader(r io.Reader) *flvReader {
	return &flvReader{r: bufio.NewReaderSize(r, 256<<10), nalLen: 4}
}

var errNotFLV = errors.New("keine FLV-Daten")

func (f *flvReader) header() error {
	var h [9]byte
	if _, err := io.ReadFull(f.r, h[:]); err != nil {
		return err
	}
	if string(h[:3]) != "FLV" {
		return errNotFLV
	}
	skip := int(binary.BigEndian.Uint32(h[5:9])) - 9 + 4 // rest of the header + PreviousTagSize0
	if skip < 4 || skip > 1<<16 {
		return errNotFLV
	}
	_, err := f.r.Discard(skip)
	return err
}

// next returns the next video frame (io.EOF at the end of the stream).
func (f *flvReader) next() (*flvFrame, error) {
	if !f.started {
		if err := f.header(); err != nil {
			return nil, err
		}
		f.started = true
	}
	for {
		var th [11]byte
		if _, err := io.ReadFull(f.r, th[:]); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		typ := th[0] & 0x1F
		size := int(th[1])<<16 | int(th[2])<<8 | int(th[3])
		ts := uint32(th[4])<<16 | uint32(th[5])<<8 | uint32(th[6]) | uint32(th[7])<<24
		if size > 32<<20 {
			return nil, fmt.Errorf("FLV-Tag zu groß (%d Bytes)", size)
		}
		body := make([]byte, size+4) // + PreviousTagSize
		if _, err := io.ReadFull(f.r, body); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		body = body[:size]
		if typ != 9 || len(body) < 2 {
			continue // audio, script data
		}
		if ts < f.lastTS && f.lastTS-ts > 1<<31 {
			f.tsHigh += 1 << 32
		}
		f.lastTS = ts
		fr, err := f.video(body, int64(ts)+f.tsHigh)
		if err != nil {
			return nil, err
		}
		if fr != nil {
			return fr, nil
		}
	}
}

// video handles one video tag body; nil = no frame (sequence header, end).
func (f *flvReader) video(b []byte, ts int64) (*flvFrame, error) {
	first := b[0]
	var key bool
	var packetType byte
	var cts int64
	var payload []byte
	if first&0x80 != 0 { // enhanced FLV
		if len(b) < 5 {
			return nil, nil
		}
		key = (first>>4)&0x07 == 1
		packetType = first & 0x0F
		switch string(b[1:5]) {
		case "hvc1":
			f.hevc = true
		case "avc1":
			f.hevc = false
		default:
			return nil, nil // other codecs
		}
		switch packetType {
		case 0: // sequence start
			return nil, f.config(b[5:])
		case 1: // coded frames with composition time
			if len(b) < 8 {
				return nil, nil
			}
			cts = int64(int32(uint32(b[5])<<16|uint32(b[6])<<8|uint32(b[7])) << 8 >> 8)
			payload = b[8:]
		case 3: // coded frames without composition time
			payload = b[5:]
		default:
			return nil, nil
		}
	} else {
		codec := first & 0x0F
		switch codec {
		case 7:
			f.hevc = false
		case 12:
			f.hevc = true
		default:
			return nil, nil
		}
		key = first>>4 == 1
		if len(b) < 5 {
			return nil, nil
		}
		packetType = b[1]
		cts = int64(int32(uint32(b[2])<<16|uint32(b[3])<<8|uint32(b[4])) << 8 >> 8)
		switch packetType {
		case 0:
			return nil, f.config(b[5:])
		case 1:
			payload = b[5:]
		default:
			return nil, nil
		}
	}
	nals := f.split(payload)
	if len(nals) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	if key && !f.hasParams(nals) {
		for _, p := range f.params {
			out.Write([]byte{0, 0, 0, 1})
			out.Write(p)
		}
	}
	for _, n := range nals {
		out.Write([]byte{0, 0, 0, 1})
		out.Write(n)
	}
	return &flvFrame{data: out.Bytes(), ms: ts + cts, key: key, hevc: f.hevc}, nil
}

// split cuts length-prefixed NAL units.
func (f *flvReader) split(b []byte) [][]byte {
	var out [][]byte
	for len(b) >= f.nalLen {
		n := 0
		for i := 0; i < f.nalLen; i++ {
			n = n<<8 | int(b[i])
		}
		b = b[f.nalLen:]
		if n <= 0 || n > len(b) {
			break
		}
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

func (f *flvReader) hasParams(nals [][]byte) bool {
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		if f.hevc {
			if t := (n[0] >> 1) & 0x3F; t == 33 || t == 32 {
				return true
			}
		} else if n[0]&0x1F == 7 {
			return true
		}
	}
	return false
}

// config parses an AVC or HEVC decoder configuration record.
func (f *flvReader) config(b []byte) error {
	var params [][]byte
	read := func(p []byte) ([]byte, []byte, bool) {
		if len(p) < 2 {
			return nil, nil, false
		}
		n := int(binary.BigEndian.Uint16(p))
		if len(p) < 2+n {
			return nil, nil, false
		}
		return p[2 : 2+n], p[2+n:], true
	}
	if f.hevc {
		if len(b) < 23 {
			return nil
		}
		f.nalLen = int(b[21]&0x03) + 1
		arrays := int(b[22])
		p := b[23:]
		for i := 0; i < arrays && len(p) >= 3; i++ {
			count := int(binary.BigEndian.Uint16(p[1:3]))
			p = p[3:]
			for j := 0; j < count; j++ {
				var nal []byte
				var ok bool
				if nal, p, ok = read(p); !ok {
					break
				}
				params = append(params, append([]byte(nil), nal...))
			}
		}
	} else {
		if len(b) < 6 {
			return nil
		}
		f.nalLen = int(b[4]&0x03) + 1
		p := b[5:]
		nsps := int(p[0] & 0x1F)
		p = p[1:]
		for i := 0; i < nsps; i++ {
			var nal []byte
			var ok bool
			if nal, p, ok = read(p); !ok {
				break
			}
			params = append(params, append([]byte(nil), nal...))
		}
		if len(p) > 0 {
			npps := int(p[0])
			p = p[1:]
			for i := 0; i < npps; i++ {
				var nal []byte
				var ok bool
				if nal, p, ok = read(p); !ok {
					break
				}
				params = append(params, append([]byte(nil), nal...))
			}
		}
	}
	if len(params) > 0 {
		f.params = params
	}
	return nil
}
