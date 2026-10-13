// Package hikvision implements Hikvision recorders
// and IP cameras over ISAPI (HTTP + Digest) and RTSP:
//
//	device info      GET  /ISAPI/System/deviceInfo
//	channels         GET  /ISAPI/ContentMgmt/InputProxy/channels (NVR, IP channels)
//	                 GET  /ISAPI/System/Video/inputs/channels    (DVR/camera, analog)
//	device clock     GET  /ISAPI/System/time
//	storage          GET  /ISAPI/ContentMgmt/Storage
//	recording search POST /ISAPI/ContentMgmt/search (CMSearchDescription, paged)
//	live             rtsp://host:554/Streaming/Channels/<ch>01 (main) / <ch>02 (sub)
//	playback         rtsp://host:554/Streaming/tracks/<ch>01?starttime=…&endtime=…
//
// Search and playback times use the "YYYY-MM-DDTHH:MM:SSZ" notation, but the
// recorders interpret them as their local wall clock; CNM therefore always
// converts through the measured device clock.
//
// Implemented from the public ISAPI documentation and tested against the
// simulator only – the capabilities are marked experimental.
package hikvision

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"cnm/internal/provider"
)

// ProviderID of the Hikvision ISAPI protocol.
const ProviderID = "hikvision"

// ProtoHikvision is the protocol id stored in the configuration.
const ProtoHikvision = "hikvision"

var caps = provider.Capabilities{
	Discovery: provider.Experimental, ChannelList: provider.Experimental,
	LiveMain: provider.Experimental, LiveSub: provider.Experimental,
	RecordingSearch: provider.Experimental, SearchKind: "list",
	Playback:    provider.Experimental,
	DeviceTime:  provider.Experimental,
	StorageInfo: provider.Experimental,
}

const auth = "ISAPI (HTTP Digest) · RTSP Digest"

type prov struct{}

func init() { provider.Register(prov{}) }

func ports() []provider.PortSpec {
	return []provider.PortSpec{{Key: "http", Label: "HTTP-Port (ISAPI)", Default: 80}, {Key: "rtsp", Label: "RTSP", Default: 554}}
}

func (prov) Info() provider.Info {
	return provider.Info{
		ID:    ProviderID,
		Name:  "Hikvision (ISAPI)",
		Order: 30,
		Models: []provider.Model{{
			ID: ProtoHikvision, Name: "Hikvision (ISAPI)", AutoDetect: true, Ports: ports(),
			Options:      []provider.OptionSpec{provider.ChannelOverride()},
			Capabilities: caps, Auth: auth,
			Notes: "Kanalzahl liest CNM vom Gerät (IP-Kanäle bzw. Videoeingänge). Nach der öffentlichen ISAPI-Dokumentation umgesetzt, noch nicht an Hardware getestet.",
		}},
	}
}

func (prov) NewDriver(cfg provider.DeviceConfig, env provider.Env) (provider.Driver, error) {
	if cfg.Host == "" {
		return nil, provider.NewError(provider.KindConfig, nil, "Host fehlt")
	}
	return &driver{cfg: cfg, env: env, hc: provider.NewHTTPClient(cfg.Username, cfg.Password, 20*time.Second)}, nil
}

type driver struct {
	cfg   provider.DeviceConfig
	env   provider.Env
	hc    *http.Client
	clock provider.Clock
	// trackList: the firmware expects <trackList> instead of the spec's
	// <trackIDList> in CMSearchDescription (detected on the first search)
	trackList atomic.Bool
}

func (d *driver) base() string { return "http://" + provider.HostPort(d.cfg.Host, d.cfg.Port("http")) }

// isapi performs a request and decodes the XML answer into out (may be nil).
func (d *driver) isapi(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base()+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/xml; charset=UTF-8")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return provider.ClassifyNetErr(err, "ISAPI")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return provider.ClassifyNetErr(err, "ISAPI")
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return provider.NewError(provider.KindAuth, nil, "ISAPI: Benutzername oder Passwort falsch")
	case resp.StatusCode == http.StatusForbidden:
		return provider.NewError(provider.KindAuth, nil, "ISAPI: Zugriff verweigert (Benutzerrechte oder gesperrte IP)")
	case resp.StatusCode == http.StatusNotFound:
		return provider.NewError(provider.KindUnsupported, nil, "ISAPI: %s wird nicht unterstützt", path)
	case resp.StatusCode != http.StatusOK:
		return provider.NewError(provider.KindProtocol, nil, "ISAPI antwortet mit HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if err := xml.Unmarshal(data, out); err != nil {
			return provider.NewError(provider.KindProtocol, err, "ISAPI: ungültige XML-Antwort")
		}
	}
	return nil
}

