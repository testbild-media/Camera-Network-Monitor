package sofia

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cnm/internal/provider"
)

// Control is an authenticated DVRIP control connection. Requests may be issued
// concurrently; responses are matched by message id in FIFO order.
type Control struct {
	conn    net.Conn
	session uint32
	alive   time.Duration
	chans   int

	wmu sync.Mutex
	seq uint32

	pmu     sync.Mutex
	pending map[uint16][]chan packet
	done    chan struct{}
	errOnce sync.Once
	err     atomic.Value // error

	lastRx atomic.Int64
}

type packet struct {
	h       Header
	payload []byte
}

// LoginResult carries login reply details.
type LoginResult struct {
	Session  uint32
	Alive    time.Duration
	Channels int
	Raw      Response
}

// login performs the DVRIP login on an unauthenticated connection.
func login(conn net.Conn, rd *bufio.Reader, user, pass string, timeout time.Duration) (*LoginResult, error) {
	payload := jsonPayload(map[string]any{
		"EncryptType": "MD5",
		"LoginType":   "DVRIP-Web",
		"PassWord":    SofiaHash(pass),
		"UserName":    user,
	})
	_ = conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})
	if _, err := conn.Write(encodePacket(0, 0, MsgLogin, payload)); err != nil {
		return nil, provider.ClassifyNetErr(err, "DVRIP-Login")
	}
	for {
		h, p, err := readPacket(rd)
		if err != nil {
			return nil, provider.ClassifyNetErr(err, "DVRIP-Login")
		}
		if h.MsgID != MsgLoginRsp {
			continue
		}
		r, err := decodeResponse(p)
		if err != nil {
			return nil, provider.NewError(provider.KindProtocol, err, "DVRIP-Login: ungültige Antwort")
		}
		if !r.OK() {
			if msg, auth := IsAuthRet(r.Ret()); auth {
				return nil, provider.NewError(provider.KindAuth, nil, "DVRIP-Login abgelehnt: %s (Code %d)", msg, r.Ret())
			}
			return nil, provider.NewError(provider.KindProtocol, nil, "DVRIP-Login fehlgeschlagen (Code %d)", r.Ret())
		}
		res := &LoginResult{Session: h.Session, Alive: 20 * time.Second, Raw: r}
		if s, ok := ParseSession(r["SessionID"]); ok && s != 0 {
			res.Session = s
		}
		if v, ok := r["AliveInterval"].(float64); ok && v >= 5 && v <= 300 {
			res.Alive = time.Duration(v) * time.Second
		}
		if v, ok := r["ChannelNum"].(float64); ok {
			res.Channels = int(v)
		}
		return res, nil
	}
}

// DialControl connects and logs in. env.AllowLogin must have been checked by the caller.
func DialControl(ctx context.Context, host string, port int, user, pass string) (*Control, error) {
	conn, err := provider.Dial(ctx, host, port, 5*time.Second)
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "DVRIP")
	}
	rd := bufio.NewReaderSize(conn, 64<<10)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	lr, err := login(conn, rd, user, pass, 8*time.Second)
	stop()
	if err != nil {
		conn.Close()
		return nil, err
	}
	c := &Control{
		conn:    conn,
		session: lr.Session,
		alive:   lr.Alive,
		chans:   lr.Channels,
		seq:     1,
		pending: map[uint16][]chan packet{},
		done:    make(chan struct{}),
	}
	c.lastRx.Store(time.Now().UnixNano())
	go c.readLoop(rd)
	go c.keepAliveLoop()
	return c, nil
}

// Session returns the session id.
func (c *Control) Session() uint32 { return c.session }

// Channels returns the channel count reported at login.
func (c *Control) Channels() int { return c.chans }

// Done is closed when the connection is gone.
func (c *Control) Done() <-chan struct{} { return c.done }

// Err returns the terminal error.
func (c *Control) Err() error {
	if v := c.err.Load(); v != nil {
		return v.(error)
	}
	return nil
}

func (c *Control) fail(err error) {
	c.errOnce.Do(func() {
		if err == nil {
			err = errClosed
		}
		c.err.Store(err)
		close(c.done)
		c.conn.Close()
		c.pmu.Lock()
		for id, ws := range c.pending {
			for _, w := range ws {
				close(w)
			}
			delete(c.pending, id)
		}
		c.pmu.Unlock()
	})
}

// Close terminates the connection without waiting for the device.
func (c *Control) Close() { c.fail(errClosed) }

func (c *Control) readLoop(rd *bufio.Reader) {
	for {
		h, p, err := readPacket(rd)
		if err != nil {
			c.fail(provider.ClassifyNetErr(err, "DVRIP-Steuerverbindung"))
			return
		}
		c.lastRx.Store(time.Now().UnixNano())
		c.pmu.Lock()
		ws := c.pending[h.MsgID]
		var w chan packet
		if len(ws) > 0 {
			w, c.pending[h.MsgID] = ws[0], ws[1:]
		}
		c.pmu.Unlock()
		if w != nil {
			w <- packet{h: h, payload: p}
			close(w)
		}
		// unsolicited packets (alarms, stray media) are ignored
	}
}

