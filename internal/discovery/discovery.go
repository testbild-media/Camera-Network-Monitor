// Package discovery finds cameras and recorders in the local network on
// request (never in the background). It speaks the vendor search protocols
// – ONVIF WS-Discovery, NETSurveillance/XMEye (DVRIP) search, Dahua DHIP and
// legacy Dahua search, Hikvision SADP, Axis SSDP – and, for an explicitly given address
// range (e.g. a routed subnet behind a firewall), unicast probes plus a TCP
// port check of the typical service ports.
//
// Results carry a suggestion (protocol, non-default ports) the UI uses to
// prefill the device dialog; variant and channels are detected when the
// device connects. Nothing is ever configured automatically.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// Protocol names shown in the UI.
const (
	ProtoONVIF     = "ONVIF"
	ProtoDVRIP     = "DVRIP"
	ProtoDahua     = "Dahua"
	ProtoHikvision = "Hikvision SADP"
	ProtoAxis      = "Axis SSDP"
	ProtoTCP       = "TCP-Ports"
)

// Protocol ids used in suggestions (checked against the provider registry
// in the tests).
const (
	protoDahua     = "dahua"
	protoSofia     = "sofia"
	protoHikvision = "hikvision"
	protoAxis      = "axis"
	protoUniview   = "uniview"
	protoONVIF     = "onvif"
	protoRTSP      = "rtsp"
)

// Device is one discovered device.
type Device struct {
	IP           string         `json:"ip"`
	MAC          string         `json:"mac,omitempty"`
	Serial       string         `json:"serial,omitempty"`
	Name         string         `json:"name,omitempty"`
	Manufacturer string         `json:"-"` // used for the protocol choice only, never shown
	Model        string         `json:"-"`
	Firmware     string         `json:"firmware,omitempty"`
	Channels     int            `json:"channels,omitempty"`
	Protocols    []string       `json:"protocols"`
	Ports        map[string]int `json:"ports,omitempty"`
	Suggestion   *Suggestion    `json:"suggestion,omitempty"`
	Notes        []string       `json:"notes,omitempty"`
	Added        bool           `json:"added"`
	DeviceID     string         `json:"deviceId,omitempty"`
	DeviceLabel  string         `json:"deviceLabel,omitempty"`

	rank int // suggestion priority (higher wins)
}

