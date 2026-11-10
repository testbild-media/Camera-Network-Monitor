// Package reolink implements cameras and recorders with the Reolink HTTP API
// (JSON commands on /cgi-bin/api.cgi with a login token) and RTSP:
//
//	login / logout   Login → Token{name, leaseTime}, Logout
//	device           GetDevInfo, GetChannelstatus (recorders), GetAbility
//	ports            GetNetPort; SetNetPort switches RTSP/RTMP on when off
//	live             GetRtspUrl per stream channel, else
//	                 rtsp://host:554/<h264|h265>Preview_<NN>_<main|sub>
//	clock, storage   GetTime, GetHddInfo (SD card or disk)
//	recordings       Search (per day), playback over HTTP-FLV
//	                 /flv?port=1935&app=bcs&stream=playback.bcs&channel=…&start=<file>&seek=<s>
//	PTZ              PtzCtrl, GetPtzPreset, Get/SetPtzGuard, Get/SetAiCfg
//
// Dual-lens cameras (wide angle and tele on one motorised head) report
// "supportAutoTrackStream"; CNM offers both lenses as camera channels 0
// (wide angle) and 1 (tele), steered together.
//
// Implemented from public descriptions of the API (no official
// documentation) and tested against the simulator only – the capabilities
// are marked experimental.
package reolink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnm/internal/provider"
)

// ProviderID and protocol id stored in the configuration.
const (
	ProviderID   = "reolink"
	ProtoReolink = "reolink"
)

var caps = provider.Capabilities{
	ChannelList: provider.Experimental,
	LiveMain:    provider.Experimental, LiveSub: provider.Experimental,
	RecordingSearch: provider.Experimental, SearchKind: "list",
	Playback:    provider.Experimental,
	DeviceTime:  provider.Experimental,
	StorageInfo: provider.Experimental,
	PTZ:         provider.Experimental,
}

const auth = "HTTP-API mit Token (HTTPS oder HTTP) · RTSP Digest"

type prov struct{}

func init() { provider.Register(prov{}) }

func (prov) Info() provider.Info {
	return provider.Info{
		ID:    ProviderID,
		Name:  "Reolink",
		Order: 40,
		Models: []provider.Model{{
			ID: ProtoReolink, Name: "Reolink", AutoDetect: true,
			Ports: []provider.PortSpec{
				{Key: "http", Label: "HTTPS/HTTP-Port (API)", Default: 443},
				{Key: "rtsp", Label: "RTSP", Default: 554},
			},
			Options:      []provider.OptionSpec{provider.PTZReturnOption(), provider.ChannelOverride()},
			Capabilities: caps, Auth: auth,
			Notes: "CNM erkennt HTTPS oder HTTP, Kanäle und Objektive selbst und schaltet RTSP an der Kamera ein, falls es aus ist. Aufnahmen von der SD-Karte. PTZ mit Übersteuerung: Während ein Benutzer steuert, pausiert CNM das Verfolgen und die Rückkehr zur festen Position. Noch nicht an Hardware getestet.",
		}},
	}
}

func (prov) NewDriver(cfg provider.DeviceConfig, env provider.Env) (provider.Driver, error) {
	if cfg.Host == "" {
		return nil, provider.NewError(provider.KindConfig, nil, "Host fehlt")
	}
	d := &driver{cfg: cfg, env: env, api: newClient(cfg.Host, cfg.Username, cfg.Password, env)}
	return d, nil
}

// streamPaths are the RTSP paths of one stream channel.
type streamPaths struct{ main, sub string }

type driver struct {
	cfg   provider.DeviceConfig
	env   provider.Env
	api   *client
	clock provider.Clock

	mu sync.RWMutex
	// layout (set at Connect)
	nvr       bool
	dualLens  bool
	channels  int
	rtspPort  int
	paths     map[int]streamPaths
	abilities ability
	search    map[int]int // per channel: 1 = tele via iLogicChannel, 2 = tele as channel 1
}

