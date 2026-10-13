// Package generic provides standard protocol sources without manufacturer
// specific control: ONVIF cameras and recorders, RTSP cameras and recorders
// (URL template), RTMP streams, HTTP streams (MJPEG / H.264 / MPEG-TS) and
// raw TCP streams of legacy H.264 devices.
//
// ONVIF devices with Profile G additionally offer recording search (span
// only) and replay (onvif_recording.go); all other models are live only.
package generic

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cnm/internal/provider"
)

// ProviderID of the standard protocols.
const ProviderID = "generic"

// Protocol ids stored in the configuration.
const (
	ProtoONVIF = "onvif"
	ProtoRTSP  = "rtsp"
	ProtoRTMP  = "rtmp"
	ProtoHTTP  = "http"
	ProtoTCP   = "tcp"
)

// Option keys.
const (
	OptMainPath = "mainPath"
	OptSubPath  = "subPath"
)

var liveOnly = provider.Capabilities{
	LiveMain: provider.Verified, LiveSub: provider.Verified,
	RecordingSearch: provider.Unsupported, Playback: provider.Unsupported,
	DeviceTime: provider.Unsupported, StorageInfo: provider.Unsupported,
}

// ONVIF (Profile S cameras and recorders, Profile G recordings): live with
// go2rtc's ONVIF client as fallback (in production use), the rest
// implemented from the specification.
var onvifCaps = provider.Capabilities{
	Discovery: provider.Experimental, ChannelList: provider.Experimental,
	LiveMain: provider.Verified, LiveSub: provider.Verified,
	RecordingSearch: provider.Experimental, SearchKind: "span",
	Playback:    provider.Experimental,
	DeviceTime:  provider.Experimental,
	StorageInfo: provider.Unsupported,
}

const onvifAuth = "WS-UsernameToken (Digest) · RTSP Digest"

func pathOpt(key, label, def, placeholder, help string) provider.OptionSpec {
	return provider.OptionSpec{Key: key, Label: label, Default: def, Type: provider.OptionText, Section: provider.SectionNetwork, Placeholder: placeholder, Help: help}
}

type prov struct{}

func init() { provider.Register(prov{}) }

func (prov) Info() provider.Info {
	return provider.Info{
		ID:    ProviderID,
		Name:  "Standardprotokolle",
		Order: 90,
		Models: []provider.Model{
			{
				ID: ProtoONVIF, Name: "ONVIF (Profile S/G)", AutoDetect: true,
				Ports:        []provider.PortSpec{{Key: "http", Label: "HTTP-Port (ONVIF)", Default: 80}},
				Options:      []provider.OptionSpec{provider.ChannelOverride()},
				Capabilities: onvifCaps, Auth: onvifAuth,
				Notes: "Kanäle = ONVIF-Videoquellen, Mainstream = erstes, Substream = zweites Videoprofil der Quelle (Profile ohne Video zählen nicht); " +
					"CNM ermittelt die Stream-URLs selbst, go2rtcs ONVIF-Client dient als Rückfall. " +
					"Aufnahmen (Profile G): die Suche liefert nur die Zeitspanne von der ersten bis zur letzten Aufnahme, Lücken sind nicht erkennbar. " +
					"Wiedergabe über ONVIF-Replay (RTSP).",
			},
			{
				ID: ProtoRTSP, Name: "RTSP (URL)", Channels: 0,
				Ports: []provider.PortSpec{{Key: "rtsp", Label: "RTSP", Default: 554}},
				Options: []provider.OptionSpec{
					pathOpt(OptMainPath, "Pfad Mainstream", "/", "/stream1", "Pfad inkl. Query; bei mehreren Kanälen mit Platzhalter {ch} (ab 1), {ch0} (ab 0) oder {ch2} (zweistellig)"),
					pathOpt(OptSubPath, "Pfad Substream", "", "/stream2", "leer = kein Substream"),
					{Key: provider.OptChannels, Label: "Kanäle", Default: "1", Type: provider.OptionNumber, Section: provider.SectionNetwork, Min: 1, Max: 64, Help: "Anzahl Kameras (Platzhalter {ch} im Pfad)"},
				},
				Capabilities: liveOnly,
			},
			{
				ID: ProtoRTMP, Name: "RTMP", Channels: 1,
				Ports: []provider.PortSpec{{Key: "rtmp", Label: "RTMP", Default: 1935}},
				Options: []provider.OptionSpec{
					pathOpt(OptMainPath, "Pfad Mainstream", "/live/main", "/live/stream", ""),
					pathOpt(OptSubPath, "Pfad Substream", "", "/live/sub", "leer = kein Substream"),
				},
				Capabilities: liveOnly,
			},
			{
				ID: ProtoHTTP, Name: "HTTP-Stream (MJPEG / H.264 / MPEG-TS)", Channels: 1,
				Ports: []provider.PortSpec{{Key: "http", Label: "HTTP", Default: 80}},
				Options: []provider.OptionSpec{
					pathOpt(OptMainPath, "Pfad Mainstream", "/video.mjpg", "/video.mjpg", "MJPEG (multipart), JPEG, H.264-Rohstrom oder MPEG-TS"),
					pathOpt(OptSubPath, "Pfad Substream", "", "", "leer = kein Substream"),
				},
				Capabilities: liveOnly,
			},
			{
				ID: ProtoTCP, Name: "TCP-Rohstrom (H.264)", Channels: 1,
				Ports:        []provider.PortSpec{{Key: "tcp", Label: "TCP", Default: 8000}},
				Capabilities: provider.Capabilities{LiveMain: provider.Verified, LiveSub: provider.Unsupported, RecordingSearch: provider.Unsupported, Playback: provider.Unsupported, DeviceTime: provider.Unsupported, StorageInfo: provider.Unsupported},
				Notes:        "Gerät sendet nach dem Verbindungsaufbau einen H.264-/H.265-Annex-B- oder MPEG-TS-Rohstrom.",
			},
		},
	}
}

