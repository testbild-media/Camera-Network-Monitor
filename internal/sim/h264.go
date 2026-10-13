// Package sim contains DVR simulators used by automated tests and by the
// cnm-sim tool for manual end-to-end testing without hardware:
//   - Dahua JSON-RPC2 (control) incl. recording search
//   - RTSP server (live + playback URLs, digest auth) streaming real H.264
//   - DVRIP/Sofia server (modern single-socket and legacy two-socket)
//
// The simulators implement the protocol behaviour CNM relies on; they are not
// a substitute for verification on real hardware.
package sim

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
)

// AU is one H.264 access unit in Annex-B form.
type AU struct {
	Data []byte
	Key  bool
}

// Clip is a looping H.264 elementary stream.
type Clip struct {
	AUs  []AU
	FPS  int
	SPS  []byte // without start code
	PPS  []byte
	W, H int
}

func nalType(b byte) byte { return b & 0x1F }

// splitNALs returns NAL payloads (without start codes).
func splitNALs(data []byte) [][]byte {
	var out [][]byte
	start := -1
	i := 0
	for i+3 <= len(data) {
		sc := 0
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			sc = 3
		} else if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			sc = 4
		}
		if sc > 0 {
			if start >= 0 {
				out = append(out, bytes.TrimRight(data[start:i], "\x00"))
			}
			start = i + sc
			i += sc
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		out = append(out, data[start:])
	}
	return out
}

// ParseClip splits an Annex-B stream with AUD delimiters into access units.
func ParseClip(data []byte, fps, w, h int) (*Clip, error) {
	c := &Clip{FPS: fps, W: w, H: h}
	var cur [][]byte
	flush := func() {
		if len(cur) == 0 {
			return
		}
		var b bytes.Buffer
		key := false
		for _, n := range cur {
			b.Write([]byte{0, 0, 0, 1})
			b.Write(n)
			if nalType(n[0]) == 5 {
				key = true
			}
		}
		c.AUs = append(c.AUs, AU{Data: b.Bytes(), Key: key})
		cur = nil
	}
	for _, n := range splitNALs(data) {
		if len(n) == 0 {
			continue
		}
		switch nalType(n[0]) {
		case 9: // AUD starts a new access unit (not emitted)
			flush()
			continue
		case 7:
			if c.SPS == nil {
				c.SPS = append([]byte(nil), n...)
			}
		case 8:
			if c.PPS == nil {
				c.PPS = append([]byte(nil), n...)
			}
		}
		cur = append(cur, n)
	}
	flush()
	if len(c.AUs) == 0 || c.SPS == nil || c.PPS == nil {
		return nil, errors.New("sim: no usable H.264 access units")
	}
	// start the loop at a keyframe
	for i, au := range c.AUs {
		if au.Key {
			c.AUs = append(c.AUs[i:], c.AUs[:i]...)
			break
		}
	}
	return c, nil
}

// Generate renders a test pattern clip with FFmpeg (frame counter visible).
func Generate(ffmpeg string, w, h, fps, seconds, hue int) (*Clip, error) {
	vf := fmt.Sprintf("testsrc=size=%dx%d:rate=%d,hue=h=%d", w, h, fps, hue)
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", vf,
		"-t", fmt.Sprint(seconds), "-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-profile:v", "baseline", "-pix_fmt", "yuv420p", "-g", fmt.Sprint(fps), "-keyint_min", fmt.Sprint(fps),
		"-sc_threshold", "0", "-bf", "0", "-x264-params", "aud=1:repeat-headers=1", "-f", "h264", "pipe:1")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sim: ffmpeg: %v: %s", err, errb.String())
	}
	return ParseClip(out.Bytes(), fps, w, h)
}

// Synthetic returns a fake clip with structurally valid NAL units (for
// protocol tests that never decode video).
func Synthetic(frames int) *Clip {
	sps := []byte{0x67, 0x42, 0xC0, 0x1E, 0xD9, 0x00, 0xA0, 0x47, 0xFE, 0xC8}
	pps := []byte{0x68, 0xCE, 0x3C, 0x80}
	c := &Clip{FPS: 10, SPS: sps, PPS: pps, W: 320, H: 240}
	for i := 0; i < frames; i++ {
		var b bytes.Buffer
		if i%10 == 0 {
			b.Write([]byte{0, 0, 0, 1})
			b.Write(sps)
			b.Write([]byte{0, 0, 0, 1})
			b.Write(pps)
			b.Write([]byte{0, 0, 0, 1, 0x65})
			b.Write(bytes.Repeat([]byte{byte(i)}, 3000+i))
			c.AUs = append(c.AUs, AU{Data: b.Bytes(), Key: true})
			continue
		}
		b.Write([]byte{0, 0, 0, 1, 0x41})
		b.Write(bytes.Repeat([]byte{byte(i)}, 700+i))
		c.AUs = append(c.AUs, AU{Data: b.Bytes()})
	}
	return c
}