// Suggestion prefills the device dialog: the protocol and the ports that
// differ from the protocol's defaults.
type Suggestion struct {
	Protocol string            `json:"protocol"`
	Ports    map[string]int    `json:"ports,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
	Label    string            `json:"label,omitempty"`
}

// Options of a scan.
type Options struct {
	// Range is an optional CIDR or a single address (e.g. a /24) that
	// is probed by unicast in addition to the local broadcast/multicast search.
	Range string
	// Timeout of the listening phase (default 4 s).
	Timeout time.Duration
	// Targets overrides where the search requests go (tests).
	Targets *Targets
}

// Targets are the destinations of the search requests.
type Targets struct {
	ONVIF, DVRIP, DahuaDHIP, DahuaLegacy, SADP, SSDP []string // host:port
	// Local search: open per-interface multicast sockets.
	Interfaces bool
	// TCP ports checked for unicast hosts.
	TCPPorts []int
	Hosts    []netip.Addr
}

// Result of a scan.
type Result struct {
	Devices    []*Device `json:"devices"`
	DurationMs int64     `json:"durationMs"`
	Warnings   []string  `json:"warnings,omitempty"`
}

// MaxRangeHosts limits the unicast range (a /22).
const MaxRangeHosts = 1024

// ParseRange expands a CIDR or single address into host addresses.
func ParseRange(s string) ([]netip.Addr, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("ungültige IPv4-Adresse %q", s)
		}
		return []netip.Addr{a}, nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return nil, fmt.Errorf("ungültiger IPv4-Adressbereich %q – erwartet wird Netzadresse/Präfix (CIDR), höchstens /22", s)
	}
	p = p.Masked()
	if p.Bits() < 22 {
		return nil, fmt.Errorf("Adressbereich zu groß – höchstens /22 (%d Adressen)", MaxRangeHosts)
	}
	var out []netip.Addr
	for a := p.Addr(); p.Contains(a); a = a.Next() {
		if p.Bits() <= 30 && (a == p.Addr() || !p.Contains(a.Next())) {
			continue // network and broadcast address
		}
		out = append(out, a)
	}
	return out, nil
}

var defaultTCPPorts = []int{80, 554, 8000, 34567, 37777}

func defaultTargets(hosts []netip.Addr) *Targets {
	t := &Targets{
		ONVIF:       []string{"239.255.255.250:3702"},
		DVRIP:       []string{"255.255.255.255:34569"},
		DahuaDHIP:   []string{"239.255.255.251:37810"},
		DahuaLegacy: []string{"255.255.255.255:5050"},
		SADP:        []string{"239.255.255.250:37020"},
		SSDP:        []string{"239.255.255.250:1900"},
		Interfaces:  true,
		TCPPorts:    defaultTCPPorts,
		Hosts:       hosts,
	}
	for _, b := range interfaceBroadcasts() {
		t.DVRIP = append(t.DVRIP, b+":34569")
		t.DahuaLegacy = append(t.DahuaLegacy, b+":5050")
	}
	for _, h := range hosts {
		s := h.String()
		t.ONVIF = append(t.ONVIF, s+":3702")
		t.DVRIP = append(t.DVRIP, s+":34569")
		t.DahuaDHIP = append(t.DahuaDHIP, s+":37810")
		t.DahuaLegacy = append(t.DahuaLegacy, s+":5050")
		t.SADP = append(t.SADP, s+":37020")
		t.SSDP = append(t.SSDP, s+":1900")
	}
	return t
}

// Scanner serialises scans (one at a time).
type Scanner struct{ mu sync.Mutex }

// ErrBusy is returned while another scan runs.
var ErrBusy = errors.New("Es läuft bereits eine Netzwerksuche")

// Scan runs all searches in parallel and merges the answers per IP.
func (s *Scanner) Scan(ctx context.Context, opt Options) (*Result, error) {
	if !s.mu.TryLock() {
		return nil, ErrBusy
	}
	defer s.mu.Unlock()
	hosts, err := ParseRange(opt.Range)
	if err != nil {
		return nil, err
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 4 * time.Second
	}
	t := opt.Targets
	if t == nil {
		t = defaultTargets(hosts)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout+2*time.Second)
	defer cancel()
	col := newCollector()
	var wg sync.WaitGroup
	run := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				col.warn(name + ": " + err.Error())
			}
		}()
	}
	run(ProtoONVIF, func() error {
		return udpSearch(ctx, t, opt.Timeout, onvifGroup, 0, t.ONVIF, onvifProbes(), col.add(parseONVIF))
	})
	run(ProtoDahua, func() error {
		return udpSearch(ctx, t, opt.Timeout, dahuaGroup, 0, t.DahuaDHIP, [][]byte{dhipProbe()}, col.add(parseDHIP))
	})
	run(ProtoHikvision, func() error {
		return udpSearch(ctx, t, opt.Timeout, sadpGroup, 37020, t.SADP, sadpProbes(), col.add(parseSADP))
	})
	run(ProtoDVRIP, func() error {
		return udpSearch(ctx, t, opt.Timeout, onvifGroup, 34569, t.DVRIP, [][]byte{dvripProbe()}, col.add(parseDVRIP))
	})
	run(ProtoAxis, func() error {
		return udpSearch(ctx, t, opt.Timeout, onvifGroup, 0, t.SSDP, [][]byte{ssdpProbe()}, col.add(parseSSDP))
	})
	run(ProtoDahua+" (alt)", func() error {
		return udpSearch(ctx, t, opt.Timeout, nil, 5050, t.DahuaLegacy, [][]byte{dahuaLegacyProbe()}, col.add(parseDahuaLegacy))
	})
	if len(t.Hosts) > 0 && len(t.TCPPorts) > 0 {
		run(ProtoTCP, func() error { tcpScan(ctx, t.Hosts, t.TCPPorts, col); return nil })
	}
	wg.Wait()
	res := &Result{Devices: col.devices(), DurationMs: time.Since(start).Milliseconds(), Warnings: col.warnings}
	return res, nil
}

// ---- collection / merging ------------------------------------------------------------------

type collector struct {
	mu       sync.Mutex
	byIP     map[string]*Device
	warnings []string
}

func newCollector() *collector { return &collector{byIP: map[string]*Device{}} }

func (c *collector) warn(w string) {
	c.mu.Lock()
	c.warnings = append(c.warnings, w)
	c.mu.Unlock()
}

// add returns a packet handler for a parser.
func (c *collector) add(parse func(src netip.Addr, b []byte) *Device) func(netip.Addr, []byte) {
	return func(src netip.Addr, b []byte) {
		d := parse(src, b)
		if d == nil || d.IP == "" {
			return
		}
		c.merge(d)
	}
}

func (c *collector) merge(d *Device) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ex := c.byIP[d.IP]
	if ex == nil {
		if d.Ports == nil {
			d.Ports = map[string]int{}
		}
		c.byIP[d.IP] = d
		return
	}
	for _, p := range d.Protocols {
		if !contains(ex.Protocols, p) {
			ex.Protocols = append(ex.Protocols, p)
		}
	}
	for k, v := range d.Ports {
		if _, ok := ex.Ports[k]; !ok {
			ex.Ports[k] = v
		}
	}
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	// vendor protocols know more than ONVIF scopes or open ports
	if d.rank > ex.rank {
		ex.Suggestion, ex.rank = d.Suggestion, d.rank
		if d.Manufacturer != "" {
			ex.Manufacturer = d.Manufacturer
		}
		if d.Model != "" {
			ex.Model = d.Model
		}
	}
	fill(&ex.Manufacturer, d.Manufacturer)
	fill(&ex.Model, d.Model)
	fill(&ex.MAC, d.MAC)
	fill(&ex.Serial, d.Serial)
	fill(&ex.Name, d.Name)
	fill(&ex.Firmware, d.Firmware)
	if ex.Channels == 0 {
		ex.Channels = d.Channels
	}
	for _, n := range d.Notes {
		if !contains(ex.Notes, n) {
			ex.Notes = append(ex.Notes, n)
		}
	}
}

func (c *collector) devices() []*Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Device, 0, len(c.byIP))
	for _, d := range c.byIP {
		// a pure port hit without any recognisable service is noise
		if len(d.Protocols) == 1 && d.Protocols[0] == ProtoTCP && d.Suggestion == nil {
			continue
		}
		sort.Strings(d.Protocols)
		if d.Suggestion != nil && d.Suggestion.Label == "" {
			d.Suggestion.Label = firstNonEmpty(d.Name, d.IP)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := netip.ParseAddr(out[i].IP)
		b, _ := netip.ParseAddr(out[j].IP)
		return a.Less(b)
	})
	return out
}

// MarkAdded flags devices whose address is already configured. hosts maps
// a configured host (IP or name) to the device id and label.
func MarkAdded(devs []*Device, hosts map[string][2]string) {
	resolved := map[string][2]string{}
	for h, v := range hosts {
		h = strings.Trim(strings.TrimSpace(h), "[]")
		if a, err := netip.ParseAddr(h); err == nil {
			resolved[a.String()] = v
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ips, err := net.DefaultResolver.LookupHost(ctx, h)
		cancel()
		if err == nil {
			for _, ip := range ips {
				resolved[ip] = v
			}
		}
	}
	for _, d := range devs {
		if v, ok := resolved[d.IP]; ok {
			d.Added, d.DeviceID, d.DeviceLabel = true, v[0], v[1]
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ---- TCP port check (unicast ranges only) -------------------------------------------------

func tcpScan(ctx context.Context, hosts []netip.Addr, ports []int, col *collector) {
	type job struct {
		h netip.Addr
		p int
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	open := map[netip.Addr][]int{}
	var mu sync.Mutex
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := net.Dialer{Timeout: 700 * time.Millisecond}
			for j := range jobs {
				c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(j.h, uint16(j.p)).String())
				if err != nil {
					continue
				}
				c.Close()
				mu.Lock()
				open[j.h] = append(open[j.h], j.p)
				mu.Unlock()
			}
		}()
	}
	for _, h := range hosts {
		for _, p := range ports {
			select {
			case jobs <- job{h, p}:
			case <-ctx.Done():
			}
		}
	}
	close(jobs)
	wg.Wait()
	for h, ps := range open {
		col.merge(guessFromPorts(h, ps))
	}
}

// guessFromPorts derives a (low priority) suggestion from open ports.
func guessFromPorts(h netip.Addr, ps []int) *Device {
	d := &Device{IP: h.String(), Protocols: []string{ProtoTCP}, Ports: map[string]int{}}
	has := map[int]bool{}
	for _, p := range ps {
		has[p] = true
	}
	for _, p := range []struct {
		port int
		key  string
	}{{80, "http"}, {554, "rtsp"}, {8000, "command"}, {34567, "dvrip"}, {37777, "dahua"}} {
		if has[p.port] {
			d.Ports[p.key] = p.port
		}
	}
	switch {
	case has[34567]:
		d.Suggestion = &Suggestion{Protocol: protoSofia}
		d.Notes = append(d.Notes, "Nur anhand offener Ports erkannt (DVRIP 34567)")
	case has[37777]:
		d.Suggestion = &Suggestion{Protocol: protoDahua}
		d.Notes = append(d.Notes, "Nur anhand offener Ports erkannt (Dahua 37777)")
	case has[8000] && has[80]:
		d.Suggestion = &Suggestion{Protocol: protoHikvision}
		d.Notes = append(d.Notes, "Nur anhand offener Ports erkannt (Hikvision 8000)")
	case has[554]:
		d.Suggestion = &Suggestion{Protocol: protoRTSP}
		d.Notes = append(d.Notes, "RTSP-Port offen – Stream-Pfad muss angegeben werden")
	}
	return d
}
