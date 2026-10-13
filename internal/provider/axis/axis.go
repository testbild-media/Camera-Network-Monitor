// Package axis implements Axis network cameras and recorders over VAPIX
// (HTTP + Digest) and RTSP (developer.axis.com):
//
//	device info   GET  /axis-cgi/param.cgi?action=list&group=…
//	device clock  POST /axis-cgi/time.cgi {"method":"getDateTimeInfo"}
//	storage       GET  /axis-cgi/disks/list.cgi?diskid=all
//	recordings    GET  /axis-cgi/record/list.cgi?recordingid=all&starttime=…&stoptime=…
//	live          rtsp://host/axis-media/media.amp?camera=<n>&videocodec=h264
//	playback      rtsp://host/axis-media/media.amp?recordingid=<id>, PLAY with
//	              "Range: clock=<from>-<to>" (Edge Storage API)
//
// Axis reports recording times in UTC, so no device clock mapping is needed.
// Implemented from the public VAPIX documentation and tested against the
// simulator only – the capabilities are marked experimental.
package axis

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnm/internal/provider"
	"cnm/internal/rtsp"
)

// ProviderID of the Axis VAPIX protocol.
const ProviderID = "axis"

// ProtoAxis is the protocol id stored in the configuration.
const ProtoAxis = "axis"

type prov struct{}

func init() { provider.Register(prov{}) }

