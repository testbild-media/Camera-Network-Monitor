package dahua

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnm/internal/provider"
)

// familyDriver detects which Dahua variant a device speaks and delegates to
// the RPC2 or the native driver. A stored variant (from an earlier
// detection) is used directly; it is only re-detected when the device stops
// speaking it (protocol error), never after an authentication failure.
//
// Detection, in this order:
//  1. RPC2 on the HTTP port with each login variant (verified ones first).
//     The challenge step is harmless; a variant whose challenge is not
//     answered costs no login attempt. At most maxDetectFailures logins may
//     be rejected (recorders lock accounts after a few failures).
//  2. Without RPC2: the native protocol's challenge on the native port; the
//     credentials are then checked with an RTSP DESCRIBE (as at every
//     connect of such devices).
type familyDriver struct {
	cfg provider.DeviceConfig
	env provider.Env

	mu      sync.Mutex
	impl    provider.Driver // *rpcDriver or *nativeDriver
	variant string
}

const maxDetectFailures = 2

func newFamilyDriver(cfg provider.DeviceConfig, env provider.Env) *familyDriver {
	return &familyDriver{cfg: cfg, env: env}
}

func (d *familyDriver) current() provider.Driver {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.impl
}

func (d *familyDriver) build(variant string) provider.Driver {
	if variant == VariantNative {
		return newNativeDriver(d.cfg, d.env)
	}
	return newRPCDriver(d.cfg, d.env, variant)
}

func (d *familyDriver) Connect(ctx context.Context) (*provider.DeviceInfo, error) {
	impl, variant := d.current(), ""
	d.mu.Lock()
	variant = d.variant
	d.mu.Unlock()
	if impl == nil {
		if !d.env.AllowLogin() {
			return nil, provider.ErrLoginBlocked
		}
		stored := d.cfg.Variant
		if _, known := rpcProfiles[stored]; known || stored == VariantNative {
			variant, impl = stored, d.build(stored)
		} else {
			var err error
			if variant, impl, err = d.detect(ctx); err != nil {
				return nil, err
			}
		}
		d.mu.Lock()
		d.impl, d.variant = impl, variant
		d.mu.Unlock()
	}
	info, err := impl.Connect(ctx)
	if err != nil {
		if provider.KindOf(err) == provider.KindProtocol && d.cfg.Variant != "" {
			// the device no longer speaks the stored variant: detect again next time
			d.mu.Lock()
			d.impl, d.cfg.Variant = nil, ""
			d.mu.Unlock()
			impl.Close()
		}
		return nil, err
	}
	info.Variant, info.Summary = variant, d.summary(variant)
	if n := channelOverride(d.cfg.Options); n > 0 {
		info.Channels = n
	} else if info.Channels == 0 {
		if d.cfg.Channels > 0 {
			info.Channels = d.cfg.Channels // known from an earlier detection
		} else {
			info.Channels = d.countRTSPChannels(ctx)
		}
	}
	return info, nil
}

// detect finds the variant (see familyDriver). Rejected logins are reported
// to the supervisor only once detection is over.
func (d *familyDriver) detect(ctx context.Context) (string, provider.Driver, error) {
	var authErr, rpcErr error
	failures := 0
	for _, v := range rpcVariants {
		if failures >= maxDetectFailures {
			break
		}
		cl := newRPCClient(d.cfg.Host, d.cfg.Port("http"), d.cfg.Username, d.cfg.Password, v, quietEnv{d.env})
		err := cl.login(ctx)
		cl.close()
		switch {
		case err == nil:
			return v, newRPCDriver(d.cfg, d.env, v), nil
		case provider.KindOf(err) == provider.KindAuth:
			failures++
			authErr = err
			if strings.Contains(err.Error(), "gesperrt") {
				failures = maxDetectFailures // never provoke a longer lock
			}
		case provider.KindOf(err) == provider.KindNetwork:
			rpcErr = err // HTTP port not reachable: no RPC2 at all
		default:
			if rpcErr == nil || provider.KindOf(rpcErr) != provider.KindNetwork {
				rpcErr = err // answered, but not with this variant's RPC2
			}
		}
		if provider.KindOf(err) == provider.KindNetwork || ctx.Err() != nil {
			break
		}
	}
	if authErr != nil {
		d.env.ReportAuthFailure(authErr)
		return "", nil, authErr
	}
	nerr := probeNative(ctx, d.cfg.Host, d.cfg.Port("native"))
	if nerr == nil {
		return VariantNative, newNativeDriver(d.cfg, d.env), nil
	}
	if ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	kind := provider.KindProtocol
	if provider.KindOf(rpcErr) == provider.KindNetwork && provider.KindOf(nerr) == provider.KindNetwork {
		kind = provider.KindNetwork // nothing answers: retry with backoff
	}
	return "", nil, provider.NewError(kind, errors.Join(rpcErr, nerr),
		"Kein Dahua-Protokoll erkannt – HTTP-Port %d (RPC2): %s; nativer Port %d: %s",
		d.cfg.Port("http"), errText(rpcErr), d.cfg.Port("native"), errText(nerr))
}

