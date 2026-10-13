package generic

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"cnm/internal/provider"
)

const onvifDevicePath = "/onvif/device_service"

var tagRe = func(name string) *regexp.Regexp {
	return regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?` + name + `>([^<]*)</`)
}

var (
	reManufacturer = tagRe("Manufacturer")
	reModel        = tagRe("Model")
	reFirmware     = tagRe("FirmwareVersion")
	reSerial       = tagRe("SerialNumber")
	reYear         = tagRe("Year")
	reMonth        = tagRe("Month")
	reDay          = tagRe("Day")
	reHour         = tagRe("Hour")
	reMinute       = tagRe("Minute")
	reSecond       = tagRe("Second")
	reUTC          = regexp.MustCompile(`(?s)<(?:[A-Za-z0-9_]+:)?UTCDateTime>(.*?)</(?:[A-Za-z0-9_]+:)?UTCDateTime>`)
)

func find(re *regexp.Regexp, b []byte) string {
	m := re.FindSubmatch(b)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(string(m[1])))
}

// wsSecurity builds the WS-UsernameToken header with a password digest:
// base64(sha1(nonce + created + password)).
func wsSecurity(user, pass string, created time.Time) string {
	if user == "" {
		return ""
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	c := created.UTC().Format("2006-01-02T15:04:05Z")
	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(c))
	h.Write([]byte(pass))
	digest := base64.StdEncoding.EncodeToString(h.Sum(nil))
	esc := func(s string) string { return html.EscapeString(s) }
	return `<s:Header><Security s:mustUnderstand="1" xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">` +
		`<UsernameToken><Username>` + esc(user) + `</Username>` +
		`<Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` + digest + `</Password>` +
		`<Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` + base64.StdEncoding.EncodeToString(nonce) + `</Nonce>` +
		`<Created xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` + c + `</Created>` +
		`</UsernameToken></Security></s:Header>`
}

func soap(ctx context.Context, endpoint, header, body string) (int, []byte, error) {
	env := `<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">` + header +
		`<s:Body xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">` + body + `</s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(env))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", `application/soap+xml; charset=utf-8`)
	hc := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 8 * time.Second}}
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// onvifTime reads the camera clock (unauthenticated per ONVIF spec) so that
// the WS-Security "Created" stamp matches the device time.
func onvifTime(ctx context.Context, endpoint string) (time.Time, bool) {
	status, b, err := soap(ctx, endpoint, "", `<GetSystemDateAndTime xmlns="http://www.onvif.org/ver10/device/wsdl"/>`)
	if err != nil || status != http.StatusOK {
		return time.Time{}, false
	}
	m := reUTC.FindSubmatch(b)
	if m == nil {
		return time.Time{}, false
	}
	part := m[1]
	var y, mo, d, h, mi, s int
	_, e1 := fmt.Sscan(find(reYear, part), &y)
	_, e2 := fmt.Sscan(find(reMonth, part), &mo)
	_, e3 := fmt.Sscan(find(reDay, part), &d)
	_, e4 := fmt.Sscan(find(reHour, part), &h)
	_, e5 := fmt.Sscan(find(reMinute, part), &mi)
	_, e6 := fmt.Sscan(find(reSecond, part), &s)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil {
		return time.Time{}, false
	}
	return time.Date(y, time.Month(mo), d, h, mi, s, 0, time.UTC), true
}

// onvifDeviceInfo performs an authenticated GetDeviceInformation (a real
// login check) and returns manufacturer, model, firmware and serial. The
// second result is the measured device clock offset (device UTC - real UTC),
// nil when the device did not report its time.
func onvifDeviceInfo(ctx context.Context, host string, port int, user, pass string) (*provider.DeviceInfo, *time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	endpoint := "http://" + provider.HostPort(host, port) + onvifDevicePath
	created := time.Now()
	var offset *time.Duration
	if t, ok := onvifTime(ctx, endpoint); ok {
		created = t
		off := provider.MeasureOffset(t, time.Now())
		offset = &off
	}
	status, b, err := soap(ctx, endpoint, wsSecurity(user, pass, created), `<GetDeviceInformation xmlns="http://www.onvif.org/ver10/device/wsdl"/>`)
	if err != nil {
		return nil, nil, provider.ClassifyNetErr(err, "ONVIF")
	}
	body := string(b)
	if status != http.StatusOK {
		if status == http.StatusUnauthorized || strings.Contains(body, "NotAuthorized") || strings.Contains(strings.ToLower(body), "not authorized") {
			return nil, nil, provider.NewError(provider.KindAuth, nil, "ONVIF: Benutzername oder Passwort falsch")
		}
		return nil, nil, provider.NewError(provider.KindProtocol, nil, "ONVIF antwortet mit HTTP %d", status)
	}
	info := &provider.DeviceInfo{
		Model:    strings.TrimSpace(find(reManufacturer, b) + " " + find(reModel, b)),
		Firmware: find(reFirmware, b),
		Serial:   find(reSerial, b),
	}
	return info, offset, nil
}
