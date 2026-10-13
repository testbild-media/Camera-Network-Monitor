package dahua

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cnm/internal/provider"
)

// rpcProfile holds the details of one RPC2 web login variant (what a
// device's own web client sends). The first two were verified on hardware
// with CNM v0.3.25; which one a device needs is detected at Connect.
type rpcProfile struct {
	contentType      string
	clientType       string
	challengeClient  string
	authorityType    string
	passwordType     string // "" = not sent
	sessionCookie    string
	sendEmptySession bool   // send "<cookie>=" before a session exists
	usernameCookie   bool   // send "username=<user>" with a session
	requestedWith    bool   // X-Requested-With: XMLHttpRequest
	loginType        string // "" = not sent
	extraCookies     []string
}

var rpcProfiles = map[string]rpcProfile{
	// "Web3.0" client, form encoded requests (older web interfaces)
	VariantRPC2Form: {
		contentType: "application/x-www-form-urlencoded; charset=UTF-8", clientType: "Web3.0", challengeClient: "Web3.0",
		authorityType: "Default", passwordType: "Default", sessionCookie: "WebClientSessionID", sendEmptySession: true,
		requestedWith: true, extraCookies: []string{"curLanguage=German"},
	},
	// "Dahua3.0-Web3.0" client, JSON requests, session cookie
	VariantRPC2Classic: {
		contentType: "application/json", clientType: "Dahua3.0-Web3.0", challengeClient: "Dahua3.0-Web3.0",
		authorityType: "Default", sessionCookie: "DhWebClientSessionID", usernameCookie: true,
	},
	// current web interfaces: "Web3.0" with loginType Direct, session in the JSON body
	VariantRPC2Direct: {
		contentType: "application/json", clientType: "Web3.0", challengeClient: "Web3.0", loginType: "Direct",
		authorityType: "Default", passwordType: "Default", sessionCookie: "DWebClientSessionID",
	},
}

func profileFor(variant string) rpcProfile {
	if p, ok := rpcProfiles[variant]; ok {
		return p
	}
	return rpcProfiles[VariantRPC2Classic]
}

// rpcClient speaks the Dahua JSON-RPC ("RPC2") HTTP protocol.
type rpcClient struct {
	base string
	host string
	user string
	pass string
	prof rpcProfile
	env  provider.Env
	hc   *http.Client

	mu      sync.Mutex
	session json.RawMessage // string or number, echoed verbatim
	loginMu sync.Mutex      // serialises (re-)logins
	id      atomic.Int64
}

type rpcRequest struct {
	Method  string          `json:"method"`
	Params  any             `json:"params"`
	ID      int64           `json:"id"`
	Session json.RawMessage `json:"session,omitempty"`
	Object  any             `json:"object,omitempty"`
}

type rpcError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Params  json.RawMessage `json:"params"`
	Error   *rpcError       `json:"error"`
	Session json.RawMessage `json:"session"`
}

// ok reports a truthy result (true or a non-zero number/object).
func (r *rpcResponse) ok() bool {
	if r.Error != nil && r.Error.Code != 0 {
		return false
	}
	s := strings.TrimSpace(string(r.Result))
	return s != "" && s != "false" && s != "null" && s != "0"
}

// Dahua error codes relevant for classification.
const (
	codeLoginChallenge = 268632079 // 0x1003000F
	codeInvalidSession = 287637505 // 0x11250001 "Invalid session in request data"
	codeNoSession      = 287637504
)

var errSession = errors.New("rpc2: session invalid")

func newRPCClient(host string, port int, user, pass, variant string, env provider.Env) *rpcClient {
	tr := &http.Transport{
		Proxy:                 nil, // DVRs are local; never route through a proxy
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		DisableCompression:    true,
	}
	return &rpcClient{
		base: "http://" + provider.HostPort(host, port),
		host: host,
		user: user,
		pass: pass,
		prof: profileFor(variant),
		env:  env,
		hc:   &http.Client{Transport: tr, Timeout: 15 * time.Second},
	}
}

func (c *rpcClient) close() {
	if tr, ok := c.hc.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (c *rpcClient) post(ctx context.Context, path string, req *rpcRequest) (*rpcResponse, error) {
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(hr, req.Session)
	resp, err := c.hc.Do(hr)
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "RPC2")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "RPC2")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errSession
	}
	if resp.StatusCode != http.StatusOK {
		return nil, provider.NewError(provider.KindProtocol, nil, "RPC2: HTTP %d", resp.StatusCode)
	}
	var r rpcResponse
	if err := json.Unmarshal(bytes.TrimSpace(data), &r); err != nil {
		return nil, provider.NewError(provider.KindProtocol, err, "RPC2: ungültige Antwort (kein JSON-RPC)")
	}
	return &r, nil
}

