package discovery

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Wire formats were taken from the specifications (WS-Discovery 2005/04,
// ONVIF Core) and cross-checked against open-source implementations
// (go2rtc, python-dvr, alexshpilkin/dvrip, python-dhip, mcw0/DahuaConsole,
// hikvision-tooling, hiktools).

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func normMAC(s string) string {
	s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, "-", ":")))
	if len(s) == 12 && !strings.Contains(s, ":") {
		var p []string
		for i := 0; i < 12; i += 2 {
			p = append(p, s[i:i+2])
		}
		s = strings.Join(p, ":")
	}
	return s
}

// ---- ONVIF WS-Discovery ------------------------------------------------------------------------

// onvifProbes are two probes: a probe matches only if all listed types
// match, and some cameras only announce one of them.
func onvifProbes() [][]byte {
	var out [][]byte
	for _, typ := range []string{"dn:NetworkVideoTransmitter", "tds:Device"} {
		out = append(out, []byte(`<?xml version="1.0" encoding="UTF-8"?>`+
			`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" `+
			`xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl" xmlns:tds="http://www.onvif.org/ver10/device/wsdl">`+
			`<s:Header><a:Action s:mustUnderstand="1">http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action>`+
			`<a:MessageID>urn:uuid:`+uuid()+`</a:MessageID>`+
			`<a:ReplyTo><a:Address>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:Address></a:ReplyTo>`+
			`<a:To s:mustUnderstand="1">urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To></s:Header>`+
			`<s:Body><d:Probe><d:Types>`+typ+`</d:Types></d:Probe></s:Body></s:Envelope>`))
	}
	return out
}

