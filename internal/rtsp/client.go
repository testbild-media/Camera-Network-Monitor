// Package rtsp is a minimal RTSP 1.0 client (RFC 2326) that pulls one H.264
// or H.265 video track over TCP (RTP/AVP/TCP interleaved) and delivers
// Annex-B access units.
//
// CNM uses it where FFmpeg cannot be the RTSP client, e.g. ONVIF replay
// (Profile G), which needs "Require: onvif-replay" and "Range: clock=…"
// headers on PLAY and carries the recording time in an RTP header
// extension:
//
//	c, err := rtsp.Dial(ctx, uri, rtsp.Options{User: u, Pass: p, SetupHeader: h})
//	err = c.Play(ctx, map[string]string{"Range": "clock=20260101T120000Z-"})
//	err = c.Read(func(au rtsp.AccessUnit) error { … })
//	c.Close() // TEARDOWN
//
// Basic and Digest authentication are supported; a rejected answer to a
// challenge is reported as a provider.KindAuth error and never retried.
package rtsp

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnm/internal/provider"
)

// Options configure a client.
type Options struct {
	// User and Pass answer Basic/Digest challenges. Credentials embedded in
	// the URL are used when User is empty; they never appear in requests.
	User, Pass string
	// SetupHeader is added to the SETUP request (e.g. "Require: onvif-replay").
	SetupHeader map[string]string
	// Timeout bounds connecting and every request/response exchange (default 10 s).
	Timeout time.Duration
	// IdleTimeout ends Read with ErrIdle when no data arrives (default 10 s).
	IdleTimeout time.Duration
}

var (
	// ErrIdle is returned by Read when the server stopped sending media
	// (ONVIF replay servers do this at the end of the recorded footage).
	ErrIdle = errors.New("rtsp: no media data received")
	// ErrClosed is returned by Read after Close.
	ErrClosed = errors.New("rtsp: client closed")
)

// StatusError is a non-success RTSP status. It is wrapped in a classified
// provider.Error.
type StatusError struct {
	Method string
	Code   int
	Reason string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("rtsp: %s: %d %s", e.Method, e.Code, e.Reason)
}

// StatusCode returns the RTSP status of a failed request, 0 if err is not a
// status error.
func StatusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// Client is one RTSP session with a single video track.
type Client struct {
	opt        Options
	conn       net.Conn
	rd         *bufio.Reader
	user, pass string
	reqURL     string // request URL without userinfo
	aggregate  string // control URL for PLAY / GET_PARAMETER / TEARDOWN
	track      Track
	rtpCh      int
	rtcpCh     int

	wmu       sync.Mutex // serialises writes and CSeq / auth state
	cseq      int
	challenge string
	session   string
	sessTO    time.Duration

	playing   bool     // PLAY sent: interleaved data may precede its response
	early     [][]byte // interleaved frames received before the PLAY response
	closed    chan struct{}
	closeOnce sync.Once
}

// Dial connects, sends DESCRIBE and SETUP for the first H.264/H.265 video
// track. ctx bounds the handshake only.
func Dial(ctx context.Context, rawURL string, opt Options) (*Client, error) {
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	if opt.IdleTimeout <= 0 {
		opt.IdleTimeout = 10 * time.Second
	}
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Scheme, "rtsp") || u.Hostname() == "" {
		return nil, provider.NewError(provider.KindConfig, nil, "RTSP: ungültige URL")
	}
	c := &Client{opt: opt, user: opt.User, pass: opt.Pass, rtpCh: 0, rtcpCh: 1, closed: make(chan struct{})}
	if c.user == "" && u.User != nil {
		c.user = u.User.Username()
		c.pass, _ = u.User.Password()
	}
	u.User = nil
	c.reqURL = u.String()
	port := 554
	if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 {
		port = p
	}
	conn, err := provider.Dial(ctx, u.Hostname(), port, opt.Timeout)
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "RTSP")
	}
	c.conn = conn
	c.rd = bufio.NewReaderSize(conn, 256<<10)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := c.describe(); err != nil {
		conn.Close()
		return nil, ctxErr(ctx, err)
	}
	if err := c.setup(); err != nil {
		conn.Close()
		return nil, ctxErr(ctx, err)
	}
	return c, nil
}

