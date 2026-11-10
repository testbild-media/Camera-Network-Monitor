package reolink

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cnm/internal/provider"
)

// cmd is one command of an api.cgi request (requests carry a JSON array).
type cmd struct {
	Cmd    string `json:"cmd"`
	Action int    `json:"action"`
	Param  any    `json:"param"`
}

// result is the answer to one command.
type result struct {
	Cmd   string          `json:"cmd"`
	Code  int             `json:"code"`
	Value json.RawMessage `json:"value"`
	Error *struct {
		Detail  string `json:"detail"`
		RspCode int    `json:"rspCode"`
	} `json:"error"`
}

func (r result) err() error {
	if r.Code == 0 {
		return nil
	}
	detail, code := "", r.Code
	if r.Error != nil {
		detail, code = r.Error.Detail, r.Error.RspCode
	}
	switch {
	case credError(detail):
		return provider.NewError(provider.KindAuth, nil, "Kamera: Benutzername oder Passwort falsch (%s)", detail)
	case strings.Contains(strings.ToLower(detail), "not support"), strings.Contains(strings.ToLower(detail), "ability error"):
		return provider.NewError(provider.KindUnsupported, nil, "Kamera: %s wird nicht unterstützt", r.Cmd)
	}
	return provider.NewError(provider.KindProtocol, nil, "Kamera lehnt %s ab: %s (%d)", r.Cmd, detail, code)
}

func credError(detail string) bool {
	d := strings.ToLower(detail)
	return strings.Contains(d, "invalid user") || strings.Contains(d, "login failed") ||
		strings.Contains(d, "password wrong") || strings.Contains(d, "has been locked")
}

// client speaks the camera's JSON API (/cgi-bin/api.cgi) with a login
// token. The cameras use HTTPS with a self-signed certificate by default;
// plain HTTP is often switched off.
type client struct {
	host       string
	user, pass string
	env        provider.Env
	hc         *http.Client

	loginMu sync.Mutex // one login at a time (the cameras allow only a few sessions)

	mu     sync.Mutex
	scheme string // "https" or "http"
	port   int
	token  string
	lease  time.Time
}

func newClient(host, user, pass string, env provider.Env) *client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		// self-signed certificates; older firmware also needs RSA key exchange
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, CipherSuites: cipherSuites()}, //nolint:gosec // LAN camera, self-signed
	}
	return &client{host: host, user: user, pass: pass, env: env,
		hc: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func cipherSuites() []uint16 {
	var out []uint16
	for _, s := range tls.CipherSuites() {
		out = append(out, s.ID)
	}
	return out
}

// base returns scheme://host:port.
func (c *client) base() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scheme + "://" + provider.HostPort(c.host, c.port)
}

func (c *client) setTransport(scheme string, port int) {
	c.mu.Lock()
	if c.scheme != scheme || c.port != port {
		c.token = ""
	}
	c.scheme, c.port = scheme, port
	c.mu.Unlock()
}

// post sends a command array and returns the per-command results.
func (c *client) post(ctx context.Context, token string, cmds []cmd) ([]result, error) {
	body, err := json.Marshal(cmds)
	if err != nil {
		return nil, err
	}
	q := url.Values{"cmd": {cmds[0].Cmd}}
	if token != "" {
		q.Set("token", token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/cgi-bin/api.cgi?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "Kamera-API")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "Kamera-API")
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, provider.NewError(provider.KindAuth, nil, "Kamera-API: Zugriff verweigert (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, provider.NewError(provider.KindProtocol, nil, "Kamera-API leitet um (HTTP %d) – falsches Protokoll (HTTP/HTTPS)?", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return nil, provider.NewError(provider.KindUnsupported, nil, "Keine Kamera-API unter %s (HTTP 404)", provider.HostPort(c.host, c.port))
	case resp.StatusCode != http.StatusOK:
		return nil, provider.NewError(provider.KindProtocol, nil, "Kamera-API antwortet mit HTTP %d", resp.StatusCode)
	}
	var out []result
	if err := json.Unmarshal(bytes.TrimSpace(data), &out); err != nil || len(out) == 0 {
		return nil, provider.NewError(provider.KindProtocol, err, "Kamera-API: unerwartete Antwort")
	}
	return out, nil
}