func (prov) Info() provider.Info {
	return provider.Info{
		ID:    ProviderID,
		Name:  "Axis (VAPIX)",
		Order: 40,
		Models: []provider.Model{{
			ID: ProtoAxis, Name: "Axis (VAPIX)", AutoDetect: true,
			Ports:   []provider.PortSpec{{Key: "http", Label: "HTTP-Port (VAPIX)", Default: 80}, {Key: "rtsp", Label: "RTSP", Default: 554}},
			Options: []provider.OptionSpec{provider.ChannelOverride()},
			Capabilities: provider.Capabilities{
				Discovery: provider.Experimental, ChannelList: provider.Experimental,
				LiveMain: provider.Experimental, LiveSub: provider.Experimental,
				RecordingSearch: provider.Experimental, SearchKind: "list",
				Playback: provider.Experimental, DeviceTime: provider.Experimental, StorageInfo: provider.Experimental,
			},
			Auth:  "VAPIX (HTTP Digest) · RTSP Digest",
			Notes: "Kanäle = Videoquellen (vom Gerät gelesen). Aufnahmen auf SD-Karte/NAS (Edge Storage). Nach der öffentlichen VAPIX-Dokumentation umgesetzt, noch nicht an Hardware getestet. Substream: 640×360 vom Gerät skaliert.",
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
	cfg provider.DeviceConfig
	env provider.Env
	hc  *http.Client

	mu     sync.Mutex
	offset *time.Duration // device clock error (UTC)
}

func (d *driver) base() string { return "http://" + provider.HostPort(d.cfg.Host, d.cfg.Port("http")) }

func (d *driver) vapix(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.base()+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "VAPIX")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, provider.ClassifyNetErr(err, "VAPIX")
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, provider.NewError(provider.KindAuth, nil, "VAPIX: Benutzername oder Passwort falsch")
	case resp.StatusCode == http.StatusForbidden:
		return nil, provider.NewError(provider.KindAuth, nil, "VAPIX: Zugriff verweigert (Benutzerrechte)")
	case resp.StatusCode == http.StatusNotFound:
		return nil, provider.NewError(provider.KindUnsupported, nil, "VAPIX: %s wird nicht unterstützt", strings.SplitN(path, "?", 2)[0])
	case resp.StatusCode != http.StatusOK:
		return nil, provider.NewError(provider.KindProtocol, nil, "VAPIX antwortet mit HTTP %d", resp.StatusCode)
	}
	return data, nil
}

// params parses "root.Group.Name=value" lines of param.cgi.
func params(b []byte) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

func (d *driver) login(ctx context.Context) (*provider.DeviceInfo, error) {
	if !d.env.AllowLogin() {
		return nil, provider.ErrLoginBlocked
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	b, err := d.vapix(ctx, http.MethodGet, "/axis-cgi/param.cgi?action=list&group=root.Brand.ProdFullName,root.Brand.ProdNbr,root.Properties.System.SerialNumber,root.Properties.Firmware.Version,root.ImageSource", nil)
	if err != nil {
		if provider.KindOf(err) == provider.KindAuth {
			d.env.ReportAuthFailure(err)
		}
		return nil, err
	}
	p := params(b)
	info := &provider.DeviceInfo{Model: strings.TrimSpace(firstNonEmpty(p["root.Brand.ProdFullName"], p["root.Brand.ProdNbr"])),
		Serial: p["root.Properties.System.SerialNumber"], Firmware: p["root.Properties.Firmware.Version"],
		Variant: "vapix", Summary: fmt.Sprintf("VAPIX über HTTP %d · Live über RTSP %d", d.cfg.Port("http"), d.cfg.Port("rtsp"))}
	// channels = video sources (root.ImageSource.I0, I1, …)
	for k := range p {
		if m := reImageSource.FindStringSubmatch(k); m != nil {
			if n, _ := strconv.Atoi(m[1]); n+1 > info.Channels {
				info.Channels = n + 1
			}
		}
	}
	if n, err := strconv.Atoi(strings.TrimSpace(d.cfg.Options[provider.OptChannels])); err == nil && n > 0 {
		info.Channels = n
	}
	if info.Channels == 0 {
		info.Channels = 1
	}
	return info, nil
}

var reImageSource = regexp.MustCompile(`^root\.ImageSource\.I(\d+)\.`)

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func (d *driver) Connect(ctx context.Context) (*provider.DeviceInfo, error) {
	info, err := d.login(ctx)
	if err != nil {
		return nil, err
	}
	d.syncTime(ctx)
	return info, nil
}

// syncTime measures the device clock error (time.cgi, UTC).
func (d *driver) syncTime(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	b, err := d.vapix(ctx, http.MethodPost, "/axis-cgi/time.cgi", strings.NewReader(`{"apiVersion":"1.0","method":"getDateTimeInfo"}`))
	if err != nil {
		return
	}
	var r struct {
		Data struct {
			DateTime string `json:"dateTime"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &r) != nil {
		return
	}
	t, err := time.Parse(time.RFC3339, r.Data.DateTime)
	if err != nil {
		return
	}
	off := t.Sub(time.Now()).Round(time.Second)
	d.mu.Lock()
	d.offset = &off
	d.mu.Unlock()
}

func (d *driver) storage(ctx context.Context) provider.StorageState {
	b, err := d.vapix(ctx, http.MethodGet, "/axis-cgi/disks/list.cgi?diskid=all", nil)
	if err != nil {
		return provider.StorageUnknown
	}
	var r struct {
		Disks []struct {
			Status    string `xml:"status,attr"`
			TotalSize string `xml:"totalsize,attr"`
		} `xml:"disks>disk"`
	}
	if xml.Unmarshal(b, &r) != nil {
		return provider.StorageUnknown
	}
	for _, disk := range r.Disks {
		if n, _ := strconv.ParseFloat(disk.TotalSize, 64); n > 0 && strings.EqualFold(disk.Status, "OK") {
			return provider.StoragePresent
		}
	}
	if len(r.Disks) == 0 {
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
	d.mu.Lock()
	h.ClockOffset = d.offset
	d.mu.Unlock()
	return h, nil
}

func (d *driver) LiveSources(channel int, kind provider.StreamKind) []string {
	q := url.Values{"camera": {strconv.Itoa(channel + 1)}, "videocodec": {"h264"}}
	if kind == provider.Sub {
		q.Set("resolution", "640x360")
		q.Set("fps", "15")
	}
	return []string{provider.CredURL("rtsp", d.cfg.Username, d.cfg.Password, d.cfg.Host, d.cfg.Port("rtsp"), "/axis-media/media.amp", q, "backchannel=0#media=video")}
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

type recording struct {
	ID         string
	Disk       string
	Source     int // channel, 1 based
	Start, End time.Time
}

const axisTime = "2006-01-02T15:04:05Z"

func parseAxisTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	return t, err == nil
}

// list fetches the recordings overlapping [from, to] (all sources, paged).
func (d *driver) list(ctx context.Context, from, to time.Time, max int) ([]recording, error) {
	var out []recording
	for start := 1; ; {
		q := url.Values{"recordingid": {"all"}, "starttime": {from.UTC().Format(axisTime)}, "stoptime": {to.UTC().Format(axisTime)},
			"sortorder": {"ascending"}, "maxnumberofresults": {"100"}, "startatresultnumber": {strconv.Itoa(start)}}
		b, err := d.vapix(ctx, http.MethodGet, "/axis-cgi/record/list.cgi?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var x struct {
			Recordings struct {
				Total int `xml:"totalnumberofrecordings,attr"`
				Recs  []struct {
					ID     string `xml:"recordingid,attr"`
					Disk   string `xml:"diskid,attr"`
					Start  string `xml:"starttime,attr"`
					Stop   string `xml:"stoptime,attr"`
					Source string `xml:"source,attr"`
				} `xml:"recording"`
			} `xml:"recordings"`
		}
		if err := xml.Unmarshal(b, &x); err != nil {
			return nil, provider.NewError(provider.KindProtocol, err, "VAPIX: ungültige Aufnahmeliste")
		}
		r := x.Recordings
		for _, x := range r.Recs {
			st, ok := parseAxisTime(x.Start)
			if !ok {
				continue
			}
			en, ok := parseAxisTime(x.Stop)
			if !ok {
				en = time.Now() // recording in progress
			}
			src, _ := strconv.Atoi(x.Source)
			if src == 0 {
				src = 1
			}
			out = append(out, recording{ID: x.ID, Disk: x.Disk, Source: src, Start: st, End: en})
		}
		start += len(r.Recs)
		if len(r.Recs) == 0 || start > r.Total || len(out) >= max {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func (d *driver) SearchRecordings(ctx context.Context, channel int, from, to time.Time) ([]provider.Recording, error) {
	recs, err := d.list(ctx, from, to, 2000)
	if err != nil {
		return nil, err
	}
	var out []provider.Recording
	for _, r := range recs {
		if r.Source == channel+1 {
			out = append(out, provider.Recording{Start: r.Start, End: r.End})
		}
	}
	return provider.MergeRecordings(out, from, to, 2*time.Second), nil
}

func (d *driver) HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error) {
	recs, err := d.SearchRecordings(ctx, channel, from, to)
	return len(recs) > 0, err
}

// OpenPlayback replays the recording that contains from (Range: clock).
func (d *driver) OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	recs, err := d.list(ctx, from.Add(-24*time.Hour), to, 2000)
	if err != nil {
		return nil, err
	}
	// the recording that contains from, else the first one after it
	var pick *recording
	for i := range recs {
		r := &recs[i]
		if r.Source != channel+1 || !r.End.After(from) || !r.Start.Before(to) {
			continue
		}
		if !r.Start.After(from) {
			pick = r // ascending order: the last one starting before from wins
		} else if pick == nil {
			pick = r
			break
		}
	}
	if pick == nil {
		return nil, provider.NewError(provider.KindNotFound, nil, "keine Aufnahme im gewählten Zeitraum")
	}
	start := from
	if start.Before(pick.Start) {
		start = pick.Start
	}
	u := provider.CredURL("rtsp", "", "", d.cfg.Host, d.cfg.Port("rtsp"), "/axis-media/media.amp", url.Values{"recordingid": {pick.ID}}, "")
	rd, err := rtsp.OpenReplay(ctx, u, rtsp.Options{User: d.cfg.Username, Pass: d.cfg.Password},
		map[string]string{"Range": "clock=" + rtsp.ClockStamp(start, false) + "-" + rtsp.ClockStamp(to, true)}, start, to)
	if err != nil {
		return nil, err
	}
	return &provider.PlaybackSource{Reader: rd, Format: "mpegts"}, nil
}
