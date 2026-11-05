package dahua

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cnm/internal/provider"
)

const dahuaTime = "2006-01-02 15:04:05"

// dahuaTimeRe accepts the zero padded form ("2026-11-04 15:00:45") and the
// space padded one some firmware sends in recording lists
// ("2026-11- 4 15: 0:45").
var dahuaTimeRe = regexp.MustCompile(`^(\d{4})-\s*(\d{1,2})-\s*(\d{1,2})[ T]+(\d{1,2}):\s*(\d{1,2}):\s*(\d{1,2})$`)

// parseDahuaTime reads a device wall clock time (location UTC = device fields).
func parseDahuaTime(s string) (time.Time, error) {
	m := dahuaTimeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return time.Time{}, fmt.Errorf("dahua: unreadable time %q", s)
	}
	var n [6]int
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	if n[1] < 1 || n[1] > 12 || n[2] < 1 || n[2] > 31 || n[3] > 24 || n[4] > 59 || n[5] > 59 {
		return time.Time{}, fmt.Errorf("dahua: invalid time %q", s)
	}
	// hour 24 (end of day) normalises to 00:00 of the next day
	return time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, time.UTC), nil
}

type rpcDriver struct {
	cfg   provider.DeviceConfig
	env   provider.Env
	rpc   *rpcClient
	clock provider.Clock
}

func newRPCDriver(cfg provider.DeviceConfig, env provider.Env, variant string) *rpcDriver {
	return &rpcDriver{
		cfg: cfg,
		env: env,
		rpc: newRPCClient(cfg.Host, cfg.Port("http"), cfg.Username, cfg.Password, variant, env),
	}
}

func (d *rpcDriver) Connect(ctx context.Context) (*provider.DeviceInfo, error) {
	d.rpc.clearSession()
	if err := d.rpc.login(ctx); err != nil {
		return nil, err
	}
	info := &provider.DeviceInfo{} // channels: from ChannelTitle below
	ictx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if r, err := d.rpc.call(ictx, "magicBox.getDeviceType", nil, nil); err == nil && r.ok() {
		var p struct {
			Type string `json:"type"`
		}
		if decodeParams(r, &p) == nil {
			info.Model = p.Type
		}
	}
	if r, err := d.rpc.call(ictx, "magicBox.getSerialNo", nil, nil); err == nil && r.ok() {
		var p struct {
			SN string `json:"sn"`
		}
		if decodeParams(r, &p) == nil {
			info.Serial = p.SN
		}
	}
	if r, err := d.rpc.call(ictx, "magicBox.getSoftwareVersion", nil, nil); err == nil && r.ok() {
		var p struct {
			Version struct {
				Version   string `json:"Version"`
				BuildDate string `json:"BuildDate"`
			} `json:"version"`
		}
		if decodeParams(r, &p) == nil {
			info.Firmware = strings.TrimSpace(p.Version.Version + " " + p.Version.BuildDate)
		}
	}
	if r, err := d.rpc.call(ictx, "configManager.getConfig", map[string]any{"name": "ChannelTitle"}, nil); err == nil && r.ok() {
		var p struct {
			Table []struct {
				Name string `json:"Name"`
			} `json:"table"`
		}
		if decodeParams(r, &p) == nil {
			info.ChannelNames = map[int]string{}
			info.Channels = len(p.Table)
			for i, t := range p.Table {
				if t.Name != "" {
					info.ChannelNames[i] = t.Name
				}
			}
		}
	}
	d.syncTime(ictx)
	return info, nil
}

func (d *rpcDriver) syncTime(ctx context.Context) {
	r, err := d.rpc.call(ctx, "global.getCurrentTime", nil, nil)
	if err != nil || !r.ok() {
		return
	}
	var p struct {
		Time string `json:"time"`
	}
	if decodeParams(r, &p) != nil {
		return
	}
	wall, err := parseDahuaTime(p.Time)
	if err != nil {
		return
	}
	d.clock.Set(provider.MeasureOffset(wall, time.Now()))
}

func (d *rpcDriver) Check(ctx context.Context) (*provider.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	r, err := d.rpc.call(ctx, "global.keepAlive", map[string]any{"timeout": 300, "active": true}, nil)
	if err != nil {
		return nil, err
	}
	if !r.ok() {
		return nil, provider.NewError(provider.KindProtocol, nil, "Keepalive abgelehnt")
	}
	h := &provider.Health{Storage: d.storage(ctx)}
	d.syncTime(ctx)
	if off, ok := d.clock.Offset(); ok {
		h.ClockOffset = &off
	}
	return h, nil
}

func (d *rpcDriver) storage(ctx context.Context) provider.StorageState {
	r, err := d.rpc.call(ctx, "storage.getDeviceAllInfo", nil, nil)
	if err != nil || !r.ok() {
		return provider.StorageUnknown
	}
	var p struct {
		Info []struct {
			Detail []struct {
				TotalBytes json.Number `json:"TotalBytes"`
			} `json:"Detail"`
		} `json:"info"`
	}
	if decodeParams(r, &p) != nil {
		return provider.StorageUnknown
	}
	if len(p.Info) == 0 {
		return provider.StorageAbsent
	}
	for _, i := range p.Info {
		for _, det := range i.Detail {
			if f, err := det.TotalBytes.Float64(); err == nil && f > 0 {
				return provider.StoragePresent
			}
		}
	}
	return provider.StorageUnknown
}

func (d *rpcDriver) LiveSources(channel int, kind provider.StreamKind) []string {
	return []string{liveURL(d.cfg, channel, kind)}
}