func xmlTag(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)<(?:[\w-]+:)?` + name + `\b[^>]*>(.*?)</(?:[\w-]+:)?` + name + `>`)
}

var (
	reXAddrs = xmlTag("XAddrs")
	reScopes = xmlTag("Scopes")
)

func tag(re *regexp.Regexp, b []byte) string {
	if m := re.FindSubmatch(b); m != nil {
		return strings.TrimSpace(html.UnescapeString(string(m[1])))
	}
	return ""
}

// protocolOf picks the own protocol of a device that announced itself via
// ONVIF ("" = ONVIF it is, for every manufacturer without a protocol of its own in CNM).
func protocolOf(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "hikvision"), strings.Contains(l, "hiwatch"), strings.Contains(l, "hilook"):
		return protoHikvision
	case strings.Contains(l, "dahua"), strings.Contains(l, "amcrest"), strings.Contains(l, "lupus"):
		return protoDahua
	case strings.Contains(l, "axis"):
		return protoAxis
	case strings.Contains(l, "uniview"), strings.HasPrefix(strings.TrimSpace(l), "unv"):
		return protoUniview
	}
	return ""
}

func parseONVIF(src netip.Addr, b []byte) *Device {
	if !bytes.Contains(b, []byte("ProbeMatch")) {
		return nil
	}
	d := &Device{IP: src.String(), Protocols: []string{ProtoONVIF}, Ports: map[string]int{}, rank: 1}
	// several XAddrs are possible: prefer the one with the sender's address
	var chosen *url.URL
	for _, x := range strings.Fields(tag(reXAddrs, b)) {
		u, err := url.Parse(x)
		if err != nil || u.Hostname() == "" {
			continue
		}
		if chosen == nil || u.Hostname() == src.String() {
			chosen = u
		}
	}
	port := 80
	if chosen != nil {
		if p, err := strconv.Atoi(chosen.Port()); err == nil {
			port = p
		} else if chosen.Scheme == "https" {
			port = 443
		}
	}
	d.Ports["onvif"] = port
	for _, sc := range strings.Fields(tag(reScopes, b)) {
		sc, _ = url.PathUnescape(sc)
		l := strings.ToLower(sc)
		val := func(prefix string) (string, bool) {
			i := strings.Index(l, prefix)
			if i < 0 {
				return "", false
			}
			return strings.TrimSpace(sc[i+len(prefix):]), true
		}
		if v, ok := val("onvif.org/name/"); ok {
			d.Name = v
		} else if v, ok := val("onvif.org/hardware/"); ok {
			d.Model = v
		} else if v, ok := val("onvif.org/mac/"); ok {
			d.MAC = normMAC(v)
		} else if v, ok := val("onvif.org/macaddress/"); ok {
			d.MAC = normMAC(v)
		} else if v, ok := val("onvif.org/serialnumber/"); ok {
			d.Serial = v
		}
	}
	switch proto := protocolOf(d.Name + " " + d.Model); proto {
	case protoHikvision, protoAxis:
		d.Suggestion = &Suggestion{Protocol: proto, Ports: nonDefault(map[string]int{"http": port}, 80)}
		d.rank = 2
	case protoDahua:
		d.Suggestion = &Suggestion{Protocol: proto, Ports: nonDefault(map[string]int{"http": port}, 80)}
		d.rank = 2
	case protoUniview:
		d.Suggestion = &Suggestion{Protocol: proto}
		d.rank = 2
	default:
		d.Suggestion = &Suggestion{Protocol: protoONVIF, Ports: nonDefault(map[string]int{"http": port}, 80)}
	}
	return d
}

// nonDefault keeps the ports that differ from def (nil when none).
func nonDefault(ports map[string]int, def int) map[string]int {
	out := map[string]int{}
	for k, v := range ports {
		if v > 0 && v != def {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---- NETSurveillance / XMEye (DVRIP) -----------------------------------------------------------

// dvripProbe is the 20 byte search header with message id 1530.
func dvripProbe() []byte {
	b := make([]byte, 20)
	b[0] = 0xff
	binary.LittleEndian.PutUint16(b[14:], 1530)
	return b
}

// hexIP decodes the little endian hex notation ("0x676433C6" = 198.51.100.103).
func hexIP(s string) (netip.Addr, bool) {
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x"), 16, 32)
	if err != nil || v == 0 {
		return netip.Addr{}, false
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	return netip.AddrFrom4(b), true
}

func parseDVRIP(src netip.Addr, b []byte) *Device {
	if len(b) < 20 || b[0] != 0xff || binary.LittleEndian.Uint16(b[14:]) != 1531 {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(b[16:]))
	if n <= 0 || 20+n > len(b) {
		n = len(b) - 20
	}
	payload := bytes.TrimRight(b[20:20+n], "\x00\n\r ")
	var msg map[string]json.RawMessage
	if json.Unmarshal(payload, &msg) != nil {
		return nil
	}
	var nc struct {
		HostIP, HostName, MAC, SN, Version string
		TCPPort, HttpPort, ChannelNum      int
	}
	if json.Unmarshal(msg["NetWork.NetCommon"], &nc) != nil {
		return nil
	}
	d := &Device{IP: src.String(), Protocols: []string{ProtoDVRIP}, Ports: map[string]int{}, rank: 3,
		Manufacturer: "XMEye / NETSurveillance", Name: nc.HostName, MAC: normMAC(nc.MAC), Serial: nc.SN, Firmware: nc.Version, Channels: nc.ChannelNum}
	if ip, ok := hexIP(nc.HostIP); ok && ip != src {
		d.Notes = append(d.Notes, fmt.Sprintf("Gerät meldet die Adresse %s", ip))
	}
	port := nc.TCPPort
	if port <= 0 {
		port = 34567
	}
	d.Ports["dvrip"] = port
	if nc.HttpPort > 0 {
		d.Ports["http"] = nc.HttpPort
	}
	if nc.ChannelNum > 64 {
		d.Notes = append(d.Notes, fmt.Sprintf("%d Kanäle gemeldet", nc.ChannelNum))
	}
	d.Suggestion = &Suggestion{Protocol: protoSofia, Ports: nonDefault(map[string]int{"native": port}, 34567)}
	return d
}

// ---- Dahua --------------------------------------------------------------------------------------

const dhipSearch = `{"method":"DHDiscover.search","params":{"mac":"","uni":1}}`

// dhipProbe is the DHIP search (32 byte header: size, "DHIP", session,
// request id, packet length, index, message length, data length).
func dhipProbe() []byte {
	b := make([]byte, 32, 32+len(dhipSearch))
	binary.LittleEndian.PutUint32(b[0:], 0x20)
	copy(b[4:], "DHIP")
	binary.LittleEndian.PutUint32(b[16:], uint32(len(dhipSearch)))
	binary.LittleEndian.PutUint32(b[24:], uint32(len(dhipSearch)))
	return append(b, dhipSearch...)
}

func dahuaSuggestion(http, native int) *Suggestion {
	ports := nonDefault(map[string]int{"http": http}, 80)
	if n := nonDefault(map[string]int{"native": native}, 37777); n != nil {
		if ports == nil {
			ports = map[string]int{}
		}
		ports["native"] = n["native"]
	}
	return &Suggestion{Protocol: protoDahua, Ports: ports}
}

func parseDHIP(src netip.Addr, b []byte) *Device {
	if len(b) < 32 || string(b[4:8]) != "DHIP" {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(b[24:]))
	if n <= 0 || 32+n > len(b) {
		n = len(b) - 32
	}
	var msg struct {
		Mac    string `json:"mac"`
		Params struct {
			DeviceInfo *struct {
				DeviceClass, DeviceType, SerialNo, Vendor, Version, MachineName, MAC string
				HttpPort, Port                                                       int
				VideoInputChannels, RemoteVideoInputChannels                         int
				IPv4Address                                                          struct{ IPAddress string }
			} `json:"deviceInfo"`
		} `json:"params"`
	}
	if json.Unmarshal(bytes.TrimRight(b[32:32+n], "\x00\n\r "), &msg) != nil || msg.Params.DeviceInfo == nil {
		return nil // e.g. our own search looped back
	}
	di := msg.Params.DeviceInfo
	d := &Device{IP: src.String(), Protocols: []string{ProtoDahua}, Ports: map[string]int{}, rank: 3,
		Manufacturer: firstNonEmpty(di.Vendor, "Dahua"), Model: firstNonEmpty(di.DeviceType, di.DeviceClass), Name: di.MachineName,
		Serial: di.SerialNo, Firmware: di.Version, MAC: normMAC(firstNonEmpty(msg.Mac, di.MAC)),
		Channels: di.VideoInputChannels + di.RemoteVideoInputChannels}
	if ip, err := netip.ParseAddr(di.IPv4Address.IPAddress); err == nil && ip != src {
		d.Notes = append(d.Notes, fmt.Sprintf("Gerät meldet die Adresse %s", ip))
	}
	if di.HttpPort > 0 {
		d.Ports["http"] = di.HttpPort
	}
	if di.Port > 0 {
		d.Ports["dahua"] = di.Port
	}
	d.Suggestion = dahuaSuggestion(di.HttpPort, di.Port)
	return d
}

// dahuaLegacyProbe is the binary search of old Dahua firmware (UDP 5050).
func dahuaLegacyProbe() []byte {
	b := make([]byte, 32)
	b[0], b[1] = 0xa3, 0x01
	b[16] = 0x02
	return b
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

func parseDahuaLegacy(src netip.Addr, b []byte) *Device {
	if len(b) < 137 || b[0] != 0xb3 {
		return nil
	}
	const base = 32
	web := int(binary.LittleEndian.Uint16(b[base+62:]))
	tcp := int(binary.LittleEndian.Uint16(b[base+66:]))
	mac := cstr(b[base+88 : base+88+17])
	model := ""
	if len(b) > base+88+17 {
		model = cstr(b[base+88+17:])
	}
	d := &Device{IP: src.String(), Protocols: []string{ProtoDahua}, Ports: map[string]int{}, rank: 2,
		Manufacturer: "Dahua-OEM", Name: cstr(b[base+8 : base+24]), MAC: normMAC(mac), Model: model}
	if ip := netip.AddrFrom4([4]byte(b[base+24 : base+28])); ip.IsValid() && ip != src && !ip.IsUnspecified() {
		d.Notes = append(d.Notes, fmt.Sprintf("Gerät meldet die Adresse %s", ip))
	}
	if web > 0 {
		d.Ports["http"] = web
	}
	if tcp > 0 {
		d.Ports["dahua"] = tcp
	}
	d.Suggestion = dahuaSuggestion(web, tcp)
	return d
}

// ---- Hikvision SADP ------------------------------------------------------------------------------

func sadpProbes() [][]byte {
	var out [][]byte
	for _, typ := range []string{"inquiry", "inquiry_v32"} {
		out = append(out, []byte(`<?xml version="1.0" encoding="utf-8"?><Probe><Uuid>`+strings.ToUpper(uuid())+`</Uuid><Types>`+typ+`</Types></Probe>`))
	}
	return out
}

var sadpFields = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for _, f := range []string{"DeviceType", "DeviceDescription", "DeviceSN", "IPv4Address", "CommandPort", "HttpPort", "MAC", "SoftwareVersion", "AnalogChannelNum", "DigitalChannelNum", "Activated"} {
		m[f] = regexp.MustCompile(`<` + f + `>([^<]*)</` + f + `>`)
	}
	return m
}()

func parseSADP(src netip.Addr, b []byte) *Device {
	i := bytes.Index(b, []byte("<ProbeMatch"))
	if i < 0 {
		return nil
	}
	b = b[i:]
	f := func(name string) string {
		if m := sadpFields[name].FindSubmatch(b); m != nil {
			return strings.TrimSpace(html.UnescapeString(string(m[1])))
		}
		return ""
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	d := &Device{IP: src.String(), Protocols: []string{ProtoHikvision}, Ports: map[string]int{}, rank: 3,
		Manufacturer: "Hikvision", Model: f("DeviceDescription"), Serial: f("DeviceSN"), MAC: normMAC(f("MAC")),
		Firmware: f("SoftwareVersion"), Channels: atoi(f("AnalogChannelNum")) + atoi(f("DigitalChannelNum"))}
	if ip, err := netip.ParseAddr(f("IPv4Address")); err == nil && ip != src {
		d.Notes = append(d.Notes, fmt.Sprintf("Gerät meldet die Adresse %s", ip))
	}
	http := atoi(f("HttpPort"))
	if http <= 0 {
		http = 80
	}
	d.Ports["http"] = http
	if p := atoi(f("CommandPort")); p > 0 {
		d.Ports["command"] = p
	}
	if strings.EqualFold(f("Activated"), "false") {
		d.Notes = append(d.Notes, "Gerät ist nicht aktiviert – zuerst ein Passwort am Gerät setzen")
	}
	d.Suggestion = &Suggestion{Protocol: protoHikvision, Ports: nonDefault(map[string]int{"http": http}, 80)}
	return d
}

// AllSuggestions returns one suggestion of every kind the parsers produce
// (used by the tests to check them against the provider registry).
func AllSuggestions() []*Suggestion {
	out := []*Suggestion{dahuaSuggestion(8080, 37778)}
	ip := netip.MustParseAddr("192.0.2.1")
	for _, ps := range [][]int{{34567}, {80, 37777}, {80, 8000}, {554}} {
		if d := guessFromPorts(ip, ps); d.Suggestion != nil {
			out = append(out, d.Suggestion)
		}
	}
	for _, xml := range []string{"name/HIKVISION", "name/Dahua", "name/Generic", "type/NetworkVideoRecorder", "name/AXIS", "name/UNIVIEW", "name/OTHER"} {
		d := parseONVIF(ip, []byte(`<d:ProbeMatch><d:Scopes>onvif://www.onvif.org/`+xml+`</d:Scopes><d:XAddrs>http://192.0.2.1:8080/onvif/device_service</d:XAddrs></d:ProbeMatch>`))
		out = append(out, d.Suggestion)
	}
	js := `{"NetWork.NetCommon":{"ChannelNum":8,"TCPPort":34568}}`
	b := dvripProbe()
	b[14], b[15] = 0xFB, 0x05
	binary.LittleEndian.PutUint32(b[16:], uint32(len(js)))
	out = append(out, parseDVRIP(ip, append(b, js...)).Suggestion)
	out = append(out, parseSADP(ip, []byte(`<ProbeMatch><DeviceDescription>X</DeviceDescription><HttpPort>81</HttpPort></ProbeMatch>`)).Suggestion)
	out = append(out, parseSSDP(ip, []byte("HTTP/1.1 200 OK\r\nST: "+axisST+"\r\nLOCATION: http://192.0.2.1:8081/x.xml\r\nUSN: uuid:Upnp-BasicDevice-1_0-ACCC8E000001::"+axisST+"\r\n\r\n")).Suggestion)
	return out
}