// ---- transport detection ---------------------------------------------------------------

// transports returns the HTTPS/HTTP candidates to try: the stored variant
// first; for the default port also plain HTTP on 80.
func (d *driver) transports() [][2]string {
	port := d.cfg.Port("http")
	var out [][2]string
	if v := d.cfg.Variant; strings.Contains(v, ":") {
		parts := strings.SplitN(v, ":", 2)
		if parts[0] == "https" || parts[0] == "http" {
			out = append(out, [2]string{parts[0], parts[1]})
		}
	}
	add := func(s string, p int) {
		c := [2]string{s, strconv.Itoa(p)}
		for _, x := range out {
			if x == c {
				return
			}
		}
		out = append(out, c)
	}
	if port == 80 {
		add("http", 80)
		add("https", 443)
	} else {
		add("https", port)
		if port == 443 {
			add("http", 80)
		} else {
			add("http", port)
		}
	}
	return out
}

// connectTransport logs in over the first transport that answers. A
// credential error stops at once (at most one rejected login).
func (d *driver) connectTransport(ctx context.Context) error {
	var first error
	for _, t := range d.transports() {
		port, _ := strconv.Atoi(t[1])
		d.api.setTransport(t[0], port)
		tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err := d.api.login(tctx)
		cancel()
		if err == nil {
			return nil
		}
		if k := provider.KindOf(err); k == provider.KindAuth || ctx.Err() != nil {
			return err
		}
		if first == nil {
			first = err
		}
	}
	return first
}

// ---- abilities ---------------------------------------------------------------------------------

// ability is the GetAbility answer: host level abilities and one map per
// channel ("abilityChn"), each value {"permit":…, "ver":…}.
type ability struct {
	host map[string]json.RawMessage
	chn  []map[string]json.RawMessage
}

