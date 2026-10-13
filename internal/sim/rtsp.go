package sim

import (
	"bufio"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RTSPServer serves Dahua style URLs:
//
//	/cam/realmonitor?channel=N&subtype=0|1           live
//	/cam/playback?channel=N&starttime=..&endtime=..  playback (finite)
//
// over RTSP/TCP interleaved with digest authentication.
type RTSPServer struct {
	User, Pass string
	// Clip returns the clip for a channel (1 based) and subtype.
	Clip func(channel, subtype int) *Clip
	// HasRecording decides whether a playback start time has material.
	HasRecording func(channel int, start time.Time) bool
	// Location of the device wall clock used in playback URLs.
	Loc *time.Location
	// ReplayNoRequire serves clock based replay (ONVIFReplay) without the
	// ONVIF "Require: onvif-replay" header (Axis edge storage replay).
	ReplayNoRequire bool
	// Stall makes a live stream accept PLAY but never send video (like a
	// camera behind a firewall that blocks the media data).
	Stall func(channel, subtype int) bool
	// Route maps a request to channel/subtype and, for playback, the
	// requested range. Nil = Dahua paths (DahuaRoute).
	Route func(u *url.URL, loc *time.Location) (r Route, ok bool)
	// ONVIFReplay, when set, switches playback routes to ONVIF replay
	// semantics (see onvifPlay): the range comes from the PLAY request. It
	// gets the channel (1 based) and the requested range (end zero when
	// open) and returns the end of the footage to stream; ok=false answers
	// 457 Invalid Range.
	ONVIFReplay func(channel int, start, end time.Time) (until time.Time, ok bool)

	ln        net.Listener
	AuthFails atomic.Int32
	Sessions  atomic.Int32
	nonce     string
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
}

// Route is a resolved RTSP request.
type Route struct {
	Channel, Subtype int
	Playback         bool
	Start, End       time.Time
}

// DahuaRoute resolves /cam/realmonitor and /cam/playback.
func DahuaRoute(u *url.URL, loc *time.Location) (Route, bool) {
	q := u.Query()
	ch, _ := strconv.Atoi(q.Get("channel"))
	sub, _ := strconv.Atoi(q.Get("subtype"))
	switch {
	case strings.HasPrefix(u.Path, "/cam/realmonitor"):
		return Route{Channel: ch, Subtype: sub}, true
	case strings.HasPrefix(u.Path, "/cam/playback"):
		st, e1 := time.ParseInLocation("2006_01_02_15_04_05", q.Get("starttime"), loc)
		en, e2 := time.ParseInLocation("2006_01_02_15_04_05", q.Get("endtime"), loc)
		return Route{Channel: ch, Subtype: sub, Playback: true, Start: st, End: en}, e1 == nil && e2 == nil
	}
	return Route{}, false
}

// HikvisionRoute resolves /Streaming/Channels/101 (live) and
// /Streaming/tracks/101?starttime=20260101T120000Z&endtime=… (playback);
// the "Z" times are device wall clock like on real recorders.
func HikvisionRoute(u *url.URL, loc *time.Location) (Route, bool) {
	path := strings.TrimSuffix(u.Path, "/")
	var id int
	var playback bool
	switch {
	case strings.HasPrefix(path, "/Streaming/Channels/"):
		id, _ = strconv.Atoi(strings.TrimPrefix(path, "/Streaming/Channels/"))
	case strings.HasPrefix(path, "/Streaming/tracks/"):
		id, _ = strconv.Atoi(strings.TrimPrefix(path, "/Streaming/tracks/"))
		playback = true
	default:
		return Route{}, false
	}
	r := Route{Channel: id / 100, Subtype: id%100 - 1, Playback: playback}
	if r.Channel < 1 || r.Subtype < 0 || r.Subtype > 1 {
		return Route{}, false
	}
	if playback {
		q := u.Query()
		var e1, e2 error
		r.Start, e1 = time.ParseInLocation("20060102T150405Z", q.Get("starttime"), loc)
		r.End, e2 = time.ParseInLocation("20060102T150405Z", q.Get("endtime"), loc)
		if e1 != nil || e2 != nil {
			return Route{}, false
		}
	}
	return r, true
}

// Listen starts the server.
func (s *RTSPServer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.nonce = fmt.Sprintf("%x", rand.Int63())
	s.conns = map[net.Conn]struct{}{}
	if s.Loc == nil {
		s.Loc = time.Local
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns[c] = struct{}{}
			s.mu.Unlock()
			go s.serve(c)
		}
	}()
	return nil
}

// Addr returns the listen address.
func (s *RTSPServer) Addr() string { return s.ln.Addr().String() }

