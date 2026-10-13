package rtsp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cnm/internal/provider"
)

// ---- fake server ---------------------------------------------------------------------------

type fakeReq struct {
	method, uri string
	hdr         map[string]string
}

type fakeServer struct {
	user, pass     string
	sdp            string
	requireReplay  bool     // SETUP and PLAY must carry "Require: onvif-replay"
	packets        [][]byte // RTP packets sent after PLAY (channel 0)
	early          int      // packets sent before the PLAY reply
	bye            bool     // RTCP BYE after the packets
	closeAfter     bool     // close the connection after the packets
	sessionTimeout int

	ln        net.Listener
	mu        sync.Mutex
	reqs      []fakeReq
	authFails int
}

func startFake(t *testing.T, s *fakeServer) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return ln.Addr().String()
}

func (s *fakeServer) requests(method string) []fakeReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fakeReq
	for _, r := range s.reqs {
		if method == "" || r.method == method {
			out = append(out, r)
		}
	}
	return out
}

var authRe = regexp.MustCompile(`(\w+)="?([^",]*)"?`)

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *fakeServer) authorized(r fakeReq) bool {
	if s.user == "" {
		return true
	}
	a, ok := strings.CutPrefix(r.hdr["authorization"], "Digest ")
	if !ok {
		return false
	}
	p := map[string]string{}
	for _, m := range authRe.FindAllStringSubmatch(a, -1) {
		p[strings.ToLower(m[1])] = m[2]
	}
	ha1 := md5hex(s.user + ":" + p["realm"] + ":" + s.pass)
	ha2 := md5hex(r.method + ":" + p["uri"])
	return p["username"] == s.user && p["uri"] == r.uri && p["response"] == md5hex(ha1+":"+p["nonce"]+":"+ha2)
}

func (s *fakeServer) serve(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	var wmu sync.Mutex
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_, err := c.Write(b)
		return err
	}
	reply := func(r fakeReq, status string, hdr map[string]string, body string) {
		var b strings.Builder
		fmt.Fprintf(&b, "RTSP/1.0 %s\r\nCSeq: %s\r\n", status, r.hdr["cseq"])
		for k, v := range hdr {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
		if body != "" {
			fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
		}
		b.WriteString("\r\n" + body)
		write([]byte(b.String()))
	}
	frame := func(ch byte, pkt []byte) []byte {
		f := []byte{'$', ch, 0, 0}
		binary.BigEndian.PutUint16(f[2:], uint16(len(pkt)))
		return append(f, pkt...)
	}
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			return
		}
		r := fakeReq{method: parts[0], uri: parts[1], hdr: map[string]string{}}
		for {
			l, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			l = strings.TrimRight(l, "\r\n")
			if l == "" {
				break
			}
			k, v, _ := strings.Cut(l, ":")
			r.hdr[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, r)
		s.mu.Unlock()
		if !s.authorized(r) {
			if r.hdr["authorization"] != "" {
				s.mu.Lock()
				s.authFails++
				s.mu.Unlock()
			}
			reply(r, "401 Unauthorized", map[string]string{"WWW-Authenticate": `Digest realm="fake", nonce="n0nce"`}, "")
			continue
		}
		replay := r.hdr["require"] == "onvif-replay"
		switch r.method {
		case "DESCRIBE":
			reply(r, "200 OK", map[string]string{"Content-Type": "application/sdp", "Content-Base": r.uri + "/"}, s.sdp)
		case "SETUP":
			if s.requireReplay && !replay {
				reply(r, "551 Option not supported", nil, "")
				continue
			}
			sess := "F00D"
			if s.sessionTimeout > 0 {
				sess += ";timeout=" + strconv.Itoa(s.sessionTimeout)
			}
			reply(r, "200 OK", map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1", "Session": sess}, "")
		case "PLAY":
			if s.requireReplay && (!replay || !strings.HasPrefix(r.hdr["range"], "clock=")) {
				reply(r, "457 Invalid Range", nil, "")
				continue
			}
			n := min(s.early, len(s.packets))
			for _, p := range s.packets[:n] {
				write(frame(0, p))
			}
			reply(r, "200 OK", map[string]string{"Session": "F00D"}, "")
			go func() {
				for _, p := range s.packets[n:] {
					if write(frame(0, p)) != nil {
						return
					}
				}
				if s.bye {
					write(frame(1, []byte{0x81, 203, 0, 1, 1, 2, 3, 4}))
				}
				if s.closeAfter {
					c.Close()
				}
			}()
		case "GET_PARAMETER", "TEARDOWN":
			reply(r, "200 OK", map[string]string{"Session": "F00D"}, "")
		default:
			reply(r, "405 Method Not Allowed", nil, "")
		}
	}
}

