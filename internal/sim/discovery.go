package sim

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
)

// DiscoveryResponder answers Dahua DHIP (239.255.255.251:37810) and ONVIF
// WS-Discovery (239.255.255.250:3702) searches like real devices, so the
// network scan can be demonstrated without hardware.
type DiscoveryResponder struct {
	// DahuaHTTP is announced as the HTTP port of a Dahua NVR (0 = off).
	DahuaHTTP int
	// ONVIFPort is announced in the XAddrs of an ONVIF camera (0 = off).
	ONVIFPort int
	// AxisHTTP is announced as the HTTP port of an Axis camera answering
	// SSDP M-SEARCH on 239.255.255.250:1900 (0 = off).
	AxisHTTP int
	conns    []*net.UDPConn
}

// Start joins the groups on every interface.
func (r *DiscoveryResponder) Start() error {
	ifs, _ := net.Interfaces()
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		ifi := ifi
		if r.DahuaHTTP > 0 {
			if c, err := net.ListenMulticastUDP("udp4", &ifi, &net.UDPAddr{IP: net.IPv4(239, 255, 255, 251), Port: 37810}); err == nil {
				r.conns = append(r.conns, c)
				go r.serve(c, r.dhip)
			}
		}
		if r.ONVIFPort > 0 {
			if c, err := net.ListenMulticastUDP("udp4", &ifi, &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}); err == nil {
				r.conns = append(r.conns, c)
				go r.serve(c, r.onvif)
			}
		}
		if r.AxisHTTP > 0 {
			if c, err := net.ListenMulticastUDP("udp4", &ifi, &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}); err == nil {
				r.conns = append(r.conns, c)
				go r.serve(c, r.ssdp)
			}
		}
	}
	if len(r.conns) == 0 {
		return fmt.Errorf("no multicast interface")
	}
	return nil
}

// Close stops answering.
func (r *DiscoveryResponder) Close() {
	for _, c := range r.conns {
		c.Close()
	}
}

func (r *DiscoveryResponder) serve(c *net.UDPConn, answer func([]byte) []byte) {
	buf := make([]byte, 65536)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if a := answer(buf[:n]); a != nil {
			// reply unicast from a fresh socket (like the devices do)
			if s, err := net.DialUDP("udp4", nil, from); err == nil {
				s.Write(a)
				s.Close()
			}
		}
	}
}

func (r *DiscoveryResponder) dhip(req []byte) []byte {
	if len(req) < 32 || string(req[4:8]) != "DHIP" || !bytes.Contains(req, []byte("DHDiscover.search")) {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"mac": "3c:ef:8c:00:00:01", "method": "client.notifyDevInfo", "params": map[string]any{"deviceInfo": map[string]any{
		"DeviceClass": "NVR", "DeviceType": "NVR-SIM-8", "SerialNo": "SIMDAHUA0001", "Vendor": "Dahua", "Version": "4.001.SIM",
		"MachineName": "Dahua-Sim", "HttpPort": r.DahuaHTTP, "Port": 37777, "RemoteVideoInputChannels": 8,
	}}})
	h := make([]byte, 32)
	binary.LittleEndian.PutUint32(h, 0x20)
	copy(h[4:], "DHIP")
	binary.LittleEndian.PutUint32(h[16:], uint32(len(body)))
	binary.LittleEndian.PutUint32(h[24:], uint32(len(body)))
	return append(h, body...)
}

const axisST = "urn:axis-com:service:BasicService:1"

func (r *DiscoveryResponder) ssdp(req []byte) []byte {
	if !bytes.HasPrefix(req, []byte("M-SEARCH")) || !(bytes.Contains(req, []byte(axisST)) || bytes.Contains(req, []byte("ssdp:all"))) {
		return nil
	}
	return []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nEXT:\r\nLOCATION: http://0.0.0.0:%d/rootdesc1.xml\r\n"+
		"SERVER: Linux, UPnP/1.0, Portable SDK for UPnP devices/1.6.18\r\nST: %s\r\nUSN: uuid:Upnp-BasicDevice-1_0-ACCC8E000001::%s\r\n\r\n", r.AxisHTTP, axisST, axisST))
}

func (r *DiscoveryResponder) onvif(req []byte) []byte {
	if !bytes.Contains(req, []byte("Probe")) || bytes.Contains(req, []byte("ProbeMatch")) {
		return nil
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"><s:Body><d:ProbeMatches><d:ProbeMatch>`+
		`<d:Types>dn:NetworkVideoTransmitter</d:Types><d:Scopes>onvif://www.onvif.org/name/SIM-ONVIF onvif://www.onvif.org/hardware/IPC-SIM onvif://www.onvif.org/MAC/02:00:00:00:00:02</d:Scopes>`+
		`<d:XAddrs>http://0.0.0.0:%d/onvif/device_service</d:XAddrs><d:MetadataVersion>1</d:MetadataVersion></d:ProbeMatch></d:ProbeMatches></s:Body></s:Envelope>`, r.ONVIFPort))
}
