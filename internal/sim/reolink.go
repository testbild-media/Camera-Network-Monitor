package sim

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ReolinkServer simulates a camera with the Reolink HTTP API: token login,
// device information and abilities, ports, clock, SD card, recording search,
// HTTP-FLV playback and PTZ (moves, presets, guard position, tracking).
// Live video comes from an RTSPServer with ReolinkRoute.
type ReolinkServer struct {
	User, Pass string
	// DualLens: wide angle and tele lens on one motorised head.
	DualLens bool
	// PTZ: the camera can pan, tilt and zoom (with presets, guard, tracking).
	PTZ bool
	// HTTPOnly serves plain HTTP instead of HTTPS.
	HTTPOnly bool
	// RTSPOff starts with RTSP and RTMP switched off (factory default of
	// newer firmware).
	RTSPOff bool
	// TeleAsChannel addresses tele lens recordings as channel 1 instead of
	// logical channel 1 of channel 0.
	TeleAsChannel bool
	// FLVZeroAtSeek starts HTTP-FLV timestamps at 0 after a seek (otherwise
	// at the seek position).
	FLVZeroAtSeek bool
	// RTSPPort is reported in GetNetPort / GetRtspUrl.
	RTSPPort int
	// FailGuardGoto refuses moving to the guard position (e.g. it was
	// deleted in the camera's app).
	FailGuardGoto atomic.Bool
	// Clip returns the video of a channel (0 based).
	Clip func(channel int, main bool) *Clip
	Loc  *time.Location

	ln  net.Listener
	srv *http.Server

	mu        sync.Mutex
	tokens    map[string]time.Time
	netPort   map[string]any
	guard     map[string]any
	ai        map[string]any
	presets   []map[string]any
	ptzOps    []string
	tokenSeq  int
	LoginFail atomic.Int32
	Logins    atomic.Int32
	FLVOpens  atomic.Int32
	lastFLV   atomic.Value // url.Values of the last FLV request
}

// Listen starts the server.
func (s *ReolinkServer) Listen(addr string) error {
	if s.Loc == nil {
		s.Loc = time.Local
	}
	s.tokens = map[string]time.Time{}
	en := 1
	if s.RTSPOff {
		en = 0
	}
	s.netPort = map[string]any{"httpEnable": 0, "httpPort": 80, "httpsEnable": 1, "httpsPort": 443, "mediaPort": 9000,
		"onvifEnable": 1, "onvifPort": 8000, "rtmpEnable": en, "rtmpPort": 1935, "rtspEnable": en, "rtspPort": s.RTSPPort}
	s.guard = map[string]any{"channel": 0, "benable": 1, "bexistPos": 1, "timeout": 60}
	s.ai = map[string]any{"channel": 0, "aiTrack": 1, "bSmartTrack": 1, "aiDisappearBackTime": 10, "aiStopBackTime": 30,
		"AiDetectType": map[string]any{"people": 1, "vehicle": 1, "dog_cat": 0, "face": 0}}
	s.presets = []map[string]any{
		{"channel": 0, "enable": 1, "id": 1, "name": "Einfahrt"},
		{"channel": 0, "enable": 1, "id": 2, "name": "Hof"},
		{"channel": 0, "enable": 0, "id": 3, "name": ""},
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/api.cgi", s.api)
	mux.HandleFunc("/flv", s.flv)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if s.HTTPOnly {
		go s.srv.Serve(ln)
		return nil
	}
	cert, err := selfSigned()
	if err != nil {
		ln.Close()
		return err
	}
	s.srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	go s.srv.ServeTLS(ln, "", "")
	return nil
}

// Addr returns the listen address.
func (s *ReolinkServer) Addr() string { return s.ln.Addr().String() }

// Close stops the server.
func (s *ReolinkServer) Close() { s.srv.Close() }

// PTZOps returns the PTZ commands received so far ("Left:32", "ToPos:2",
// "GuardToPos", …).
func (s *ReolinkServer) PTZOps() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ptzOps...)
}

