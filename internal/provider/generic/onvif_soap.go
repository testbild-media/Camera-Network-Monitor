package generic

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnm/internal/provider"
)

// ONVIF service namespaces as announced by GetServices.
const (
	nsDevice    = "http://www.onvif.org/ver10/device/wsdl"
	nsMedia     = "http://www.onvif.org/ver10/media/wsdl"
	nsSearch    = "http://www.onvif.org/ver10/search/wsdl"
	nsReplay    = "http://www.onvif.org/ver10/replay/wsdl"
	nsRecording = "http://www.onvif.org/ver10/recording/wsdl"
)

// soapEnvelope declares every prefix used in request bodies. The ONVIF
// schema is elementFormDefault="qualified": child elements of tt types need
// the tt: prefix.
const soapEnvelope = `<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"` +
	` xmlns:tds="` + nsDevice + `" xmlns:trt="` + nsMedia + `" xmlns:tse="` + nsSearch + `"` +
	` xmlns:trp="` + nsReplay + `" xmlns:trc="` + nsRecording + `" xmlns:tt="http://www.onvif.org/ver10/schema">`

// svcRetry is the minimum interval between GetServices attempts after a failure.
const svcRetry = time.Minute

// onvifDev is the state shared by the ONVIF models: device clock, service
// addresses, the NVR channel/stream table and the recording search cache.
type onvifDev struct {
	host       string
	port       int
	user, pass string
	endpoint   string // device service
	hc         *http.Client

	mu       sync.Mutex
	clockOff time.Duration // device UTC - real UTC
	clockOK  bool
	svc      map[string]string // namespace → XAddr; nil until GetServices answered
	svcErr   error
	svcAt    time.Time
	media    *onvifMedia // ONVIF-NVR: channels and live stream URIs
	recs     []onvifRecording
	recsErr  error
	recsAt   time.Time

	searchMu sync.Mutex // one recording search per device at a time
}

func newONVIFDev(cfg provider.DeviceConfig) *onvifDev {
	port := cfg.Port("http")
	return &onvifDev{
		host: cfg.Host, port: port, user: cfg.Username, pass: cfg.Password,

		endpoint: "http://" + provider.HostPort(cfg.Host, port) + onvifDevicePath,
		hc:       provider.NewHTTPClient(cfg.Username, cfg.Password, 20*time.Second),
	}
}

func (o *onvifDev) close() { provider.CloseIdle(o.hc) }

func (o *onvifDev) setClock(off *time.Duration) {
	if off == nil {
		return
	}
	o.mu.Lock()
	o.clockOff, o.clockOK = *off, true
	o.mu.Unlock()
}

// created returns the WS-Security timestamp in device time. The clock is
// read (unauthenticated) when it has not been measured yet.
func (o *onvifDev) created(ctx context.Context) time.Time {
	o.mu.Lock()
	off, ok := o.clockOff, o.clockOK
	o.mu.Unlock()
	if !ok {
		// devices without a readable clock get the local time (asked once)
		off = 0
		if t, found := onvifTime(ctx, o.endpoint); found {
			off = provider.MeasureOffset(t, time.Now())
		}
		o.setClock(&off)
	}
	return time.Now().Add(off)
}

var (
	reFault  = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?Fault[\s>]`)
	reReason = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?(?:Text|faultstring)[^>]*>([^<]*)</`)
)

