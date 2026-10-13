package provider

import (
	"net/url"
	"strings"
	"testing"
)

func TestRedactPlainAndEncodedUserinfo(t *testing.T) {
	src := CredURL("rtsp", "admin", "p@ss:w/rd", "192.0.2.5", 554, "/cam/realmonitor", url.Values{"channel": {"1"}}, "")
	q := url.Values{"src": {src, "http://cnm:token123@127.0.0.1:5000/live"}}.Encode()
	for _, in := range []string{
		"Put \"http://127.0.0.1:1984/api/streams?name=x&" + q + "\": dial tcp: refused",
		"error=EOF url=" + src,
	} {
		out := Redact(in)
		for _, secret := range []string{"p%40ss", "p@ss", "token123", "w%2Frd", "rd%40"} {
			if strings.Contains(out, secret) {
				t.Fatalf("secret %q left in %q", secret, out)
			}
		}
		if !strings.Contains(out, "192.0.2.5") {
			t.Fatalf("host removed: %q", out)
		}
	}
}
