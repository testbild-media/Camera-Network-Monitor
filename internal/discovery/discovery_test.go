package discovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

var src = netip.MustParseAddr("198.51.100.123")

func TestParseONVIFProbeMatch(t *testing.T) {
	// real Hikvision reply (edgex-docs), trimmed; plus a second XAddr and URL encoding
	reply := []byte(`<?xml version="1.0"?><env:Envelope><env:Body><d:ProbeMatches><d:ProbeMatch>
 <wsadis:EndpointReference><wsadis:Address>urn:uuid:cea94000-fb96-11b3-8260-686dbc5cb15d</wsadis:Address></wsadis:EndpointReference>
 <d:Types>dn:NetworkVideoTransmitter tds:Device</d:Types>
 <d:Scopes>onvif://www.onvif.org/type/video_encoder onvif://www.onvif.org/Profile/Streaming onvif://www.onvif.org/MAC/68:6d:bc:5c:b1:5d onvif://www.onvif.org/hardware/CAM-X1 onvif://www.onvif.org/name/HIKVISION%20CAM http:123 </d:Scopes>
 <d:XAddrs>http://[2001:db8::1]/onvif/device_service http://198.51.100.123:8080/onvif/device_service</d:XAddrs>
 <d:MetadataVersion>10</d:MetadataVersion></d:ProbeMatch></d:ProbeMatches></env:Body></env:Envelope>`)
	d := parseONVIF(src, reply)
	if d == nil || d.Ports["onvif"] != 8080 || d.Model != "CAM-X1" || d.Name != "HIKVISION CAM" || d.MAC != "68:6d:bc:5c:b1:5d" {
		t.Fatalf("%+v", d)
	}
	if d.Suggestion.Protocol != protoHikvision || d.Suggestion.Ports["http"] != 8080 {
		t.Fatalf("suggestion %+v", d.Suggestion)
	}
	if parseONVIF(src, onvifProbes()[0]) != nil {
		t.Fatal("own probe must be ignored")
	}
}

func dvripReply(js string) []byte {
	b := make([]byte, 20)
	b[0], b[1] = 0xff, 0x01
	binary.LittleEndian.PutUint16(b[14:], 1531)
	binary.LittleEndian.PutUint32(b[16:], uint32(len(js)+2))
	return append(append(b, js...), '\n', 0)
}

const xmReply = `{"NetWork.NetCommon":{"BuildDate":"2020-11-24 16:47:14","ChannelNum":8,"DeviceType":24,"GateWay":"0x016433C6","HostIP":"0x7B6433C6","HostName":"DVR-Front","HttpPort":80,"MAC":"00:12:31:aa:bb:cc","MonMode":"TCP","SN":"abc123","TCPPort":20051,"UDPPort":34568,"Version":"V4.02.R11"},"Ret":100,"SessionID":"0x00000000"}`

func TestParseDVRIPSearchReply(t *testing.T) {
	if ip, ok := hexIP("0x676433C6"); !ok || ip.String() != "198.51.100.103" {
		t.Fatalf("hex ip %v", ip)
	}
	d := parseDVRIP(src, dvripReply(xmReply))
	if d == nil || d.Name != "DVR-Front" || d.Channels != 8 || d.Ports["dvrip"] != 20051 || len(d.Notes) != 0 {
		t.Fatalf("%+v", d)
	}
	if s := d.Suggestion; s.Protocol != protoSofia || s.Ports["native"] != 20051 {
		t.Fatalf("suggestion %+v", s)
	}
	if parseDVRIP(src, dvripProbe()) != nil {
		t.Fatal("own (empty) request must be ignored")
	}
}

func dhipReply(js string) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b, 0x20)
	copy(b[4:], "DHIP")
	binary.LittleEndian.PutUint32(b[16:], uint32(len(js)))
	binary.LittleEndian.PutUint32(b[24:], uint32(len(js)))
	return append(b, js...)
}

func TestParseDahuaReplies(t *testing.T) {
	js := `{"mac":"3c:ef:8c:01:02:03","method":"client.notifyDevInfo","params":{"deviceInfo":{"DeviceClass":"NVR","DeviceType":"NVR-8CH","SerialNo":"5J0ABC","Vendor":"Dahua","Version":"4.001","HttpPort":80,"Port":37777,"IPv4Address":{"IPAddress":"198.51.100.123"},"VideoInputChannels":0,"RemoteVideoInputChannels":8}}}`
	d := parseDHIP(src, dhipReply(js))
	// protocol only; default ports are not repeated, channels are detected
	if d == nil || d.Channels != 8 || d.MAC != "3c:ef:8c:01:02:03" || d.Suggestion.Protocol != protoDahua || d.Suggestion.Ports != nil || d.Suggestion.Options != nil {
		t.Fatalf("%+v %+v", d, d.Suggestion)
	}
	other := strings.Replace(js, `"HttpPort":80`, `"HttpPort":8080`, 1)
	if d := parseDHIP(src, dhipReply(other)); d.Suggestion.Protocol != protoDahua || d.Suggestion.Ports["http"] != 8080 {
		t.Fatalf("non-default port: %+v", d.Suggestion)
	}
	if parseDHIP(src, dhipProbe()) != nil {
		t.Fatal("own search must be ignored")
	}
	// legacy binary reply (0xb3): hostname, ports, MAC and model text
	b := make([]byte, 32+120)
	b[0] = 0xb3
	copy(b[32+8:], "DVR-Back")
	copy(b[32+24:], []byte{198, 51, 100, 123})
	binary.LittleEndian.PutUint16(b[32+62:], 80)
	binary.LittleEndian.PutUint16(b[32+66:], 20051)
	copy(b[32+88:], "00:11:22:33:44:55DVR-4CH")
	l := parseDahuaLegacy(src, b)
	if l == nil || l.Name != "DVR-Back" || l.Suggestion.Protocol != protoDahua || l.Suggestion.Ports["native"] != 20051 {
		t.Fatalf("legacy %+v", l)
	}
}