func (d *driver) login(ctx context.Context) (*provider.DeviceInfo, error) {
	if !d.env.AllowLogin() {
		return nil, provider.ErrLoginBlocked
	}
	var di struct {
		DeviceName      string `xml:"deviceName"`
		Model           string `xml:"model"`
		SerialNumber    string `xml:"serialNumber"`
		FirmwareVersion string `xml:"firmwareVersion"`
		FirmwareDate    string `xml:"firmwareReleasedDate"`
	}
	ictx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := d.isapi(ictx, http.MethodGet, "/ISAPI/System/deviceInfo", nil, &di); err != nil {
		if provider.KindOf(err) == provider.KindAuth {
			d.env.ReportAuthFailure(err)
		}
		return nil, err
	}
	info := &provider.DeviceInfo{Model: di.Model, Serial: di.SerialNumber,
		Firmware: strings.TrimSpace(di.FirmwareVersion + " " + di.FirmwareDate)}
	return info, nil
}

func (d *driver) Connect(ctx context.Context) (*provider.DeviceInfo, error) {
	info, err := d.login(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info.ChannelNames, info.Channels = d.channelNames(cctx)
	if n, err := strconv.Atoi(strings.TrimSpace(d.cfg.Options[provider.OptChannels])); err == nil && n > 0 {
		info.Channels = n
	}
	info.Variant = "isapi"
	info.Summary = fmt.Sprintf("ISAPI über HTTP %d · Live über RTSP %d", d.cfg.Port("http"), d.cfg.Port("rtsp"))
	d.syncTime(cctx)
	return info, nil
}

// channelNames reads the IP channels (recorders) or the video inputs
// (cameras, analog recorders) and returns their names and count.
func (d *driver) channelNames(ctx context.Context) (map[int]string, int) {
	type ch struct {
		ID   int    `xml:"id"`
		Name string `xml:"name"`
	}
	var proxy struct {
		Channels []ch `xml:"InputProxyChannel"`
	}
	var analog struct {
		Channels []ch `xml:"VideoInputChannel"`
	}
	out, n := map[int]string{}, 0
	add := func(list []ch) {
		for _, c := range list {
			if c.ID > 0 && c.Name != "" {
				out[c.ID-1] = c.Name
			}
			n = max(n, c.ID)
		}
	}
	if d.isapi(ctx, http.MethodGet, "/ISAPI/ContentMgmt/InputProxy/channels", nil, &proxy) == nil {
		add(proxy.Channels)
	}
	if n == 0 && d.isapi(ctx, http.MethodGet, "/ISAPI/System/Video/inputs/channels", nil, &analog) == nil {
		add(analog.Channels)
	}
	if n == 0 {
		n = 1
	}
	return out, n
}

// syncTime measures the device clock: localTime carries the wall clock
// fields the recorder uses for search and playback.
func (d *driver) syncTime(ctx context.Context) {
	var t struct {
		LocalTime string `xml:"localTime"`
	}
	if d.isapi(ctx, http.MethodGet, "/ISAPI/System/time", nil, &t) != nil {
		return
	}
	s := strings.TrimSpace(t.LocalTime)
	if len(s) >= 19 {
		if wall, err := time.Parse("2006-01-02T15:04:05", s[:19]); err == nil {
			d.clock.Set(provider.MeasureOffset(wall, time.Now()))
		}
	}
}

func (d *driver) storage(ctx context.Context) provider.StorageState {
	var st struct {
		HDDs []struct {
			Capacity string `xml:"capacity"`
			Status   string `xml:"status"`
		} `xml:"hddList>hdd"`
		NAS []struct {
			Capacity string `xml:"capacity"`
		} `xml:"nasList>nas"`
	}
	if d.isapi(ctx, http.MethodGet, "/ISAPI/ContentMgmt/Storage", nil, &st) != nil {
		return provider.StorageUnknown
	}
	for _, h := range st.HDDs {
		if n, _ := strconv.ParseFloat(h.Capacity, 64); n > 0 && !strings.EqualFold(h.Status, "unformatted") {
			return provider.StoragePresent
		}
	}
	for _, n := range st.NAS {
		if c, _ := strconv.ParseFloat(n.Capacity, 64); c > 0 {
			return provider.StoragePresent
		}
	}
	if len(st.HDDs) == 0 && len(st.NAS) == 0 {
		return provider.StorageAbsent
	}
	return provider.StorageUnknown
}

func (d *driver) Check(ctx context.Context) (*provider.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := d.login(ctx); err != nil {
		return nil, err
	}
	h := &provider.Health{Storage: d.storage(ctx)}
	d.syncTime(ctx)
	if off, ok := d.clock.Offset(); ok {
		h.ClockOffset = &off
	}
	return h, nil
}

// trackID is the ISAPI stream/track id: channel (1 based) * 100 + stream.
func trackID(channel int, kind provider.StreamKind) int {
	n := (channel+1)*100 + 1
	if kind == provider.Sub {
		n++
	}
	return n
}

func (d *driver) LiveSources(channel int, kind provider.StreamKind) []string {
	path := "/Streaming/Channels/" + strconv.Itoa(trackID(channel, kind))
	return []string{provider.CredURL("rtsp", d.cfg.Username, d.cfg.Password, d.cfg.Host, d.cfg.Port("rtsp"), path, nil, "backchannel=0#media=video")}
}

func (d *driver) Close() { provider.CloseIdle(d.hc) }

func (d *driver) Test(ctx context.Context) (*provider.TestResult, error) {
	info, err := d.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return &provider.TestResult{Info: info, Message: fmt.Sprintf("Erkannt: %s · %d Kanäle", info.Summary, info.Channels)}, nil
}

// ---- recordings ----------------------------------------------------------------------------------

const isapiTime = "2006-01-02T15:04:05Z"

func (d *driver) devTime(t time.Time) string { return d.clock.ToDevice(t).Format(isapiTime) }

type searchResult struct {
	Status  string `xml:"responseStatusStrg"`
	Matches int    `xml:"numOfMatches"`
	Items   []struct {
		Start string `xml:"timeSpan>startTime"`
		End   string `xml:"timeSpan>endTime"`
	} `xml:"matchList>searchMatchItem"`
}

// search pages through CMSearchDescription results (max. pages requests).
func (d *driver) search(ctx context.Context, channel int, from, to time.Time, pages, perPage int) ([]provider.Recording, error) {
	var out []provider.Recording
	id := randomID()
	pos := 0
	for page := 0; page < pages; page++ {
		var r searchResult
		err := d.isapi(ctx, http.MethodPost, "/ISAPI/ContentMgmt/search", d.searchBody(id, channel, from, to, perPage, pos), &r)
		if pos == 0 && !d.trackList.Load() && (provider.KindOf(err) == provider.KindProtocol || strings.EqualFold(strings.TrimSpace(r.Status), "INVALID TRACK ID")) {
			// older firmware only knows <trackList>
			d.trackList.Store(true)
			r = searchResult{}
			err = d.isapi(ctx, http.MethodPost, "/ISAPI/ContentMgmt/search", d.searchBody(id, channel, from, to, perPage, pos), &r)
		}
		if err != nil {
			return nil, err
		}
		for _, it := range r.Items {
			s, e1 := time.Parse(isapiTime, strings.TrimSpace(it.Start))
			e, e2 := time.Parse(isapiTime, strings.TrimSpace(it.End))
			if e1 == nil && e2 == nil {
				out = append(out, provider.Recording{Start: d.clock.FromDevice(s), End: d.clock.FromDevice(e)})
			}
		}
		if !strings.EqualFold(strings.TrimSpace(r.Status), "MORE") || len(r.Items) == 0 {
			break
		}
		pos += len(r.Items)
	}
	return out, nil
}

func (d *driver) searchBody(id string, channel int, from, to time.Time, perPage, pos int) []byte {
	list := "trackIDList"
	if d.trackList.Load() {
		list = "trackList"
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><CMSearchDescription><searchID>%s</searchID>`+
		`<%s><trackID>%d</trackID></%s>`+
		`<timeSpanList><timeSpan><startTime>%s</startTime><endTime>%s</endTime></timeSpan></timeSpanList>`+
		`<maxResults>%d</maxResults><searchResultPostion>%d</searchResultPostion>`+
		`<metadataList><metadataDescriptor>//recordType.meta.std-cgi.com</metadataDescriptor></metadataList></CMSearchDescription>`,
		id, list, trackID(channel, provider.Main), list, d.devTime(from), d.devTime(to), perPage, pos))
}

func (d *driver) SearchRecordings(ctx context.Context, channel int, from, to time.Time) ([]provider.Recording, error) {
	recs, err := d.search(ctx, channel, from, to, 25, 40)
	if err != nil {
		return nil, err
	}
	return provider.MergeRecordings(recs, from, to, 2*time.Second), nil
}

func (d *driver) HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error) {
	recs, err := d.search(ctx, channel, from, to, 1, 1)
	return len(recs) > 0, err
}

// OpenPlayback returns the RTSP track URL for FFmpeg.
func (d *driver) OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	const layout = "20060102T150405Z"
	q := "starttime=" + d.clock.ToDevice(from).Format(layout) + "&endtime=" + d.clock.ToDevice(to).Format(layout)
	u := provider.CredURL("rtsp", d.cfg.Username, d.cfg.Password, d.cfg.Host, d.cfg.Port("rtsp"), "/Streaming/tracks/"+strconv.Itoa(trackID(channel, provider.Main)), nil, "")
	return &provider.PlaybackSource{URL: u + "?" + q, InputArgs: []string{"-rtsp_transport", "tcp"}}, nil
}

func randomID() string {
	return fmt.Sprintf("CNM-%d", time.Now().UnixNano())
}
