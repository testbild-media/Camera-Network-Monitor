package dahua

import (
	"context"
	"sync"
	"time"

	"cnm/internal/provider"
)

// nativeDriver handles devices without RPC2: live over RTSP, control health via an
// authenticated RTSP DESCRIBE (a real credential check, not a port check) and
// recordings over the native protocol (see native.go), connected on demand.
type nativeDriver struct {
	cfg provider.DeviceConfig
	env provider.Env

	mu sync.Mutex
	nc *nativeConn
	// the native protocol reports no device time: the server time zone is used
	clock provider.Clock
}

func newNativeDriver(cfg provider.DeviceConfig, env provider.Env) *nativeDriver {
	return &nativeDriver{cfg: cfg, env: env}
}

const nativeProbePath = "/cam/realmonitor?channel=1&subtype=1"

func (d *nativeDriver) probe(ctx context.Context) error {
	if !d.env.AllowLogin() {
		return provider.ErrLoginBlocked
	}
	err := provider.RTSPProbe(ctx, d.cfg.Host, d.cfg.Port("rtsp"), d.cfg.Username, d.cfg.Password, nativeProbePath)
	if err != nil && provider.KindOf(err) == provider.KindAuth {
		d.env.ReportAuthFailure(err)
	}
	return err
}

func (d *nativeDriver) Connect(ctx context.Context) (*provider.DeviceInfo, error) {
	if err := d.probe(ctx); err != nil {
		return nil, err
	}
	info := &provider.DeviceInfo{} // channels: counted by the family driver
	d.mu.Lock()
	if d.nc != nil {
		info.Serial = d.nc.serial
	}
	d.mu.Unlock()
	return info, nil
}

func (d *nativeDriver) Check(ctx context.Context) (*provider.Health, error) {
	if err := d.probe(ctx); err != nil {
		return nil, err
	}
	return &provider.Health{Storage: provider.StorageUnknown}, nil
}

func (d *nativeDriver) LiveSources(channel int, kind provider.StreamKind) []string {
	return []string{liveURL(d.cfg, channel, kind)}
}

func (d *nativeDriver) Close() {
	d.mu.Lock()
	nc := d.nc
	d.nc = nil
	d.mu.Unlock()
	if nc != nil {
		go nc.close()
	}
}

// native runs fn on the (lazily opened) native connection. A broken
// connection is dropped and the call retried once on a fresh one. Native
// login failures do not lock the device: live video uses RTSP and keeps
// working; the error is reported to the recording request only.
func (d *nativeDriver) native(ctx context.Context, fn func(c *nativeConn) error) error {
	for attempt := 0; ; attempt++ {
		if !d.env.AllowLogin() {
			return provider.ErrLoginBlocked
		}
		d.mu.Lock()
		c := d.nc
		d.mu.Unlock()
		fresh := false
		if c == nil {
			var err error
			c, err = dialNative(ctx, d.cfg.Host, d.cfg.Port("native"), d.cfg.Username, d.cfg.Password)
			if err != nil {
				return err
			}
			fresh = true
			d.mu.Lock()
			if d.nc != nil {
				d.nc.close()
			}
			d.nc = c
			d.mu.Unlock()
		}
		err := fn(c)
		if err == nil || provider.KindOf(err) == provider.KindNotFound {
			return err
		}
		// drop the connection after transport/protocol errors
		d.mu.Lock()
		if d.nc == c {
			d.nc = nil
		}
		d.mu.Unlock()
		c.close()
		if fresh || attempt > 0 || provider.KindOf(err) != provider.KindNetwork {
			return err
		}
	}
}

// SearchRecordings lists recording clusters by repeated lookups: each A5/B6
// lookup reports one cluster of the window, the next lookup starts after it.
func (d *nativeDriver) SearchRecordings(ctx context.Context, channel int, from, to time.Time) ([]provider.Recording, error) {
	devFrom, devTo := d.clock.ToDevice(from), d.clock.ToDevice(to)
	var recs []provider.Recording
	err := d.native(ctx, func(c *nativeConn) error {
		recs = recs[:0]
		c.mu.Lock()
		defer c.mu.Unlock()
		cur := devFrom
		for i := 0; i < 200 && cur.Before(devTo) && ctx.Err() == nil; i++ {
			cl, err := c.lookup(channel, cur, devTo)
			if err != nil {
				return err
			}
			if cl.number == 0 {
				break
			}
			recs = append(recs, provider.Recording{Start: d.clock.FromDevice(cl.start), End: d.clock.FromDevice(cl.end)})
			if !cl.end.After(cur) {
				break // no progress
			}
			cur = cl.end.Add(time.Second)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return provider.MergeRecordings(recs, from, to, 2*time.Second), nil
}

// HasRecordings performs a single lookup.
func (d *nativeDriver) HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error) {
	has := false
	err := d.native(ctx, func(c *nativeConn) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		cl, err := c.lookup(channel, d.clock.ToDevice(from), d.clock.ToDevice(to))
		has = cl.number != 0
		return err
	})
	return has, err
}

// OpenPlayback starts the native time based download and returns DHAV.
func (d *nativeDriver) OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	var pb *nativePlayback
	err := d.native(ctx, func(c *nativeConn) error {
		var err error
		pb, err = c.openPlayback(ctx, channel, d.clock.ToDevice(from), d.clock.ToDevice(to), nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &provider.PlaybackSource{Reader: pb, Format: "dhav"}, nil
}