// call sends an authenticated SOAP request (WS-UsernameToken digest; HTTP
// Digest is answered by the transport) and decodes the envelope into out.
// Field paths of out start at the envelope ("Body>…Response>…") and match
// local names, so any namespace prefix is accepted.
func (o *onvifDev) call(ctx context.Context, xaddr, op, body string, out any) error {
	env := soapEnvelope + wsSecurity(o.user, o.pass, o.created(ctx)) + `<s:Body>` + body + `</s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, xaddr, strings.NewReader(env))
	if err != nil {
		return provider.NewError(provider.KindConfig, nil, "ONVIF %s: ungültige Dienstadresse", op)
	}
	req.Header.Set("Content-Type", `application/soap+xml; charset=utf-8`)
	resp, err := o.hc.Do(req)
	if err != nil {
		return provider.ClassifyNetErr(err, "ONVIF "+op)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return provider.ClassifyNetErr(err, "ONVIF "+op)
	}
	if resp.StatusCode != http.StatusOK || reFault.Match(b) {
		return soapFault(op, resp.StatusCode, b)
	}
	if out != nil {
		if err := xml.Unmarshal(b, out); err != nil {
			return provider.NewError(provider.KindProtocol, err, "ONVIF %s: ungültige Antwort", op)
		}
	}
	return nil
}

// soapFault classifies an error answer.
func soapFault(op string, status int, b []byte) error {
	s := string(b)
	reason := ""
	if m := reReason.FindSubmatch(b); m != nil {
		reason = strings.TrimSpace(html.UnescapeString(string(m[1])))
		if len(reason) > 120 {
			reason = reason[:120]
		}
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || strings.Contains(s, "NotAuthorized") || strings.Contains(strings.ToLower(s), "not authorized"):
		return provider.NewError(provider.KindAuth, nil, "ONVIF %s: Zugriff verweigert", op)
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed || strings.Contains(s, "ActionNotSupported") || strings.Contains(s, "NotImplemented"):
		return provider.NewError(provider.KindUnsupported, nil, "ONVIF %s wird vom Gerät nicht unterstützt", op)
	}
	msg := fmt.Sprintf("ONVIF %s: Gerät meldet Fehler (HTTP %d)", op, status)
	if reason != "" {
		msg += ": " + reason
	}
	return provider.NewError(provider.KindProtocol, nil, "%s", provider.Redact(msg))
}

// services returns the service addresses (GetServices). Devices without
// GetServices (ONVIF < 2.0) are assumed to serve media at the device endpoint.
func (o *onvifDev) services(ctx context.Context) (map[string]string, error) {
	o.mu.Lock()
	if o.svc != nil {
		defer o.mu.Unlock()
		return o.svc, nil
	}
	if o.svcErr != nil && time.Since(o.svcAt) < svcRetry {
		defer o.mu.Unlock()
		return nil, o.svcErr
	}
	o.mu.Unlock()

	var r struct {
		Services []struct {
			Namespace string `xml:"Namespace"`
			XAddr     string `xml:"XAddr"`
		} `xml:"Body>GetServicesResponse>Service"`
	}
	err := o.call(ctx, o.endpoint, "GetServices", `<tds:GetServices><tds:IncludeCapability>false</tds:IncludeCapability></tds:GetServices>`, &r)
	svc := map[string]string{}
	switch {
	case err == nil:
		for _, s := range r.Services {
			if x := o.fixXAddr(strings.TrimSpace(s.XAddr)); x != "" {
				svc[strings.TrimSpace(s.Namespace)] = x
			}
		}
	case provider.KindOf(err) == provider.KindUnsupported:
		svc[nsDevice], svc[nsMedia] = o.endpoint, o.endpoint
	default:
		o.mu.Lock()
		o.svcErr, o.svcAt = err, time.Now()
		o.mu.Unlock()
		return nil, err
	}
	if svc[nsMedia] == "" {
		svc[nsMedia] = o.endpoint
	}
	o.mu.Lock()
	o.svc, o.svcErr, o.svcAt = svc, nil, time.Now()
	o.mu.Unlock()
	return svc, nil
}

// fixXAddr points an announced address at the configured device: devices
// behind NAT/port forwarding or with several interfaces announce addresses
// CNM cannot reach. The path is kept.
func (o *onvifDev) fixXAddr(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	u.User = nil
	if !strings.EqualFold(strings.Trim(u.Hostname(), "[]"), strings.Trim(o.host, "[]")) {
		u.Scheme = "http"
		u.Host = provider.HostPort(o.host, o.port)
	}
	return u.String()
}

// fixStreamURI removes credentials from an announced RTSP URI and replaces
// a foreign host by the configured one (the port is kept).
func (o *onvifDev) fixStreamURI(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || !strings.HasPrefix(strings.ToLower(u.Scheme), "rtsp") {
		return ""
	}
	u.User = nil
	if !strings.EqualFold(strings.Trim(u.Hostname(), "[]"), strings.Trim(o.host, "[]")) {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port <= 0 {
			port = 554
		}
		u.Host = provider.HostPort(o.host, port)
	}
	return u.String()
}

// withCredentials injects the device credentials into a stream URI (the
// result contains secrets and must only be handed to go2rtc/FFmpeg).
func (o *onvifDev) withCredentials(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	if o.user != "" || o.pass != "" {
		u.User = url.UserPassword(o.user, o.pass)
	}
	return u.String()
}

// xmlText escapes a token for a request body.
func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// parseONVIFTime parses xs:dateTime values (UTC when no zone is given).
func parseONVIFTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			// devices report 1970/0001 for "no data"
			return t.UTC(), t.Year() > 1971
		}
	}
	return time.Time{}, false
}