func errText(err error) string {
	if err == nil {
		return "keine Antwort"
	}
	return provider.Redact(err.Error())
}

// quietEnv defers authentication failure reports during detection.
type quietEnv struct{ provider.Env }

func (quietEnv) ReportAuthFailure(error) {}

func (d *familyDriver) summary(variant string) string {
	if variant == VariantNative {
		return fmt.Sprintf("nativ über TCP %d · Live über RTSP %d", d.cfg.Port("native"), d.cfg.Port("rtsp"))
	}
	return fmt.Sprintf("RPC2 über HTTP %d · Live über RTSP %d", d.cfg.Port("http"), d.cfg.Port("rtsp"))
}

// countRTSPChannels asks the RTSP server channel by channel (sub stream)
// until a channel is refused – for devices that do not list their channels.
func (d *familyDriver) countRTSPChannels(ctx context.Context) int {
	const max = 32
	n := 0
	for ch := 1; ch <= max; ch++ {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, err := provider.RTSPDescribe(cctx, d.cfg.Host, d.cfg.Port("rtsp"), d.cfg.Username, d.cfg.Password, "/cam/realmonitor?channel="+strconv.Itoa(ch)+"&subtype=1")
		cancel()
		if err != nil || status != 200 {
			break
		}
		n = ch
	}
	if n == 0 {
		n = 1
	}
	return n
}

func channelOverride(opts map[string]string) int {
	n, err := strconv.Atoi(strings.TrimSpace(opts[provider.OptChannels]))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func (d *familyDriver) Check(ctx context.Context) (*provider.Health, error) {
	impl := d.current()
	if impl == nil {
		return nil, provider.NewError(provider.KindNetwork, nil, "nicht verbunden")
	}
	return impl.Check(ctx)
}

func (d *familyDriver) LiveSources(channel int, kind provider.StreamKind) []string {
	return []string{liveURL(d.cfg, channel, kind)}
}

func (d *familyDriver) Close() {
	if impl := d.current(); impl != nil {
		impl.Close()
	}
}

func (d *familyDriver) SearchRecordings(ctx context.Context, channel int, from, to time.Time) ([]provider.Recording, error) {
	if s, ok := d.current().(provider.RecordingSearcher); ok {
		return s.SearchRecordings(ctx, channel, from, to)
	}
	return nil, provider.NewError(provider.KindUnsupported, nil, "nicht verbunden")
}

func (d *familyDriver) HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error) {
	if p, ok := d.current().(provider.RecordingProber); ok {
		return p.HasRecordings(ctx, channel, from, to)
	}
	return false, provider.NewError(provider.KindUnsupported, nil, "nicht verbunden")
}

func (d *familyDriver) OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	if p, ok := d.current().(provider.PlaybackOpener); ok {
		return p.OpenPlayback(ctx, channel, from, to)
	}
	return nil, provider.NewError(provider.KindUnsupported, nil, "nicht verbunden")
}

// Test detects like Connect and reports what was found.
func (d *familyDriver) Test(ctx context.Context) (*provider.TestResult, error) {
	info, err := d.Connect(ctx)
	if err != nil {
		return nil, err
	}
	res := &provider.TestResult{Info: info, Message: fmt.Sprintf("Erkannt: %s · %d Kanäle", info.Summary, info.Channels)}
	if n, ok := d.current().(*nativeDriver); ok {
		if nerr := n.native(ctx, func(c *nativeConn) error { info.Serial = c.serial; return nil }); nerr != nil {
			res.Message += " · Aufnahmen (nativer Port): " + provider.Redact(nerr.Error())
		}
	}
	d.Close()
	return res, nil
}
