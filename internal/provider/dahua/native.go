package dahua

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnm/internal/provider"
)

// Native Dahua binary protocol (TCP 37777) of older recorders. The
// sequences below were verified on real hardware:
//
//	packet  = 32 byte header (magic[4], payload length LE[4], control[24]) + payload
//	login   = fixed challenge packet → b0010068 "Realm:…\r\nRandom:…\r\n"
//	          a0010060 "user&&<d2><d3>" → b0010068, header[20] == 1 on success
//	init    = a4 1, a4 7 (serial), a4 2, F4 GetParameterValues Decode.Cfg,
//	          a4 26, F4 GetParameterValues Record.General (like the SDK)
//	F4      = text commands ("Key:Value\r\n" lines), answered with f4000058
//	lookup  = a5000000 (channel, start, end) → b6000058 with one recording
//	          cluster (start, end, cluster number; 0 = none)
//	replay  = AddObject ControlConnection.Passive → connection id + port,
//	          data socket AckSubChannel, PlayBack.Stop, PlayBack.download
//	          (IsTime:1) → bb11ee00 packets with DHAV, empty packet = end;
//	          afterwards PlayBack.Stop + DeleteObject
//
// Live video stays on RTSP; the native connection is opened on demand for
// recordings only.

var challengePacket, _ = hex.DecodeString("a001006000000000c4a3af489956b6b451c22c9ca07259d5040200010000a1aa")

const (
	nativeHdrSize  = 32
	nativeMaxFrame = 16 << 20
)

var (
	magicLoginRsp = [4]byte{0xb0, 0x01, 0x00, 0x68}
	magicA4Rsp    = [4]byte{0xb4, 0x00, 0x00, 0x58}
	magicF4Rsp    = [4]byte{0xf4, 0x00, 0x00, 0x58}
	magicB6       = [4]byte{0xb6, 0x00, 0x00, 0x58}
	magicData     = [4]byte{0xbb, 0x11, 0xee, 0x00}
)

type nativeFrame struct {
	hdr     [nativeHdrSize]byte
	payload []byte
}

func (f nativeFrame) magic() [4]byte { return [4]byte(f.hdr[:4]) }

func encodeFrame(magic [4]byte, control []byte, payload []byte) []byte {
	b := make([]byte, nativeHdrSize, nativeHdrSize+len(payload))
	copy(b, magic[:])
	binary.LittleEndian.PutUint32(b[4:], uint32(len(payload)))
	copy(b[8:], control)
	return append(b, payload...)
}

func readFrame(r io.Reader) (nativeFrame, error) {
	var f nativeFrame
	if _, err := io.ReadFull(r, f.hdr[:]); err != nil {
		return f, err
	}
	n := binary.LittleEndian.Uint32(f.hdr[4:8])
	if n > nativeMaxFrame {
		return f, fmt.Errorf("native: implausible payload length %d", n)
	}
	f.payload = make([]byte, n)
	_, err := io.ReadFull(r, f.payload)
	return f, err
}

func a4(cmd uint32) []byte {
	ctl := make([]byte, 24)
	binary.LittleEndian.PutUint32(ctl, cmd)
	return encodeFrame([4]byte{0xa4}, ctl, nil)
}

// f4 builds a text command; lines are "Key:Value".
func f4(lines ...string) []byte {
	return encodeFrame([4]byte{0xf4}, nil, []byte(strings.Join(lines, "\r\n")+"\r\n\r\n"))
}

func md5Upper(s string) string {
	h := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

// superEncipher is the 8 character legacy password hash of old Dahua firmware.
func superEncipher(pass string) string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	sum := md5.Sum([]byte(pass))
	out := make([]byte, 8)
	for i := range out {
		out[i] = chars[(int(sum[2*i])+int(sum[2*i+1]))%62]
	}
	return string(out)
}

// nativeDigest is the login answer: d2 + d3 (see package comment).
func nativeDigest(user, pass, realm, random string) string {
	d1 := md5Upper(user + ":" + realm + ":" + pass)
	d2 := md5Upper(user + ":" + random + ":" + d1)
	d3 := md5Upper(user + ":" + random + ":" + superEncipher(pass))
	return d2 + d3
}

func compactTime(t time.Time) []byte {
	b := make([]byte, 7)
	binary.LittleEndian.PutUint16(b, uint16(t.Year()))
	b[2], b[3], b[4], b[5], b[6] = byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second())
	return b
}