func TestParseSADP(t *testing.T) {
	reply := []byte("\x00\x01junk<?xml version=\"1.0\" encoding=\"UTF-8\"?><ProbeMatch><Uuid>X</Uuid><Types>inquiry</Types><DeviceType>139</DeviceType><DeviceDescription>NVR-8CH</DeviceDescription><DeviceSN>NVR8CH0820190101CCRR</DeviceSN><CommandPort>8000</CommandPort><HttpPort>80</HttpPort><MAC>44-19-b6-01-02-03</MAC><IPv4Address>198.51.100.123</IPv4Address><SoftwareVersion>V4.30.085</SoftwareVersion><AnalogChannelNum>0</AnalogChannelNum><DigitalChannelNum>8</DigitalChannelNum><Activated>true</Activated></ProbeMatch>\x00")
	d := parseSADP(src, reply)
	if d == nil || d.MAC != "44:19:b6:01:02:03" || d.Channels != 8 || d.Ports["command"] != 8000 {
		t.Fatalf("%+v", d)
	}
	if s := d.Suggestion; s.Protocol != protoHikvision || s.Ports != nil {
		t.Fatalf("suggestion %+v", s)
	}
	if parseSADP(src, sadpProbes()[0]) != nil {
		t.Fatal("own probe must be ignored")
	}
}

func TestParseRange(t *testing.T) {
	h, err := ParseRange("192.0.2.0/24")
	if err != nil || len(h) != 254 || h[0].String() != "192.0.2.1" {
		t.Fatalf("%d %v", len(h), err)
	}
	if _, err := ParseRange("192.0.2.0/16"); err == nil {
		t.Fatal("too large range accepted")
	}
	if h, _ := ParseRange("203.0.113.3"); len(h) != 1 {
		t.Fatal("single address")
	}
}

// responder answers search requests like a device on 127.0.0.1.
func responder(t *testing.T, answer func(req []byte) []byte) string {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if r := answer(buf[:n]); r != nil {
				c.WriteToUDP(r, from)
			}
		}
	}()
	return c.LocalAddr().String()
}

func TestScanMergesProtocolsPerAddress(t *testing.T) {
	dvrip := responder(t, func(req []byte) []byte {
		if bytes.Equal(req, dvripProbe()) {
			return dvripReply(xmReply)
		}
		return nil
	})
	onvif := responder(t, func(req []byte) []byte {
		if !bytes.Contains(req, []byte("Probe")) {
			return nil
		}
		return []byte(`<d:ProbeMatch><d:Scopes>onvif://www.onvif.org/name/XM onvif://www.onvif.org/hardware/AHB7008</d:Scopes><d:XAddrs>http://127.0.0.1:8899/onvif/device_service</d:XAddrs></d:ProbeMatch>`)
	})
	var sc Scanner
	res, err := sc.Scan(context.Background(), Options{Timeout: 1500 * time.Millisecond, Targets: &Targets{ONVIF: []string{onvif}, DVRIP: []string{dvrip}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Devices) != 1 {
		t.Fatalf("devices: %+v", res.Devices)
	}
	d := res.Devices[0]
	if d.IP != "127.0.0.1" || len(d.Protocols) != 2 || d.Suggestion.Protocol != protoSofia || d.Ports["onvif"] != 8899 || d.Channels != 8 {
		t.Fatalf("merge: %+v %+v", d, d.Suggestion)
	}
	MarkAdded(res.Devices, map[string][2]string{"127.0.0.1": {"dev1", "Front"}})
	if !d.Added || d.DeviceLabel != "Front" {
		t.Fatal("configured device not marked")
	}
}

func TestBusyScanner(t *testing.T) {
	var sc Scanner
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if _, err := sc.Scan(context.Background(), Options{Targets: &Targets{}}); err != ErrBusy {
		t.Fatalf("want busy, got %v", err)
	}
}

// TestLiveScan searches the real network (manual: CNM_LIVE_SCAN=1 [CNM_LIVE_RANGE=cidr]).
func TestLiveScan(t *testing.T) {
	if os.Getenv("CNM_LIVE_SCAN") == "" {
		t.Skip("set CNM_LIVE_SCAN=1")
	}
	var sc Scanner
	res, err := sc.Scan(context.Background(), Options{Range: os.Getenv("CNM_LIVE_RANGE")})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("%s", b)
}
