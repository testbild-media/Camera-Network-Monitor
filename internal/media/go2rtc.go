package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cnm/internal/provider"
)

// Client is a minimal go2rtc HTTP API client. It uses its own transport with
// a small, reused connection pool (no listener accumulation).
//
// go2rtc's API is not safe for concurrent use: parallel PUT /api/streams
// patch its YAML configuration at the same time (panics with "index out of
// range") and listing the streams while one is added can crash the whole
// process ("concurrent map iteration and map write"). The client therefore
// sends one request at a time.
type Client struct {
	base string
	hc   *http.Client
	turn chan struct{} // held while a request is in flight
}

type opTimeoutKey struct{}

// withOpTimeout bounds every request made with the returned context to d,
// counted from the moment it is sent – not including the wait for its turn.
func withOpTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithCancel(context.WithValue(ctx, opTimeoutKey{}, d))
}

// NewClient creates a client for the go2rtc API base URL.
func NewClient(base string) *Client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		DisableCompression:    true,
	}
	return &Client{base: strings.TrimRight(base, "/"), hc: &http.Client{Transport: tr}, turn: make(chan struct{}, 1)}
}

// Base returns the API base URL.
func (c *Client) Base() string { return c.base }

// Close releases idle connections.
func (c *Client) Close() {
	if tr, ok := c.hc.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// ErrUnreachable marks transport level failures (go2rtc down).
var ErrUnreachable = errors.New("go2rtc nicht erreichbar")

// ErrNotFound marks a 404 (e.g. preload API missing in older go2rtc).
var ErrNotFound = errors.New("go2rtc: nicht gefunden")

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("go2rtc HTTP %d: %s", e.Status, provider.Redact(strings.TrimSpace(e.Body)))
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values) ([]byte, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	select {
	case c.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: Zeitüberschreitung", ErrUnreachable)
	}
	defer func() { <-c.turn }()
	if d, ok := ctx.Value(opTimeoutKey{}).(time.Duration); ok && d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: Zeitüberschreitung", ErrUnreachable)
		}
		// never include the request URL: its query carries stream sources with credentials
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, provider.Redact(err.Error()))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, err.Error())
	}
	if resp.StatusCode == http.StatusNotFound {
		return body, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return body, &apiError{Status: resp.StatusCode, Body: string(body)}
	}
	return body, nil
}

// ProducerInfo is the subset of go2rtc producer JSON CNM needs.
type ProducerInfo struct {
	URL        string `json:"url"`
	FormatName string `json:"format_name"`
	BytesRecv  int64  `json:"bytes_recv"`
}

// Active reports whether the producer currently holds a connection.
func (p ProducerInfo) Active() bool { return p.FormatName != "" }

// ConsumerInfo is the subset of go2rtc consumer JSON CNM needs.
type ConsumerInfo struct {
	FormatName string `json:"format_name"`
}

// StreamInfo describes a go2rtc stream.
type StreamInfo struct {
	Producers []ProducerInfo `json:"producers"`
	Consumers []ConsumerInfo `json:"consumers"`
}

// HasPreloadConsumer reports whether the preload probe is attached to this stream object.
func (s StreamInfo) HasPreloadConsumer() bool {
	for _, c := range s.Consumers {
		if c.FormatName == "preload" {
			return true
		}
	}
	return false
}

// ActiveProducers counts producers holding a connection.
func (s StreamInfo) ActiveProducers() int {
	n := 0
	for _, p := range s.Producers {
		if p.Active() {
			n++
		}
	}
	return n
}

// BytesRecv sums received bytes of all producers.
func (s StreamInfo) BytesRecv() int64 {
	var n int64
	for _, p := range s.Producers {
		n += p.BytesRecv
	}
	return n
}

// Streams lists all streams.
func (c *Client) Streams(ctx context.Context) (map[string]StreamInfo, error) {
	body, err := c.do(ctx, http.MethodGet, "/api/streams", nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("go2rtc: ungültige Antwort von /api/streams")
	}
	out := make(map[string]StreamInfo, len(raw))
	for name, r := range raw {
		var si StreamInfo
		if len(r) > 0 && string(r) != "null" {
			_ = json.Unmarshal(r, &si) // tolerate unexpected shapes
		}
		out[name] = si
	}
	return out, nil
}

// Preloads lists active preloads. supported is false when the go2rtc build
// has no preload API (live keeps working, only warm-keeping is lost).
func (c *Client) Preloads(ctx context.Context) (names map[string]bool, supported bool, err error) {
	body, err := c.do(ctx, http.MethodGet, "/api/preload", nil)
	if errors.Is(err, ErrNotFound) {
		return map[string]bool{}, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return map[string]bool{}, false, nil
	}
	out := make(map[string]bool, len(raw))
	for k := range raw {
		out[k] = true
	}
	return out, true, nil
}

// notPersisted reports go2rtc's answer when it cannot write the change to
// its configuration file: CNM starts it without one on purpose (see
// GenerateConfig), the stream itself was created or deleted.
func notPersisted(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusBadRequest && strings.TrimSpace(ae.Body) == "config file disabled"
}

// PutStream creates or replaces a stream with the given sources (in order).
func (c *Client) PutStream(ctx context.Context, name string, sources []string) error {
	q := url.Values{"name": {name}}
	for _, s := range sources {
		q.Add("src", s)
	}
	_, err := c.do(ctx, http.MethodPut, "/api/streams", q)
	if notPersisted(err) {
		return nil
	}
	return err
}

// DeleteStream removes a stream definition.
func (c *Client) DeleteStream(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/streams", url.Values{"src": {name}})
	if errors.Is(err, ErrNotFound) || notPersisted(err) {
		return nil
	}
	return err
}

// PutPreload keeps a stream's producer warm (video only).
func (c *Client) PutPreload(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodPut, "/api/preload", url.Values{"src": {name}, "video": {""}})
	return err
}

// DeletePreload stops keeping a stream warm.
func (c *Client) DeletePreload(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/preload", url.Values{"src": {name}})
	return err
}

// Version returns the go2rtc version (best effort).
func (c *Client) Version(ctx context.Context) (string, error) {
	body, err := c.do(ctx, http.MethodGet, "/api", nil)
	if err != nil {
		return "", err
	}
	var info struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(body, &info)
	return info.Version, nil
}

// sourcesMatch compares go2rtc's view of a stream with desired sources.
// Idle producers echo the configured source verbatim; active ones may omit
// option fragments. Unknown producer URLs count as mismatch.
func sourcesMatch(si StreamInfo, desired []string) bool {
	if len(si.Producers) != len(desired) {
		return false
	}
	for i, p := range si.Producers {
		if p.URL == "" || normalizeSource(p.URL) != normalizeSource(desired[i]) {
			return false
		}
	}
	return true
}
