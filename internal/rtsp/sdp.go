package rtsp

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
)

// Codec of the video track.
type Codec string

// Supported codecs.
const (
	H264 Codec = "H264"
	H265 Codec = "H265"
)

// Track is the video track selected from the SDP.
type Track struct {
	Codec       Codec
	PayloadType int
	ClockRate   int
	// Control is the absolute URL used for SETUP.
	Control string
	// ParamSets are the out-of-band parameter sets without start codes:
	// SPS, PPS (H.264) or VPS, SPS, PPS (H.265).
	ParamSets [][]byte
}

type sdpMedia struct {
	kind    string
	formats []string
	control string
	rtpmap  map[string]string // payload type → "H264/90000"
	fmtp    map[string]string // payload type → parameters
}

// parseSDP returns the first H.264/H.265 video track and the aggregate
// control URL (used for PLAY, GET_PARAMETER and TEARDOWN). base is the
// Content-Base (or request URL) that relative control attributes refer to.
func parseSDP(body []byte, base string) (Track, string, bool) {
	var medias []*sdpMedia
	var cur *sdpMedia
	sessionControl := ""
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "m="):
			f := strings.Fields(line[2:])
			cur = &sdpMedia{rtpmap: map[string]string{}, fmtp: map[string]string{}}
			if len(f) > 0 {
				cur.kind = strings.ToLower(f[0])
			}
			if len(f) > 3 {
				cur.formats = f[3:]
			}
			medias = append(medias, cur)
		case strings.HasPrefix(line, "a=control:"):
			v := strings.TrimSpace(line[len("a=control:"):])
			if cur == nil {
				sessionControl = v
			} else {
				cur.control = v
			}
		case strings.HasPrefix(line, "a=rtpmap:") && cur != nil:
			pt, enc, _ := strings.Cut(strings.TrimSpace(line[len("a=rtpmap:"):]), " ")
			cur.rtpmap[pt] = strings.TrimSpace(enc)
		case strings.HasPrefix(line, "a=fmtp:") && cur != nil:
			pt, params, _ := strings.Cut(strings.TrimSpace(line[len("a=fmtp:"):]), " ")
			cur.fmtp[pt] = strings.TrimSpace(params)
		}
	}
	// like FFmpeg: an absolute session level control replaces the base, a
	// relative one ("*") refers to the base itself
	aggregate := base
	if isAbsolute(sessionControl) {
		aggregate = stripUserinfo(sessionControl)
	}
	for _, m := range medias {
		if m.kind != "video" {
			continue
		}
		for _, pt := range m.formats {
			name, rate, _ := strings.Cut(m.rtpmap[pt], "/")
			var codec Codec
			switch strings.ToUpper(name) {
			case "H264":
				codec = H264
			case "H265", "HEVC":
				codec = H265
			default:
				continue
			}
			t := Track{Codec: codec, ClockRate: 90000, Control: resolveControl(aggregate, m.control)}
			t.PayloadType, _ = strconv.Atoi(pt)
			if r, _, _ := strings.Cut(rate, "/"); r != "" {
				if n, err := strconv.Atoi(r); err == nil && n > 0 {
					t.ClockRate = n
				}
			}
			t.ParamSets = paramSets(codec, fmtpParams(m.fmtp[pt]))
			return t, aggregate, true
		}
	}
	return Track{}, "", false
}

func fmtpParams(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok {
			out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out
}

func paramSets(codec Codec, p map[string]string) [][]byte {
	keys := []string{"sprop-parameter-sets"}
	if codec == H265 {
		keys = []string{"sprop-vps", "sprop-sps", "sprop-pps"}
	}
	var out [][]byte
	for _, k := range keys {
		for _, v := range strings.Split(p[k], ",") {
			if b := decodeB64(strings.TrimSpace(v)); len(b) > 0 {
				out = append(out, b)
			}
		}
	}
	return out
}

func decodeB64(s string) []byte {
	if s == "" {
		return nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b
	}
	if b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return b
	}
	return nil
}

// resolveControl resolves an SDP control attribute like FFmpeg and most
// servers do: absolute URLs are used as they are, "*" or empty means the base
// itself, anything else is appended to the base with a single slash.
func resolveControl(base, ctl string) string {
	switch {
	case ctl == "" || ctl == "*":
		return base
	case isAbsolute(ctl):
		return stripUserinfo(ctl)
	case strings.HasSuffix(base, "/"):
		return base + strings.TrimPrefix(ctl, "/")
	case strings.HasPrefix(ctl, "/"):
		return base + ctl
	}
	return base + "/" + ctl
}

func isAbsolute(ctl string) bool {
	l := strings.ToLower(ctl)
	return strings.HasPrefix(l, "rtsp://") || strings.HasPrefix(l, "rtsps://")
}

func stripUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
