package sim

import (
	"bufio"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SofiaServer simulates a NETSurveillance/XMEye DVRIP device.
// Legacy=false: claim + start + media on the login socket (single socket).
// Legacy=true:  claim on a separate media socket, start on the control
// socket; a claim on the control socket is acknowledged but never streams.
type SofiaServer struct {
	User, Pass string
	Legacy     bool
	Channels   int
	Clip       func(channel int, main bool) *Clip
	// ChunkSize splits media into packets that cross frame boundaries.
	ChunkSize int
	// Recorded is the recorded period per channel (device wall clock,
	// location UTC) split into files of FileLength. Nil = last 2 hours.
	Recorded   func(channel int) (from, to time.Time)
	FileLength time.Duration
	// NoDisk reports no recording storage (an empty list; NullDisk: null).
	NoDisk   bool
	NullDisk bool

	ln           net.Listener
	Logins       atomic.Int32
	FailedLogins atomic.Int32
	Streams      atomic.Int32

	mu       sync.Mutex
	nextSess uint32
	sessions map[uint32]*sofiaSession
	conns    map[net.Conn]struct{}
}

type sofiaSession struct {
	pending map[string]net.Conn // "ch|stream" → claimed media conn (legacy)
	stops   map[string]chan struct{}
	// playback: file name → claimed media conn
	pbConns map[string]net.Conn
	pbStops map[string]chan struct{}
}

// DeviceNow is the simulated device wall clock: the local time of the host,
// labelled UTC (like CNM's provider.Clock device times).
func DeviceNow() time.Time {
	l := time.Now()
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC)
}

type simFile struct {
	name       string
	start, end time.Time
}

const dvripTimeFmt = "2006-01-02 15:04:05"

// files lists the recorded files of a channel overlapping [from, to].
func (s *SofiaServer) files(ch int, from, to time.Time) []simFile {
	rf, rt := DeviceNow().Add(-2*time.Hour), DeviceNow()
	if s.Recorded != nil {
		rf, rt = s.Recorded(ch)
	}
	l := s.FileLength
	if l <= 0 {
		l = 10 * time.Minute
	}
	var out []simFile
	for st := rf.Truncate(l); st.Before(rt); st = st.Add(l) {
		fs, fe := st, st.Add(l)
		if fs.Before(rf) {
			fs = rf
		}
		if fe.After(rt) {
			fe = rt
		}
		if fe.After(from) && fs.Before(to) {
			out = append(out, simFile{name: fmt.Sprintf("/idea0/%s/%03d/%s-%s[R][@1][0].h264", fs.Format("2006-01-02"), ch, fs.Format("15.04.05"), fe.Format("15.04.05")), start: fs, end: fe})
		}
	}
	return out
}

func sofiaHash(p string) string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	sum := md5.Sum([]byte(p))
	out := make([]byte, 0, 8)
	for i := 0; i < 16; i += 2 {
		out = append(out, chars[(uint16(sum[i])+uint16(sum[i+1]))%62])
	}
	return string(out)
}