// SearchRecordings uses the mediaFileFind object API.
func (d *rpcDriver) SearchRecordings(ctx context.Context, channel int, from, to time.Time) ([]provider.Recording, error) {
	r, err := d.rpc.call(ctx, "mediaFileFind.factory.create", nil, nil)
	if err != nil {
		return nil, err
	}
	var object int64
	if err := json.Unmarshal(r.Result, &object); err != nil || object == 0 {
		return nil, provider.NewError(provider.KindProtocol, err, "Aufnahmesuche: Suchobjekt konnte nicht erstellt werden")
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = d.rpc.call(cctx, "mediaFileFind.close", nil, object)
		_, _ = d.rpc.call(cctx, "mediaFileFind.destroy", nil, object)
	}()
	cond := map[string]any{
		"condition": map[string]any{
			"Channel":     channel,
			"StartTime":   d.clock.ToDevice(from).Format(dahuaTime),
			"EndTime":     d.clock.ToDevice(to).Format(dahuaTime),
			"Types":       []string{"dav"},
			"Dirs":        []string{},
			"Flags":       []string{"Timing", "Event", "Manual", "Marker"},
			"VideoStream": "Main",
		},
	}
	r, err = d.rpc.call(ctx, "mediaFileFind.findFile", cond, object)
	if err != nil {
		return nil, err
	}
	if !r.ok() {
		return nil, nil // no matching files
	}
	var out []provider.Recording
	unreadable := ""
	for i := 0; i < 50; i++ {
		r, err := d.rpc.call(ctx, "mediaFileFind.findNextFile", map[string]any{"count": 100}, object)
		if err != nil {
			return nil, err
		}
		var p struct {
			Found json.Number `json:"found"`
			Infos []struct {
				StartTime string `json:"StartTime"`
				EndTime   string `json:"EndTime"`
			} `json:"infos"`
		}
		if !r.ok() || decodeParams(r, &p) != nil {
			break
		}
		for _, inf := range p.Infos {
			s, e1 := parseDahuaTime(inf.StartTime)
			e, e2 := parseDahuaTime(inf.EndTime)
			if e1 != nil || e2 != nil {
				unreadable = inf.StartTime + " – " + inf.EndTime
				continue
			}
			out = append(out, provider.Recording{Start: d.clock.FromDevice(s), End: d.clock.FromDevice(e)})
		}
		n, _ := strconv.Atoi(p.Found.String())
		if n < 100 || len(p.Infos) == 0 {
			break
		}
	}
	if len(out) == 0 && unreadable != "" {
		// the device found recordings, but CNM cannot read their times:
		// say so instead of showing an empty timeline
		return nil, provider.NewError(provider.KindProtocol, nil, "Aufnahmeliste: unbekanntes Zeitformat (%q)", unreadable)
	}
	return provider.MergeRecordings(out, from, to, 2*time.Second), nil
}

// HasRecordings checks for recordings without listing them: findFile
// answers false when nothing matches.
func (d *rpcDriver) HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error) {
	r, err := d.rpc.call(ctx, "mediaFileFind.factory.create", nil, nil)
	if err != nil {
		return false, err
	}
	var object int64
	if err := json.Unmarshal(r.Result, &object); err != nil || object == 0 {
		return false, provider.NewError(provider.KindProtocol, err, "Aufnahmesuche: Suchobjekt konnte nicht erstellt werden")
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = d.rpc.call(cctx, "mediaFileFind.close", nil, object)
		_, _ = d.rpc.call(cctx, "mediaFileFind.destroy", nil, object)
	}()
	r, err = d.rpc.call(ctx, "mediaFileFind.findFile", map[string]any{"condition": map[string]any{
		"Channel": channel, "StartTime": d.clock.ToDevice(from).Format(dahuaTime), "EndTime": d.clock.ToDevice(to).Format(dahuaTime),
		"Types": []string{"dav"}, "Dirs": []string{}, "Flags": []string{"Timing", "Event", "Manual", "Marker"}, "VideoStream": "Main",
	}}, object)
	if err != nil {
		return false, err
	}
	if !r.ok() {
		return false, nil
	}
	r, err = d.rpc.call(ctx, "mediaFileFind.findNextFile", map[string]any{"count": 1}, object)
	if err != nil {
		return false, err
	}
	if !r.ok() {
		return false, nil
	}
	// a truthy answer alone is not enough: some firmware answers true with
	// an empty list
	var p struct {
		Found json.Number       `json:"found"`
		Infos []json.RawMessage `json:"infos"`
	}
	if decodeParams(r, &p) != nil {
		return false, nil
	}
	if n, err := strconv.Atoi(p.Found.String()); err == nil {
		return n > 0, nil
	}
	return len(p.Infos) > 0, nil
}

// OpenPlayback returns the Dahua RTSP playback URL for FFmpeg.
func (d *rpcDriver) OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	const layout = "2006_01_02_15_04_05"
	path := "/cam/playback"
	q := "channel=" + strconv.Itoa(channel+1) + "&subtype=0&starttime=" + d.clock.ToDevice(from).Format(layout) + "&endtime=" + d.clock.ToDevice(to).Format(layout)
	u := provider.CredURL("rtsp", d.cfg.Username, d.cfg.Password, d.cfg.Host, d.cfg.Port("rtsp"), path, nil, "")
	return &provider.PlaybackSource{URL: u + "?" + q, InputArgs: []string{"-rtsp_transport", "tcp"}}, nil
}

func (d *rpcDriver) Close() {
	go func() {
		d.rpc.logout()
		d.rpc.close()
	}()
}
