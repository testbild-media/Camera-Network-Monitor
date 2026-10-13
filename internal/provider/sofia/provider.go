package sofia

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnm/internal/config"
	"cnm/internal/provider"
)

// ProviderID of the Sofia (DVRIP) protocol family.
const ProviderID = "sofia"

// defaultPort is the factory DVRIP port of XMeye/NETSurveillance (Sofia) devices.
const defaultPort = 34567

// ProtoSofia is the protocol id stored in the configuration.
const ProtoSofia = "sofia"

// VariantDVRIP is the only variant (the live transport is detected per connect).
const VariantDVRIP = "dvrip"

type prov struct{}

func init() { provider.Register(prov{}) }

func (prov) Info() provider.Info {
	return provider.Info{
		ID:    ProviderID,
		Name:  "Sofia (DVRIP)",
		Order: 20,
		Models: []provider.Model{{
			ID: ProtoSofia, Name: "Sofia (DVRIP)", AutoDetect: true,
			Ports:   []provider.PortSpec{{Key: "native", Label: "DVRIP-Port (TCP)", Default: defaultPort}},
			Options: []provider.OptionSpec{provider.ChannelOverride()},
			Capabilities: provider.Capabilities{
				Discovery:       provider.Experimental,
				ChannelList:     provider.Verified,
				LiveMain:        provider.Verified,
				LiveSub:         provider.Verified,
				RecordingSearch: provider.Verified, SearchKind: "list",
				Playback:    provider.Verified,
				DeviceTime:  provider.Experimental,
				StorageInfo: provider.Experimental,
			},
			Auth: "DVRIP-Login (Sofia-MD5)",
			Notes: "Kanalzahl und Live-Übertragung (Single-/Two-Socket) erkennt CNM beim Verbinden selbst. " +
				"Wiedergabe dateibasiert über eine zweite Verbindung (auf Hardware verifiziert).",
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

	mu       sync.Mutex
	ctl      *Control
	detected string // detected live transport ("" = not yet)
	clock    provider.Clock
}

func (d *driver) port() int { return d.cfg.Port("native") }

// effectiveMode returns the live transport detected at login (modern =
// single socket, legacy = two sockets), or "" while detection is pending.
// There is no manual override: every firmware is detected automatically.
func (d *driver) effectiveMode() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.detected
}

func (d *driver) Connect(ctx context.Context) (*provider.DeviceInfo, error) {
	if !d.env.AllowLogin() {
		return nil, provider.ErrLoginBlocked
	}
	ctl, err := DialControl(ctx, d.cfg.Host, d.port(), d.cfg.Username, d.cfg.Password)
	if err != nil {
		if provider.KindOf(err) == provider.KindAuth {
			d.env.ReportAuthFailure(err)
		}
		return nil, err
	}
	d.mu.Lock()
	old := d.ctl
	d.ctl = ctl
	d.mu.Unlock()
	if old != nil {
		old.Close()
	}
	ictx, cancel := context.WithTimeout(ctx, 6*time.Second)
	info, err := ctl.SystemInfo(ictx)
	cancel()
	if err != nil || info == nil {
		info = &provider.DeviceInfo{Channels: ctl.Channels()}
	}
	tctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	info.ChannelNames = ctl.ChannelTitles(tctx)
	cancel()
	d.syncTime(ctx, ctl)
	mode := d.detect(ctx, ctl)
	d.mu.Lock()
	d.detected = mode
	d.mu.Unlock()
	info.Extra = map[string]string{"liveMode": mode}
	info.Variant = VariantDVRIP
	info.Summary = fmt.Sprintf("DVRIP über TCP %d · Live %s", d.port(), liveModeName(mode))
	if n, err := strconv.Atoi(strings.TrimSpace(d.cfg.Options[provider.OptChannels])); err == nil && n > 0 {
		info.Channels = n
	}
	return info, nil
}

func liveModeName(mode string) string {
	if mode == config.LiveLegacy {
		return "Two-Socket"
	}
	return "Single-Socket"
}

// detect determines the live transport. Order: single-socket first (cheap,
// handled natively by go2rtc), then the two-socket bridge. When neither
// delivers data the result is "modern" and Check reports a degraded state.
func (d *driver) detect(ctx context.Context, ctl *Control) string {
	if !d.env.AllowLogin() {
		return config.LiveModern
	}
	ok, err := ProbeModern(ctx, d.cfg.Host, d.port(), d.cfg.Username, d.cfg.Password, 0)
	if err != nil && provider.KindOf(err) == provider.KindAuth {
		d.env.ReportAuthFailure(err)
		return config.LiveModern
	}
	if ok {
		return config.LiveModern
	}
	if ProbeLegacy(ctx, ctl, d.cfg.Host, d.port(), 0) {
		return config.LiveLegacy
	}
	return "undetected"
}

func (d *driver) Check(ctx context.Context) (*provider.Health, error) {
	d.mu.Lock()
	ctl := d.ctl
	d.mu.Unlock()
	if ctl == nil {
		return nil, provider.NewError(provider.KindNetwork, nil, "nicht verbunden")
	}
	select {
	case <-ctl.Done():
		return nil, ctl.Err()
	default:
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := ctl.Request(cctx, MsgKeepAlive, MsgKeepAliveRsp, map[string]any{"Name": "KeepAlive", "SessionID": SessionString(ctl.Session())})
	if err != nil {
		ctl.Close()
		return nil, err
	}
	if !r.OK() && r.Ret() != -1 {
		ctl.Close()
		return nil, provider.NewError(provider.KindProtocol, nil, "Keepalive abgelehnt (Code %d) – Sitzung ungültig", r.Ret())
	}
	h := &provider.Health{Storage: ctl.Storage(cctx)}
	d.syncTime(cctx, ctl)
	if off, ok := d.clock.Offset(); ok {
		h.ClockOffset = &off
	}
	mode := d.effectiveMode()
	h.LiveMode = mode
	if mode == "undetected" {
		h.Degraded = "Live-Modus nicht erkannt (Modern und Legacy liefern keine Daten auf Kanal 1)"
	}
	return h, nil
}

func (d *driver) LiveSources(channel int, kind provider.StreamKind) []string {
	switch d.effectiveMode() {
	case config.LiveLegacy:
		if u := d.env.LegacyLiveURL(channel, kind); u != "" {
			return []string{u}
		}
		return nil
	case "":
		return nil // auto detection pending
	default: // modern or undetected
		subtype := "0"
		if kind == provider.Sub {
			subtype = "1"
		}
		q := url.Values{"channel": {strconv.Itoa(channel)}, "subtype": {subtype}}
		return []string{provider.CredURL("dvrip", d.cfg.Username, d.cfg.Password, d.cfg.Host, d.port(), "", q, "")}
	}
}

func (d *driver) OpenLegacyLive(ctx context.Context, channel int, kind provider.StreamKind) (io.ReadCloser, error) {
	d.mu.Lock()
	ctl := d.ctl
	d.mu.Unlock()
	if ctl == nil {
		return nil, provider.NewError(provider.KindNetwork, nil, "Gerät nicht verbunden")
	}
	select {
	case <-ctl.Done():
		return nil, provider.NewError(provider.KindNetwork, ctl.Err(), "Steuerverbindung getrennt")
	default:
	}
	return LegacyLive(ctx, ctl, d.cfg.Host, d.port(), channel, kind == provider.Main)
}

func (d *driver) Close() {
	d.mu.Lock()
	ctl := d.ctl
	d.ctl = nil
	d.mu.Unlock()
	if ctl != nil {
		ctl.Close()
	}
}

// Test performs a one-shot login including the live transport detection.
func (d *driver) Test(ctx context.Context) (*provider.TestResult, error) {
	info, err := d.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	res := &provider.TestResult{Info: info, LiveMode: d.effectiveMode(), Message: fmt.Sprintf("Erkannt: %s · %d Kanäle", info.Summary, info.Channels)}
	if ctl, err := d.control(); err == nil {
		switch ctl.Storage(ctx) {
		case provider.StoragePresent:
			res.Message += " · Festplatte vorhanden"
		case provider.StorageAbsent:
			res.Message += " · keine Festplatte"
		}
	}
	if res.LiveMode != config.LiveModern && res.LiveMode != config.LiveLegacy {
		res.Message += " · Live-Übertragung konnte nicht erkannt werden"
	}
	return res, nil
}