func (prov) NewDriver(cfg provider.DeviceConfig, env provider.Env) (provider.Driver, error) {
	if cfg.Host == "" {
		return nil, provider.NewError(provider.KindConfig, nil, "Host fehlt")
	}
	d := &driver{cfg: cfg, env: env}
	if cfg.Model.ID == ProtoONVIF {
		d.ov = newONVIFDev(cfg)
	}
	return d, nil
}

type driver struct {
	cfg provider.DeviceConfig
	env provider.Env
	ov  *onvifDev // ONVIF: clock, services, streams, recordings
}

func (d *driver) opt(key string) string { return strings.TrimSpace(d.cfg.Option(key)) }

// expand replaces channel placeholders in URL templates.
func expand(tpl string, channel int) string {
	r := strings.NewReplacer("{ch0}", strconv.Itoa(channel), "{ch2}", fmt.Sprintf("%02d", channel+1), "{ch}", strconv.Itoa(channel+1))
	return r.Replace(tpl)
}

// buildURL composes scheme://user:pass@host:port/path?query keeping the
// query exactly as configured.
func buildURL(scheme, user, pass, host string, port int, pathQuery string) string {
	if pathQuery == "" {
		pathQuery = "/"
	}
	if !strings.HasPrefix(pathQuery, "/") {
		pathQuery = "/" + pathQuery
	}
	path, query, _ := strings.Cut(pathQuery, "?")
	u := url.URL{Scheme: scheme, Host: provider.HostPort(host, port), Path: path, RawQuery: query}
	if user != "" || pass != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}

func (d *driver) streamPath(channel int, kind provider.StreamKind) string {
	key := OptMainPath
	if kind == provider.Sub {
		key = OptSubPath
	}
	return expand(d.opt(key), channel)
}

func (d *driver) LiveSources(channel int, kind provider.StreamKind) []string {
	c := d.cfg
	switch c.Model.ID {
	case ProtoONVIF:
		return d.onvifSources(channel, kind)
	case ProtoRTSP:
		p := d.streamPath(channel, kind)
		if kind == provider.Sub && d.opt(OptSubPath) == "" {
			return nil
		}
		return []string{buildURL("rtsp", c.Username, c.Password, c.Host, c.Port("rtsp"), p) + "#backchannel=0#media=video"}
	case ProtoRTMP:
		if kind == provider.Sub && d.opt(OptSubPath) == "" {
			return nil
		}
		return []string{buildURL("rtmp", c.Username, c.Password, c.Host, c.Port("rtmp"), d.streamPath(channel, kind))}
	case ProtoHTTP:
		if kind == provider.Sub && d.opt(OptSubPath) == "" {
			return nil
		}
		return []string{buildURL("http", c.Username, c.Password, c.Host, c.Port("http"), d.streamPath(channel, kind))}
	case ProtoTCP:
		if kind == provider.Sub {
			return nil
		}
		return []string{"tcp://" + provider.HostPort(c.Host, c.Port("tcp"))}
	}
	return nil
}

func (d *driver) Connect(ctx context.Context) (*provider.DeviceInfo, error) {
	info, err := d.probe(ctx)
	if err != nil {
		if provider.KindOf(err) == provider.KindAuth {
			d.env.ReportAuthFailure(err)
		}
		return nil, err
	}
	return info, nil
}

func (d *driver) Check(ctx context.Context) (*provider.Health, error) {
	if d.ov != nil {
		return d.onvifCheck(ctx)
	}
	if _, err := d.Connect(ctx); err != nil {
		return nil, err
	}
	return &provider.Health{Storage: provider.StorageUnknown}, nil
}