func (c *Control) keepAliveLoop() {
	interval := c.alive
	if interval > 20*time.Second {
		interval = 20 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := c.Request(ctx, MsgKeepAlive, MsgKeepAliveRsp, map[string]any{"Name": "KeepAlive", "SessionID": SessionString(c.session)})
			cancel()
			if err != nil {
				c.fail(provider.NewError(provider.KindNetwork, err, "DVRIP-Keepalive fehlgeschlagen"))
				return
			}
		}
	}
}

// Send writes a raw command without waiting for an answer.
func (c *Control) Send(msgID uint16, payload []byte) error {
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	seq := c.seq
	c.seq++
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := c.conn.Write(encodePacket(c.session, seq, msgID, payload))
	if err != nil {
		c.fail(provider.ClassifyNetErr(err, "DVRIP-Steuerverbindung"))
	}
	return err
}

// RequestRaw sends payload and waits for the response message id.
func (c *Control) RequestRaw(ctx context.Context, msgID, rspID uint16, payload []byte) (Response, error) {
	w := make(chan packet, 1)
	c.pmu.Lock()
	select {
	case <-c.done:
		c.pmu.Unlock()
		return nil, c.Err()
	default:
	}
	c.pending[rspID] = append(c.pending[rspID], w)
	c.pmu.Unlock()
	if err := c.Send(msgID, payload); err != nil {
		return nil, err
	}
	select {
	case p, ok := <-w:
		if !ok {
			return nil, c.Err()
		}
		return decodeResponse(p.payload)
	case <-ctx.Done():
		// remove the waiter so later responses are matched correctly
		c.pmu.Lock()
		ws := c.pending[rspID]
		for i, x := range ws {
			if x == w {
				c.pending[rspID] = append(ws[:i], ws[i+1:]...)
				break
			}
		}
		c.pmu.Unlock()
		return nil, provider.NewError(provider.KindNetwork, ctx.Err(), "DVRIP: keine Antwort (Zeitüberschreitung)")
	case <-c.done:
		return nil, c.Err()
	}
}

// Request sends a JSON command and waits for its response.
func (c *Control) Request(ctx context.Context, msgID, rspID uint16, cmd any) (Response, error) {
	return c.RequestRaw(ctx, msgID, rspID, jsonPayload(cmd))
}

// SystemInfo queries device information (best effort).
func (c *Control) SystemInfo(ctx context.Context) (*provider.DeviceInfo, error) {
	r, err := c.Request(ctx, MsgSysInfo, MsgSysInfoRsp, map[string]any{"Name": "SystemInfo", "SessionID": SessionString(c.session)})
	if err != nil {
		return nil, err
	}
	info := &provider.DeviceInfo{Channels: c.chans}
	if si, ok := r["SystemInfo"].(map[string]any); ok {
		info.Model = str(si["DeviceModel"])
		info.Serial = str(si["SerialNo"])
		info.Firmware = str(si["SoftWareVersion"])
		if v, ok := si["VideoInChannel"].(float64); ok && v > 0 {
			info.Channels = int(v)
		}
	}
	return info, nil
}

// Storage reports whether a recording disk is installed (StorageInfo): a
// partition with capacity = present; a valid answer (Ret 100) whose disk
// list is empty or null, or lists no partition with capacity = absent
// (devices without disk answer like that). Failed requests and answers in
// an unexpected format stay unknown, so playback is never hidden by mistake.
func (c *Control) Storage(ctx context.Context) provider.StorageState {
	r, err := c.Request(ctx, MsgSysInfo, MsgSysInfoRsp, map[string]any{"Name": "StorageInfo", "SessionID": SessionString(c.session)})
	if err != nil || !r.OK() {
		return provider.StorageUnknown
	}
	raw, present := r["StorageInfo"]
	if present && raw == nil {
		return provider.StorageAbsent
	}
	disks, ok := raw.([]any)
	if !ok {
		return provider.StorageUnknown
	}
	for _, d := range disks {
		dm, _ := d.(map[string]any)
		parts, _ := dm["Partition"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if total, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(str(pm["TotalSpace"])), "0x"), 16, 64); err == nil && total > 0 {
				return provider.StoragePresent
			}
		}
	}
	return provider.StorageAbsent
}

// ChannelTitles queries channel names (best effort).
func (c *Control) ChannelTitles(ctx context.Context) map[int]string {
	r, err := c.Request(ctx, MsgChannelTitle, MsgChannelTitleRsp, map[string]any{"Name": "ChannelTitle", "SessionID": SessionString(c.session)})
	if err != nil {
		return nil
	}
	out := map[int]string{}
	if arr, ok := r["ChannelTitle"].([]any); ok {
		for i, v := range arr {
			if s := str(v); s != "" {
				out[i] = s
			}
		}
	}
	return out
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