// Guard returns the guard settings (benable, bexistPos, timeout).
func (s *ReolinkServer) Guard() (enabled, set bool, timeout int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return asNum(s.guard["benable"]) == 1, asNum(s.guard["bexistPos"]) == 1, int(asNum(s.guard["timeout"]))
}

// Tracking reports whether auto tracking is on.
func (s *ReolinkServer) Tracking() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return asNum(s.ai["bSmartTrack"]) == 1
}

// PortsEnabled reports RTSP and RTMP.
func (s *ReolinkServer) PortsEnabled() (rtsp, rtmp bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return asNum(s.netPort["rtspEnable"]) == 1, asNum(s.netPort["rtmpEnable"]) == 1
}

// LastFLV returns the query of the last HTTP-FLV request (without
// credentials).
func (s *ReolinkServer) LastFLV() url.Values {
	v, _ := s.lastFLV.Load().(url.Values)
	return v
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "sim"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour * 365),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

type reoCmd struct {
	Cmd    string         `json:"cmd"`
	Action int            `json:"action"`
	Param  map[string]any `json:"param"`
}

func reoOK(name string, v any) map[string]any {
	return map[string]any{"cmd": name, "code": 0, "value": v}
}

func reoErr(name, detail string, code int) map[string]any {
	return map[string]any{"cmd": name, "code": 1, "error": map[string]any{"detail": detail, "rspCode": code}}
}