// ctxErr prefers the context error when the handshake was aborted by ctx.
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return provider.ClassifyNetErr(ctx.Err(), "RTSP")
	}
	return err
}

// Track returns the selected video track.
func (c *Client) Track() Track { return c.track }

func (c *Client) describe() error {
	res, err := c.do("DESCRIBE", c.reqURL, map[string]string{"Accept": "application/sdp"})
	if err != nil {
		return err
	}
	base := res.header["content-base"]
	if base == "" {
		base = res.header["content-location"]
	}
	if base == "" {
		base = c.reqURL
	}
	base = stripUserinfo(base)
	t, aggregate, ok := parseSDP(res.body, base)
	if !ok {
		return provider.NewError(provider.KindUnsupported, nil, "RTSP: kein H.264-/H.265-Videostrom angeboten")
	}
	c.track, c.aggregate = t, aggregate
	return nil
}

func (c *Client) setup() error {
	hdr := map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1"}
	for k, v := range c.opt.SetupHeader {
		hdr[k] = v
	}
	res, err := c.do("SETUP", c.track.Control, hdr)
	if err != nil {
		return err
	}
	tr := res.header["transport"]
	if tr != "" && !strings.Contains(strings.ToUpper(tr), "TCP") {
		return provider.NewError(provider.KindUnsupported, nil, "RTSP: Server bietet keinen TCP-Transport an")
	}
	for _, part := range strings.Split(tr, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "interleaved="); ok {
			a, b, _ := strings.Cut(v, "-")
			if n, err := strconv.Atoi(a); err == nil && n >= 0 && n < 256 {
				c.rtpCh, c.rtcpCh = n, n+1
				if m, err := strconv.Atoi(b); err == nil && m >= 0 && m < 256 {
					c.rtcpCh = m
				}
			}
		}
	}
	id, params, _ := strings.Cut(res.header["session"], ";")
	c.session = strings.TrimSpace(id)
	if c.session == "" {
		return provider.NewError(provider.KindProtocol, nil, "RTSP: SETUP ohne Session-Kennung")
	}
	c.sessTO = 60 * time.Second
	for _, p := range strings.Split(params, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(p), "timeout="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				c.sessTO = time.Duration(n) * time.Second
			}
		}
	}
	return nil
}

// Play starts the stream. header is added to the request, e.g. ONVIF replay:
// "Require: onvif-replay", "Range: clock=20260101T120000Z-20260101T121500Z",
// "Rate-Control: yes". A keepalive (GET_PARAMETER) runs until Close.
func (c *Client) Play(ctx context.Context, header map[string]string) error {
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	c.playing = true
	if _, err := c.do("PLAY", c.aggregate, header); err != nil {
		return ctxErr(ctx, err)
	}
	go c.keepalive()
	return nil
}

func (c *Client) keepalive() {
	interval := 25 * time.Second
	if t := c.sessTO / 2; t < interval {
		interval = max(t, time.Second)
	}
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-tk.C:
			// the answer is consumed (and ignored) by Read
			if err := c.send("GET_PARAMETER", c.aggregate, nil); err != nil {
				return
			}
		}
	}
}

