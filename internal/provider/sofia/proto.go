// Package sofia implements the NETSurveillance / Sofia / XMEye "DVRIP"
// protocol family (provider id "sofia").
//
// Live video is delivered either directly by go2rtc ("modern" single-socket
// firmware, dvrip:// source) or – for old two-socket firmware – through a CNM
// bridge that performs the verified legacy sequence:
//
//	login on control connection
//	open separate media connection
//	OPMonitor Claim on the media connection
//	OPMonitor Start on the control connection
//	media packets arrive on the media connection, keepalive on control
//
// Recording search and file based playback: see playback.go.
package sofia

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Message ids.
const (
	MsgLogin           = 1000
	MsgLoginRsp        = 1001
	MsgKeepAlive       = 1006
	MsgKeepAliveRsp    = 1007
	MsgSysInfo         = 1020
	MsgSysInfoRsp      = 1021
	MsgConfigGet       = 1042
	MsgConfigGetRsp    = 1043
	MsgChannelTitle    = 1048
	MsgChannelTitleRsp = 1049
	MsgMonitor         = 1410 // Start / Stop
	MsgMonitorRsp      = 1411
	MsgMonitorData     = 1412
	MsgMonitorClaim    = 1413
	MsgMonitorClaimRsp = 1414
)

const headerLen = 20

// maxPayload bounds a single protocol packet (protects against garbage).
const maxPayload = 8 << 20

// Header of a DVRIP packet (little endian).
type Header struct {
	Session uint32
	Seq     uint32
	Total   byte
	Current byte
	MsgID   uint16
	Length  uint32
}

func encodePacket(session, seq uint32, msgID uint16, payload []byte) []byte {
	b := make([]byte, headerLen, headerLen+len(payload))
	b[0] = 0xFF
	b[1] = 0x01
	binary.LittleEndian.PutUint32(b[4:], session)
	binary.LittleEndian.PutUint32(b[8:], seq)
	binary.LittleEndian.PutUint16(b[14:], msgID)
	binary.LittleEndian.PutUint32(b[16:], uint32(len(payload)))
	return append(b, payload...)
}

// readPacket reads one packet. Payload is freshly allocated.
func readPacket(r io.Reader) (Header, []byte, error) {
	var hb [headerLen]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return Header{}, nil, err
	}
	if hb[0] != 0xFF {
		return Header{}, nil, fmt.Errorf("dvrip: invalid packet header %x", hb[:4])
	}
	h := Header{
		Session: binary.LittleEndian.Uint32(hb[4:]),
		Seq:     binary.LittleEndian.Uint32(hb[8:]),
		Total:   hb[12],
		Current: hb[13],
		MsgID:   binary.LittleEndian.Uint16(hb[14:]),
		Length:  binary.LittleEndian.Uint32(hb[16:]),
	}
	if h.Length > maxPayload {
		return h, nil, fmt.Errorf("dvrip: packet too large (%d bytes)", h.Length)
	}
	payload := make([]byte, h.Length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return h, nil, err
	}
	return h, payload, nil
}

// jsonPayload terminates a JSON command the way devices expect.
func jsonPayload(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n', 0)
}

// Response is a decoded JSON answer.
type Response map[string]any

func decodeResponse(payload []byte) (Response, error) {
	p := bytes.TrimRight(payload, "\x00\n\r\t ")
	if i := bytes.IndexByte(p, 0); i >= 0 {
		p = p[:i]
	}
	var r Response
	if err := json.Unmarshal(p, &r); err != nil {
		return nil, fmt.Errorf("dvrip: invalid JSON response")
	}
	return r, nil
}

// Ret returns the result code of a response (-1 if absent).
func (r Response) Ret() int {
	switch v := r["Ret"].(type) {
	case float64:
		return int(v)
	case string:
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return -1
}

// OK reports success codes (100 = OK, 515 = OK/upgrade).
func (r Response) OK() bool { ret := r.Ret(); return ret == 100 || ret == 515 }

// ParseSession parses "0x0000001F".
func ParseSession(v any) (uint32, bool) {
	s, ok := v.(string)
	if !ok {
		if f, ok := v.(float64); ok {
			return uint32(f), true
		}
		return 0, false
	}
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	n, err := strconv.ParseUint(s, 16, 32)
	return uint32(n), err == nil
}

// SessionString formats a session id the way devices expect.
func SessionString(s uint32) string { return fmt.Sprintf("0x%08X", s) }

// authRetCodes are login results that indicate credential problems (never retried automatically).
var authRetCodes = map[int]string{
	106: "Benutzername oder Passwort falsch",
	107: "keine Berechtigung",
	113: "Benutzer existiert nicht",
	203: "Passwort falsch",
	204: "Benutzer unzulässig",
	205: "Benutzer gesperrt",
	206: "Benutzer auf Sperrliste",
}

// IsAuthRet reports whether a login result code is an authentication failure.
func IsAuthRet(ret int) (string, bool) {
	m, ok := authRetCodes[ret]
	return m, ok
}

// SofiaHash is the 8 character password hash used by DVRIP logins.
func SofiaHash(password string) string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	sum := md5.Sum([]byte(password))
	out := make([]byte, 0, 8)
	for i := 0; i < md5.Size; i += 2 {
		out = append(out, chars[(uint16(sum[i])+uint16(sum[i+1]))%62])
	}
	return string(out)
}

// StreamType maps a CNM stream kind onto the Sofia stream name.
func StreamType(main bool) string {
	if main {
		return "Main"
	}
	return "Extra1"
}

func monitorCmd(session uint32, action string, channel int, main bool) []byte {
	return jsonPayload(map[string]any{
		"Name":      "OPMonitor",
		"SessionID": SessionString(session),
		"OPMonitor": map[string]any{
			"Action": action,
			"Parameter": map[string]any{
				"Channel":    channel,
				"CombinMode": "NONE",
				"StreamType": StreamType(main),
				"TransMode":  "TCP",
			},
		},
	})
}

var errClosed = errors.New("dvrip: connection closed")