// Listen starts the server.
func (s *SofiaServer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.sessions = map[uint32]*sofiaSession{}
	s.conns = map[net.Conn]struct{}{}
	s.nextSess = 0x10
	if s.ChunkSize == 0 {
		s.ChunkSize = 8192
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
func (s *SofiaServer) Addr() string { return s.ln.Addr().String() }

// Close stops the server.
func (s *SofiaServer) Close() {
	s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

func packet(session, seq uint32, msg uint16, payload []byte) []byte {
	b := make([]byte, 20, 20+len(payload))
	b[0] = 0xFF
	b[1] = 0x01
	binary.LittleEndian.PutUint32(b[4:], session)
	binary.LittleEndian.PutUint32(b[8:], seq)
	binary.LittleEndian.PutUint16(b[14:], msg)
	binary.LittleEndian.PutUint32(b[16:], uint32(len(payload)))
	return append(b, payload...)
}

func jsonP(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n', 0)
}

func (s *SofiaServer) serve(c net.Conn) {
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
	var session uint32
	loggedIn := false
	for {
		hdr := make([]byte, 20)
		if _, err := io.ReadFull(rd, hdr); err != nil {
			return
		}
		if hdr[0] != 0xFF {
			return
		}
		msg := binary.LittleEndian.Uint16(hdr[14:])
		n := binary.LittleEndian.Uint32(hdr[16:])
		payload := make([]byte, n)
		if _, err := io.ReadFull(rd, payload); err != nil {
			return
		}
		var cmd map[string]any
		json.Unmarshal([]byte(strings.TrimRight(string(payload), "\x00\n")), &cmd)
		switch msg {
		case 1000: // login
			if cmd["UserName"] != s.User || cmd["PassWord"] != sofiaHash(s.Pass) {
				s.FailedLogins.Add(1)
				write(packet(0, 0, 1001, jsonP(map[string]any{"Ret": 203, "SessionID": "0x00000000"})))
				continue
			}
			s.Logins.Add(1)
			s.mu.Lock()
			s.nextSess++
			session = s.nextSess
			s.sessions[session] = &sofiaSession{pending: map[string]net.Conn{}, stops: map[string]chan struct{}{}, pbConns: map[string]net.Conn{}, pbStops: map[string]chan struct{}{}}
			s.mu.Unlock()
			loggedIn = true
			write(packet(session, 0, 1001, jsonP(map[string]any{"Ret": 100, "SessionID": fmt.Sprintf("0x%08X", session), "AliveInterval": 20, "ChannelNum": s.Channels, "DeviceType ": "DVR"})))
		case 1006:
			write(packet(session, 0, 1007, jsonP(map[string]any{"Ret": 100, "Name": "KeepAlive"})))
		case 1020:
			if cmd["Name"] == "StorageInfo" {
				disks := []any{}
				if !s.NoDisk {
					disks = append(disks, map[string]any{"PlysicalNo": 0, "PartNumber": 1, "Partition": []any{map[string]any{"TotalSpace": "0x000E8E0D", "RemainSpace": "0x00001000", "IsCurrent": true}}})
				}
				var answer any = disks
				if s.NoDisk && s.NullDisk {
					answer = nil
				}
				write(packet(session, 0, 1021, jsonP(map[string]any{"Ret": 100, "Name": "StorageInfo", "StorageInfo": answer})))
				continue
			}
			write(packet(session, 0, 1021, jsonP(map[string]any{"Ret": 100, "Name": "SystemInfo", "SystemInfo": map[string]any{"DeviceModel": "SIM-DVRIP", "SerialNo": "SIMSOFIA1", "SoftWareVersion": "V4.02.SIM", "VideoInChannel": s.Channels}})))
		case 1452: // OPTimeQuery
			write(packet(session, 0, 1453, jsonP(map[string]any{"Ret": 100, "Name": "OPTimeQuery", "OPTimeQuery": DeviceNow().Format(dvripTimeFmt)})))
		case 1440: // OPFileQuery
			q, _ := cmd["OPFileQuery"].(map[string]any)
			from, e1 := time.Parse(dvripTimeFmt, fmt.Sprint(q["BeginTime"]))
			to, e2 := time.Parse(dvripTimeFmt, fmt.Sprint(q["EndTime"]))
			ch := int(toF(q["Channel"]))
			var fs []simFile
			if e1 == nil && e2 == nil && !s.NoDisk {
				fs = s.files(ch, from, to)
			}
			if len(fs) == 0 {
				write(packet(session, 0, 1441, jsonP(map[string]any{"Ret": 119, "Name": "OPFileQuery"})))
				continue
			}
			if len(fs) > 64 {
				fs = fs[:64]
			}
			rows := []any{}
			for _, f := range fs {
				rows = append(rows, map[string]any{"BeginTime": f.start.Format(dvripTimeFmt), "EndTime": f.end.Format(dvripTimeFmt), "FileName": f.name, "FileLength": "0x00010000", "DiskNo": 0, "SerialNo": 0})
			}
			write(packet(session, 0, 1441, jsonP(map[string]any{"Ret": 100, "Name": "OPFileQuery", "OPFileQuery": rows})))
		case 1424: // OPPlayBack Claim on the (not logged in) media connection
			op, _ := cmd["OPPlayBack"].(map[string]any)
			par, _ := op["Parameter"].(map[string]any)
			sid := parseSess(cmd["SessionID"])
			s.mu.Lock()
			sess := s.sessions[sid]
			s.mu.Unlock()
			if sess == nil || loggedIn {
				write(packet(sid, 0, 1425, jsonP(map[string]any{"Ret": 105})))
				continue
			}
			write(packet(sid, 0, 1425, jsonP(map[string]any{"Ret": 100})))
			s.mu.Lock()
			sess.pbConns[fmt.Sprint(par["FileName"])] = c
			s.mu.Unlock()
			<-waitClosed(rd)
			return
		case 1420: // OPPlayBack DownloadStart / DownloadStop on the control connection
			op, _ := cmd["OPPlayBack"].(map[string]any)
			par, _ := op["Parameter"].(map[string]any)
			name := fmt.Sprint(par["FileName"])
			ch := int(toF(par["Channel"]))
			s.mu.Lock()
			sess := s.sessions[session]
			s.mu.Unlock()
			if sess == nil {
				continue
			}
			if op["Action"] == "DownloadStop" {
				s.mu.Lock()
				if st := sess.pbStops[name]; st != nil {
					close(st)
					delete(sess.pbStops, name)
				}
				s.mu.Unlock()
				write(packet(session, 0, 1421, jsonP(map[string]any{"Ret": 100})))
				continue
			}
			s.mu.Lock()
			media := sess.pbConns[name]
			delete(sess.pbConns, name)
			stop := make(chan struct{})
			sess.pbStops[name] = stop
			s.mu.Unlock()
			var file *simFile
			for _, f := range s.files(ch, time.Time{}, DeviceNow().Add(time.Hour)) {
				if f.name == name {
					f := f
					file = &f
				}
			}
			clip := s.Clip(ch, true)
			if media == nil || file == nil || clip == nil {
				write(packet(session, 0, 1421, jsonP(map[string]any{"Ret": 103})))
				continue
			}
			write(packet(session, 0, 1421, jsonP(map[string]any{"Ret": 100})))
			// like real firmware, ByName playback honours StartTime inside the file
			from := file.start
			if st, err := time.Parse(dvripTimeFmt, fmt.Sprint(op["StartTime"])); err == nil && st.After(file.start) && st.Before(file.end) {
				from = st
			}
			go s.download(clip, session, *file, from, media, stop)
		case 1048:
			var titles []string
			for i := 0; i < s.Channels; i++ {
				titles = append(titles, fmt.Sprintf("XM CAM %d", i+1))
			}
			write(packet(session, 0, 1049, jsonP(map[string]any{"Ret": 100, "ChannelTitle": titles})))
		case 1413: // claim
			op, _ := cmd["OPMonitor"].(map[string]any)
			par, _ := op["Parameter"].(map[string]any)
			ch := int(toF(par["Channel"]))
			main := par["StreamType"] == "Main"
			sid := parseSess(cmd["SessionID"])
			s.mu.Lock()
			sess := s.sessions[sid]
			s.mu.Unlock()
			if sess == nil {
				write(packet(sid, 0, 1414, jsonP(map[string]any{"Ret": 105})))
				continue
			}
			write(packet(sid, 0, 1414, jsonP(map[string]any{"Ret": 100})))
			if !loggedIn && s.Legacy {
				// this connection is the media socket of a legacy session
				s.mu.Lock()
				sess.pending[key(ch, main)] = c
				s.mu.Unlock()
				<-waitClosed(rd) // keep the socket; streaming happens from the control side
				return
			}
		case 1410: // start / stop
			op, _ := cmd["OPMonitor"].(map[string]any)
			par, _ := op["Parameter"].(map[string]any)
			ch := int(toF(par["Channel"]))
			main := par["StreamType"] == "Main"
			action, _ := op["Action"].(string)
			s.mu.Lock()
			sess := s.sessions[session]
			s.mu.Unlock()
			if sess == nil {
				continue
			}
			k := key(ch, main)
			if action == "Stop" {
				s.mu.Lock()
				if st := sess.stops[k]; st != nil {
					close(st)
					delete(sess.stops, k)
				}
				s.mu.Unlock()
				write(packet(session, 0, 1411, jsonP(map[string]any{"Ret": 100})))
				continue
			}
			clip := s.Clip(ch, main)
			if clip == nil {
				write(packet(session, 0, 1411, jsonP(map[string]any{"Ret": 103})))
				continue
			}
			stop := make(chan struct{})
			s.mu.Lock()
			sess.stops[k] = stop
			media := sess.pending[k]
			delete(sess.pending, k)
			s.mu.Unlock()
			if s.Legacy {
				write(packet(session, 0, 1411, jsonP(map[string]any{"Ret": 100})))
				if media == nil {
					continue // claim was on the control socket: legacy firmware never streams
				}
				go s.stream(clip, session, func(b []byte) error {
					_ = media.SetWriteDeadline(time.Now().Add(10 * time.Second))
					_, err := media.Write(b)
					return err
				}, stop, func() { media.Close() })
			} else {
				go s.stream(clip, session, write, stop, func() {})
			}
		}
	}
}

func waitClosed(rd *bufio.Reader) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		io.Copy(io.Discard, rd)
		close(ch)
	}()
	return ch
}

func key(ch int, main bool) string { return strconv.Itoa(ch) + "|" + strconv.FormatBool(main) }

func toF(v any) float64 {
	f, _ := v.(float64)
	return f
}

func parseSess(v any) uint32 {
	s, _ := v.(string)
	n, _ := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(s), "0x"), 16, 32)
	return uint32(n)
}

