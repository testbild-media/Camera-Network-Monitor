package provider

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DigestTransport answers HTTP Basic/Digest challenges (ISAPI, VAPIX,
// …). Every request is sent once without credentials; a 401 with a
// challenge is answered once. A second 401 is returned to the caller (wrong
// credentials) – there is no retry loop that could lock an account.
type DigestTransport struct {
	User, Pass string
	Base       http.RoundTripper
}

// NewHTTPClient returns a client for LAN devices (no proxy) with digest auth.
func NewHTTPClient(user, pass string, timeout time.Duration) *http.Client {
	base := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	return &http.Client{Transport: &DigestTransport{User: user, Pass: pass, Base: base}, Timeout: timeout}
}

// RoundTrip implements http.RoundTripper.
func (t *DigestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	clone := func() *http.Request {
		r := req.Clone(req.Context())
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		return r
	}
	resp, err := t.Base.RoundTrip(clone())
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	var challenge string
	for _, c := range resp.Header.Values("WWW-Authenticate") {
		// prefer Digest over Basic
		if challenge == "" || strings.HasPrefix(strings.ToLower(c), "digest") {
			challenge = c
		}
	}
	if challenge == "" || (t.User == "" && t.Pass == "") {
		return resp, nil
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	auth, err := DigestAuthorization(challenge, t.User, t.Pass, req.Method, req.URL.RequestURI())
	if err != nil {
		return nil, NewError(KindProtocol, err, "HTTP-Authentifizierung nicht unterstützt")
	}
	r := clone()
	r.Header.Set("Authorization", auth)
	return t.Base.RoundTrip(r)
}

// CloseIdle releases idle connections of a client created by NewHTTPClient.
func CloseIdle(c *http.Client) {
	if dt, ok := c.Transport.(*DigestTransport); ok {
		if tr, ok := dt.Base.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
}
