package dahua

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// capture records what the client sends to /RPC2_Login and answers like a
// Dahua web interface (challenge, then success).
type capture struct {
	mu      sync.Mutex
	headers []http.Header
	params  []map[string]any
}

func (c *capture) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var q struct {
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(body, &q)
	c.mu.Lock()
	c.headers = append(c.headers, r.Header.Clone())
	c.params = append(c.params, q.Params)
	n := len(c.params)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if n == 1 {
		_, _ = w.Write([]byte(`{"id":1,"result":false,"session":"abc123","error":{"code":268632079},"params":{"realm":"R","random":"42","encryption":"Default"}}`))
		return
	}
	_, _ = w.Write([]byte(`{"id":2,"result":true,"session":"abc123","params":{"keepAliveInterval":60}}`))
}

// TestRPCLoginMatchesVerifiedProfiles: headers and login parameters per variant
// exactly as the device's web client (verified with CNM v0.3.25).
func TestRPCLoginMatchesVerifiedProfiles(t *testing.T) {
	for _, tc := range []struct {
		model                         string
		contentType, clientType       string
		cookie1, cookie2              string
		passwordType, requested, xreq bool
		loginType                     any
	}{
		{VariantRPC2Classic, "application/json", "Dahua3.0-Web3.0", "", "DhWebClientSessionID=abc123; username=admin", false, false, true, nil},
		{VariantRPC2Form, "application/x-www-form-urlencoded; charset=UTF-8", "Web3.0", "curLanguage=German; WebClientSessionID=", "curLanguage=German; WebClientSessionID=abc123", true, true, false, nil},
		{VariantRPC2Direct, "application/json", "Web3.0", "", "DWebClientSessionID=abc123", true, false, true, "Direct"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			cp := &capture{}
			srv := httptest.NewServer(http.HandlerFunc(cp.handler))
			defer srv.Close()
			host, p := port(strings.TrimPrefix(srv.URL, "http://"))
			c := newRPCClient(host, p, "admin", "pw123456", tc.model, &env{})
			if err := c.login(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(cp.params) != 2 {
				t.Fatalf("expected challenge + login, got %d requests", len(cp.params))
			}
			for i, h := range cp.headers {
				if h.Get("Content-Type") != tc.contentType || h.Get("Referer") != "http://"+host+"/" {
					t.Fatalf("request %d headers: %v", i, h)
				}
				if (h.Get("X-Requested-With") == "XMLHttpRequest") != tc.requested || (h.Get("X-Request") == "JSON") != tc.xreq {
					t.Fatalf("request %d X- headers: %v", i, h)
				}
			}
			if got := cp.headers[0].Get("Cookie"); got != tc.cookie1 {
				t.Fatalf("challenge cookie %q, want %q", got, tc.cookie1)
			}
			if got := cp.headers[1].Get("Cookie"); got != tc.cookie2 {
				t.Fatalf("login cookie %q, want %q", got, tc.cookie2)
			}
			ch, lg := cp.params[0], cp.params[1]
			if ch["clientType"] != tc.clientType || ch["password"] != "" || ch["loginType"] != tc.loginType {
				t.Fatalf("challenge params %v", ch)
			}
			want := upperMD5("admin:42:" + upperMD5("admin:R:pw123456"))
			if lg["clientType"] != tc.clientType || lg["password"] != want || lg["authorityType"] != "Default" || lg["loginType"] != tc.loginType {
				t.Fatalf("login params %v", lg)
			}
			if _, has := lg["passwordType"]; has != tc.passwordType {
				t.Fatalf("passwordType presence %v: %v", has, lg)
			}
		})
	}
}

func TestRefusedConnectionNamesAddress(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	host, p := port(strings.TrimPrefix(srv.URL, "http://"))
	srv.Close() // nothing listens on the port any more
	err := newRPCClient(host, p, "admin", "pw", VariantRPC2Classic, &env{}).login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Verbindung abgelehnt") || !strings.Contains(err.Error(), host) || !strings.Contains(err.Error(), "Port prüfen") {
		t.Fatalf("message: %v", err)
	}
}