// ---- Axis (SSDP) ---------------------------------------------------------------------------------

// axisST is the search target Axis devices answer (also with ONVIF off).
const axisST = "urn:axis-com:service:BasicService:1"

func ssdpProbe() []byte {
	return []byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + axisST + "\r\n\r\n")
}

// reAxisUSN finds the MAC address in the USN (Axis: "uuid:Upnp-BasicDevice-1_0-<MAC>::urn:…").
var reAxisUSN = regexp.MustCompile(`(?i)-([0-9a-f]{12})(?:::|$)`)

func parseSSDP(src netip.Addr, b []byte) *Device {
	text := string(b)
	if !strings.HasPrefix(text, "HTTP/1.1 200") || !strings.Contains(text, axisST) {
		return nil
	}
	hdr := map[string]string{}
	for _, l := range strings.Split(text, "\r\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			hdr[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	port := 80
	if u, err := url.Parse(hdr["location"]); err == nil {
		if p, err := strconv.Atoi(u.Port()); err == nil {
			port = p
		}
	}
	d := &Device{IP: src.String(), Protocols: []string{ProtoAxis}, Ports: map[string]int{"http": port}, rank: 3, Manufacturer: "Axis"}
	if m := reAxisUSN.FindStringSubmatch(hdr["usn"]); m != nil {
		d.MAC = normMAC(m[1])
		d.Serial = strings.ToUpper(m[1]) // the Axis serial number is the MAC address
	}
	d.Suggestion = &Suggestion{Protocol: protoAxis, Ports: nonDefault(map[string]int{"http": port}, 80)}
	return d
}
