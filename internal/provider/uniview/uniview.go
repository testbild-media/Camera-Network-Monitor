// Package uniview implements Uniview (UNV) recorders and IP cameras over
// their documented RTSP interface ("How to Get the URLs for Uniview IPC and
// NVR", V1.1):
//
//	live      rtsp://host:554/unicast/c<ch>/s0/live   (main; s1 = sub)
//	playback  rtsp://host:554/c<ch>/b<unix start>/e<unix end>/replay/
//
// Channels are 1 based, playback times are Unix seconds (UTC). The control
// check is an authenticated RTSP DESCRIBE. There is no documented recording
// search, so playback offers the selected range and reports missing
// material from the stream (like the time probe of other devices).
package uniview

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cnm/internal/provider"
)

// ProviderID of the Uniview RTSP protocol.
const ProviderID = "uniview"

// ProtoUniview is the protocol id stored in the configuration.
const ProtoUniview = "uniview"

var caps = provider.Capabilities{
	LiveMain: provider.Experimental, LiveSub: provider.Experimental,
	RecordingSearch: provider.Unsupported, SearchKind: "probe",
	Playback: provider.Experimental,
}

const notes = "Nach der Uniview-URL-Dokumentation umgesetzt, noch nicht an Hardware getestet. Die Kanalzahl ermittelt CNM per RTSP. " +
	"Keine Aufnahmeliste: ob Material vorhanden ist, fragt CNM per RTSP an; die Wiedergabe meldet fehlendes Material. " +
	"Mit ONVIF Profile G steht alternativ das Protokoll ONVIF zur Verfügung."

type prov struct{}

func init() { provider.Register(prov{}) }

func (prov) Info() provider.Info {
	return provider.Info{
		ID:    ProviderID,
		Name:  "Uniview (RTSP)",
		Order: 50,
		Models: []provider.Model{{
			ID: ProtoUniview, Name: "Uniview (RTSP)", AutoDetect: true,
			Ports:        []provider.PortSpec{{Key: "rtsp", Label: "RTSP", Default: 554}},
			Options:      []provider.OptionSpec{provider.ChannelOverride()},
			Capabilities: caps, Auth: "RTSP Digest", Notes: notes,
		}},
	}
}

func (prov) NewDriver(cfg provider.DeviceConfig, env provider.Env) (provider.Driver, error) {
	if cfg.Host == "" {
		return nil, provider.NewError(provider.KindConfig, nil, "Host fehlt")
	}
	return &driver{cfg: cfg, env: env}, nil
}

type driver struct {
	cfg provider.DeviceConfig
	env provider.Env
}

func livePath(channel int, kind provider.StreamKind) string {
	s := "s0"
	if kind == provider.Sub {
		s = "s1"
	}
	return fmt.Sprintf("/unicast/c%d/%s/live", channel+1, s)
}

func (d *driver) probe(ctx context.Context) error {
	if !d.env.AllowLogin() {
		return provider.ErrLoginBlocked
	}
	err := provider.RTSPProbe(ctx, d.cfg.Host, d.cfg.Port("rtsp"), d.cfg.Username, d.cfg.Password, livePath(0, provider.Main))
	if err != nil && provider.KindOf(err) == provider.KindAuth {
		d.env.ReportAuthFailure(err)
	}
	return err
}

func (d *driver) Connect(ctx context.Context) (*provider.DeviceInfo, error) {
	if err := d.probe(ctx); err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(d.cfg.Options[provider.OptChannels]))
	switch {
	case err == nil && n > 0:
	case d.cfg.Channels > 0 && d.cfg.Variant != "":
		n = d.cfg.Channels // known from an earlier detection
	default:
		n = d.countChannels(ctx)
	}
	return &provider.DeviceInfo{Channels: n, Variant: "rtsp", Summary: fmt.Sprintf("RTSP %d", d.cfg.Port("rtsp"))}, nil
}

// countChannels asks for the sub streams channel by channel until one is refused.
func (d *driver) countChannels(ctx context.Context) int {
	n := 0
	for ch := 0; ch < 64; ch++ {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, err := provider.RTSPDescribe(cctx, d.cfg.Host, d.cfg.Port("rtsp"), d.cfg.Username, d.cfg.Password, livePath(ch, provider.Sub))
		cancel()
		if err != nil || status != 200 {
			break
		}
		n = ch + 1
	}
	return max(n, 1)
}

func (d *driver) Check(ctx context.Context) (*provider.Health, error) {
	if err := d.probe(ctx); err != nil {
		return nil, err
	}
	return &provider.Health{Storage: provider.StorageUnknown}, nil
}

func (d *driver) LiveSources(channel int, kind provider.StreamKind) []string {
	return []string{provider.CredURL("rtsp", d.cfg.Username, d.cfg.Password, d.cfg.Host, d.cfg.Port("rtsp"), livePath(channel, kind), nil, "backchannel=0#media=video")}
}

func (d *driver) Close() {}

func replayPath(channel int, from, to time.Time) string {
	return "/c" + strconv.Itoa(channel+1) + "/b" + strconv.FormatInt(from.Unix(), 10) + "/e" + strconv.FormatInt(to.Unix(), 10) + "/replay/"
}

// OpenPlayback returns the replay URL (Unix seconds, UTC) for FFmpeg.
func (d *driver) OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	return &provider.PlaybackSource{URL: provider.CredURL("rtsp", d.cfg.Username, d.cfg.Password, d.cfg.Host, d.cfg.Port("rtsp"), replayPath(channel, from, to), nil, ""), InputArgs: []string{"-rtsp_transport", "tcp"}}, nil
}

// HasRecordings asks the recorder with a DESCRIBE of the replay range
// (there is no recording list): 200 = material, 404/454 = none.
func (d *driver) HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error) {
	if !d.env.AllowLogin() {
		return false, provider.ErrLoginBlocked
	}
	status, err := provider.RTSPDescribe(ctx, d.cfg.Host, d.cfg.Port("rtsp"), d.cfg.Username, d.cfg.Password, replayPath(channel, from, to))
	switch {
	case err != nil:
		return false, err
	case status == 200:
		return true, nil
	case status == 404 || status == 454:
		return false, nil
	case status == 401 || status == 403:
		err := provider.NewError(provider.KindAuth, nil, "RTSP: Benutzername oder Passwort falsch")
		d.env.ReportAuthFailure(err)
		return false, err
	}
	return false, provider.NewError(provider.KindProtocol, nil, "RTSP antwortet mit Status %d", status)
}

func (d *driver) Test(ctx context.Context) (*provider.TestResult, error) {
	info, err := d.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &provider.TestResult{Info: info, Message: fmt.Sprintf("Erkannt: %s · %d Kanäle", info.Summary, info.Channels)}, nil
}