func verOf(raw json.RawMessage) int {
	var v struct {
		Ver int `json:"ver"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	return v.Ver
}

// ver returns the ability version of a channel (dual-lens cameras report
// one channel for both lenses).
func (a ability) ver(channel int, key string) int {
	if len(a.chn) == 0 {
		return 0
	}
	if channel >= len(a.chn) {
		channel = 0
	}
	return verOf(a.chn[channel][key])
}

func (a ability) hostVer(key string) int { return verOf(a.host[key]) }

// ---- connect -----------------------------------------------------------------------------------

type devInfo struct {
	Model      string `json:"model"`
	Name       string `json:"name"`
	Serial     string `json:"serial"`
	FirmVer    string `json:"firmVer"`
	HardVer    string `json:"hardVer"`
	ChannelNum int    `json:"channelNum"`
	Type       string `json:"type"`
	ExactType  string `json:"exactType"`
}

func (d *driver) Connect(ctx context.Context) (*provider.DeviceInfo, error) {
	if err := d.connectTransport(ctx); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := d.api.do(cctx,
		cmd{Cmd: "GetDevInfo", Param: map[string]any{}},
		cmd{Cmd: "GetAbility", Param: map[string]any{"User": map[string]any{"userName": d.cfg.Username}}},
		cmd{Cmd: "GetChannelstatus", Param: map[string]any{}},
		cmd{Cmd: "GetNetPort", Param: map[string]any{}},
	)
	if err != nil {
		return nil, err
	}
	var di devInfo
	var ab struct {
		Ability map[string]json.RawMessage `json:"Ability"`
	}
	var chs struct {
		Count  int `json:"count"`
		Status []struct {
			Channel int    `json:"channel"`
			Name    string `json:"name"`
			Online  int    `json:"online"`
		} `json:"status"`
	}
	var np struct {
		NetPort map[string]any `json:"NetPort"`
	}
	for _, r := range res {
		if r.Code != 0 {
			continue
		}
		switch r.Cmd {
		case "GetDevInfo":
			var v struct {
				DevInfo devInfo `json:"DevInfo"`
			}
			if json.Unmarshal(r.Value, &v) == nil {
				di = v.DevInfo
			}
		case "GetAbility":
			_ = json.Unmarshal(r.Value, &ab)
		case "GetChannelstatus":
			_ = json.Unmarshal(r.Value, &chs)
		case "GetNetPort":
			_ = json.Unmarshal(r.Value, &np)
		}
	}
	if di.Model == "" && di.Serial == "" && di.ChannelNum == 0 {
		if err := res[0].err(); err != nil {
			return nil, err
		}
		return nil, provider.NewError(provider.KindProtocol, nil, "Kamera-API: keine Geräteinformation")
	}
	a := ability{host: ab.Ability}
	if raw, ok := ab.Ability["abilityChn"]; ok {
		_ = json.Unmarshal(raw, &a.chn)
	}
	nvr := isNVR(di.Type) || isNVR(di.ExactType)
	channels := max(di.ChannelNum, 1)
	if nvr && chs.Count > 0 {
		channels = chs.Count
	}
	dual := !nvr && a.ver(0, "supportAutoTrackStream") > 0
	if dual {
		channels = 2
	}
	if n, err := strconv.Atoi(strings.TrimSpace(d.cfg.Options[provider.OptChannels])); err == nil && n > 0 {
		channels = n
	}

	// RTSP (live) and RTMP (behind HTTP-FLV playback) must be switched on
	notes := []string{}
	rtspPort := d.cfg.Port("rtsp")
	if np.NetPort != nil {
		if p := numOf(np.NetPort["rtspPort"]); p > 0 && d.cfg.Ports["rtsp"] == 0 {
			rtspPort = p
		}
		if switched := d.enablePorts(cctx, np.NetPort); switched != "" {
			notes = append(notes, switched)
		}
	}

	info := &provider.DeviceInfo{Model: di.Model, Serial: di.Serial, Firmware: strings.TrimSpace(di.FirmVer + " " + di.HardVer),
		Channels: channels, ChannelNames: map[int]string{}}
	switch {
	case nvr:
		for _, s := range chs.Status {
			if s.Name != "" {
				info.ChannelNames[s.Channel] = s.Name
			}
		}
	case dual:
		info.ChannelNames[0] = strings.TrimSpace(di.Name + " Weitwinkel")
		info.ChannelNames[1] = strings.TrimSpace(di.Name + " Tele")
	case di.Name != "":
		info.ChannelNames[0] = di.Name
	}
	// PTZ heads: dual-lens cameras steer both lenses with channel 0
	for ch := 0; ch < channels; ch++ {
		head := ch
		if dual {
			head = 0
		}
		if ptzType(a, head) != 0 {
			if info.PTZ == nil {
				info.PTZ = map[int]int{}
			}
			info.PTZ[ch] = head
		}
	}

	d.api.mu.Lock()
	scheme, port := d.api.scheme, d.api.port
	d.api.mu.Unlock()
	info.Variant = fmt.Sprintf("%s:%d", scheme, port)
	kind := "Kamera"
	switch {
	case nvr:
		kind = fmt.Sprintf("Rekorder mit %d Kanälen", channels)
	case dual:
		kind = "Kamera mit zwei Objektiven (Weitwinkel und Tele)"
	}
	info.Summary = fmt.Sprintf("%s · API über %s %d · Live über RTSP %d", kind, strings.ToUpper(scheme), port, rtspPort)
	if len(info.PTZ) > 0 {
		info.Summary += " · PTZ"
	}
	if len(notes) > 0 {
		info.Summary += " · " + strings.Join(notes, ", ")
	}

	d.mu.Lock()
	d.nvr, d.dualLens, d.channels, d.rtspPort, d.abilities = nvr, dual, channels, rtspPort, a
	d.paths = map[int]streamPaths{}
	d.search = map[int]int{}
	d.mu.Unlock()
	d.readStreams(cctx)
	d.syncTime(cctx)
	return info, nil
}

func isNVR(t string) bool {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "NVR", "WIFI_NVR", "HOMEHUB":
		return true
	}
	return false
}

// ptzType is the PTZ ability (0 = none; 1 zoom only, 2/5 pan, tilt and
// zoom, 3/6 pan and tilt, 7 pan only).
func ptzType(a ability, channel int) int { return a.ver(channel, "ptzType") }

// enablePorts switches RTSP and RTMP on when they are off (tokens become
// invalid afterwards). It returns a note for the summary.
func (d *driver) enablePorts(ctx context.Context, np map[string]any) string {
	var on []string
	set := map[string]any{}
	for k, v := range np {
		set[k] = v
	}
	if v, ok := np["rtspEnable"]; ok && numOf(v) == 0 {
		set["rtspEnable"] = 1
		on = append(on, "RTSP")
	}
	if v, ok := np["rtmpEnable"]; ok && numOf(v) == 0 {
		set["rtmpEnable"] = 1
		on = append(on, "RTMP")
	}
	if len(on) == 0 {
		return ""
	}
	if err := d.api.call(ctx, "SetNetPort", map[string]any{"NetPort": set}, nil); err != nil {
		return strings.Join(on, " und ") + " an der Kamera ausgeschaltet"
	}
	d.api.forget()
	return strings.Join(on, " und ") + " an der Kamera eingeschaltet"
}

func numOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

// readStreams reads the RTSP paths (GetRtspUrl) and the codecs (GetEnc) of
// every stream channel.
func (d *driver) readStreams(ctx context.Context) {
	d.mu.RLock()
	n := d.channels
	d.mu.RUnlock()
	var cmds []cmd
	for ch := 0; ch < n && ch < 64; ch++ {
		cmds = append(cmds, cmd{Cmd: "GetRtspUrl", Param: map[string]any{"channel": ch}}, cmd{Cmd: "GetEnc", Param: map[string]any{"channel": ch}})
	}
	if len(cmds) == 0 {
		return
	}
	res, err := d.api.do(ctx, cmds...)
	if err != nil {
		return
	}
	paths := map[int]streamPaths{}
	for i, r := range res {
		if i >= len(cmds) || r.Code != 0 {
			continue
		}
		ch := i / 2
		p := paths[ch]
		switch r.Cmd {
		case "GetRtspUrl":
			var v struct {
				RtspURL struct {
					Main string `json:"mainStream"`
					Sub  string `json:"subStream"`
				} `json:"rtspUrl"`
			}
			if json.Unmarshal(r.Value, &v) == nil {
				if pm := rtspPath(v.RtspURL.Main); pm != "" {
					p.main = pm
				}
				if ps := rtspPath(v.RtspURL.Sub); ps != "" {
					p.sub = ps
				}
			}
		case "GetEnc":
			var v struct {
				Enc struct {
					Main struct {
						VType string `json:"vType"`
					} `json:"mainStream"`
					Sub struct {
						VType string `json:"vType"`
					} `json:"subStream"`
				} `json:"Enc"`
			}
			if json.Unmarshal(r.Value, &v) == nil {
				if p.main == "" && v.Enc.Main.VType != "" {
					p.main = fmt.Sprintf("/%sPreview_%02d_main", strings.ToLower(v.Enc.Main.VType), ch+1)
				}
				if p.sub == "" && v.Enc.Sub.VType != "" {
					p.sub = fmt.Sprintf("/%sPreview_%02d_sub", strings.ToLower(v.Enc.Sub.VType), ch+1)
				}
			}
		}
		paths[ch] = p
	}
	d.mu.Lock()
	for ch, p := range paths {
		d.paths[ch] = p
	}
	d.mu.Unlock()
}

// rtspPath returns path and query of an RTSP URL reported by the device
// (its host may be an internal address; CNM uses the configured one).
func rtspPath(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Path == "" || u.Path == "/" {
		return ""
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// ---- live -----------------------------------------------------------------------------------------

func (d *driver) LiveSources(channel int, kind provider.StreamKind) []string {
	d.mu.RLock()
	p, ok := d.paths[channel]
	port := d.rtspPort
	main := mainEnc(d.abilities, channel)
	d.mu.RUnlock()
	if port == 0 {
		port = d.cfg.Port("rtsp")
	}
	path := p.main
	if kind == provider.Sub {
		path = p.sub
	}
	if !ok || path == "" {
		path = fmt.Sprintf("/h264Preview_%02d_sub", channel+1)
		if kind == provider.Main {
			path = fmt.Sprintf("/%sPreview_%02d_main", main, channel+1)
		}
	}
	u := provider.CredURL("rtsp", d.cfg.Username, d.cfg.Password, d.cfg.Host, port, "", nil, "")
	return []string{u + path + "#backchannel=0#media=video"}
}

// mainEnc guesses the main stream codec when GetEnc is unavailable.
func mainEnc(a ability, channel int) string {
	if a.ver(channel, "mainEncType") > 0 {
		return "h265"
	}
	return "h264"
}

// ---- health ---------------------------------------------------------------------------------------

type reoTime struct {
	Year int `json:"year"`
	Mon  int `json:"mon"`
	Day  int `json:"day"`
	Hour int `json:"hour"`
	Min  int `json:"min"`
	Sec  int `json:"sec"`
}

func (t reoTime) wall() time.Time {
	return time.Date(t.Year, time.Month(t.Mon), t.Day, t.Hour, t.Min, t.Sec, 0, time.UTC)
}

func toReoTime(wall time.Time) reoTime {
	return reoTime{Year: wall.Year(), Mon: int(wall.Month()), Day: wall.Day(), Hour: wall.Hour(), Min: wall.Minute(), Sec: wall.Second()}
}

// syncTime measures the device clock (recordings carry its wall clock).
func (d *driver) syncTime(ctx context.Context) {
	var v struct {
		Time reoTime `json:"Time"`
	}
	if d.api.call(ctx, "GetTime", nil, &v) == nil && v.Time.Year > 2000 {
		d.clock.Set(provider.MeasureOffset(v.Time.wall(), time.Now()))
	}
}

func (d *driver) storage(ctx context.Context) provider.StorageState {
	var v struct {
		HddInfo []struct {
			Capacity int `json:"capacity"`
			Mount    int `json:"mount"`
			Format   int `json:"format"`
		} `json:"HddInfo"`
	}
	if d.api.call(ctx, "GetHddInfo", nil, &v) != nil {
		return provider.StorageUnknown
	}
	for _, h := range v.HddInfo {
		if h.Capacity > 0 && h.Mount == 1 {
			return provider.StoragePresent
		}
	}
	return provider.StorageAbsent
}

func (d *driver) Check(ctx context.Context) (*provider.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var np struct {
		NetPort map[string]any `json:"NetPort"`
	}
	if err := d.api.call(ctx, "GetNetPort", nil, &np); err != nil && provider.KindOf(err) != provider.KindUnsupported {
		return nil, err
	}
	h := &provider.Health{Storage: d.storage(ctx)}
	if np.NetPort != nil {
		if v, ok := np.NetPort["rtspEnable"]; ok && numOf(v) == 0 {
			h.Degraded = "RTSP ist an der Kamera ausgeschaltet – kein Livebild. In der Kamera unter Netzwerk → Erweitert → Server-Einstellungen einschalten."
		}
	}
	d.syncTime(ctx)
	if off, ok := d.clock.Offset(); ok {
		h.ClockOffset = &off
	}
	return h, nil
}

func (d *driver) Close() {
	go func() {
		d.api.logout()
		d.api.closeIdle()
	}()
}

func (d *driver) Test(ctx context.Context) (*provider.TestResult, error) {
	info, err := d.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return &provider.TestResult{Info: info, Message: "Erkannt: " + info.Summary}, nil
}
