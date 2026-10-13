// Package hub pushes live state to browsers over the control WebSocket (/ws).
//
//   - lightweight events (device status, media status) are sent immediately
//   - full structural state is debounced and rendered once per broadcast; the
//     rendered bytes are shared by all clients with the same scope (kiosk
//     clients receive only their server-side filtered monitor)
//   - slow clients are disconnected instead of blocking the hub
package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"cnm/internal/auth"
	"cnm/internal/connreg"
)

// Renderer builds the full state JSON for a scope ("" = unrestricted).
type Renderer func(scopeMonitor string) []byte

// Close codes understood by the browser.
const (
	CloseRevoked  = 4403 // session ended / kiosk key revoked → do not silently retry
	CloseShutdown = 4503 // server shutting down → reconnect with backoff
)

// Hub manages control WebSocket clients.
type Hub struct {
	render Renderer
	conns  *connreg.Registry
	log    *slog.Logger

	mu       sync.Mutex
	clients  map[*client]struct{}
	presence map[string]*presence // kiosk key id → connection state
	timer    *time.Timer
	closed   bool

	// Debounce delays full-state broadcasts to coalesce bursts.
	Debounce time.Duration
}

// presence tracks whether a kiosk identity currently has a control socket.
type presence struct {
	clients int
	since   time.Time // last transition between connected and disconnected
}

type client struct {
	p      *auth.Principal
	send   chan []byte
	done   chan struct{}
	once   sync.Once
	code   websocket.StatusCode
	reason string
}

// New creates a hub.
func New(render Renderer, conns *connreg.Registry, log *slog.Logger) *Hub {
	return &Hub{render: render, conns: conns, log: log, clients: map[*client]struct{}{}, presence: map[string]*presence{}, Debounce: 250 * time.Millisecond}
}

// Presence reports how many control sockets a kiosk identity (key id) has
// and when that last changed between zero and non-zero (zero time = never
// connected since the start).
func (h *Hub) Presence(keyID string) (clients int, since time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pr := h.presence[keyID]; pr != nil {
		return pr.clients, pr.since
	}
	return 0, time.Time{}
}

func (h *Hub) trackPresence(keyID string, delta int) {
	if keyID == "" {
		return
	}
	pr := h.presence[keyID]
	if pr == nil {
		pr = &presence{}
		h.presence[keyID] = pr
	}
	before := pr.clients
	pr.clients += delta
	if (before == 0) != (pr.clients == 0) {
		pr.since = time.Now()
	}
}

func (c *client) close(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		c.code, c.reason = code, reason
		close(c.done)
	})
}

func (c *client) enqueue(b []byte) bool {
	select {
	case c.send <- b:
		return true
	default:
		return false
	}
}

// Count returns the number of connected clients.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Serve upgrades the request and runs the client until it disconnects.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	h.mu.Unlock()
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return // Accept already wrote an error (e.g. cross origin)
	}
	ws.SetReadLimit(4096)
	ctx := r.Context()
	c := &client{p: p, send: make(chan []byte, 64), done: make(chan struct{})}
	remove := h.conns.Add(p.ConnKey(), func() { c.close(CloseRevoked, "revoked") })
	defer remove()
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.trackPresence(p.KeyID, 1)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.trackPresence(p.KeyID, -1)
		h.mu.Unlock()
	}()

	hello, _ := json.Marshal(map[string]any{"type": "hello", "principal": p, "serverTime": time.Now().UnixMilli()})
	c.enqueue(hello)
	c.enqueue(wrapState(h.render(p.ScopeMonitor())))

	readCtx := ws.CloseRead(ctx)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-readCtx.Done(): // peer closed or request context ended
			_ = ws.CloseNow()
			return
		case <-c.done: // revoked, logout, shutdown or slow client
			wctx, wcancel := context.WithTimeout(context.Background(), time.Second)
			_ = ws.Write(wctx, websocket.MessageText, []byte(`{"type":"closing","reason":"`+c.reason+`"}`))
			wcancel()
			// complete the close handshake (so the browser sees the code) but
			// never wait long for a misbehaving peer
			closed := make(chan struct{})
			go func() { _ = ws.Close(c.code, c.reason); close(closed) }()
			select {
			case <-closed:
			case <-time.After(1500 * time.Millisecond):
				_ = ws.CloseNow()
			}
			return
		case b := <-c.send:
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Write(wctx, websocket.MessageText, b)
			wcancel()
			if err != nil {
				_ = ws.CloseNow()
				return
			}
		case <-ping.C:
			// protocol ping detects dead peers server side; the JSON tick lets
			// the browser detect a silently dead server (no message for 70 s)
			pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Ping(pctx)
			if err == nil {
				err = ws.Write(pctx, websocket.MessageText, []byte(`{"type":"tick"}`))
			}
			pcancel()
			if err != nil {
				_ = ws.CloseNow()
				return
			}
		}
	}
}

func wrapState(state []byte) []byte {
	out := make([]byte, 0, len(state)+32)
	out = append(out, `{"type":"state","state":`...)
	out = append(out, state...)
	out = append(out, '}')
	return out
}

// ScheduleState requests a debounced full state broadcast.
func (h *Hub) ScheduleState() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.timer != nil {
		return
	}
	h.timer = time.AfterFunc(h.Debounce, func() {
		h.mu.Lock()
		h.timer = nil
		h.mu.Unlock()
		h.BroadcastState()
	})
}

// BroadcastState renders once per scope and sends to every client.
func (h *Hub) BroadcastState() {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	cache := map[string][]byte{}
	for _, c := range clients {
		scope := c.p.ScopeMonitor()
		b, ok := cache[scope]
		if !ok {
			b = wrapState(h.render(scope))
			cache[scope] = b
		}
		if !c.enqueue(b) {
			h.log.Info("dropping slow websocket client")
			c.close(websocket.StatusPolicyViolation, "slow")
		}
	}
}

// Event sends a lightweight event to clients selected by filter.
func (h *Hub) Event(v any, filter func(p *auth.Principal) bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if filter == nil || filter(c.p) {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()
	for _, c := range clients {
		if !c.enqueue(b) {
			c.close(websocket.StatusPolicyViolation, "slow")
		}
	}
}

// EventFunc sends a per-client event (nil result = skip client).
func (h *Hub) EventFunc(fn func(p *auth.Principal) any) {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		v := fn(c.p)
		if v == nil {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		if !c.enqueue(b) {
			c.close(websocket.StatusPolicyViolation, "slow")
		}
	}
}

// Shutdown closes every client with the shutdown code.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	h.closed = true
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.close(CloseShutdown, "shutdown")
	}
}