// setHeaders sends what the device's own web client sends (per variant).
func (c *rpcClient) setHeaders(hr *http.Request, session json.RawMessage) {
	p := c.prof
	hr.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	hr.Header.Set("Content-Type", p.contentType)
	hr.Header.Set("Referer", "http://"+c.host+"/")
	if p.requestedWith {
		hr.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	if strings.HasPrefix(p.contentType, "application/json") {
		hr.Header.Set("X-Request", "JSON")
	}
	cookies := append([]string(nil), p.extraCookies...)
	if s := sessionString(session); s != "" {
		cookies = append(cookies, p.sessionCookie+"="+s)
		if p.usernameCookie {
			cookies = append(cookies, "username="+url.QueryEscape(c.user))
		}
	} else if p.sendEmptySession {
		cookies = append(cookies, p.sessionCookie+"=")
	}
	if len(cookies) > 0 {
		hr.Header.Set("Cookie", strings.Join(cookies, "; "))
	}
}

// sessionString renders a session id (JSON string or number) for a cookie.
func sessionString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func upperMD5(s string) string {
	h := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

// oldDigest is the 8 character legacy Dahua password hash.
func oldDigest(pass string) string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	sum := md5.Sum([]byte(pass))
	out := make([]byte, 0, 8)
	for i := 0; i < 16; i += 2 {
		out = append(out, chars[(uint16(sum[i])+uint16(sum[i+1]))%62])
	}
	return string(out)
}

func loginHash(encryption, user, pass, realm, random string) string {
	switch strings.ToLower(encryption) {
	case "basic":
		return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	case "olddigest":
		return oldDigest(pass)
	default: // "Default"
		ha1 := upperMD5(user + ":" + realm + ":" + pass)
		return upperMD5(user + ":" + random + ":" + ha1)
	}
}

// login performs the two-step challenge login. A rejected second step is an
// authentication failure and is reported to the supervisor (no auto retry).
func (c *rpcClient) login(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if !c.env.AllowLogin() {
		return provider.ErrLoginBlocked
	}
	base := map[string]any{"userName": c.user, "password": "", "clientType": c.prof.challengeClient}
	if c.prof.loginType != "" {
		base["loginType"] = c.prof.loginType
	}
	r1, err := c.post(ctx, "/RPC2_Login", &rpcRequest{Method: "global.login", Params: base, ID: c.id.Add(1)})
	if err != nil {
		if errors.Is(err, errSession) {
			return provider.NewError(provider.KindProtocol, nil, "RPC2-Login: HTTP 401 – Schnittstelle nicht kompatibel")
		}
		return err
	}
	if r1.ok() {
		// device accepted without challenge (should not happen, but keep the session)
		c.setSession(r1.Session)
		return nil
	}
	var ch struct {
		Realm      string `json:"realm"`
		Random     string `json:"random"`
		Encryption string `json:"encryption"`
	}
	_ = json.Unmarshal(r1.Params, &ch)
	if ch.Random == "" && (r1.Error == nil || r1.Error.Code != codeLoginChallenge) {
		return provider.NewError(provider.KindProtocol, nil, "RPC2-Login: keine Login-Challenge erhalten")
	}
	enc := ch.Encryption
	if enc == "" {
		enc = "Default"
	}
	params := map[string]any{
		"userName":   c.user,
		"password":   loginHash(enc, c.user, c.pass, ch.Realm, ch.Random),
		"clientType": c.prof.clientType,
	}
	if c.prof.loginType != "" {
		params["loginType"] = c.prof.loginType
	}
	if strings.EqualFold(enc, "Default") {
		// exactly as the verified v0.3.25 login of this variant
		params["authorityType"] = c.prof.authorityType
		if c.prof.passwordType != "" {
			params["passwordType"] = c.prof.passwordType
		}
	} else {
		params["authorityType"], params["passwordType"] = enc, enc
	}
	r2, err := c.post(ctx, "/RPC2_Login", &rpcRequest{Method: "global.login", Params: params, ID: c.id.Add(1), Session: r1.Session})
	if err != nil {
		if errors.Is(err, errSession) {
			return provider.NewError(provider.KindProtocol, nil, "RPC2-Login: HTTP 401")
		}
		return err
	}
	if !r2.ok() {
		msg := "Benutzername oder Passwort falsch"
		code := int64(0)
		if r2.Error != nil {
			code = r2.Error.Code
			if m := strings.ToLower(r2.Error.Message); strings.Contains(m, "lock") {
				msg = "Benutzerkonto gesperrt"
			}
		}
		e := provider.NewError(provider.KindAuth, nil, "RPC2-Login abgelehnt: %s (Code %d)", msg, code)
		c.env.ReportAuthFailure(e)
		return e
	}
	sess := r2.Session
	if len(sess) == 0 || string(sess) == "null" {
		sess = r1.Session
	}
	c.setSession(sess)
	return nil
}

func (c *rpcClient) setSession(s json.RawMessage) {
	c.mu.Lock()
	c.session = append(json.RawMessage(nil), s...)
	c.mu.Unlock()
}

func (c *rpcClient) getSession() json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

func (c *rpcClient) hasSession() bool { return len(c.getSession()) > 0 }

func (c *rpcClient) clearSession() { c.setSession(nil) }

// call performs an RPC with the current session. On an invalid session it
// logs in once more (subject to the login gate) and retries.
func (c *rpcClient) call(ctx context.Context, method string, params any, object any) (*rpcResponse, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if !c.hasSession() {
			if err := c.login(ctx); err != nil {
				return nil, err
			}
		}
		r, err := c.post(ctx, "/RPC2", &rpcRequest{Method: method, Params: params, ID: c.id.Add(1), Session: c.getSession(), Object: object})
		if err == nil && r.Error != nil && (r.Error.Code == codeInvalidSession || r.Error.Code == codeNoSession) {
			err = errSession
		}
		if errors.Is(err, errSession) {
			c.clearSession()
			continue
		}
		return r, err
	}
	return nil, provider.NewError(provider.KindProtocol, nil, "RPC2: Sitzung wird nicht akzeptiert")
}

func (c *rpcClient) logout() {
	if !c.hasSession() {
		return
	}
	// best effort, never block shutdown on an unreachable device
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	_, _ = c.post(ctx, "/RPC2", &rpcRequest{Method: "global.logout", Params: nil, ID: c.id.Add(1), Session: c.getSession()})
	c.clearSession()
}

// decode helper
func decodeParams(r *rpcResponse, v any) error {
	if len(r.Params) == 0 {
		return fmt.Errorf("rpc2: response without params")
	}
	return json.Unmarshal(r.Params, v)
}
