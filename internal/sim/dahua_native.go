package sim

import (
	"bufio"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// NativeCluster is one recording cluster (device wall clock, location UTC).
type NativeCluster struct {
	Start, End time.Time
}

// DahuaNativeServer simulates the native port of an old Dahua recorder (Dahua
// binary protocol): challenge login, A4/F4 commands, A5/B6 cluster lookup and
// time based download over a passive data connection.
type DahuaNativeServer struct {
	User, Pass string
	Serial     string
	Clip       *Clip
	Recorded   func(channel int) []NativeCluster

	ln         net.Listener
	Logins     atomic.Int32
	Failed     atomic.Int32
	Downloads  atomic.Int32
	Stops      atomic.Int32
	Deletes    atomic.Int32
	mu         sync.Mutex
	data       map[string]net.Conn // connection id → data socket
	nextConnID int
	conns      map[net.Conn]struct{}
}

const nativeRandom = 41234567

// Listen starts the server.
func (s *DahuaNativeServer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.data = map[string]net.Conn{}
	s.conns = map[net.Conn]struct{}{}
	s.nextConnID = 4
	if s.Serial == "" {
		s.Serial = "SIMNATIVE001"
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
func (s *DahuaNativeServer) Addr() string { return s.ln.Addr().String() }

// Close stops the server.
func (s *DahuaNativeServer) Close() {
	s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

func nativePacket(magic string, ctl []byte, payload []byte) []byte {
	b := make([]byte, 32, 32+len(payload))
	m, _ := hex.DecodeString(magic)
	copy(b, m)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(payload)))
	copy(b[8:], ctl)
	return append(b, payload...)
}

func md5U(s string) string {
	h := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

func superEnc(p string) string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	sum := md5.Sum([]byte(p))
	out := make([]byte, 8)
	for i := range out {
		out[i] = chars[(int(sum[2*i])+int(sum[2*i+1]))%62]
	}
	return string(out)
}

var nativeKV = regexp.MustCompile(`(?m)^([A-Za-z]+):(.*?)\r?$`)

func kv(text string) map[string]string {
	out := map[string]string{}
	for _, m := range nativeKV.FindAllStringSubmatch(text, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func compact(b []byte) time.Time {
	return time.Date(int(binary.LittleEndian.Uint16(b)), time.Month(b[2]), int(b[3]), int(b[4]), int(b[5]), int(b[6]), 0, time.UTC)
}

func ampParse(v string) (time.Time, bool) {
	p := strings.Split(v, "&")
	if len(p) != 6 {
		return time.Time{}, false
	}
	n := make([]int, 6)
	for i, x := range p {
		n[i], _ = strconv.Atoi(x)
	}
	return time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, time.UTC), true
}

func (s *DahuaNativeServer) clusterFor(ch int, from, to time.Time) (NativeCluster, int) {
	if s.Recorded == nil {
		return NativeCluster{}, 0
	}
	for i, c := range s.Recorded(ch) {
		if c.End.After(from) && c.Start.Before(to) {
			return c, i + 1
		}
	}
	return NativeCluster{}, 0
}

func (s *DahuaNativeServer) serve(c net.Conn) {
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	rd := bufio.NewReader(c)
	authed := false
	for {
		hdr := make([]byte, 32)
		if _, err := io.ReadFull(rd, hdr); err != nil {
			return
		}
		payload := make([]byte, binary.LittleEndian.Uint32(hdr[4:8]))
		if _, err := io.ReadFull(rd, payload); err != nil {
			return
		}
		switch {
		case hdr[0] == 0xa0 && len(payload) == 0: // challenge
			ctl := make([]byte, 24)
			binary.LittleEndian.PutUint32(ctl[8:], nativeRandom)
			c.Write(nativePacket("b0010068", ctl, []byte(fmt.Sprintf("Realm:Login to SIMNATIVE\r\nRandom:%d\r\n", nativeRandom))))
		case hdr[0] == 0xa0: // login "user&&d2d3"
			user, auth, _ := strings.Cut(string(payload), "&&")
			r := strconv.Itoa(nativeRandom)
			want := md5U(user+":"+r+":"+md5U(user+":Login to SIMNATIVE:"+s.Pass)) + md5U(user+":"+r+":"+superEnc(s.Pass))
			ctl := make([]byte, 24)
			if user == s.User && auth == want {
				authed = true
				s.Logins.Add(1)
				binary.LittleEndian.PutUint32(ctl[12:], 1)
			} else {
				s.Failed.Add(1)
			}
			c.Write(nativePacket("b0010068", ctl, nil))
		case hdr[0] == 0xa4 && authed:
			if binary.LittleEndian.Uint32(hdr[8:]) == 7 {
				c.Write(nativePacket("b4000058", nil, append([]byte(s.Serial), 0, 0, 0)))
			} else {
				c.Write(nativePacket("b4000058", nil, nil))
			}
		case hdr[0] == 0xa5 && authed:
			ch := int(hdr[8]) - 1
			cl, no := s.clusterFor(ch, compact(hdr[9:16]), compact(payload))
			p := make([]byte, 24)
			binary.LittleEndian.PutUint32(p, 24)
			if no > 0 {
				binary.LittleEndian.PutUint32(p[4:], PackTime(cl.Start))
				binary.LittleEndian.PutUint32(p[8:], PackTime(cl.End))
				binary.LittleEndian.PutUint32(p[16:], uint32(no))
			}
			c.Write(nativePacket("b6000058", nil, p))
		case hdr[0] == 0xf4:
			m := kv(string(payload))
			name := m["ParameterName"]
			ok := "FaultCode:OK\r\n"
			switch {
			case strings.HasSuffix(name, "AckSubChannel"): // first message on a data connection
				s.mu.Lock()
				s.data[m["ConnectionID"]] = c
				s.mu.Unlock()
				c.Write(nativePacket("f4000058", nil, []byte("TransactionID:0\r\n"+ok)))
				<-waitClosed(rd)
				return
			case !authed:
				return
			case m["Method"] == "AddObject":
				s.mu.Lock()
				s.nextConnID++
				id := s.nextConnID
				s.mu.Unlock()
				_, port, _ := net.SplitHostPort(s.ln.Addr().String())
				c.Write(nativePacket("f4000058", nil, []byte(fmt.Sprintf("TransactionID:3\r\n%sConnectionID:%d\r\nPort:%s\r\n", ok, id, port))))
			case m["Method"] == "DeleteObject":
				s.Deletes.Add(1)
				c.Write(nativePacket("f4000058", nil, []byte(ok)))
			case strings.HasSuffix(name, "PlayBack.Stop"):
				s.Stops.Add(1)
				c.Write(nativePacket("f4000058", nil, []byte(ok)))
			case strings.HasSuffix(name, "PlayBack.download"):
				ch, _ := strconv.Atoi(m["channel"])
				from, ok1 := ampParse(m["StartTime"])
				to, ok2 := ampParse(m["EndTime"])
				s.mu.Lock()
				dc := s.data[m["ConnectionID"]]
				s.mu.Unlock()
				if !ok1 || !ok2 || dc == nil || m["IsTime"] != "1" {
					c.Write(nativePacket("f4000058", nil, []byte("FaultCode:Failed\r\n")))
					continue
				}
				c.Write(nativePacket("f4000058", nil, []byte(ok)))
				s.Downloads.Add(1)
				go s.download(dc, ch-1, from, to)
			default:
				c.Write(nativePacket("f4000058", nil, []byte(ok)))
			}
		case hdr[0] == 0x0a: // logout
			return
		}
	}
}

// download sends DHAV for [from, to] ∩ recorded clusters, then an empty frame.
func (s *DahuaNativeServer) download(dc net.Conn, ch int, from, to time.Time) {
	for _, cl := range s.Recorded(ch) {
		st, en := cl.Start, cl.End
		if st.Before(from) {
			st = from
		}
		if en.After(to) {
			en = to
		}
		if !en.After(st) {
			continue
		}
		dhav := DHAV(s.Clip, st, int(en.Sub(st).Seconds())*s.Clip.FPS)
		for len(dhav) > 0 {
			n := min(len(dhav), 16384)
			_ = dc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := dc.Write(nativePacket("bb11ee00", nil, dhav[:n])); err != nil {
				return
			}
			dhav = dhav[n:]
		}
	}
	_, _ = dc.Write(nativePacket("bb11ee00", nil, nil))
}