// ---- RTP helpers ----------------------------------------------------------------------------

func rtp(seq uint16, ts uint32, marker bool, ntp time.Time, payload []byte) []byte {
	p := make([]byte, 12)
	p[0] = 0x80
	p[1] = 96
	if marker {
		p[1] |= 0x80
	}
	binary.BigEndian.PutUint16(p[2:], seq)
	binary.BigEndian.PutUint32(p[4:], ts)
	binary.BigEndian.PutUint32(p[8:], 0x11223344)
	if !ntp.IsZero() {
		p[0] |= 0x10
		ext := make([]byte, 16)
		binary.BigEndian.PutUint16(ext, 0xABAC)
		binary.BigEndian.PutUint16(ext[2:], 3)
		binary.BigEndian.PutUint32(ext[4:], uint32(ntp.Unix()+2208988800))
		binary.BigEndian.PutUint32(ext[8:], uint32(uint64(ntp.Nanosecond())<<32/1e9))
		ext[12] = 0x80 // clean point
		p = append(p, ext...)
	}
	return append(p, payload...)
}

// fuA splits an H.264 NAL unit into n FU-A payloads.
func fuA(nal []byte, n int) [][]byte {
	data := nal[1:]
	step := (len(data) + n - 1) / n
	var out [][]byte
	for i := 0; i < len(data); i += step {
		end := min(i+step, len(data))
		h := nal[0] & 0x1F
		if i == 0 {
			h |= 0x80
		}
		if end == len(data) {
			h |= 0x40
		}
		out = append(out, append([]byte{nal[0]&0xE0 | 28, h}, data[i:end]...))
	}
	return out
}

// fuH265 splits an H.265 NAL unit into n FU payloads.
func fuH265(nal []byte, n int) [][]byte {
	data := nal[2:]
	typ := nal[0] >> 1 & 0x3F
	step := (len(data) + n - 1) / n
	var out [][]byte
	for i := 0; i < len(data); i += step {
		end := min(i+step, len(data))
		h := typ
		if i == 0 {
			h |= 0x80
		}
		if end == len(data) {
			h |= 0x40
		}
		out = append(out, append([]byte{nal[0]&0x81 | 49<<1, nal[1], h}, data[i:end]...))
	}
	return out
}

func agg(hdr []byte, nals ...[]byte) []byte {
	out := append([]byte(nil), hdr...)
	for _, n := range nals {
		out = binary.BigEndian.AppendUint16(out, uint16(len(n)))
		out = append(out, n...)
	}
	return out
}

func annexB(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(append(b, 0, 0, 0, 1), n...)
	}
	return b
}

func fill(hdr []byte, n int, v byte) []byte {
	return append(append([]byte(nil), hdr...), bytes.Repeat([]byte{v}, n)...)
}

func collect(t *testing.T, c *Client) ([]AccessUnit, error) {
	t.Helper()
	var aus []AccessUnit
	err := c.Read(func(au AccessUnit) error {
		aus = append(aus, au)
		return nil
	})
	return aus, err
}

