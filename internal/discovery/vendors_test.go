package discovery

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestParseSSDPAxis(t *testing.T) {
	reply := []byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nEXT:\r\nLOCATION: http://198.51.100.90:8080/rootdesc1.xml\r\n" +
		"SERVER: Linux/4.9, UPnP/1.0, Portable SDK for UPnP devices/1.6.18\r\nST: urn:axis-com:service:BasicService:1\r\n" +
		"USN: uuid:Upnp-BasicDevice-1_0-ACCC8E7A4B21::urn:axis-com:service:BasicService:1\r\n\r\n")
	d := parseSSDP(src, reply)
	if d == nil || d.MAC != "ac:cc:8e:7a:4b:21" || d.Serial != "ACCC8E7A4B21" || d.Ports["http"] != 8080 {
		t.Fatalf("%+v", d)
	}
	if s := d.Suggestion; s.Protocol != protoAxis || s.Ports["http"] != 8080 || len(s.Ports) != 1 {
		t.Fatalf("suggestion %+v", s)
	}
	if parseSSDP(src, ssdpProbe()) != nil {
		t.Fatal("own M-SEARCH must be ignored")
	}
	// other UPnP devices (routers, TVs) answer with other search targets
	if parseSSDP(src, []byte("HTTP/1.1 200 OK\r\nST: upnp:rootdevice\r\nUSN: uuid:1234::upnp:rootdevice\r\n\r\n")) != nil {
		t.Fatal("non-Axis UPnP device accepted")
	}
}

func onvifMatch(scopes string) []byte {
	return []byte(`<d:ProbeMatch><d:Scopes>` + scopes + `</d:Scopes><d:XAddrs>http://198.51.100.50/onvif/device_service</d:XAddrs></d:ProbeMatch>`)
}

// ONVIF scopes reveal a device's own protocol where CNM has one; all
// others are offered as ONVIF.
func TestONVIFProtocolHints(t *testing.T) {
	for _, c := range []struct{ scopes, proto string }{
		{"onvif://www.onvif.org/name/AXIS onvif://www.onvif.org/hardware/CAM1", protoAxis},
		{"onvif://www.onvif.org/name/UNIVIEW onvif://www.onvif.org/hardware/CAM2", protoUniview},
		{"onvif://www.onvif.org/name/Dahua onvif://www.onvif.org/type/NetworkVideoRecorder", protoDahua},
		{"onvif://www.onvif.org/name/VENDOR1 onvif://www.onvif.org/hardware/CAM3", protoONVIF},
		{"onvif://www.onvif.org/name/VENDOR2 onvif://www.onvif.org/type/NetworkVideoRecorder", protoONVIF},
		{"onvif://www.onvif.org/name/VENDOR3 onvif://www.onvif.org/hardware/NVR4", protoONVIF},
		{"onvif://www.onvif.org/name/IPCAM onvif://www.onvif.org/hardware/X1", protoONVIF},
	} {
		d := parseONVIF(src, onvifMatch(c.scopes))
		if d == nil || d.Suggestion.Protocol != c.proto {
			t.Fatalf("%s: %+v %+v", c.scopes, d, d.Suggestion)
		}
	}
}

// An Axis camera answers SSDP and ONVIF: the vendor protocol supplies MAC
// and serial, both are listed, the suggestion is the VAPIX adapter.
func TestScanAxisSSDPAndONVIF(t *testing.T) {
	ssdp := responder(t, func(req []byte) []byte {
		if !bytes.HasPrefix(req, []byte("M-SEARCH")) || !bytes.Contains(req, []byte(axisST)) {
			return nil
		}
		return []byte("HTTP/1.1 200 OK\r\nLOCATION: http://127.0.0.1:80/rootdesc1.xml\r\nST: " + axisST + "\r\nUSN: uuid:Upnp-BasicDevice-1_0-ACCC8E000001::" + axisST + "\r\n\r\n")
	})
	onvif := responder(t, func(req []byte) []byte {
		if !bytes.Contains(req, []byte("Probe")) {
			return nil
		}
		return onvifMatch("onvif://www.onvif.org/name/AXIS onvif://www.onvif.org/hardware/CAM5")
	})
	var sc Scanner
	res, err := sc.Scan(context.Background(), Options{Timeout: 1500 * time.Millisecond, Targets: &Targets{ONVIF: []string{onvif}, SSDP: []string{ssdp}}})
	if err != nil || len(res.Devices) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	d := res.Devices[0]
	if len(d.Protocols) != 2 || d.Serial != "ACCC8E000001" || d.Suggestion.Protocol != protoAxis {
		t.Fatalf("%+v %+v", d, d.Suggestion)
	}
}