func (d *driver) Close() {
	if d.ov != nil {
		d.ov.close()
	}
}

func (d *driver) Test(ctx context.Context) (*provider.TestResult, error) {
	info, err := d.Connect(ctx)
	if err != nil {
		return nil, err
	}
	msg := "Verbindung erfolgreich"
	switch d.cfg.Model.ID {
	case ProtoONVIF:
		msg = fmt.Sprintf("Erkannt: %s · %d Kanäle", info.Summary, info.Channels)
	case ProtoRTSP:
		msg = "Login erfolgreich"
	}
	return &provider.TestResult{Info: info, Message: msg}, nil
}

// probe performs a real protocol level check (login where the protocol has one).
func (d *driver) probe(ctx context.Context) (*provider.DeviceInfo, error) {
	c := d.cfg
	info := &provider.DeviceInfo{Channels: provider.ChannelCount(c.Model, c.Options)}
	switch c.Model.ID {
	case ProtoONVIF:
		return d.onvifConnect(ctx)
	case ProtoRTSP:
		if !d.env.AllowLogin() {
			return nil, provider.ErrLoginBlocked
		}
		p := d.streamPath(0, provider.Main)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return info, provider.RTSPProbe(ctx, c.Host, c.Port("rtsp"), c.Username, c.Password, p)
	case ProtoRTMP:
		return info, rtmpHandshake(ctx, c.Host, c.Port("rtmp"))
	case ProtoHTTP:
		if !d.env.AllowLogin() {
			return nil, provider.ErrLoginBlocked
		}
		return info, httpProbe(ctx, buildURL("http", "", "", c.Host, c.Port("http"), d.streamPath(0, provider.Main)), c.Username, c.Password)
	case ProtoTCP:
		return info, tcpProbe(ctx, c.Host, c.Port("tcp"))
	}
	return nil, provider.NewError(provider.KindConfig, nil, "unbekanntes Modell")
}

// rtmpHandshake verifies that an RTMP server answers (C0/C1 → S0/S1).
func rtmpHandshake(ctx context.Context, host string, port int) error {
	conn, err := provider.Dial(ctx, host, port, 5*time.Second)
	if err != nil {
		return provider.ClassifyNetErr(err, "RTMP")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
	c1 := make([]byte, 1+1536)
	c1[0] = 3
	_, _ = rand.Read(c1[9:])
	if _, err := conn.Write(c1); err != nil {
		return provider.ClassifyNetErr(err, "RTMP")
	}
	s := make([]byte, 1+1536)
	if _, err := io.ReadFull(conn, s); err != nil {
		return provider.ClassifyNetErr(err, "RTMP")
	}
	if s[0] != 3 {
		return provider.NewError(provider.KindProtocol, nil, "RTMP: unerwartete Handshake-Version %d", s[0])
	}
	return nil
}

// tcpProbe expects the device to start sending data after connecting.
func tcpProbe(ctx context.Context, host string, port int) error {
	conn, err := provider.Dial(ctx, host, port, 5*time.Second)
	if err != nil {
		return provider.ClassifyNetErr(err, "TCP")
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	b := make([]byte, 1)
	if _, err := io.ReadFull(bufio.NewReader(conn), b); err != nil {
		return provider.ClassifyNetErr(err, "TCP-Datenstrom")
	}
	return nil
}

// httpProbe opens the stream URL (Basic or Digest authentication) and reads
// the first bytes.
func httpProbe(ctx context.Context, rawURL, user, pass string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 8 * time.Second}}
	defer hc.CloseIdleConnections()
	do := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		} else if user != "" {
			req.SetBasicAuth(user, pass)
		}
		return hc.Do(req)
	}
	resp, err := do("")
	if err != nil {
		return provider.ClassifyNetErr(err, "HTTP")
	}
	if resp.StatusCode == http.StatusUnauthorized && strings.HasPrefix(strings.ToLower(resp.Header.Get("WWW-Authenticate")), "digest") && user != "" {
		ch := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		u, _ := url.Parse(rawURL)
		auth, aerr := provider.DigestAuthorization(ch, user, pass, http.MethodGet, u.RequestURI())
		if aerr != nil {
			return provider.NewError(provider.KindProtocol, aerr, "HTTP-Authentifizierung nicht unterstützt")
		}
		if resp, err = do(auth); err != nil {
			return provider.ClassifyNetErr(err, "HTTP")
		}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return provider.NewError(provider.KindAuth, nil, "HTTP: Benutzername oder Passwort falsch")
	case resp.StatusCode != http.StatusOK:
		return provider.NewError(provider.KindProtocol, nil, "HTTP antwortet mit Status %d", resp.StatusCode)
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, b); err != nil {
		return provider.ClassifyNetErr(err, "HTTP-Datenstrom")
	}
	return nil
}