func dial(t *testing.T, addr, path string, opt Options) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, "rtsp://"+addr+path, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func play(t *testing.T, c *Client, hdr map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Play(ctx, hdr); err != nil {
		t.Fatal(err)
	}
}

// ---- tests ----------------------------------------------------------------------------------

var (
	sdpSPS = []byte{0x67, 0x42, 0xC0, 0x1E, 0xD9}
	sdpPPS = []byte{0x68, 0xCE, 0x3C, 0x80}
)

func h264SDP() string {
	return "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=x\r\nt=0 0\r\na=control:*\r\n" +
		"m=audio 0 RTP/AVP 0\r\na=control:track=audio\r\n" +
		"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n" +
		"a=fmtp:96 packetization-mode=1;sprop-parameter-sets=" + base64.StdEncoding.EncodeToString(sdpSPS) + "," + base64.StdEncoding.EncodeToString(sdpPPS) + "\r\n" +
		"a=control:track=video\r\n"
}

func TestH264DepacketizationAndReplayHeaders(t *testing.T) {
	inSPS := []byte{0x67, 0x64, 0x00, 0x28, 0xAC}
	inPPS := []byte{0x68, 0xEE, 0x3C, 0xB0}
	idr := fill([]byte{0x65, 0x88}, 4000, 0xAA)
	p1 := fill([]byte{0x41, 0x9A}, 300, 0x11)
	idr2 := fill([]byte{0x65, 0x88}, 500, 0xBB)
	sliceA := fill([]byte{0x41, 0x9B}, 50, 0x21)
	sliceB := fill([]byte{0x41, 0x9C}, 60, 0x22)
	sei := []byte{0x06, 0x05, 0x01, 0xFF}
	p3 := fill([]byte{0x41, 0x9D}, 40, 0x31)
	p4 := fill([]byte{0x41, 0x9E}, 40, 0x32)

	base := uint32(0xFFFFF000) // wraps during the test
	ntp0 := time.Date(2026, 1, 1, 12, 0, 0, 500_000_000, time.UTC)
	var pk [][]byte
	seq := uint16(65530) // sequence numbers wrap too
	add := func(ts uint32, marker bool, ntp time.Time, payload []byte) {
		pk = append(pk, rtp(seq, ts, marker, ntp, payload))
		seq++
	}
	// AU0: STAP-A (in-band SPS/PPS) + IDR in 3 FU-A fragments, NTP on the first packet
	add(base, false, ntp0, agg([]byte{24}, inSPS, inPPS))
	frags := fuA(idr, 3)
	for i, f := range frags {
		add(base, i == len(frags)-1, time.Time{}, f)
	}
	// AU1: single NAL P slice, timestamp crosses 2^32
	add(base+3600, true, ntp0.Add(40*time.Millisecond), p1)
	// AU2: IDR without parameter sets → latest (in-band) sets are prepended
	add(base+7200, true, time.Time{}, idr2)
	// SEI only: not emitted
	add(base+10800, true, time.Time{}, sei)
	// AU3: two slices in two packets, marker on the second
	add(base+14400, false, time.Time{}, sliceA)
	add(base+14400, true, time.Time{}, sliceB)
	// AU4/AU5: no marker, completed by the timestamp change and by RTCP BYE
	add(base+18000, false, time.Time{}, p3)
	add(base+21600, false, time.Time{}, p4)

	s := &fakeServer{user: "admin", pass: "geheim", sdp: h264SDP(), requireReplay: true, packets: pk, early: 2, bye: true}
	addr := startFake(t, s)
	c := dial(t, addr, "/onvif/replay/1", Options{User: "admin", Pass: "geheim", SetupHeader: map[string]string{"Require": "onvif-replay"}})
	if tr := c.Track(); tr.Codec != H264 || tr.Control != "rtsp://"+addr+"/onvif/replay/1/track=video" || len(tr.ParamSets) != 2 {
		t.Fatalf("track: %+v", tr)
	}
	play(t, c, map[string]string{"Require": "onvif-replay", "Range": "clock=20260101T120000Z-20260101T121500Z", "Rate-Control": "yes"})
	aus, err := collect(t, c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []struct {
		data []byte
		key  bool
		pts  int64
	}{
		{annexB(inSPS, inPPS, idr), true, 0},
		{annexB(p1), false, 3600},
		{annexB(inSPS, inPPS, idr2), true, 7200},
		{annexB(sliceA, sliceB), false, 14400},
		{annexB(p3), false, 18000},
		{annexB(p4), false, 21600},
	}
	if len(aus) != len(want) {
		t.Fatalf("got %d access units, want %d", len(aus), len(want))
	}
	for i, w := range want {
		if !bytes.Equal(aus[i].Data, w.data) || aus[i].Key != w.key || aus[i].PTS90k != w.pts || aus[i].Codec != H264 {
			t.Fatalf("AU %d: key=%v pts=%d len=%d (want key=%v pts=%d len=%d)", i, aus[i].Key, aus[i].PTS90k, len(aus[i].Data), w.key, w.pts, len(w.data))
		}
	}
	if !aus[0].NTP.Equal(ntp0) || aus[1].NTP.Sub(ntp0.Add(40*time.Millisecond)).Abs() > time.Microsecond || !aus[2].NTP.IsZero() {
		t.Fatalf("NTP times: %v %v %v", aus[0].NTP, aus[1].NTP, aus[2].NTP)
	}

	setup := s.requests("SETUP")
	plays := s.requests("PLAY")
	if len(setup) != 1 || setup[0].hdr["require"] != "onvif-replay" || !strings.HasSuffix(setup[0].hdr["transport"], "interleaved=0-1") {
		t.Fatalf("SETUP: %+v", setup)
	}
	if len(plays) != 1 || plays[0].hdr["session"] != "F00D" || plays[0].hdr["rate-control"] != "yes" || plays[0].uri != "rtsp://"+addr+"/onvif/replay/1/" {
		t.Fatalf("PLAY: %+v", plays)
	}
	c.Close()
	waitFor(t, func() bool { return len(s.requests("TEARDOWN")) == 1 })
}

func TestSDPParameterSetsPrependedBeforeFirstKeyframe(t *testing.T) {
	idr := fill([]byte{0x65, 0x88}, 100, 0xAA)
	aud := []byte{0x09, 0xF0}
	s := &fakeServer{sdp: h264SDP(), packets: [][]byte{
		rtp(1, 1000, false, time.Time{}, aud),
		rtp(2, 1000, true, time.Time{}, idr),
	}, closeAfter: true}
	addr := startFake(t, s)
	c := dial(t, addr, "/live", Options{})
	play(t, c, nil)
	aus, err := collect(t, c)
	if err != nil || len(aus) != 1 {
		t.Fatalf("aus=%d err=%v", len(aus), err)
	}
	// the AUD stays first, SDP parameter sets follow
	if !bytes.Equal(aus[0].Data, annexB(aud, sdpSPS, sdpPPS, idr)) || !aus[0].Key {
		t.Fatalf("got % x", aus[0].Data[:20])
	}
}

func TestH265Depacketization(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0C, 0x01}
	sps := []byte{0x42, 0x01, 0x01, 0x01, 0x60}
	pps := []byte{0x44, 0x01, 0xC1, 0x72}
	sdp := "v=0\r\ns=x\r\nt=0 0\r\nm=video 0 RTP/AVP 98\r\na=rtpmap:98 H265/90000\r\n" +
		"a=fmtp:98 sprop-vps=" + base64.StdEncoding.EncodeToString(vps) + ";sprop-sps=" + base64.StdEncoding.EncodeToString(sps) + ";sprop-pps=" + base64.StdEncoding.EncodeToString(pps) + "\r\n" +
		"a=control:rtsp://ignored-host/abs/trackID=1\r\n"
	idr := fill([]byte{19 << 1, 0x01}, 3000, 0xAB) // IDR_W_RADL
	tr1 := fill([]byte{1 << 1, 0x01}, 80, 0x01)    // TRAIL_R
	tr2 := fill([]byte{1 << 1, 0x01}, 90, 0x02)
	cra := fill([]byte{21 << 1, 0x01}, 70, 0x03) // CRA
	var pk [][]byte
	seq := uint16(10)
	add := func(ts uint32, marker bool, payload []byte) {
		p := rtp(seq, ts, marker, time.Time{}, payload)
		p[1] = p[1]&0x80 | 98
		pk = append(pk, p)
		seq++
	}
	frags := fuH265(idr, 4)
	for i, f := range frags {
		add(5000, i == len(frags)-1, f)
	}
	add(8000, true, agg([]byte{48 << 1, 0x01}, tr1, tr2))
	add(11000, true, cra)
	s := &fakeServer{sdp: sdp, packets: pk, bye: true}
	addr := startFake(t, s)
	c := dial(t, addr, "/h265", Options{})
	if tr := c.Track(); tr.Codec != H265 || tr.PayloadType != 98 || tr.Control != "rtsp://ignored-host/abs/trackID=1" || len(tr.ParamSets) != 3 {
		t.Fatalf("track %+v", tr)
	}
	play(t, c, nil)
	aus, err := collect(t, c)
	if err != nil || len(aus) != 3 {
		t.Fatalf("aus=%d err=%v", len(aus), err)
	}
	if !bytes.Equal(aus[0].Data, annexB(vps, sps, pps, idr)) || !aus[0].Key || aus[0].Codec != H265 {
		t.Fatalf("H.265 IDR: key=%v % x", aus[0].Key, aus[0].Data[:24])
	}
	if !bytes.Equal(aus[1].Data, annexB(tr1, tr2)) || aus[1].Key || aus[1].PTS90k != 3000 {
		t.Fatalf("H.265 AP: key=%v pts=%d", aus[1].Key, aus[1].PTS90k)
	}
	if !bytes.Equal(aus[2].Data, annexB(vps, sps, pps, cra)) || !aus[2].Key || aus[2].PTS90k != 6000 {
		t.Fatalf("H.265 CRA: key=%v pts=%d", aus[2].Key, aus[2].PTS90k)
	}
}