// Read delivers access units until the stream ends. It returns nil when the
// server closed the connection or sent RTCP BYE, ErrIdle when no data arrived
// for Options.IdleTimeout, ErrClosed after Close, or the error returned by fn.
func (c *Client) Read(fn func(AccessUnit) error) error {
	d := newDepacketizer(c.track)
	end := func() error { // flush a complete last frame
		if d.fuOn {
			return nil
		}
		return d.flush(fn)
	}
	handle := func(ch int, pkt []byte) (bool, error) {
		switch ch {
		case c.rtpCh:
			return false, d.push(pkt, fn)
		case c.rtcpCh:
			return isBye(pkt), nil
		}
		return false, nil
	}
	early := c.early
	c.early = nil
	for _, f := range early {
		bye, err := handle(int(f[0]), f[1:])
		if err != nil {
			return err
		}
		if bye {
			return end()
		}
	}
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(c.opt.IdleTimeout))
		b, err := c.rd.Peek(1)
		if err == nil && b[0] == '$' {
			var ch int
			var pkt []byte
			if ch, pkt, err = c.readFrame(); err == nil {
				bye, herr := handle(ch, pkt)
				if herr != nil {
					return herr
				}
				if bye {
					return end()
				}
				continue
			}
		}
		if err == nil {
			// answer to a keepalive or a request from the server: skip it
			_, err = c.readMessage()
		}
		if err != nil {
			select {
			case <-c.closed:
				return ErrClosed
			default:
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return ErrIdle
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return end()
			}
			return provider.ClassifyNetErr(err, "RTSP")
		}
	}
}

func isBye(pkt []byte) bool {
	for len(pkt) >= 4 {
		if pkt[1] == 203 {
			return true
		}
		n := 4 * (int(binary.BigEndian.Uint16(pkt[2:])) + 1)
		if n > len(pkt) {
			return false
		}
		pkt = pkt[n:]
	}
	return false
}

// Close sends TEARDOWN (best effort, skipped while another write is stuck)
// and closes the connection.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.session != "" && c.wmu.TryLock() {
			_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, _ = c.sendLocked("TEARDOWN", c.aggregate, nil)
			c.wmu.Unlock()
		}
		c.conn.Close()
	})
	return nil
}

// ---- requests ------------------------------------------------------------------------------

type response struct {
	status int
	reason string
	header map[string]string // lower case keys
	body   []byte
}

// send writes a request and returns its CSeq.
func (c *Client) send(method, uri string, hdr map[string]string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.sendLocked(method, uri, hdr)
	return err
}

func (c *Client) sendLocked(method, uri string, hdr map[string]string) (int, error) {
	c.cseq++
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: CNM\r\n", method, uri, c.cseq)
	if c.challenge != "" {
		auth, err := provider.DigestAuthorization(c.challenge, c.user, c.pass, method, uri)
		if err != nil {
			return 0, provider.NewError(provider.KindProtocol, err, "RTSP-Authentifizierung nicht unterstützt")
		}
		fmt.Fprintf(&b, "Authorization: %s\r\n", auth)
	}
	if c.session != "" && method != "SETUP" {
		fmt.Fprintf(&b, "Session: %s\r\n", c.session)
	}
	keys := make([]string, 0, len(hdr))
	for k := range hdr {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\r\n", k, hdr[k])
	}
	b.WriteString("\r\n")
	if c.closedNow() && method != "TEARDOWN" {
		return 0, ErrClosed
	}
	if method != "TEARDOWN" {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.opt.Timeout))
	}
	_, err := io.WriteString(c.conn, b.String())
	return c.cseq, err
}

func (c *Client) closedNow() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// do performs a request/response exchange. A 401 is answered once with the
// offered challenge; a second 401 is an authentication error.
func (c *Client) do(method, uri string, hdr map[string]string) (*response, error) {
	what := "RTSP " + method
	for attempt := 0; ; attempt++ {
		c.wmu.Lock()
		cseq, err := c.sendLocked(method, uri, hdr)
		c.wmu.Unlock()
		if err != nil {
			return nil, provider.ClassifyNetErr(err, what)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(c.opt.Timeout))
		res, err := c.response(cseq)
		if err != nil {
			return nil, provider.ClassifyNetErr(err, what)
		}
		if res.status == 401 {
			ch := res.header["www-authenticate"]
			if attempt > 0 || ch == "" || (c.user == "" && c.pass == "") {
				return nil, provider.NewError(provider.KindAuth, statusErr(method, res), "RTSP: Benutzername oder Passwort falsch")
			}
			c.wmu.Lock()
			c.challenge = ch
			c.wmu.Unlock()
			continue
		}
		if res.status < 200 || res.status > 299 {
			return nil, classifyStatus(method, res)
		}
		return res, nil
	}
}