func (s *ReolinkServer) api(w http.ResponseWriter, r *http.Request) {
	var cmds []reoCmd
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&cmds); err != nil || len(cmds) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	var out []map[string]any
	if cmds[0].Cmd == "Login" {
		out = append(out, s.login(cmds[0]))
	} else if !s.validToken(r.URL.Query().Get("token")) {
		for _, c := range cmds {
			out = append(out, reoErr(c.Cmd, "please login first", -6))
		}
	} else {
		for _, c := range cmds {
			out = append(out, s.command(c))
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *ReolinkServer) login(c reoCmd) map[string]any {
	u, _ := c.Param["User"].(map[string]any)
	if u == nil || u["userName"] != s.User || u["password"] != s.Pass {
		s.LoginFail.Add(1)
		return reoErr("Login", "login failed", -7)
	}
	s.Logins.Add(1)
	s.mu.Lock()
	s.tokenSeq++
	tok := fmt.Sprintf("simtok%04d", s.tokenSeq)
	s.tokens[tok] = time.Now().Add(time.Hour)
	s.mu.Unlock()
	return reoOK("Login", map[string]any{"Token": map[string]any{"name": tok, "leaseTime": 3600}})
}

func (s *ReolinkServer) validToken(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[tok]
	return ok && time.Now().Before(exp)
}

func paramChannel(p map[string]any) int {
	if v, ok := p["channel"]; ok {
		return int(asNum(v))
	}
	for _, x := range p {
		if m, ok := x.(map[string]any); ok {
			if v, ok := m["channel"]; ok {
				return int(asNum(v))
			}
		}
	}
	return 0
}

func (s *ReolinkServer) channels() int {
	if s.DualLens {
		return 2
	}
	return 1
}

func (s *ReolinkServer) command(c reoCmd) map[string]any {
	ch := paramChannel(c.Param)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch c.Cmd {
	case "Logout":
		return reoOK(c.Cmd, map[string]any{"rspCode": 200})
	case "GetDevInfo":
		return reoOK(c.Cmd, map[string]any{"DevInfo": map[string]any{"model": "SIM-IPC", "name": "Sim Kamera", "serial": "SIMREO0001",
			"firmVer": "v3.1.0.0_sim", "hardVer": "SIM", "channelNum": 1, "type": "IPC", "exactType": "IPC"}})
	case "GetChannelstatus":
		return reoErr(c.Cmd, "not support", -9)
	case "GetAbility":
		chn := map[string]any{"mainEncType": map[string]any{"permit": 0, "ver": 0}}
		if s.PTZ {
			chn["ptzType"] = map[string]any{"permit": 0, "ver": 2}
			chn["ptzPreset"] = map[string]any{"permit": 7, "ver": 1}
			chn["GetPtzGuard"] = map[string]any{"permit": 7, "ver": 1}
			chn["aiTrack"] = map[string]any{"permit": 7, "ver": 1}
			chn["supportPtzSpeed"] = map[string]any{"permit": 0, "ver": 1}
		}
		if s.DualLens {
			chn["supportAutoTrackStream"] = map[string]any{"permit": 0, "ver": 1}
		}
		return reoOK(c.Cmd, map[string]any{"Ability": map[string]any{"abilityChn": []any{chn}, "rtsp": map[string]any{"permit": 0, "ver": 3}}})
	case "GetNetPort":
		return reoOK(c.Cmd, map[string]any{"NetPort": s.netPort})
	case "SetNetPort":
		np, _ := c.Param["NetPort"].(map[string]any)
		for k, v := range np {
			s.netPort[k] = v
		}
		s.tokens = map[string]time.Time{} // like the real cameras: tokens become invalid
		return reoOK(c.Cmd, map[string]any{"rspCode": 200})
	case "GetTime":
		n := time.Now().In(s.Loc)
		return reoOK(c.Cmd, map[string]any{"Time": map[string]any{"year": n.Year(), "mon": int(n.Month()), "day": n.Day(),
			"hour": n.Hour(), "min": n.Minute(), "sec": n.Second(), "timeZone": 0}})
	case "GetHddInfo":
		return reoOK(c.Cmd, map[string]any{"HddInfo": []any{map[string]any{"capacity": 121000, "format": 1, "mount": 1, "number": 1, "size": 60000, "storageType": 2}}})
	case "GetEnc":
		if ch >= s.channels() {
			return reoErr(c.Cmd, "channel error", -4)
		}
		return reoOK(c.Cmd, map[string]any{"Enc": map[string]any{"channel": ch, "mainStream": map[string]any{"vType": "h264"}, "subStream": map[string]any{"vType": "h264"}}})
	case "GetRtspUrl":
		if ch >= s.channels() {
			return reoErr(c.Cmd, "channel error", -4)
		}
		base := fmt.Sprintf("rtsp://192.0.2.1:%d/Preview_%02d_", s.RTSPPort, ch+1)
		return reoOK(c.Cmd, map[string]any{"rtspUrl": map[string]any{"channel": ch, "mainStream": base + "main", "subStream": base + "sub"}})
	case "Search":
		return s.search(c)
	}
	if !s.PTZ {
		return reoErr(c.Cmd, "not support", -9)
	}
	switch c.Cmd {
	case "PtzCtrl":
		op, _ := c.Param["op"].(string)
		entry := op
		if v, ok := c.Param["speed"]; ok {
			entry += fmt.Sprintf(":%d", int(asNum(v)))
		}
		if v, ok := c.Param["id"]; ok {
			entry += fmt.Sprintf(":%d", int(asNum(v)))
		}
		s.ptzOps = append(s.ptzOps, entry)
		return reoOK(c.Cmd, map[string]any{"rspCode": 200})
	case "GetPtzPreset":
		return reoOK(c.Cmd, map[string]any{"PtzPreset": s.presets})
	case "GetPtzGuard":
		return reoOK(c.Cmd, map[string]any{"PtzGuard": s.guard})
	case "SetPtzGuard":
		g, _ := c.Param["PtzGuard"].(map[string]any)
		switch g["cmdStr"] {
		case "toPos":
			if s.FailGuardGoto.Load() {
				return reoErr(c.Cmd, "guard position not set", -4)
			}
			s.ptzOps = append(s.ptzOps, "GuardToPos")
		case "setPos":
			if asNum(g["bSaveCurrentPos"]) == 1 {
				s.guard["bexistPos"] = 1
				s.ptzOps = append(s.ptzOps, "GuardSave")
			}
			if v, ok := g["benable"]; ok {
				s.guard["benable"] = int(asNum(v))
			}
			if v, ok := g["timeout"]; ok {
				t := int(asNum(v))
				if t < 10 || t > 3600 {
					return reoErr(c.Cmd, "param error", -4)
				}
				s.guard["timeout"] = t
			}
		}
		return reoOK(c.Cmd, map[string]any{"rspCode": 200})
	case "GetAiCfg":
		return reoOK(c.Cmd, s.ai)
	case "SetAiCfg":
		for k, v := range c.Param {
			if k != "channel" {
				s.ai[k] = int(asNum(v))
			}
		}
		return reoOK(c.Cmd, map[string]any{"rspCode": 200})
	}
	return reoErr(c.Cmd, "not support", -9)
}

func reoTimeOf(v any, loc *time.Location) (time.Time, bool) {
	m, _ := v.(map[string]any)
	if m == nil {
		return time.Time{}, false
	}
	return time.Date(int(asNum(m["year"])), time.Month(int(asNum(m["mon"]))), int(asNum(m["day"])), int(asNum(m["hour"])),
		int(asNum(m["min"])), int(asNum(m["sec"])), 0, loc), true
}

func reoTimeMap(t time.Time) map[string]any {
	return map[string]any{"year": t.Year(), "mon": int(t.Month()), "day": t.Day(), "hour": t.Hour(), "min": t.Minute(), "sec": t.Second()}
}

// ReolinkFiles are the simulated recording files: 10 of every 15 minutes.
func ReolinkFiles(ch int, from, to time.Time) [][2]time.Time {
	var out [][2]time.Time
	for t := from.Truncate(15 * time.Minute); t.Before(to); t = t.Add(15 * time.Minute) {
		st, en := t, t.Add(10*time.Minute)
		if en.After(from) && st.Before(to) {
			out = append(out, [2]time.Time{st, en})
		}
	}
	return out
}

func reoFileName(ch int, st, en time.Time) string {
	return fmt.Sprintf("Mp4Record/%s/RecM%02d_%s_%s_6D28808_1A468F9.mp4", st.Format("2006-01-02"), ch+1, st.Format("20060102_150405"), en.Format("150405"))
}

var reoFileRe = regexp.MustCompile(`RecM(\d{2})_(\d{8}_\d{6})_(\d{6})_`)

func (s *ReolinkServer) search(c reoCmd) map[string]any {
	p, _ := c.Param["Search"].(map[string]any)
	ch := int(asNum(p["channel"]))
	logical := asNum(p["iLogicChannel"]) == 1
	switch {
	case logical && (!s.DualLens || s.TeleAsChannel):
		return reoErr(c.Cmd, "param error", -4)
	case ch == 1 && (!s.DualLens || !s.TeleAsChannel):
		return reoErr(c.Cmd, "channel error", -4)
	}
	if logical {
		ch = 1
	}
	st, ok1 := reoTimeOf(p["StartTime"], s.Loc)
	en, ok2 := reoTimeOf(p["EndTime"], s.Loc)
	if !ok1 || !ok2 || en.Before(st) {
		return reoErr(c.Cmd, "param error", -4)
	}
	if st.YearDay() != en.YearDay() || st.Year() != en.Year() {
		return reoErr(c.Cmd, "param error", -4) // one day per search
	}
	var files []any
	for _, f := range ReolinkFiles(ch, st, en) {
		files = append(files, map[string]any{"name": reoFileName(ch, f[0], f[1]), "StartTime": reoTimeMap(f[0]), "EndTime": reoTimeMap(f[1]),
			"size": 12345678, "type": "main", "frameRate": 15, "width": 1280, "height": 720})
	}
	res := map[string]any{"channel": ch, "Status": []any{map[string]any{"mon": int(st.Month()), "table": "1", "year": st.Year()}}}
	if len(files) > 0 {
		res["File"] = files
	}
	return reoOK(c.Cmd, map[string]any{"SearchResult": res})
}

// flv serves HTTP-FLV playback of one recorded file from `seek` seconds.
func (s *ReolinkServer) flv(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("user") != s.User || q.Get("password") != s.Pass {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	clean := url.Values{}
	for k, v := range q {
		if k != "user" && k != "password" {
			clean[k] = v
		}
	}
	s.lastFLV.Store(clean)
	s.FLVOpens.Add(1)
	m := reoFileRe.FindStringSubmatch(q.Get("start"))
	if m == nil || q.Get("stream") != "playback.bcs" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	st, err1 := time.ParseInLocation("20060102_150405", m[2], s.Loc)
	enT, err2 := time.ParseInLocation("150405", m[3], s.Loc)
	if err1 != nil || err2 != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	en := time.Date(st.Year(), st.Month(), st.Day(), enT.Hour(), enT.Minute(), enT.Second(), 0, s.Loc)
	ch, _ := strconv.Atoi(m[1])
	seek, _ := strconv.Atoi(q.Get("seek"))
	clip := s.Clip(ch-1, q.Get("type") == "0" || q.Get("type") == "2")
	w.Header().Set("Content-Type", "video/x-flv")
	bw := bufio.NewWriterSize(w, 64<<10)
	writeFLV(bw, clip, en.Sub(st), time.Duration(seek)*time.Second, s.FLVZeroAtSeek, r.Context().Done())
	bw.Flush()
}

// writeFLV writes an H.264 FLV stream of the clip from `seek` to `length`.
func writeFLV(w *bufio.Writer, clip *Clip, length, seek time.Duration, zeroAtSeek bool, stop <-chan struct{}) {
	w.Write([]byte{'F', 'L', 'V', 1, 0x01, 0, 0, 0, 9, 0, 0, 0, 0})
	tag := func(ts uint32, body []byte) {
		h := []byte{9, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), byte(ts >> 16), byte(ts >> 8), byte(ts), byte(ts >> 24), 0, 0, 0}
		w.Write(h)
		w.Write(body)
		var pts [4]byte
		binary.BigEndian.PutUint32(pts[:], uint32(len(h)+len(body)))
		w.Write(pts[:])
	}
	// AVC sequence header
	rec := []byte{1, clip.SPS[1], clip.SPS[2], clip.SPS[3], 0xFF, 0xE1, byte(len(clip.SPS) >> 8), byte(len(clip.SPS))}
	rec = append(rec, clip.SPS...)
	rec = append(rec, 1, byte(len(clip.PPS)>>8), byte(len(clip.PPS)))
	rec = append(rec, clip.PPS...)
	tag(0, append([]byte{0x17, 0, 0, 0, 0}, rec...))
	frameDur := time.Second / time.Duration(clip.FPS)
	first := int(seek / frameDur)
	// start at the keyframe before the seek position
	for first > 0 && !clip.AUs[first%len(clip.AUs)].Key {
		first--
	}
	total := int(length / frameDur)
	for i := first; i < total; i++ {
		select {
		case <-stop:
			return
		default:
		}
		au := clip.AUs[i%len(clip.AUs)]
		ts := time.Duration(i) * frameDur
		if zeroAtSeek {
			ts -= time.Duration(first) * frameDur
		}
		var body []byte
		if au.Key {
			body = []byte{0x17, 1, 0, 0, 0}
		} else {
			body = []byte{0x27, 1, 0, 0, 0}
		}
		for _, n := range splitNALs(au.Data) {
			var l [4]byte
			binary.BigEndian.PutUint32(l[:], uint32(len(n)))
			body = append(body, l[:]...)
			body = append(body, n...)
		}
		tag(uint32(ts/time.Millisecond), body)
		if i%50 == 0 {
			if w.Flush() != nil {
				return
			}
		}
	}
}

// ReolinkRoute resolves /h264Preview_01_main, /Preview_02_sub … (channel 1
// based in the route).
func ReolinkRoute(u *url.URL, _ *time.Location) (Route, bool) {
	m := reoPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return Route{}, false
	}
	ch, _ := strconv.Atoi(m[1])
	sub := 0
	if m[2] == "sub" {
		sub = 1
	}
	return Route{Channel: ch, Subtype: sub}, ch >= 1
}

var reoPathRe = regexp.MustCompile(`^/(?:h264|h265)?Preview_(\d{2})_(main|sub)$`)