func TestWrongPasswordIsAuthErrorWithoutRetry(t *testing.T) {
	s := &fakeServer{user: "admin", pass: "geheim", sdp: h264SDP()}
	addr := startFake(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Dial(ctx, "rtsp://admin:falsch@"+addr+"/x", Options{})
	if provider.KindOf(err) != provider.KindAuth || StatusCode(err) != 401 {
		t.Fatalf("want auth error, got %v", err)
	}
	if strings.Contains(err.Error(), "falsch@") {
		t.Fatal("credentials leaked into the error")
	}
	s.mu.Lock()
	fails := s.authFails
	s.mu.Unlock()
	if fails != 1 || len(s.requests("DESCRIBE")) != 2 {
		t.Fatalf("auth attempts: fails=%d describes=%d", fails, len(s.requests("DESCRIBE")))
	}
	// credentials from the URL are used but never sent in the request line
	c, err := Dial(ctx, "rtsp://admin:geheim@"+addr+"/x", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, r := range s.requests("") {
		if strings.Contains(r.uri, "@") {
			t.Fatalf("userinfo in request line: %s", r.uri)
		}
	}
}

func TestReplayServerRejectsMissingRequire(t *testing.T) {
	s := &fakeServer{sdp: h264SDP(), requireReplay: true}
	addr := startFake(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Dial(ctx, "rtsp://"+addr+"/x", Options{})
	if provider.KindOf(err) != provider.KindUnsupported || StatusCode(err) != 551 {
		t.Fatalf("want unsupported (551), got %v", err)
	}
	c := dial(t, addr, "/x", Options{SetupHeader: map[string]string{"Require": "onvif-replay"}})
	err = c.Play(ctx, map[string]string{"Require": "onvif-replay"}) // no Range
	if provider.KindOf(err) != provider.KindNotFound || StatusCode(err) != 457 {
		t.Fatalf("want not found (457), got %v", err)
	}
}

func TestReadEndsOnIdleAndClose(t *testing.T) {
	idr := fill([]byte{0x65}, 10, 1)
	s := &fakeServer{sdp: h264SDP(), packets: [][]byte{rtp(1, 0, true, time.Time{}, idr)}}
	addr := startFake(t, s)
	c := dial(t, addr, "/x", Options{IdleTimeout: 300 * time.Millisecond})
	play(t, c, nil)
	aus, err := collect(t, c)
	if !errors.Is(err, ErrIdle) || len(aus) != 1 {
		t.Fatalf("want 1 AU and ErrIdle, got %d %v", len(aus), err)
	}

	c2 := dial(t, addr, "/x", Options{})
	play(t, c2, nil)
	done := make(chan error, 1)
	go func() { _, err := collect(t, c2); done <- err }()
	time.Sleep(100 * time.Millisecond)
	c2.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("want ErrClosed, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read did not return after Close")
	}
}

func TestKeepalive(t *testing.T) {
	s := &fakeServer{sdp: h264SDP(), sessionTimeout: 2}
	addr := startFake(t, s)
	c := dial(t, addr, "/x", Options{IdleTimeout: 5 * time.Second})
	play(t, c, nil)
	go collect(t, c)
	waitFor(t, func() bool { return len(s.requests("GET_PARAMETER")) >= 1 })
	if r := s.requests("GET_PARAMETER")[0]; r.hdr["session"] != "F00D" {
		t.Fatalf("keepalive without session: %+v", r)
	}
}

func TestResolveControl(t *testing.T) {
	cases := []struct{ base, ctl, want string }{
		{"rtsp://h/a/", "trackID=1", "rtsp://h/a/trackID=1"},
		{"rtsp://h/a", "trackID=1", "rtsp://h/a/trackID=1"},
		{"rtsp://h/cam?ch=1", "trackID=0", "rtsp://h/cam?ch=1/trackID=0"},
		{"rtsp://h/a", "*", "rtsp://h/a"},
		{"rtsp://h/a", "", "rtsp://h/a"},
		{"rtsp://h/a", "rtsp://u:p@x/b", "rtsp://x/b"},
	}
	for _, c := range cases {
		if got := resolveControl(c.base, c.ctl); got != c.want {
			t.Errorf("resolveControl(%q, %q) = %q, want %q", c.base, c.ctl, got, c.want)
		}
	}
	// absolute session level control replaces the base
	sdp := "v=0\r\na=control:rtsp://h/agg\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:v\r\n"
	tr, agg, ok := parseSDP([]byte(sdp), "rtsp://h/base/")
	if !ok || agg != "rtsp://h/agg" || tr.Control != "rtsp://h/agg/v" {
		t.Fatalf("aggregate %q track %q", agg, tr.Control)
	}
}

func TestNTPTime(t *testing.T) {
	want := time.Date(2026, 10, 9, 8, 7, 6, 250_000_000, time.UTC)
	got := ntpTime(uint32(want.Unix()+ntpUnixOffset), 1<<30)
	if !got.Equal(want) {
		t.Fatalf("got %v", got)
	}
	// era 1 (after 2036-02-07)
	later := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := ntpTime(uint32(later.Unix()+ntpUnixOffset), 0); !got.Equal(later) {
		t.Fatalf("era 1: got %v", got)
	}
	if !ntpTime(0, 0).IsZero() {
		t.Fatal("zero NTP must map to the zero time")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