func statusErr(method string, res *response) *StatusError {
	return &StatusError{Method: method, Code: res.status, Reason: res.reason}
}

func classifyStatus(method string, res *response) error {
	se := statusErr(method, res)
	switch res.status {
	case 403:
		return provider.NewError(provider.KindAuth, se, "RTSP %s: Zugriff verweigert", method)
	case 404, 457:
		return provider.NewError(provider.KindNotFound, se, "RTSP %s: Status %d (nicht gefunden / ungültiger Zeitraum)", method, res.status)
	case 405, 461, 501, 551:
		return provider.NewError(provider.KindUnsupported, se, "RTSP %s: vom Server nicht unterstützt (Status %d)", method, res.status)
	}
	return provider.NewError(provider.KindProtocol, se, "RTSP %s: Status %d", method, res.status)
}

// response reads messages until the answer with the given CSeq arrives.
// Interleaved data in between is kept after PLAY and dropped otherwise.
func (c *Client) response(cseq int) (*response, error) {
	for {
		b, err := c.rd.Peek(1)
		if err != nil {
			return nil, err
		}
		if b[0] == '$' {
			ch, pkt, err := c.readFrame()
			if err != nil {
				return nil, err
			}
			if c.playing && len(c.early) < 1024 {
				c.early = append(c.early, append([]byte{byte(ch)}, pkt...))
			}
			continue
		}
		res, err := c.readMessage()
		if err != nil {
			return nil, err
		}
		if res == nil {
			continue // request from the server
		}
		if n, err := strconv.Atoi(res.header["cseq"]); err == nil && n < cseq {
			continue // late answer to an earlier request
		}
		return res, nil
	}
}

func (c *Client) readFrame() (int, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.rd, hdr[:]); err != nil {
		return 0, nil, err
	}
	pkt := make([]byte, binary.BigEndian.Uint16(hdr[2:]))
	if _, err := io.ReadFull(c.rd, pkt); err != nil {
		return 0, nil, err
	}
	return int(hdr[1]), pkt, nil
}

// readMessage reads an RTSP response, or skips a request sent by the server
// (returns nil, nil).
func (c *Client) readMessage() (*response, error) {
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	var res *response
	if strings.HasPrefix(line, "RTSP/") {
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 2 {
			return nil, provider.NewError(provider.KindProtocol, nil, "RTSP: ungültige Antwort")
		}
		res = &response{header: map[string]string{}}
		res.status, _ = strconv.Atoi(parts[1])
		if len(parts) == 3 {
			res.reason = parts[2]
		}
	} else if strings.TrimSpace(line) == "" {
		return nil, nil
	} else if !strings.Contains(line, "RTSP/") {
		return nil, provider.NewError(provider.KindProtocol, nil, "RTSP: unerwartete Daten vom Server")
	}
	length := 0
	for {
		l, err := c.rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if key == "content-length" {
			length, _ = strconv.Atoi(v)
		}
		if res == nil {
			continue
		}
		// prefer Digest when several challenges are offered
		if key == "www-authenticate" && strings.HasPrefix(strings.ToLower(res.header[key]), "digest") {
			continue
		}
		res.header[key] = v
	}
	if length < 0 || length > 1<<20 {
		return nil, provider.NewError(provider.KindProtocol, nil, "RTSP: Antwort zu groß")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.rd, body); err != nil {
		return nil, err
	}
	if res != nil {
		res.body = body
	}
	return res, nil
}
