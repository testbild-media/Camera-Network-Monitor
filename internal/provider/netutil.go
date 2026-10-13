package provider

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// HostPort joins host and port, bracketing IPv6 literals.
func HostPort(host string, port int) string {
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
}

// CredURL builds scheme://user:pass@host:port/path?query with proper escaping.
// The result contains secrets and must only be handed to go2rtc/FFmpeg.
func CredURL(scheme, user, pass, host string, port int, path string, query url.Values, fragment string) string {
	u := url.URL{Scheme: scheme, Host: HostPort(host, port), Path: path}
	if user != "" || pass != "" {
		u.User = url.UserPassword(user, pass)
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	s := u.String()
	if fragment != "" {
		s += "#" + fragment
	}
	return s
}

var (
	userinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s"']*@`)
	// the same URL percent-encoded inside a query string (e.g. go2rtc API
	// requests "src=rtsp%3A%2F%2Fuser%3Apass%40host")
	encUserinfoRe = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*%3A%2F%2F)(?:[^&\s"'/%]|%[0-9a-f]{2})*?%40`)
)

// Redact removes userinfo from URLs embedded in s – plain or percent-encoded.
// Use it for every string that may leave the backend (logs, API error details).
func Redact(s string) string {
	s = userinfoRe.ReplaceAllString(s, "${1}***@")
	return encUserinfoRe.ReplaceAllString(s, "${1}***%40")
}

// ClassifyNetErr wraps transport level errors as KindNetwork with a clean message.
func ClassifyNetErr(err error, what string) error {
	if err == nil {
		return nil
	}
	var pe *Error
	if errors.As(err, &pe) {
		return err
	}
	msg := "Verbindungsfehler"
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		msg = "Zeitüberschreitung"
	case isConnRefused(err):
		msg = "Verbindung abgelehnt"
		var op *net.OpError
		if errors.As(err, &op) && op.Addr != nil {
			// port closed: name the address so a wrong port is obvious
			msg += " (" + op.Addr.String() + " – Port prüfen)"
		}
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		msg = "Host nicht erreichbar"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, os.ErrDeadlineExceeded):
		msg = "Verbindung vom Gerät geschlossen"
	case errors.Is(err, context.Canceled):
		msg = "abgebrochen"
	default:
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			msg = "Hostname nicht auflösbar"
		}
	}
	return &Error{Kind: KindNetwork, Msg: fmt.Sprintf("%s: %s", what, msg), Err: err}
}

// Dial opens a TCP connection honouring ctx.
func Dial(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout, KeepAlive: 15 * time.Second}
	return d.DialContext(ctx, "tcp", HostPort(host, port))
}

// ---- RTSP authentication probe ----------------------------------------------------

// RTSPProbe performs an authenticated RTSP DESCRIBE. It verifies reachability
// and credentials without starting any media. Returns a KindAuth error when
// the device rejects the credentials.
func RTSPProbe(ctx context.Context, host string, port int, user, pass, pathQuery string) error {
	status, err := RTSPDescribe(ctx, host, port, user, pass, pathQuery)
	if err != nil {
		return err
	}
	switch status {
	case 200:
		return nil
	case 401, 403:
		return NewError(KindAuth, nil, "RTSP: Benutzername oder Passwort falsch")
	case 404:
		// credentials accepted, path unknown – the control login succeeded
		return nil
	}
	return NewError(KindProtocol, nil, "RTSP antwortet mit Status %d", status)
}

