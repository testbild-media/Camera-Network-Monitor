package display

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"cnm/internal/config"
	"cnm/internal/secret"
)

// longPoll is the maximum time GET /v1/desired waits for a change.
const longPoll = 25 * time.Second

// AgentTimeout: an agent that has not reported for this long counts as gone.
const AgentTimeout = 15 * time.Second

// PresenceFunc reports the control socket presence of a kiosk key id.
type PresenceFunc func(keyID string) (clients int, since time.Time)

// Host is CNM's side of the display agent socket.
type Host struct {
	store    *config.Store
	keys     *secret.Keyring
	presence PresenceFunc
	baseURL  func() string
	log      *slog.Logger
	started  time.Time
	epoch    string

	// SysfsRoot overrides the DRM sysfs directory (tests, demos).
	SysfsRoot string
	// GPU restricts the displays to the connectors of one graphics card (PCI
	// address, chosen in the setup when the server has several GPUs).
	GPU string

	mu       sync.Mutex
	rev      int64
	wake     chan struct{}
	reload   map[string]int
	status   *Status
	statusAt time.Time
	srv      *http.Server
}

// NewHost creates the host side. baseURL returns the CNM URL the kiosk
// browsers use (e.g. http://127.0.0.1:8090).
func NewHost(store *config.Store, keys *secret.Keyring, presence PresenceFunc, baseURL func() string, log *slog.Logger) *Host {
	h := &Host{
		store: store, keys: keys, presence: presence, baseURL: baseURL, log: log,
		started: time.Now(), epoch: secret.RandomID(8), rev: 1, wake: make(chan struct{}), reload: map[string]int{},
	}
	store.Subscribe(func(_, _ *config.Config) { h.bump() })
	return h
}

func (h *Host) bump() {
	h.mu.Lock()
	h.rev++
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

// Reload asks the agent to restart the kiosk browser of a display.
func (h *Host) Reload(displayID string) {
	h.mu.Lock()
	h.reload[displayID]++
	h.mu.Unlock()
	h.bump()
}

// Agent returns the last agent status and when it was received (nil = never).
func (h *Host) Agent() (*Status, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status, h.statusAt
}

// AgentConnected reports whether an agent reported recently.
func (h *Host) AgentConnected() bool {
	_, at := h.Agent()
	return !at.IsZero() && time.Since(at) < AgentTimeout
}

// Connectors scans the local GPU connectors.
func (h *Host) Connectors() ([]Connector, error) {
	all, err := Scan(h.SysfsRoot)
	if err != nil || h.GPU == "" {
		return all, err
	}
	gpu := NormalizePCI(h.GPU)
	out := all[:0]
	for _, c := range all {
		if c.PCI == gpu {
			out = append(out, c)
		}
	}
	return out, nil
}

// Desired builds the state the agent should establish.
func (h *Host) Desired() Desired {
	c := h.store.Get()
	h.mu.Lock()
	rev := h.rev
	reload := make(map[string]int, len(h.reload))
	for k, v := range h.reload {
		reload[k] = v
	}
	h.mu.Unlock()
	preferred := map[string]string{}
	if conns, err := h.Connectors(); err == nil {
		for _, cn := range conns {
			for _, m := range cn.Modes {
				if m.Preferred {
					preferred[cn.Name] = m.String()
				}
			}
		}
	}
	out := Desired{Epoch: h.epoch, Rev: rev, Browser: c.System.Display.Browser, DailyRestart: c.System.Display.DailyRestart, Outputs: []DesiredOutput{}}
	now := time.Now()
	for _, d := range c.Displays {
		o := DesiredOutput{
			DisplayID: d.ID, Connector: d.Connector, Mode: d.Mode, Rotation: d.Rotation,
			Preferred: preferred[d.Connector], ReloadSeq: reload[d.ID],
		}
		m, ok := c.Monitor(d.MonitorID)
		if d.Enabled && ok && m.Enabled {
			tok, err := h.keys.Decrypt(d.TokenEnc)
			if err == nil && tok != "" {
				o.Enabled = true
				o.MonitorLabel = m.Label
				o.URL = h.baseURL() + "/viewer?mon=" + url.QueryEscape(m.ID) + "&key=" + url.QueryEscape(tok) + "&display=" + url.QueryEscape(d.Connector)
				o.ViewerDownMs = h.viewerDown(config.DisplayKeyPrefix+d.ID, now)
			} else if h.log != nil {
				h.log.Warn("display credential not readable", "display", d.Connector)
			}
		}
		out.Outputs = append(out.Outputs, o)
	}
	return out
}

func (h *Host) viewerDown(keyID string, now time.Time) int64 {
	if h.presence == nil {
		return 0
	}
	n, since := h.presence(keyID)
	if n > 0 {
		return 0
	}
	if since.Before(h.started) {
		since = h.started
	}
	return now.Sub(since).Milliseconds()
}

// Handler serves the agent protocol.
func (h *Host) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/desired", func(w http.ResponseWriter, r *http.Request) {
		known, _ := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
		h.mu.Lock()
		rev, wake := h.rev, h.wake
		h.mu.Unlock()
		if known == rev && r.URL.Query().Get("epoch") == h.epoch {
			t := time.NewTimer(longPoll)
			select {
			case <-wake:
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				return
			}
			t.Stop()
		}
		writeJSON(w, h.Desired())
	})
	mux.HandleFunc("POST /v1/status", func(w http.ResponseWriter, r *http.Request) {
		var st Status
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&st); err != nil {
			http.Error(w, "bad status", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.status, h.statusAt = &st, time.Now()
		h.mu.Unlock()
		writeJSON(w, h.Desired())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

type uidKey struct{}

// Serve listens on the Unix socket path. Only processes running as one of
// the allowed uids (the agent user, root, CNM itself) are served; on systems
// without peer credentials every local client is accepted.
func (h *Host) Serve(path string, allowed []int) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	// the uid check below is the access control; the socket itself is open
	_ = os.Chmod(path, 0o666)
	allow := map[int]bool{}
	for _, u := range allowed {
		allow[u] = true
	}
	inner := h.Handler()
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			uid, ok := peerUID(c)
			if !ok {
				uid = -1
			}
			return context.WithValue(ctx, uidKey{}, uid)
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, _ := r.Context().Value(uidKey{}).(int)
			if peerCredSupported && !allow[uid] {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			inner.ServeHTTP(w, r)
		}),
	}
	h.mu.Lock()
	h.srv = srv
	h.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && h.log != nil {
			h.log.Warn("display socket stopped", "error", err)
		}
	}()
	return nil
}

// Close stops the socket server (open long polls end immediately).
func (h *Host) Close() {
	h.mu.Lock()
	srv := h.srv
	h.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}