// login fetches a token. Only credential errors are reported to the
// supervisor (they lock further automatic logins).
func (c *client) login(ctx context.Context) error {
	if !c.env.AllowLogin() {
		return provider.ErrLoginBlocked
	}
	res, err := c.post(ctx, "", []cmd{{Cmd: "Login", Param: map[string]any{"User": map[string]any{"Version": "0", "userName": c.user, "password": c.pass}}}})
	if err != nil {
		return err
	}
	if err := res[0].err(); err != nil {
		if provider.KindOf(err) == provider.KindAuth {
			c.env.ReportAuthFailure(err)
		} else if res[0].Error != nil && strings.Contains(strings.ToLower(res[0].Error.Detail), "max session") {
			return provider.NewError(provider.KindNetwork, nil, "Zu viele gleichzeitige Anmeldungen an der Kamera – CNM versucht es später erneut")
		}
		return err
	}
	var v struct {
		Token struct {
			Name      string `json:"name"`
			LeaseTime int    `json:"leaseTime"`
		} `json:"Token"`
	}
	if json.Unmarshal(res[0].Value, &v) != nil || v.Token.Name == "" {
		return provider.NewError(provider.KindProtocol, nil, "Kamera-API: Anmeldung ohne Token")
	}
	lease := time.Duration(v.Token.LeaseTime) * time.Second
	if lease <= 0 {
		lease = time.Hour
	}
	c.mu.Lock()
	c.token, c.lease = v.Token.Name, time.Now().Add(lease)
	c.mu.Unlock()
	return nil
}

// validToken returns the current token, logging in when there is none or
// it expires within five minutes.
func (c *client) validToken(ctx context.Context) (string, error) {
	current := func() string {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.token != "" && time.Until(c.lease) > 5*time.Minute {
			return c.token
		}
		return ""
	}
	if tok := current(); tok != "" {
		return tok, nil
	}
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if tok := current(); tok != "" {
		return tok, nil // another request logged in meanwhile
	}
	if err := c.login(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, nil
}

// forget drops the token (the device invalidated it, e.g. after SetNetPort).
func (c *client) forget() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func (c *client) dropToken(tok string) {
	c.mu.Lock()
	if c.token == tok {
		c.token = ""
	}
	c.mu.Unlock()
}

// do sends commands with the token; an expired session is renewed once.
func (c *client) do(ctx context.Context, cmds ...cmd) ([]result, error) {
	for attempt := 0; ; attempt++ {
		tok, err := c.validToken(ctx)
		if err != nil {
			return nil, err
		}
		res, err := c.post(ctx, tok, cmds)
		if err != nil {
			return nil, err
		}
		if attempt == 0 && sessionLost(res) {
			c.dropToken(tok)
			continue
		}
		for _, r := range res {
			if r.Error != nil && credError(r.Error.Detail) {
				err := r.err()
				c.env.ReportAuthFailure(err)
				return nil, err
			}
		}
		return res, nil
	}
}

func sessionLost(res []result) bool {
	for _, r := range res {
		if r.Error != nil && strings.Contains(strings.ToLower(r.Error.Detail), "please login first") {
			return true
		}
	}
	return false
}

// call sends one command and decodes its value into out (may be nil).
func (c *client) call(ctx context.Context, name string, param any, out any) error {
	if param == nil {
		param = map[string]any{}
	}
	res, err := c.do(ctx, cmd{Cmd: name, Param: param})
	if err != nil {
		return err
	}
	if err := res[0].err(); err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(res[0].Value, out); err != nil {
			return provider.NewError(provider.KindProtocol, err, "Kamera-API: ungültige Antwort auf %s", name)
		}
	}
	return nil
}

// logout ends the session (best effort: the cameras allow only a few).
func (c *client) logout() {
	c.mu.Lock()
	tok := c.token
	c.token = ""
	c.mu.Unlock()
	if tok == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = c.post(ctx, tok, []cmd{{Cmd: "Logout", Param: map[string]any{}}})
}

// streamURL builds an authenticated HTTP(S) URL on the API port (HTTP-FLV
// takes the credentials as query parameters; the result is secret).
func (c *client) streamURL(path string, q url.Values) string {
	q.Set("user", c.user)
	q.Set("password", c.pass)
	return c.base() + path + "?" + q.Encode()
}

func (c *client) closeIdle() { c.hc.CloseIdleConnections() }

func (c *client) String() string {
	return fmt.Sprintf("%s://%s", c.scheme, provider.HostPort(c.host, c.port))
}