// RTSPDescribe sends an (authenticated) DESCRIBE and returns the final
// status code, e.g. to ask a recorder whether a replay range has material.
func RTSPDescribe(ctx context.Context, host string, port int, user, pass, pathQuery string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	conn, err := Dial(ctx, host, port, 5*time.Second)
	if err != nil {
		return 0, ClassifyNetErr(err, "RTSP")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	uri := "rtsp://" + HostPort(host, port) + pathQuery
	rd := bufio.NewReader(conn)
	send := func(cseq int, auth string) (int, map[string]string, error) {
		var b strings.Builder
		fmt.Fprintf(&b, "DESCRIBE %s RTSP/1.0\r\nCSeq: %d\r\nAccept: application/sdp\r\nUser-Agent: CNM\r\n", uri, cseq)
		if auth != "" {
			fmt.Fprintf(&b, "Authorization: %s\r\n", auth)
		}
		b.WriteString("\r\n")
		if _, err := io.WriteString(conn, b.String()); err != nil {
			return 0, nil, err
		}
		return readRTSPResponse(rd)
	}
	status, hdr, err := send(1, "")
	if err != nil {
		return 0, ClassifyNetErr(err, "RTSP")
	}
	if status != 401 {
		return status, nil
	}
	auth, err := rtspAuthorization(hdr["www-authenticate"], user, pass, "DESCRIBE", uri)
	if err != nil {
		return 0, NewError(KindProtocol, err, "RTSP-Authentifizierung nicht unterstützt")
	}
	status, _, err = send(2, auth)
	if err != nil {
		return 0, ClassifyNetErr(err, "RTSP")
	}
	return status, nil
}

func readRTSPResponse(rd *bufio.Reader) (int, map[string]string, error) {
	line, err := rd.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	parts := strings.Fields(line)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "RTSP/") {
		return 0, nil, NewError(KindProtocol, nil, "keine RTSP-Antwort")
	}
	status, _ := strconv.Atoi(parts[1])
	hdr := map[string]string{}
	length := 0
	for {
		l, err := rd.ReadString('\n')
		if err != nil {
			return 0, nil, err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if key == "www-authenticate" && hdr[key] != "" {
			// prefer Digest over Basic when several challenges are offered
			if strings.HasPrefix(strings.ToLower(hdr[key]), "digest") {
				continue
			}
		}
		hdr[key] = v
		if key == "content-length" {
			length, _ = strconv.Atoi(v)
		}
	}
	if length > 0 && length < 1<<20 {
		if _, err := io.CopyN(io.Discard, rd, int64(length)); err != nil {
			return 0, nil, err
		}
	}
	return status, hdr, nil
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

var authParamRe = regexp.MustCompile(`(\w+)=(?:"([^"]*)"|([^,\s]*))`)

// DigestAuthorization answers an HTTP/RTSP Basic or Digest challenge.
func DigestAuthorization(challenge, user, pass, method, uri string) (string, error) {
	return rtspAuthorization(challenge, user, pass, method, uri)
}

func rtspAuthorization(challenge, user, pass, method, uri string) (string, error) {
	lc := strings.ToLower(challenge)
	switch {
	case strings.HasPrefix(lc, "basic"):
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), nil
	case strings.HasPrefix(lc, "digest"):
		p := map[string]string{}
		for _, m := range authParamRe.FindAllStringSubmatch(challenge[6:], -1) {
			v := m[2]
			if v == "" {
				v = m[3]
			}
			p[strings.ToLower(m[1])] = v
		}
		realm, nonce := p["realm"], p["nonce"]
		ha1 := md5hex(user + ":" + realm + ":" + pass)
		ha2 := md5hex(method + ":" + uri)
		var resp string
		var extra string
		if strings.Contains(p["qop"], "auth") {
			cb := make([]byte, 8)
			_, _ = rand.Read(cb)
			cnonce := hex.EncodeToString(cb)
			nc := "00000001"
			resp = md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
			extra = fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, nc, cnonce)
		} else {
			resp = md5hex(ha1 + ":" + nonce + ":" + ha2)
		}
		h := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"%s`, user, realm, nonce, uri, resp, extra)
		if op := p["opaque"]; op != "" {
			h += fmt.Sprintf(`, opaque="%s"`, op)
		}
		return h, nil
	}
	return "", fmt.Errorf("unknown challenge")
}
