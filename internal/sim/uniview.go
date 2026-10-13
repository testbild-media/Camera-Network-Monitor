package sim

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UniviewRoute resolves Uniview RTSP paths for RTSPServer.Route:
//
//	/unicast/c<ch>/s<0|1>/live              live
//	/c<ch>/b<unix start>/e<unix end>/replay/  playback (UTC)
func UniviewRoute(u *url.URL, _ *time.Location) (Route, bool) {
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	num := func(s, prefix string) (int64, bool) {
		if !strings.HasPrefix(s, prefix) {
			return 0, false
		}
		n, err := strconv.ParseInt(s[len(prefix):], 10, 64)
		return n, err == nil
	}
	switch {
	case len(p) == 4 && p[0] == "unicast" && p[3] == "live":
		ch, ok1 := num(p[1], "c")
		st, ok2 := num(p[2], "s")
		return Route{Channel: int(ch), Subtype: int(st)}, ok1 && ok2 && st <= 1
	case len(p) == 4 && p[3] == "replay":
		ch, ok1 := num(p[0], "c")
		b, ok2 := num(p[1], "b")
		e, ok3 := num(p[2], "e")
		return Route{Channel: int(ch), Playback: true, Start: time.Unix(b, 0).UTC(), End: time.Unix(e, 0).UTC()}, ok1 && ok2 && ok3
	}
	return Route{}, false
}
