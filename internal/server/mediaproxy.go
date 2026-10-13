package server

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cnm/internal/media"
)

// handleMedia proxies the go2rtc WebSocket API for one authorised stream.
//
// Only "/media/api/ws?src=<stream>" is served. The stream name must be one
// of the CNM streams the caller may open (kiosk: only its monitor's cameras).
// The upstream request is rebuilt from scratch (single src parameter, no
// cookies, no Origin) – go2rtc would otherwise happily create streams from
// arbitrary source URLs (including exec:) passed as src. Every other go2rtc
// endpoint (stream list with credentials, config, restart, …) is unreachable.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if strings.TrimPrefix(r.URL.Path, "/media/") != "api/ws" {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "Unbekannter Medienpfad")
		return
	}
	q := r.URL.Query()
	srcs := q["src"]
	if len(srcs) != 1 || !strings.HasPrefix(srcs[0], media.Prefix) {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "Stream nicht erlaubt")
		return
	}
	name := srcs[0]
	cfg := s.Store.Get()
	if !cfg.System.Media.Enabled {
		writeErr(w, http.StatusServiceUnavailable, "MEDIA_DISABLED", "Media-Layer ist deaktiviert")
		return
	}
	if !media.AllowedStreams(cfg, p.ScopeMonitor())[name] {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "Stream nicht erlaubt")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Key") == "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "WebSocket erwartet")
		return
	}
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "Ungültige Herkunft")
		return
	}
	allowWebRTC := cfg.System.Media.AllowWebRTC && p.ScopeMonitor() == ""

	base, err := url.Parse(cfg.System.Media.Go2rtcURL)
	if err != nil || base.Host == "" {
		writeErr(w, http.StatusBadGateway, "MEDIA_OFFLINE", "go2rtc nicht konfiguriert")
		return
	}
	addr := base.Host
	if base.Port() == "" {
		addr = net.JoinHostPort(base.Hostname(), "80")
	}
	up, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "MEDIA_OFFLINE", "go2rtc nicht erreichbar")
		return
	}
	path := strings.TrimRight(base.Path, "/") + "/api/ws?" + url.Values{"src": {name}}.Encode()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nUser-Agent: CNM-media-proxy\r\n\r\n",
		path, base.Host, sanitizeHeader(r.Header.Get("Sec-WebSocket-Key")))
	_ = up.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(up, req); err != nil {
		up.Close()
		writeErr(w, http.StatusBadGateway, "MEDIA_OFFLINE", "go2rtc nicht erreichbar")
		return
	}
	upr := bufio.NewReaderSize(up, 64<<10)
	resp, err := http.ReadResponse(upr, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		up.Close()
		writeErr(w, http.StatusBadGateway, "MEDIA_ERROR", "go2rtc hat den Stream abgelehnt")
		return
	}
	_ = up.SetDeadline(time.Time{})
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "Hijacking nicht unterstützt")
		return
	}
	client, cbuf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	accept := resp.Header.Get("Sec-WebSocket-Accept")
	_, err = fmt.Fprintf(cbuf, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", sanitizeHeader(accept))
	if err == nil {
		err = cbuf.Flush()
	}
	if err != nil {
		client.Close()
		up.Close()
		return
	}

	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			client.Close()
			up.Close()
		})
	}
	removeSess := s.Conns.Add(p.ConnKey(), closeBoth)
	removeAll := s.all.Add("media", closeBoth)
	removeStream := s.streams.Add(streamKey(p.ScopeMonitor(), name), closeBoth)
	defer removeSess()
	defer removeAll()
	defer removeStream()
	// the camera may have been disabled while the session was being set up
	if !s.streamAllowed(p.ScopeMonitor(), name) {
		closeBoth()
		return
	}

	done := make(chan struct{}, 2)
	// go2rtc → browser: raw copy (includes bytes buffered during the handshake)
	go func() {
		_, _ = io.Copy(client, upr)
		closeBoth()
		done <- struct{}{}
	}()
	// browser → go2rtc: frame aware copy (enforces the transport policy)
	go func() {
		err := pipeClientFrames(up, cbuf.Reader, allowWebRTC)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			s.Log.Debug("media proxy closed", "stream", name, "reason", err.Error())
		}
		closeBoth()
		done <- struct{}{}
	}()
	<-done
	<-done
}

func sanitizeHeader(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, v)
}

// errPolicy is returned when the browser sends a forbidden message.
var errPolicy = errors.New("media policy violation")

// pipeClientFrames forwards masked client frames unchanged while inspecting
// text messages. go2rtc's client protocol only uses small JSON messages, so
// the limits are generous but bounded.
func pipeClientFrames(dst io.Writer, src *bufio.Reader, allowWebRTC bool) error {
	const maxFrame = 1 << 20
	var msg []byte
	inText := false
	hdr := make([]byte, 14)
	for {
		if _, err := io.ReadFull(src, hdr[:2]); err != nil {
			return err
		}
		fin := hdr[0]&0x80 != 0
		opcode := hdr[0] & 0x0F
		masked := hdr[1]&0x80 != 0
		n := uint64(hdr[1] & 0x7F)
		hlen := 2
		switch n {
		case 126:
			if _, err := io.ReadFull(src, hdr[2:4]); err != nil {
				return err
			}
			n = uint64(binary.BigEndian.Uint16(hdr[2:4]))
			hlen = 4
		case 127:
			if _, err := io.ReadFull(src, hdr[2:10]); err != nil {
				return err
			}
			n = binary.BigEndian.Uint64(hdr[2:10])
			hlen = 10
		}
		if !masked || n > maxFrame {
			return errPolicy
		}
		if _, err := io.ReadFull(src, hdr[hlen:hlen+4]); err != nil {
			return err
		}
		mask := hdr[hlen : hlen+4]
		hlen += 4
		payload := make([]byte, n)
		if _, err := io.ReadFull(src, payload); err != nil {
			return err
		}
		switch opcode {
		case 0x1: // text
			inText = true
			msg = msg[:0]
			fallthrough
		case 0x0: // continuation
			if inText {
				for i := range payload {
					msg = append(msg, payload[i]^mask[i%4])
				}
				if len(msg) > maxFrame {
					return errPolicy
				}
				if fin {
					inText = false
					if !allowedClientMessage(msg, allowWebRTC) {
						return errPolicy
					}
				}
			}
		case 0x2: // binary: go2rtc clients never send binary
			return errPolicy
		}
		if _, err := dst.Write(hdr[:hlen]); err != nil {
			return err
		}
		if _, err := dst.Write(payload); err != nil {
			return err
		}
	}
}

func allowedClientMessage(msg []byte, allowWebRTC bool) bool {
	var m struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return false
	}
	switch {
	case m.Type == "mse", m.Type == "mjpeg", m.Type == "mp4":
		return true
	case strings.HasPrefix(m.Type, "webrtc"):
		return allowWebRTC
	}
	return false
}