// Close stops the server and all sessions.
func (s *RTSPServer) Close() {
	s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

type rtspReq struct {
	method, uri string
	headers     map[string]string
}

func readReq(rd *bufio.Reader) (*rtspReq, error) {
	for {
		// skip interleaved RTCP packets from the client
		b, err := rd.Peek(1)
		if err != nil {
			return nil, err
		}
		if b[0] != '$' {
			break
		}
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(rd, hdr); err != nil {
			return nil, err
		}
		if _, err := io.CopyN(io.Discard, rd, int64(binary.BigEndian.Uint16(hdr[2:]))); err != nil {
			return nil, err
		}
	}
	line, err := rd.ReadString('\n')
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(line)
	if len(parts) < 3 {
		return nil, fmt.Errorf("bad request line")
	}
	r := &rtspReq{method: parts[0], uri: parts[1], headers: map[string]string{}}
	for {
		l, err := rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		k, v, _ := strings.Cut(l, ":")
		r.headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	if n, _ := strconv.Atoi(r.headers["content-length"]); n > 0 {
		io.CopyN(io.Discard, rd, int64(n))
	}
	return r, nil
}

var digestRe = regexp.MustCompile(`(\w+)="?([^",]*)"?`)

func md5s(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *RTSPServer) authorized(r *rtspReq) bool {
	a := r.headers["authorization"]
	if !strings.HasPrefix(a, "Digest ") {
		if strings.HasPrefix(a, "Basic ") {
			b, _ := base64.StdEncoding.DecodeString(a[6:])
			return string(b) == s.User+":"+s.Pass
		}
		return false
	}
	p := map[string]string{}
	for _, m := range digestRe.FindAllStringSubmatch(a[7:], -1) {
		p[strings.ToLower(m[1])] = m[2]
	}
	ha1 := md5s(s.User + ":" + p["realm"] + ":" + s.Pass)
	ha2 := md5s(r.method + ":" + p["uri"])
	want := md5s(ha1 + ":" + p["nonce"] + ":" + ha2)
	if p["qop"] != "" {
		want = md5s(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":" + p["qop"] + ":" + ha2)
	}
	return p["username"] == s.User && p["response"] == want
}

func (s *RTSPServer) serve(c net.Conn) {
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	rd := bufio.NewReader(c)
	var wmu sync.Mutex
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err := c.Write(b)
		return err
	}
	reply := func(r *rtspReq, status string, hdr map[string]string, body string) error {
		var b strings.Builder
		fmt.Fprintf(&b, "RTSP/1.0 %s\r\nCSeq: %s\r\nServer: cnm-sim\r\n", status, r.headers["cseq"])
		for k, v := range hdr {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
		if body != "" {
			fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
		}
		b.WriteString("\r\n")
		b.WriteString(body)
		return write([]byte(b.String()))
	}
	session := fmt.Sprintf("%08X", rand.Uint32())
	stop := make(chan struct{})
	var stopOnce sync.Once
	defer stopOnce.Do(func() { close(stop) })
	var clip *Clip
	var limit time.Duration
	var stalled bool
	replayCh := 0 // ONVIF replay session for this channel (1 based)
	for {
		r, err := readReq(rd)
		if err != nil {
			return
		}
		if r.method == "OPTIONS" {
			reply(r, "200 OK", map[string]string{"Public": "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN, GET_PARAMETER"}, "")
			continue
		}
		if !s.authorized(r) {
			if r.headers["authorization"] != "" {
				s.AuthFails.Add(1)
			}
			reply(r, "401 Unauthorized", map[string]string{"WWW-Authenticate": fmt.Sprintf(`Digest realm="cnm-sim", nonce="%s"`, s.nonce)}, "")
			continue
		}
		u, err := url.Parse(r.uri)
		if err != nil {
			reply(r, "400 Bad Request", nil, "")
			continue
		}
		route := s.Route
		if route == nil {
			route = DahuaRoute
		}
		switch r.method {
		case "DESCRIBE":
			rt, ok := route(u, s.Loc)
			if !ok {
				reply(r, "404 Not Found", nil, "")
				continue
			}
			clip = s.Clip(rt.Channel, rt.Subtype)
			stalled = !rt.Playback && s.Stall != nil && s.Stall(rt.Channel, rt.Subtype)
			if clip == nil {
				reply(r, "404 Not Found", nil, "")
				continue
			}
			if rt.Playback && s.ONVIFReplay != nil {
				replayCh = rt.Channel
			} else if rt.Playback {
				if !rt.End.After(rt.Start) || (s.HasRecording != nil && !s.HasRecording(rt.Channel, rt.Start)) {
					reply(r, "404 Not Found", nil, "")
					continue
				}
				limit = rt.End.Sub(rt.Start)
			}
			sdp := fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=cnm-sim\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1;profile-level-id=%s;sprop-parameter-sets=%s,%s\r\na=control:trackID=0\r\n",
				hex.EncodeToString(clip.SPS[1:4]), base64.StdEncoding.EncodeToString(clip.SPS), base64.StdEncoding.EncodeToString(clip.PPS))
			base := r.uri
			if !strings.HasSuffix(base, "/") {
				base += "/"
			}
			reply(r, "200 OK", map[string]string{"Content-Type": "application/sdp", "Content-Base": base}, sdp)
		case "SETUP":
			tr := r.headers["transport"]
			if !strings.Contains(tr, "TCP") {
				reply(r, "461 Unsupported Transport", nil, "")
				continue
			}
			if replayCh > 0 && !s.ReplayNoRequire && !requiresReplay(r) {
				reply(r, "551 Option not supported", map[string]string{"Unsupported": "onvif-replay"}, "")
				continue
			}
			reply(r, "200 OK", map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1", "Session": session + ";timeout=60"}, "")
		case "PLAY":
			if replayCh > 0 && clip != nil {
				s.onvifPlay(r, replayCh, clip, func(status string, hdr map[string]string) {
					hdr["Session"] = session
					reply(r, status, hdr, "")
				}, write, stop)
				continue
			}
			reply(r, "200 OK", map[string]string{"Session": session, "Range": "npt=0.000-"}, "")
			if clip != nil && !stalled {
				s.Sessions.Add(1)
				go func(clip *Clip, limit time.Duration) {
					defer s.Sessions.Add(-1)
					streamRTP(clip, write, stop, limit)
					if limit > 0 {
						c.Close() // end of recording
					}
				}(clip, limit)
			}
		case "TEARDOWN":
			reply(r, "200 OK", map[string]string{"Session": session}, "")
			return
		case "GET_PARAMETER", "SET_PARAMETER":
			reply(r, "200 OK", map[string]string{"Session": session}, "")
		default:
			reply(r, "405 Method Not Allowed", nil, "")
		}
	}
}

// streamRTP sends the clip in real time as RTP over interleaved channel 0.
func streamRTP(clip *Clip, write func([]byte) error, stop <-chan struct{}, limit time.Duration) {
	seq := uint16(rand.Intn(65535))
	ssrc := rand.Uint32()
	frameDur := time.Second / time.Duration(clip.FPS)
	start := time.Now()
	for i := 0; ; i++ {
		if limit > 0 && time.Duration(i)*frameDur >= limit {
			return
		}
		select {
		case <-stop:
			return
		default:
		}
		au := clip.AUs[i%len(clip.AUs)]
		ts := uint32(i * 90000 / clip.FPS)
		nals := splitNALs(au.Data)
		for ni, n := range nals {
			last := ni == len(nals)-1
			for _, p := range packetize(n, last) {
				pkt := make([]byte, 12, 12+len(p.payload))
				pkt[0] = 0x80
				pkt[1] = 96
				if p.marker {
					pkt[1] |= 0x80
				}
				binary.BigEndian.PutUint16(pkt[2:], seq)
				binary.BigEndian.PutUint32(pkt[4:], ts)
				binary.BigEndian.PutUint32(pkt[8:], ssrc)
				seq++
				pkt = append(pkt, p.payload...)
				frame := make([]byte, 4, 4+len(pkt))
				frame[0] = '$'
				frame[1] = 0
				binary.BigEndian.PutUint16(frame[2:], uint16(len(pkt)))
				if err := write(append(frame, pkt...)); err != nil {
					return
				}
			}
		}
		next := start.Add(time.Duration(i+1) * frameDur)
		select {
		case <-stop:
			return
		case <-time.After(time.Until(next)):
		}
	}
}

type rtpPayload struct {
	payload []byte
	marker  bool
}

func packetize(nal []byte, lastOfAU bool) []rtpPayload {
	const mtu = 1400
	if len(nal) <= mtu {
		return []rtpPayload{{payload: nal, marker: lastOfAU}}
	}
	var out []rtpPayload
	hdr := nal[0]
	data := nal[1:]
	first := true
	for len(data) > 0 {
		n := len(data)
		if n > mtu-2 {
			n = mtu - 2
		}
		fuIndicator := (hdr & 0xE0) | 28
		fuHeader := hdr & 0x1F
		if first {
			fuHeader |= 0x80
		}
		end := n == len(data)
		if end {
			fuHeader |= 0x40
		}
		p := append([]byte{fuIndicator, fuHeader}, data[:n]...)
		out = append(out, rtpPayload{payload: p, marker: end && lastOfAU})
		data = data[n:]
		first = false
	}
	return out
}