// SofiaFrames wraps an access unit in a DVRIP media frame.
func SofiaFrames(au AU) []byte { return SofiaFrameAt(au, 15, 0) }

// SofiaFrameAt wraps an access unit; I-frames carry fps and the packed
// recording wall clock.
func SofiaFrameAt(au AU, fps int, stamp uint32) []byte {
	if au.Key {
		h := make([]byte, 16)
		h[2], h[3] = 1, 0xFC
		h[4] = 0x02 // H.264
		h[5] = byte(fps)
		binary.LittleEndian.PutUint32(h[8:], stamp)
		binary.LittleEndian.PutUint32(h[12:], uint32(len(au.Data)))
		return append(h, au.Data...)
	}
	h := make([]byte, 8)
	h[2], h[3] = 1, 0xFD
	binary.LittleEndian.PutUint32(h[4:], uint32(len(au.Data)))
	return append(h, au.Data...)
}

// PackTime packs a wall clock like the DVRIP frame header.
func PackTime(t time.Time) uint32 {
	return uint32(t.Year()-2000)<<26 | uint32(t.Month())<<22 | uint32(t.Day())<<17 | uint32(t.Hour())<<12 | uint32(t.Minute())<<6 | uint32(t.Second())
}

// download sends a recorded file as fast as the connection allows (like the
// real "download" playback), then an empty chunk.
func (s *SofiaServer) download(clip *Clip, session uint32, f simFile, from time.Time, media net.Conn, stop chan struct{}) {
	s.Streams.Add(1)
	defer s.Streams.Add(-1)
	defer media.Close()
	f.start = from
	frames := int(f.end.Sub(f.start).Seconds()) * clip.FPS
	var pending []byte
	flush := func(force bool) bool {
		for len(pending) >= 32768 || (force && len(pending) > 0) {
			n := len(pending)
			if n > 32768 {
				n = 32768
			}
			_ = media.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := media.Write(packet(session, 0, 1426, pending[:n])); err != nil {
				return false
			}
			pending = pending[n:]
		}
		return true
	}
	for i := 0; i < frames; i++ {
		select {
		case <-stop:
			return
		default:
		}
		au := clip.AUs[i%len(clip.AUs)]
		var stamp uint32
		if au.Key {
			stamp = PackTime(f.start.Add(time.Duration(i) * time.Second / time.Duration(clip.FPS)))
		}
		pending = append(pending, SofiaFrameAt(au, clip.FPS, stamp)...)
		if !flush(false) {
			return
		}
	}
	if flush(true) {
		_, _ = media.Write(packet(session, 0, 1426, nil))
	}
}

func (s *SofiaServer) stream(clip *Clip, session uint32, write func([]byte) error, stop chan struct{}, done func()) {
	s.Streams.Add(1)
	defer s.Streams.Add(-1)
	defer done()
	frameDur := time.Second / time.Duration(clip.FPS)
	start := time.Now()
	var pending []byte
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		default:
		}
		pending = append(pending, SofiaFrames(clip.AUs[i%len(clip.AUs)])...)
		// every 7th frame an audio frame is interleaved
		if i%7 == 3 {
			pending = append(pending, 0, 0, 1, 0xFA, 0x0E, 0x02, 4, 0, 1, 2, 3, 4)
		}
		// send fixed size chunks so frames straddle packet boundaries
		for len(pending) >= s.ChunkSize || (i%3 == 2 && len(pending) > 0) {
			n := len(pending)
			if n > s.ChunkSize {
				n = s.ChunkSize
			}
			if err := write(packet(session, 0, 1412, pending[:n])); err != nil {
				return
			}
			pending = pending[n:]
		}
		next := start.Add(time.Duration(i+1) * frameDur)
		select {
		case <-stop:
			return
		case <-time.After(time.Until(next)):
		}
	}
}
