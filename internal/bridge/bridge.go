// Package bridge exposes CNM-bridged live streams (e.g. DVRIP two-socket
// firmware) to the local go2rtc instance.
//
// Security model:
//   - served on a dedicated listener bound to 127.0.0.1 (never on the public port)
//   - every request must present a random capability token that exists only
//     in this process's memory (HTTP Basic password, so it is masked in
//     go2rtc's and CNM's logs like any URL userinfo)
//   - DVR credentials never appear in the URL
package bridge

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"cnm/internal/provider"
	"cnm/internal/secret"
)

// Opener opens a bridged live stream.
type Opener interface {
	OpenLegacyLive(ctx context.Context, deviceID string, channel int, kind provider.StreamKind) (io.ReadCloser, error)
}

// Bridge is the internal live bridge server.
type Bridge struct {
	token  string
	ln     net.Listener
	srv    *http.Server
	opener atomic.Value // Opener
	log    *slog.Logger
	active atomic.Int64
}

// New listens on an ephemeral loopback port.
func New(log *slog.Logger) (*Bridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &Bridge{token: secret.GenerateToken(), ln: ln, log: log}
	b.srv = &http.Server{Handler: b, ReadHeaderTimeout: 5 * time.Second}
	return b, nil
}

// SetOpener wires the device manager (set after construction to break the
// construction cycle device manager ↔ bridge URL builder).
func (b *Bridge) SetOpener(o Opener) { b.opener.Store(o) }

// Serve runs the server until Close.
func (b *Bridge) Serve() {
	if err := b.srv.Serve(b.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		b.log.Error("bridge server stopped", "error", err)
	}
}

// Close stops the server and all bridged streams.
func (b *Bridge) Close() { _ = b.srv.Close() }

// Active returns the number of running bridged streams.
func (b *Bridge) Active() int64 { return b.active.Load() }

// URL returns the go2rtc source URL for a device stream.
func (b *Bridge) URL(deviceID string, channel int, kind provider.StreamKind) string {
	u := url.URL{
		Scheme: "http",
		User:   url.UserPassword("cnm", b.token),
		Host:   b.ln.Addr().String(),
		Path:   "/live/" + url.PathEscape(deviceID) + "/" + strconv.Itoa(channel) + "/" + string(kind),
	}
	return u.String()
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	_, pass, ok := r.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(b.token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="cnm"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/live/"), "/")
	if r.Method != http.MethodGet || len(parts) != 3 {
		http.NotFound(w, r)
		return
	}
	devID, err := url.PathUnescape(parts[0])
	ch, err2 := strconv.Atoi(parts[1])
	kind := provider.StreamKind(parts[2])
	if err != nil || err2 != nil || ch < 0 || (kind != provider.Main && kind != provider.Sub) {
		http.NotFound(w, r)
		return
	}
	op, _ := b.opener.Load().(Opener)
	if op == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	rc, err := op.OpenLegacyLive(r.Context(), devID, ch, kind)
	if err != nil {
		b.log.Info("bridged stream unavailable", "device", devID, "channel", ch+1, "kind", kind, "error", provider.Redact(err.Error()))
		http.Error(w, "stream unavailable", http.StatusServiceUnavailable)
		return
	}
	b.active.Add(1)
	defer b.active.Add(-1)
	stop := context.AfterFunc(r.Context(), func() { rc.Close() })
	defer stop()
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
