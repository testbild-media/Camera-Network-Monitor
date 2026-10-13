package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"cnm/internal/auth"
	"cnm/internal/config"
	"cnm/internal/connreg"
	"cnm/internal/device"
	"cnm/internal/media"
	"cnm/internal/playback"
	"cnm/internal/secret"

	_ "cnm/internal/provider/dahua"
	_ "cnm/internal/provider/generic"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeMedia is a go2rtc stand-in: stream API + /api/ws that streams binary data.
type fakeMedia struct {
	srv     *httptest.Server
	lastURL atomic.Value
	open    atomic.Int32
}

func newFakeMedia(t *testing.T) *fakeMedia {
	f := &fakeMedia{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/streams":
			w.Write([]byte(`{}`))
		case "/api/preload":
			w.Write([]byte(`{}`))
		case "/api":
			w.Write([]byte(`{"version":"fake"}`))
		case "/api/ws":
			f.lastURL.Store(r.URL.RawQuery)
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			f.open.Add(1)
			defer f.open.Add(-1)
			ctx := c.CloseRead(context.Background())
			for i := 0; ; i++ {
				select {
				case <-ctx.Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
				if err := c.Write(context.Background(), websocket.MessageBinary, []byte(fmt.Sprintf("frame-%d", i))); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type rig struct {
	t     *testing.T
	srv   *Server
	http  *httptest.Server
	store *config.Store
	media *fakeMedia
	keys  *secret.Keyring
	conns *connreg.Registry
}

func newRig(t *testing.T) *rig {
	fm := newFakeMedia(t)
	keys, _ := secret.NewKeyring(secret.GenerateKey())
	dir := t.TempDir()
	c := config.Default(dir)
	c.System.Media.Go2rtcURL = fm.srv.URL
	pw, _ := keys.Encrypt("dvr-secret-pw")
	pwJSON, _ := json.Marshal(pw)
	c.Devices = []config.Device{{ID: "dvr1", Label: "DVR", Protocol: "dahua", Detected: &config.Detection{Variant: "rpc2-dahua3"}, Host: "192.0.2.254",
		Credentials: config.Credentials{Username: "admin", Password: pwJSON}, ChannelCount: 8}}
	config.Normalize(c, dir)
	for i := range c.Cameras { // readable camera ids for the tests
		c.Cameras[i].ID = "dvr1:" + strconv.Itoa(c.Cameras[i].Channel)
	}
	c.Monitors = []config.Monitor{
		{ID: "m1", Label: "Eingang", Enabled: true, Layout: config.TemplateLayout("2x2", []string{"dvr1:0", "dvr1:1"})},
		{ID: "m2", Label: "Hof", Enabled: true, Layout: config.TemplateLayout("2x2", []string{"dvr1:2"})},
	}
	config.Normalize(c, dir)
	store := config.NewMemoryStore(c)
	conns := connreg.New()
	authSvc := auth.NewService(store, keys, conns)
	devs := device.NewManager(store, keys, nil, quiet())
	recon := media.NewReconciler(store.Get, devs, quiet())
	pb := playback.New(store.Get, devs, quiet())
	s := New(Deps{Store: store, Keys: keys, Auth: authSvc, Devices: devs, Recon: recon, Playback: pb, Conns: conns, Log: quiet(), Version: "test"})
	store.Subscribe(func(_, _ *config.Config) { s.Hub.ScheduleState() })
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return &rig{t: t, srv: s, http: hs, store: store, media: fm, keys: keys, conns: conns}
}

type client struct {
	r     *rig
	hc    *http.Client
	csrf  string
	kiosk string
	hdr   http.Header
}

func (r *rig) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{r: r, hc: &http.Client{Jar: jar}, hdr: http.Header{}}
}

// remote marks requests as coming through a proxy (never "local").
func (c *client) remote() *client {
	c.hdr.Set("X-Forwarded-For", "203.0.113.9")
	return c
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, c.r.http.URL+path, rd)
	for k, v := range c.hdr {
		req.Header[k] = v
	}
	if c.csrf != "" {
		req.Header.Set(auth.CSRFHeader, c.csrf)
	}
	if c.kiosk != "" {
		req.Header.Set(auth.KioskHeader, c.kiosk)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, out
}

func (c *client) login(user, pass string) {
	st, body := c.do("POST", "/api/auth/login", map[string]any{"username": user, "password": pass})
	if st != 200 {
		c.r.t.Fatalf("login %s: %d %v", user, st, body)
	}
	c.csrf, _ = body["csrf"].(string)
}

func (r *rig) bootstrapAdmin() *client {
	c := r.client()
	st, body := c.do("POST", "/api/auth/bootstrap", map[string]any{"username": "admin", "password": "test-admin-pw"})
	if st != 200 {
		r.t.Fatalf("bootstrap: %d %v", st, body)
	}
	c.csrf, _ = body["csrf"].(string)
	return c
}

func TestAccessMatrix(t *testing.T) {
	r := newRig(t)
	// remote anonymous: no access to state, media or websocket
	rem := r.client().remote()
	for _, p := range []string{"/api/state", "/api/monitors", "/api/cameras", "/api/media/status", "/api/playback/info?cameraId=dvr1:0"} {
		if st, _ := rem.do("GET", p, nil); st != 401 {
			t.Fatalf("remote anonymous %s: %d", p, st)
		}
	}
	// remote bootstrap is refused
	if st, _ := rem.do("POST", "/api/auth/bootstrap", map[string]any{"username": "x", "password": "12345678"}); st != 403 {
		t.Fatalf("remote bootstrap: %d", st)
	}
	// localhost anonymous: no access either (only the bootstrap is local)
	loc := r.client()
	if st, body := loc.do("GET", "/api/auth/me", nil); st != 401 || body["needsBootstrap"] != true || body["local"] != true {
		t.Fatalf("local me: %d %v", st, body)
	}
	for _, p := range []string{"/api/state", "/api/settings", "/api/media/status"} {
		if st, _ := loc.do("GET", p, nil); st != 401 {
			t.Fatalf("anonymous localhost %s: %d", p, st)
		}
	}
	if _, err := loc.dialWS("/ws", url.Values{}); err == nil {
		t.Fatal("anonymous localhost websocket must be refused")
	}
	admin := r.bootstrapAdmin()
	// bootstrap only once
	if st, _ := r.client().do("POST", "/api/auth/bootstrap", map[string]any{"username": "b", "password": "12345678"}); st != 403 {
		t.Fatalf("second bootstrap: %d", st)
	}
	if st, _ := admin.do("GET", "/api/settings", nil); st != 200 {
		t.Fatalf("admin settings: %d", st)
	}
	// CSRF: admin mutation without token is rejected
	noCSRF := *admin
	noCSRF.csrf = ""
	if st, _ := noCSRF.do("POST", "/api/settings/monitors", map[string]any{"label": "x"}); st != 403 {
		t.Fatalf("mutation without CSRF token: %d", st)
	}
	// viewer user
	if st, _ := admin.do("POST", "/api/settings/users", map[string]any{"username": "viewer2", "password": "viewer-pass", "role": "viewer"}); st != 200 {
		t.Fatalf("create viewer: %d", st)
	}
	v := r.client().remote()
	v.login("viewer2", "viewer-pass")
	if st, _ := v.do("GET", "/api/state", nil); st != 200 {
		t.Fatalf("viewer state: %d", st)
	}
	if st, _ := v.do("GET", "/api/settings", nil); st != 403 {
		t.Fatalf("viewer settings: %d", st)
	}
	if st, _ := v.do("PUT", "/api/settings/system", map[string]any{}); st != 403 {
		t.Fatalf("viewer system change: %d", st)
	}
	// the network scan is admin only and validates the range before scanning
	if st, _ := v.do("POST", "/api/settings/discovery", map[string]any{}); st != 403 {
		t.Fatalf("viewer network scan: %d", st)
	}
	if st, _ := admin.do("POST", "/api/settings/discovery", map[string]any{"range": "192.0.2.0/8"}); st != 400 {
		t.Fatalf("oversized scan range: %d", st)
	}
}

func TestSecretsNeverReachBrowser(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	_, key := admin.do("POST", "/api/settings/users/"+r.store.Get().Users[0].ID+"/api-keys", map[string]any{"label": "Lobby", "monitorId": "m1"})
	token := key["token"].(string)
	for _, p := range []string{"/api/settings", "/api/settings/users", "/api/state", "/api/cameras", "/api/media/status"} {
		_, body := admin.do("GET", p, nil)
		raw, _ := json.Marshal(body)
		s := string(raw)
		for _, secretText := range []string{"dvr-secret-pw", "cnm1:", token, "tokenHash", "tokenEnc", "passwordHash", "scrypt$", "192.0.2.254:554"} {
			if strings.Contains(s, secretText) {
				t.Fatalf("%s leaks %q", p, secretText)
			}
		}
	}
	// reveal returns the original token and is not cacheable
	req, _ := http.NewRequest("GET", r.http.URL+"/api/settings/users/"+r.store.Get().Users[0].ID+"/api-keys/"+key["key"].(map[string]any)["id"].(string)+"/reveal", nil)
	resp, _ := admin.hc.Do(req)
	var rev map[string]any
	json.NewDecoder(resp.Body).Decode(&rev)
	resp.Body.Close()
	if rev["token"] != token || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("reveal: %v %s", rev, resp.Header.Get("Cache-Control"))
	}
}

func (r *rig) kioskKey(admin *client, monitor string) (keyID, token string) {
	uid := r.store.Get().Users[0].ID
	st, body := admin.do("POST", "/api/settings/users/"+uid+"/api-keys", map[string]any{"label": "Kiosk", "monitorId": monitor})
	if st != 200 {
		r.t.Fatalf("create key: %d %v", st, body)
	}
	return body["key"].(map[string]any)["id"].(string), body["token"].(string)
}

func (c *client) kioskLogin(mon, token string) int {
	c.kiosk = mon
	st, body := c.do("POST", "/api/auth/kiosk", map[string]any{"mon": mon, "key": token})
	if st == 200 {
		c.csrf, _ = body["csrf"].(string)
	}
	return st
}

func TestKioskScopeAndParallelIdentity(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	_, token := r.kioskKey(admin, "m1")

	// same browser (cookie jar) holds the admin session and the kiosk session
	k := &client{r: r, hc: admin.hc, hdr: http.Header{}}
	k.hdr.Set("X-Forwarded-For", "203.0.113.5")
	if st := k.kioskLogin("m2", token); st != 403 {
		t.Fatalf("key bound to m1 must not open m2: %d", st)
	}
	if st := k.kioskLogin("m1", "wrong-key"); st != 403 {
		t.Fatalf("wrong key: %d", st)
	}
	// v0.3.x API body ({"monitorId": …}) is accepted as well
	legacy := &client{r: r, hc: admin.hc, hdr: http.Header{}, kiosk: "m1"}
	legacy.hdr.Set("X-Forwarded-For", "203.0.113.6")
	if st, _ := legacy.do("POST", "/api/auth/kiosk", map[string]any{"monitorId": "m1", "key": token}); st != 200 {
		t.Fatalf("legacy kiosk login body: %d", st)
	}
	if st := k.kioskLogin("m1", token); st != 200 {
		t.Fatalf("kiosk login: %d", st)
	}
	_, me := k.do("GET", "/api/auth/me", nil)
	p := me["principal"].(map[string]any)
	if p["kind"] != "kiosk" || p["role"] != "viewer" || p["monitorId"] != "m1" {
		t.Fatalf("kiosk principal: %v", p)
	}
	// the admin tab (no kiosk selector) is still admin in the same jar
	_, me = admin.do("GET", "/api/auth/me", nil)
	if me["principal"].(map[string]any)["role"] != "admin" {
		t.Fatal("kiosk login replaced the admin identity")
	}
	// scope enforcement
	_, st := k.do("GET", "/api/state", nil)
	state := st["state"].(map[string]any)
	mons := state["monitors"].([]any)
	cams := state["cameras"].([]any)
	if len(mons) != 1 || mons[0].(map[string]any)["id"] != "m1" || len(cams) != 2 {
		t.Fatalf("kiosk state not scoped: %d monitors, %d cameras", len(mons), len(cams))
	}
	if code, _ := k.do("GET", "/api/settings", nil); code != 403 {
		t.Fatalf("kiosk settings: %d", code)
	}
	if code, _ := k.do("GET", "/api/playback/info?cameraId=dvr1:2", nil); code != 403 {
		t.Fatalf("kiosk playback info outside scope: %d", code)
	}
	// kiosk sessions are live only – no playback even for their own cameras
	if code, _ := k.do("GET", "/api/playback/info?cameraId=dvr1:0", nil); code != 403 {
		t.Fatalf("kiosk playback info for own camera: %d", code)
	}
	for _, c := range cams {
		if c.(map[string]any)["playback"] == true {
			t.Fatal("kiosk state must never offer playback")
		}
	}
	if code, _ := k.do("POST", "/api/playback/search", map[string]any{"cameraId": "dvr1:2", "from": 1, "to": 2}); code != 403 {
		t.Fatalf("kiosk playback search outside scope: %d", code)
	}
	if code, _ := k.do("GET", "/api/playback/stream?cameraId=dvr1:2&start=1&end=2", nil); code != 403 {
		t.Fatalf("kiosk playback stream outside scope: %d", code)
	}
	_, ms := k.do("GET", "/api/media/status?streams=cnm_dvr1_ch01_sub,cnm_dvr1_ch03_sub", nil)
	streams := ms["streams"].(map[string]any)
	if _, ok := streams["cnm_dvr1_ch03_sub"]; ok || len(streams) != 1 {
		t.Fatalf("media status not scoped: %v", streams)
	}
	// media proxy: foreign stream and raw go2rtc API are refused
	if code := k.mediaStatus("cnm_dvr1_ch03_sub"); code != 403 {
		t.Fatalf("kiosk media for foreign stream: %d", code)
	}
	if code := k.mediaStatus("rtsp://evil/stream"); code != 403 {
		t.Fatalf("arbitrary source must be refused: %d", code)
	}
	if code, _ := k.do("GET", "/media/api/streams", nil); code != 404 {
		t.Fatalf("raw go2rtc API must be unreachable: %d", code)
	}
}

// mediaStatus performs a plain GET (non-upgrade) to read the authorisation result.
func (c *client) mediaStatus(stream string) int {
	q := url.Values{"src": {stream}}
	if c.kiosk != "" {
		q.Set("kiosk", c.kiosk)
	}
	req, _ := http.NewRequest("GET", c.r.http.URL+"/media/api/ws?"+q.Encode(), nil)
	for k, v := range c.hdr {
		req.Header[k] = v
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.r.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (c *client) dialWS(path string, q url.Values) (*websocket.Conn, error) {
	if c.kiosk != "" {
		q.Set("kiosk", c.kiosk)
	}
	u := "ws" + strings.TrimPrefix(c.r.http.URL, "http") + path + "?" + q.Encode()
	h := http.Header{}
	for k, v := range c.hdr {
		h[k] = v
	}
	cookieURL, _ := url.Parse(c.r.http.URL)
	var cookies []string
	for _, ck := range c.hc.Jar.Cookies(cookieURL) {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	h.Set("Cookie", strings.Join(cookies, "; "))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	return conn, err
}

func TestKioskRevokeTerminatesEverything(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	keyID, token := r.kioskKey(admin, "m1")
	k := r.client().remote()
	if st := k.kioskLogin("m1", token); st != 200 {
		t.Fatalf("kiosk login: %d", st)
	}
	// open control websocket and a media websocket through the proxy
	ctl, err := k.dialWS("/ws", url.Values{})
	if err != nil {
		t.Fatalf("control ws: %v", err)
	}
	defer ctl.CloseNow()
	med, err := k.dialWS("/media/api/ws", url.Values{"src": {"cnm_dvr1_ch01_sub"}})
	if err != nil {
		t.Fatalf("media ws: %v", err)
	}
	defer med.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, data, err := med.Read(ctx); err != nil || !strings.HasPrefix(string(data), "frame-") {
		t.Fatalf("media data: %v", err)
	}
	if q, _ := r.media.lastURL.Load().(string); q != "src=cnm_dvr1_ch01_sub" {
		t.Fatalf("upstream query must contain only the validated stream name, got %q", q)
	}
	// a second, unrelated viewer of the same camera
	if st, _ := admin.do("POST", "/api/settings/users", map[string]any{"username": "zweiter", "password": "viewer-pass", "role": "viewer"}); st != 200 {
		t.Fatal("create viewer")
	}
	other := r.client()
	other.login("zweiter", "viewer-pass")
	otherMed, err := other.dialWS("/media/api/ws", url.Values{"src": {"cnm_dvr1_ch01_sub"}})
	if err != nil {
		t.Fatal(err)
	}
	defer otherMed.CloseNow()

	uid := r.store.Get().Users[0].ID
	if st, _ := admin.do("DELETE", "/api/settings/users/"+uid+"/api-keys/"+keyID, nil); st != 200 {
		t.Fatalf("revoke: %d", st)
	}
	// media socket closes immediately
	errCh := make(chan error, 1)
	go func() {
		for {
			if _, _, err := med.Read(ctx); err != nil {
				errCh <- err
				return
			}
		}
	}()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("kiosk media connection not closed on revoke")
	}
	// control socket gets the revoke close code
	var closeStatus websocket.StatusCode = -1
	for i := 0; i < 50; i++ {
		_, _, err := ctl.Read(ctx)
		if err != nil {
			closeStatus = websocket.CloseStatus(err)
			break
		}
	}
	if closeStatus != 4403 {
		t.Fatalf("control ws close status %d, want 4403", closeStatus)
	}
	// the other viewer is not disturbed
	if _, _, err := otherMed.Read(ctx); err != nil {
		t.Fatalf("unrelated viewer was disturbed: %v", err)
	}
	// future requests with the revoked session/key fail
	if st, _ := k.do("GET", "/api/state", nil); st != 401 {
		t.Fatalf("revoked kiosk session still valid: %d", st)
	}
	if st := k.kioskLogin("m1", token); st != 403 {
		t.Fatalf("revoked key accepted: %d", st)
	}
}

func TestLastAdminProtection(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	uid := r.store.Get().Users[0].ID
	f := false
	viewer := "viewer"
	if st, _ := admin.do("PUT", "/api/settings/users/"+uid, map[string]any{"enabled": f}); st != 400 {
		t.Fatalf("disable last admin: %d", st)
	}
	if st, _ := admin.do("PUT", "/api/settings/users/"+uid, map[string]any{"role": viewer}); st != 400 {
		t.Fatalf("demote last admin: %d", st)
	}
	if st, _ := admin.do("DELETE", "/api/settings/users/"+uid, nil); st != 400 {
		t.Fatalf("delete self/last admin: %d", st)
	}
	// with a second admin, the first may be demoted, but nobody deletes himself
	_, body := admin.do("POST", "/api/settings/users", map[string]any{"username": "boss", "password": "boss-pass-1", "role": "admin"})
	bossID := body["id"].(string)
	boss := r.client()
	boss.login("boss", "boss-pass-1")
	if st, _ := boss.do("DELETE", "/api/settings/users/"+bossID, nil); st != 400 {
		t.Fatalf("self delete: %d", st)
	}
	if st, _ := boss.do("PUT", "/api/settings/users/"+uid, map[string]any{"role": viewer}); st != 200 {
		t.Fatalf("demote non-last admin: %d", st)
	}
	// the demoted admin loses admin rights immediately
	if st, _ := admin.do("GET", "/api/settings", nil); st != 403 {
		t.Fatalf("demoted admin still has settings access: %d", st)
	}
}

func TestLoginRateLimit(t *testing.T) {
	r := newRig(t)
	r.bootstrapAdmin()
	c := r.client().remote()
	for i := 0; i < 5; i++ {
		if st, _ := c.do("POST", "/api/auth/login", map[string]any{"username": "admin", "password": "bad"}); st != 401 {
			t.Fatalf("attempt %d: %d", i, st)
		}
	}
	if st, _ := c.do("POST", "/api/auth/login", map[string]any{"username": "admin", "password": "test-admin-pw"}); st != 429 {
		t.Fatalf("6th attempt must be rate limited: %d", st)
	}
}

func TestKeepAliveConnectionDoesNotLeak(t *testing.T) {
	r := newRig(t)
	c := r.bootstrapAdmin()
	tr := &http.Transport{MaxIdleConnsPerHost: 1}
	c.hc.Transport = tr
	defer tr.CloseIdleConnections()
	c.do("GET", "/api/state", nil)
	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 500; i++ {
		if st, _ := c.do("GET", "/api/media/status?streams=cnm_dvr1_ch01_sub", nil); st != 200 {
			t.Fatalf("status %d", st)
		}
	}
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutines grew from %d to %d over one keep-alive connection", before, after)
	}
}

func TestDeletingLastMonitorRecreatesDefault(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	for _, id := range []string{"m1", "m2"} {
		if st, _ := admin.do("DELETE", "/api/settings/monitors/"+id, nil); st != 200 {
			t.Fatalf("delete %s: %d", id, st)
		}
	}
	mons := r.store.Get().Monitors
	if len(mons) != 1 || mons[0].Label != "Monitor 1" || mons[0].Layout.Template != "2x2" {
		t.Fatalf("default monitor not recreated: %+v", mons)
	}
}

func TestLayoutHotApplyReachesClients(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	_, token := r.kioskKey(admin, "m2")
	ws, err := admin.dialWS("/ws", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	k := r.client().remote()
	if st := k.kioskLogin("m2", token); st != 200 {
		t.Fatal("kiosk login")
	}
	kws, err := k.dialWS("/ws", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer kws.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	readUntil := func(c *websocket.Conn, match func(map[string]any) bool) bool {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return false
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			if match(m) {
				return true
			}
		}
	}
	// drain initial state
	readUntil(ws, func(m map[string]any) bool { return m["type"] == "state" })
	readUntil(kws, func(m map[string]any) bool { return m["type"] == "state" })
	mon, _ := r.store.Get().Monitor("m1")
	changed := *mon
	changed.Label = "Eingang NEU"
	if st, _ := admin.do("POST", "/api/settings/monitors", changed); st != 200 {
		t.Fatal("save monitor")
	}
	if !readUntil(ws, func(m map[string]any) bool {
		if m["type"] != "state" {
			return false
		}
		b, _ := json.Marshal(m)
		return strings.Contains(string(b), "Eingang NEU")
	}) {
		t.Fatal("viewer did not receive the new layout")
	}
	// the kiosk (monitor m2) gets a state update but never sees m1
	if !readUntil(kws, func(m map[string]any) bool { return m["type"] == "state" }) {
		t.Fatal("kiosk did not receive a state update")
	}
	_, st := k.do("GET", "/api/state", nil)
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "Eingang NEU") {
		t.Fatal("kiosk state leaks another monitor")
	}
}

func TestCamerasAndGroupsAPI(t *testing.T) {
	r := newRig(t)
	admin := r.bootstrapAdmin()
	st, body := admin.do("GET", "/api/settings", nil)
	if st != 200 || len(body["cameras"].([]any)) != 8 {
		t.Fatalf("an 8 channel device must provide 8 cameras: %d", len(body["cameras"].([]any)))
	}
	// groups: create two, a camera may be in both
	_, g1 := admin.do("POST", "/api/settings/groups", map[string]any{"label": "Außen"})
	_, g2 := admin.do("POST", "/api/settings/groups", map[string]any{"label": "Einfahrt"})
	if st, _ := admin.do("POST", "/api/settings/groups", map[string]any{"label": "außen"}); st != 400 {
		t.Fatalf("duplicate group name accepted: %d", st)
	}
	gid1, gid2 := g1["id"].(string), g2["id"].(string)
	if st, _ := admin.do("POST", "/api/settings/cameras", map[string]any{
		"id": "dvr1:0", "label": "Tor Nord", "enabled": true,
		"streams":  map[string]any{"main": true, "sub": true},
		"view":     map[string]any{"aspectRatio": "4:3", "objectFit": "contain", "multiviewSource": "sub"},
		"groupIds": []string{gid1, gid2},
	}); st != 200 {
		t.Fatalf("save camera: %d", st)
	}
	if st, _ := admin.do("POST", "/api/settings/cameras/bulk", map[string]any{"ids": []string{"dvr1:1", "dvr1:2"}, "addGroup": gid1, "aspectRatio": "1:1"}); st != 200 {
		t.Fatalf("bulk: %d", st)
	}
	c := r.store.Get()
	cam, _ := c.Camera("dvr1:0")
	if cam.Label != "Tor Nord" || cam.View.AspectRatio != "4:3" || len(c.CameraGroups("dvr1:0")) != 2 {
		t.Fatalf("camera not saved: %+v groups=%v", cam, c.CameraGroups("dvr1:0"))
	}
	if g, _ := c.Group(gid1); len(g.CameraIDs) != 3 {
		t.Fatalf("group members: %v", g.CameraIDs)
	}
	if cam1, _ := c.Camera("dvr1:1"); cam1.View.AspectRatio != "1:1" {
		t.Fatal("bulk aspect not applied")
	}
	// viewers may not change cameras
	admin.do("POST", "/api/settings/users", map[string]any{"username": "viewer1", "password": "viewer-pass", "role": "viewer"})
	v := r.client()
	v.login("viewer1", "viewer-pass")
	if st, _ := v.do("POST", "/api/settings/groups", map[string]any{"label": "x"}); st != 403 {
		t.Fatalf("viewer created a group: %d", st)
	}
	// new device: UUID assigned by the server, cameras created automatically
	st, dev := admin.do("POST", "/api/settings/devices", map[string]any{
		"label": "Neuer Rekorder", "protocol": "rtsp", "host": "192.0.2.9",
		"options": map[string]any{"channels": "4", "mainPath": "/ch{ch}/main", "subPath": "/ch{ch}/sub"},
	})
	if st != 200 || len(dev["id"].(string)) != 36 || dev["channelCount"].(float64) != 4 {
		t.Fatalf("device create: %d %v", st, dev)
	}
	if n := len(r.store.Get().Cameras); n != 12 {
		t.Fatalf("8 + 4 channels must provide 12 cameras, got %d", n)
	}
	// deleting the device removes its cameras
	if st, _ := admin.do("DELETE", "/api/settings/devices/"+dev["id"].(string), nil); st != 200 {
		t.Fatal("delete device")
	}
	if n := len(r.store.Get().Cameras); n != 8 {
		t.Fatalf("cameras after delete: %d", n)
	}
	if st, _ := admin.do("DELETE", "/api/settings/groups/"+gid2, nil); st != 200 || len(r.store.Get().CameraGroups("dvr1:0")) != 1 {
		t.Fatal("delete group")
	}
}