// ampTime formats "YYYY&M&D&h&m&s" (no padding) for PlayBack.download.
func ampTime(t time.Time) string {
	return fmt.Sprintf("%d&%d&%d&%d&%d&%d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second())
}

// unpackTime decodes the packed DHTIME wall clock (location UTC = device fields).
func unpackTime(v uint32) time.Time {
	return time.Date(2000+int(v>>26&0x3F), time.Month(v>>22&0x0F), int(v>>17&0x1F), int(v>>12&0x1F), int(v>>6&0x3F), int(v&0x3F), 0, time.UTC)
}

var (
	reRealm  = regexp.MustCompile(`Realm:(.*?)\r\n`)
	reRandom = regexp.MustCompile(`Random:(\d+)\r\n`)
	reConnID = regexp.MustCompile(`ConnectionID:(\d+)`)
	rePort   = regexp.MustCompile(`Port:(\d+)`)
)

// nativeConn is an authenticated control connection. The protocol is
// strictly request → response, so requests are serialised.
type nativeConn struct {
	conn    net.Conn
	rd      *bufio.Reader
	mu      sync.Mutex
	session uint32
	serial  string
	host    string
}

// dialNative connects and logs in like the SDK.
// probeNative checks that the native protocol answers its login challenge
// (no credentials are sent, so it costs no login attempt).
func probeNative(ctx context.Context, host string, port int) error {
	conn, err := provider.Dial(ctx, host, port, 5*time.Second)
	if err != nil {
		return provider.ClassifyNetErr(err, "Nativer Port")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	c := &nativeConn{conn: conn, rd: bufio.NewReaderSize(conn, 64<<10), host: host}
	f, err := c.roundTrip(challengePacket)
	if err != nil {
		return err
	}
	text := string(f.payload)
	if f.magic() != magicLoginRsp || reRealm.FindStringSubmatch(text) == nil || reRandom.FindStringSubmatch(text) == nil {
		return provider.NewError(provider.KindProtocol, nil, "keine Dahua-Login-Challenge")
	}
	return nil
}

func dialNative(ctx context.Context, host string, port int, user, pass string) (*nativeConn, error) {
	conn, err := provider.Dial(ctx, host, port, 5*time.Second)
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "Nativer Port")
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	c := &nativeConn{conn: conn, rd: bufio.NewReaderSize(conn, 256<<10), host: host}
	if err := c.login(user, pass); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *nativeConn) roundTrip(req []byte) (nativeFrame, error) {
	_ = c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer c.conn.SetDeadline(time.Time{})
	if _, err := c.conn.Write(req); err != nil {
		return nativeFrame{}, provider.ClassifyNetErr(err, "Nativer Port")
	}
	f, err := readFrame(c.rd)
	if err != nil {
		return f, provider.ClassifyNetErr(err, "Nativer Port")
	}
	return f, nil
}

func (c *nativeConn) login(user, pass string) error {
	f, err := c.roundTrip(challengePacket)
	if err != nil {
		return err
	}
	text := string(f.payload)
	realm, random := reRealm.FindStringSubmatch(text), reRandom.FindStringSubmatch(text)
	if f.magic() != magicLoginRsp || realm == nil || random == nil {
		return provider.NewError(provider.KindProtocol, nil, "Nativer Login: unerwartete Challenge")
	}
	rnd, _ := strconv.ParseUint(random[1], 10, 32)
	if uint32(rnd) != binary.LittleEndian.Uint32(f.hdr[16:20]) {
		return provider.NewError(provider.KindProtocol, nil, "Nativer Login: Challenge inkonsistent")
	}
	c.session = uint32(rnd)
	ctl := make([]byte, 24)
	copy(ctl[16:], []byte{0x04, 0x02, 0x00, 0x08, 0x00, 0x00, 0xa1, 0xaa})
	f, err = c.roundTrip(encodeFrame([4]byte{0xa0, 0x01, 0x00, 0x60}, ctl, []byte(user+"&&"+nativeDigest(user, pass, realm[1], random[1]))))
	if err != nil {
		return err
	}
	if f.magic() != magicLoginRsp || binary.LittleEndian.Uint32(f.hdr[20:24]) != 1 {
		return provider.NewError(provider.KindAuth, nil, "Nativer Login abgelehnt – Benutzername oder Passwort falsch")
	}
	// initialise like the SDK; some firmware refuses later commands otherwise
	if _, err := c.roundTrip(a4(1)); err != nil {
		return err
	}
	if c.serial, err = c.readSerial(); err != nil {
		return err
	}
	for _, req := range [][]byte{a4(2), f4("TransactionID:1", "Method:GetParameterValues", "ParameterName:Dahua.Device.Decode.Cfg"),
		a4(26), f4("TransactionID:2", "Method:GetParameterValues", "ParameterName:Dahua.Device.Record.General")} {
		if _, err := c.roundTrip(req); err != nil {
			return err
		}
	}
	return nil
}

func (c *nativeConn) readSerial() (string, error) {
	f, err := c.roundTrip(a4(7))
	if err != nil {
		return "", err
	}
	if f.magic() != magicA4Rsp {
		return "", provider.NewError(provider.KindProtocol, nil, "Nativer Port: unerwartete Antwort auf Seriennummer")
	}
	if i := strings.IndexByte(string(f.payload), 0); i >= 0 {
		return string(f.payload[:i]), nil
	}
	return string(f.payload), nil
}

// command sends an F4 command and returns the answer text; FaultCode must be OK.
func (c *nativeConn) command(what string, lines ...string) (string, error) {
	f, err := c.roundTrip(f4(lines...))
	if err != nil {
		return "", err
	}
	text := string(f.payload)
	if f.magic() != magicF4Rsp || !strings.Contains(text, "FaultCode:OK") {
		return text, provider.NewError(provider.KindProtocol, nil, "Nativer Port: %s abgelehnt", what)
	}
	return text, nil
}

// cluster is one recording cluster reported by the A5/B6 lookup.
type cluster struct {
	start, end time.Time // device wall clock
	number     uint32
}

// lookup asks for the recording cluster in [from, to] (device wall clock);
// number 0 means no recording.
func (c *nativeConn) lookup(channel int, from, to time.Time) (cluster, error) {
	ctl := make([]byte, 24)
	ctl[0] = byte(channel + 1)
	copy(ctl[1:], compactTime(from))
	ctl[8] = 6
	f, err := c.roundTrip(encodeFrame([4]byte{0xa5}, ctl, compactTime(to)))
	if err != nil {
		return cluster{}, err
	}
	if f.magic() != magicB6 || len(f.payload) < 24 {
		return cluster{}, provider.NewError(provider.KindProtocol, nil, "Nativer Port: Aufnahmesuche fehlgeschlagen")
	}
	p := f.payload
	return cluster{start: unpackTime(binary.LittleEndian.Uint32(p[4:])), end: unpackTime(binary.LittleEndian.Uint32(p[8:])), number: binary.LittleEndian.Uint32(p[16:])}, nil
}

func (c *nativeConn) close() {
	hdr := make([]byte, nativeHdrSize)
	hdr[0] = 0x0a
	binary.LittleEndian.PutUint32(hdr[8:], c.session)
	_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.conn.Write(hdr)
	c.conn.Close()
}

// ---- playback ---------------------------------------------------------------------------------

// nativePlayback is the DHAV stream of one PlayBack.download.
type nativePlayback struct {
	c       *nativeConn
	data    net.Conn
	pr      *io.PipeReader
	connID  string
	channel int
	once    sync.Once
	done    func()
}

func (p *nativePlayback) Read(b []byte) (int, error) { return p.pr.Read(b) }

func (p *nativePlayback) Close() error {
	p.once.Do(func() {
		p.pr.Close()
		p.data.Close()
		go func() {
			p.c.mu.Lock()
			_, _ = p.c.command("PlayBack.Stop", "TransactionID:0", "Method:GetParameterNames", "ParameterName:Dahua.Device.Network.PlayBack.Stop", "channel:"+strconv.Itoa(p.channel+1), "ConnectionID:"+p.connID)
			_, _ = p.c.command("DeleteObject", "TransactionID:7", "Method:DeleteObject", "ParameterName:Dahua.Device.Network.ControlConnection.Passive", "ConnectionID:"+p.connID)
			p.c.mu.Unlock()
			if p.done != nil {
				p.done()
			}
		}()
	})
	return nil
}

// openPlayback starts a time based download of [from, to] (device wall clock).
func (c *nativeConn) openPlayback(ctx context.Context, channel int, from, to time.Time, done func()) (*nativePlayback, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	text, err := c.command("AddObject", "TransactionID:3", "Method:AddObject", "ParameterName:Dahua.Device.Network.ControlConnection.Passive", "ConnectProtocol:0")
	if err != nil {
		return nil, err
	}
	m := reConnID.FindStringSubmatch(text)
	if m == nil {
		return nil, provider.NewError(provider.KindProtocol, nil, "Nativer Port: keine ConnectionID")
	}
	connID := m[1]
	port := 0
	if pm := rePort.FindStringSubmatch(text); pm != nil {
		port, _ = strconv.Atoi(pm[1])
	}
	release := func() {
		_, _ = c.command("DeleteObject", "TransactionID:7", "Method:DeleteObject", "ParameterName:Dahua.Device.Network.ControlConnection.Passive", "ConnectionID:"+connID)
	}
	for _, req := range [][]byte{a4(26), f4("TransactionID:4", "Method:GetParameterValues", "ParameterName:Dahua.Device.Record.General"),
		a4(26), f4("TransactionID:5", "Method:GetParameterValues", "ParameterName:Dahua.Device.Record.General")} {
		if _, err := c.roundTrip(req); err != nil {
			release()
			return nil, err
		}
	}
	cl, err := c.lookup(channel, from, to)
	if err != nil {
		release()
		return nil, err
	}
	if cl.number == 0 {
		release()
		return nil, provider.NewError(provider.KindNotFound, nil, "keine Aufnahme im gewählten Zeitraum")
	}
	if port == 0 {
		_, p, _ := net.SplitHostPort(c.conn.RemoteAddr().String())
		port, _ = strconv.Atoi(p)
	}
	data, err := provider.Dial(ctx, c.host, port, 5*time.Second)
	if err != nil {
		release()
		return nil, provider.ClassifyNetErr(err, "Native Datenverbindung")
	}
	drd := bufio.NewReaderSize(data, 256<<10)
	_ = data.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = data.Write(f4("TransactionID:0", "Method:GetParameterNames", "ParameterName:Dahua.Device.Network.ControlConnection.AckSubChannel",
		"SessionID:"+strconv.FormatUint(uint64(c.session), 10), "ConnectionID:"+connID))
	var ack nativeFrame
	if err == nil {
		ack, err = readFrame(drd)
	}
	_ = data.SetDeadline(time.Time{})
	if err != nil || ack.magic() != magicF4Rsp || !strings.Contains(string(ack.payload), "FaultCode:OK") {
		data.Close()
		release()
		return nil, provider.NewError(provider.KindProtocol, err, "Native Datenverbindung abgelehnt")
	}
	ch := "channel:" + strconv.Itoa(channel+1)
	if _, err := c.command("PlayBack.Stop", "TransactionID:6", "Method:GetParameterNames", "ParameterName:Dahua.Device.Network.PlayBack.Stop", ch, "ConnectionID:"+connID); err != nil {
		data.Close()
		release()
		return nil, err
	}
	if _, err := c.command("PlayBack.download", "TransactionID:6", "Method:GetParameterNames", "ParameterName:Dahua.Device.Network.PlayBack.download", ch,
		"ConnectionID:"+connID, "StartTime:"+ampTime(from), "EndTime:"+ampTime(to), "DriveNo:0",
		"ClusterNo:"+strconv.FormatUint(uint64(cl.number), 10), "Hint:0", "Type:0", "IsTime:1", "OffLength:0"); err != nil {
		data.Close()
		release()
		return nil, err
	}
	pr, pw := io.Pipe()
	p := &nativePlayback{c: c, data: data, pr: pr, connID: connID, channel: channel, done: done}
	go func() {
		for {
			_ = data.SetReadDeadline(time.Now().Add(20 * time.Second))
			f, err := readFrame(drd)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			switch f.magic() {
			case magicF4Rsp:
				continue
			case magicData:
			default:
				pw.CloseWithError(errors.New("native: unexpected data frame"))
				return
			}
			if len(f.payload) == 0 {
				pw.Close() // end of recording
				return
			}
			if _, err := pw.Write(f.payload); err != nil {
				return
			}
		}
	}()
	return p, nil
}
