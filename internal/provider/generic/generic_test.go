package generic

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"cnm/internal/provider"
)

type env struct{ locked atomic.Bool }

func (e *env) AllowLogin() bool                              { return !e.locked.Load() }
func (e *env) ReportAuthFailure(error)                       { e.locked.Store(true) }
func (e *env) LegacyLiveURL(int, provider.StreamKind) string { return "" }

func drv(t *testing.T, model, host string, ports map[string]int, opts map[string]string, user, pass string) (*driver, *env) {
	m, ok := prov{}.Info().FindModel(model)
	if !ok {
		t.Fatalf("model %s", model)
	}
	e := &env{}
	d, err := prov{}.NewDriver(provider.DeviceConfig{Model: m, Host: host, Ports: ports, Options: opts, Username: user, Password: pass}, e)
	if err != nil {
		t.Fatal(err)
	}
	return d.(*driver), e
}

func TestURLTemplates(t *testing.T) {
	d, _ := drv(t, ProtoRTSP, "192.0.2.5", map[string]int{"rtsp": 554},
		map[string]string{provider.OptChannels: "8", OptMainPath: "/Streaming/Channels/{ch}01", OptSubPath: "/cam?ch={ch0}&s={ch2}"}, "admin", "p@ss:w/rd")
	main := d.LiveSources(2, provider.Main)[0]
	if !strings.HasPrefix(main, "rtsp://admin:p%40ss%3Aw%2Frd@192.0.2.5:554/Streaming/Channels/301") {
		t.Fatalf("main url %s", main)
	}
	sub := d.LiveSources(2, provider.Sub)[0]
	if !strings.Contains(sub, "/cam?ch=2&s=03#") {
		t.Fatalf("sub url %s", sub)
	}
	if provider.ChannelCount(d.cfg.Model, d.cfg.Options) != 8 {
		t.Fatal("channel count from option")
	}
	single, _ := drv(t, ProtoRTSP, "cam", map[string]int{"rtsp": 8554}, map[string]string{OptMainPath: "/live", OptSubPath: ""}, "", "")
	if single.LiveSources(0, provider.Sub) != nil {
		t.Fatal("empty sub path must mean no substream")
	}
	onvif, _ := drv(t, ProtoONVIF, "cam", map[string]int{"http": 8080}, nil, "u", "p")
	if got := onvif.LiveSources(0, provider.Sub)[0]; got != "onvif://u:p@cam:8080?subtype=1#backchannel=0#media=video" {
		t.Fatalf("onvif url %s", got)
	}
	tcp, _ := drv(t, ProtoTCP, "enc", map[string]int{"tcp": 9000}, nil, "", "")
	if tcp.LiveSources(0, provider.Main)[0] != "tcp://enc:9000" || tcp.LiveSources(0, provider.Sub) != nil {
		t.Fatal("tcp sources")
	}
}

var reCreated = regexp.MustCompile(`<Created[^>]*>([^<]+)</Created>`)
var reNonce = regexp.MustCompile(`<Nonce[^>]*>([^<]+)</Nonce>`)
var rePassword = regexp.MustCompile(`<Password[^>]*>([^<]+)</Password>`)

// fake ONVIF device validating the WS-UsernameToken digest
func onvifServer(t *testing.T, user, pass string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		if strings.Contains(body, "GetSystemDateAndTime") {
			io.WriteString(w, `<s:Envelope><s:Body><tds:GetSystemDateAndTimeResponse><tt:UTCDateTime><tt:Time><tt:Hour>10</tt:Hour><tt:Minute>11</tt:Minute><tt:Second>12</tt:Second></tt:Time><tt:Date><tt:Year>2026</tt:Year><tt:Month>3</tt:Month><tt:Day>4</tt:Day></tt:Date></tt:UTCDateTime></tds:GetSystemDateAndTimeResponse></s:Body></s:Envelope>`)
			return
		}
		nonce, _ := base64.StdEncoding.DecodeString(reNonce.FindStringSubmatch(body + "<Nonce>x</Nonce>")[1])
		created := reCreated.FindStringSubmatch(body + "<Created>x</Created>")[1]
		h := sha1.New()
		h.Write(nonce)
		h.Write([]byte(created))
		h.Write([]byte(pass))
		want := base64.StdEncoding.EncodeToString(h.Sum(nil))
		got := rePassword.FindStringSubmatch(body + "<Password>x</Password>")[1]
		if !strings.Contains(body, "<Username>"+user+"</Username>") || got != want || created != "2026-03-04T10:11:12Z" {
			w.WriteHeader(400)
			io.WriteString(w, `<s:Fault><s:Code><s:Subcode><s:Value>ter:NotAuthorized</s:Value></s:Subcode></s:Code></s:Fault>`)
			return
		}
		io.WriteString(w, `<s:Envelope><s:Body><tds:GetDeviceInformationResponse><tds:Manufacturer>ACME</tds:Manufacturer><tds:Model>Cam 3000</tds:Model><tds:FirmwareVersion>1.2</tds:FirmwareVersion><tds:SerialNumber>SN9</tds:SerialNumber></tds:GetDeviceInformationResponse></s:Body></s:Envelope>`)
	}))
}

func hostPort(t *testing.T, u string) (string, int) {
	h, p, _ := net.SplitHostPort(strings.TrimPrefix(u, "http://"))
	n, _ := strconv.Atoi(p)
	return h, n
}

func TestONVIFLogin(t *testing.T) {
	srv := onvifServer(t, "admin", "geheim")
	defer srv.Close()
	h, p := hostPort(t, srv.URL)
	d, _ := drv(t, ProtoONVIF, h, map[string]int{"http": p}, nil, "admin", "geheim")
	info, err := d.Connect(context.Background())
	if err != nil || info.Model != "ACME Cam 3000" || info.Serial != "SN9" {
		t.Fatalf("onvif info: %+v %v", info, err)
	}
	bad, e := drv(t, ProtoONVIF, h, map[string]int{"http": p}, nil, "admin", "falsch")
	_, err = bad.Connect(context.Background())
	if provider.KindOf(err) != provider.KindAuth || !e.locked.Load() {
		t.Fatalf("wrong ONVIF password must be an auth error and lock: %v", err)
	}
}

func TestRTMPHandshakeAndHTTPProbe(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1537)
		io.ReadFull(c, buf)
		c.Write(append([]byte{3}, make([]byte, 1536)...))
	}()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	if err := rtmpHandshake(context.Background(), "127.0.0.1", port); err != nil {
		t.Fatalf("rtmp handshake: %v", err)
	}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, pw, ok := r.BasicAuth()
		if !ok || u != "cam" || pw != "pw" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=x")
		io.WriteString(w, "--x\r\n")
	}))
	defer hs.Close()
	if err := httpProbe(context.Background(), hs.URL+"/video.mjpg", "cam", "pw"); err != nil {
		t.Fatalf("http probe: %v", err)
	}
	if err := httpProbe(context.Background(), hs.URL+"/video.mjpg", "cam", "nope"); provider.KindOf(err) != provider.KindAuth {
		t.Fatalf("http auth error expected, got %v", err)
	}
}
