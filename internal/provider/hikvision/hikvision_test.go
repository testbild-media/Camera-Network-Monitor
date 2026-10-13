package hikvision

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cnm/internal/provider"
	"cnm/internal/sim"
)

type env struct{ locked atomic.Bool }

func (e *env) AllowLogin() bool                              { return !e.locked.Load() }
func (e *env) ReportAuthFailure(error)                       { e.locked.Store(true) }
func (e *env) LegacyLiveURL(int, provider.StreamKind) string { return "" }

func hostPort(a string) (string, int) {
	h, p, _ := net.SplitHostPort(a)
	n, _ := strconv.Atoi(p)
	return h, n
}

type rig struct {
	isapi *sim.HikvisionServer
	rtsp  *sim.RTSPServer
	drv   *driver
	env   *env
}

func setup(t *testing.T, pass string, trackListOnly bool) *rig {
	t.Helper()
	hs := &sim.HikvisionServer{User: "admin", Pass: "hik-pass-1", Channels: 4, HasDisk: true, TrackListOnly: trackListOnly}
	if err := hs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hs.Close)
	clip := sim.Synthetic(20)
	rs := &sim.RTSPServer{User: "admin", Pass: "hik-pass-1", Clip: func(int, int) *sim.Clip { return clip }, Route: sim.HikvisionRoute, Loc: time.UTC,
		HasRecording: func(ch int, st time.Time) bool { return st.Minute()%15 < 10 }}
	if err := rs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rs.Close)
	h, hp := hostPort(hs.Addr())
	_, rp := hostPort(rs.Addr())
	m, _ := prov{}.Info().FindModel(ProtoHikvision)
	e := &env{}
	d, err := prov{}.NewDriver(provider.DeviceConfig{Model: m, Host: h, Username: "admin", Password: pass, Ports: map[string]int{"http": hp, "rtsp": rp}, Channels: 4}, e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return &rig{isapi: hs, rtsp: rs, drv: d.(*driver), env: e}
}

func TestISAPIDeviceAndRecordings(t *testing.T) {
	for _, trackListOnly := range []bool{false, true} {
		r := setup(t, "hik-pass-1", trackListOnly)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		info, err := r.drv.Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.Serial != "HIKSIM0001" || info.ChannelNames[2] != "HIK CAM 3" || info.Channels != 4 {
			t.Fatalf("info %+v", info)
		}
		h, err := r.drv.Check(ctx)
		if err != nil || h.Storage != provider.StoragePresent || h.ClockOffset == nil {
			t.Fatalf("health %+v %v", h, err)
		}
		now := time.Now()
		recs, err := r.drv.SearchRecordings(ctx, 1, now.Add(-3*time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		// 10 of every 15 minutes, more than one page of 40 results
		if len(recs) < 11 || recs[0].End.Sub(recs[0].Start) > 10*time.Minute {
			t.Fatalf("trackListOnly=%v: %d ranges %+v", trackListOnly, len(recs), recs[:min(2, len(recs))])
		}
		if r.drv.trackList.Load() != trackListOnly {
			t.Fatalf("trackList detection = %v", r.drv.trackList.Load())
		}
		if has, err := r.drv.HasRecordings(ctx, 1, now.Add(-time.Hour), now); !has || err != nil {
			t.Fatalf("has %v %v", has, err)
		}
		// live and playback URLs reach the RTSP server (DESCRIBE with digest)
		live := r.drv.LiveSources(1, provider.Sub)[0]
		if !strings.Contains(live, "/Streaming/Channels/202") {
			t.Fatalf("live %s", live)
		}
		_, rp := hostPort(r.rtsp.Addr())
		if err := provider.RTSPProbe(ctx, "127.0.0.1", rp, "admin", "hik-pass-1", "/Streaming/Channels/202"); err != nil {
			t.Fatal(err)
		}
		st := recs[0].Start.Add(time.Minute)
		src, err := r.drv.OpenPlayback(ctx, 1, st, st.Add(time.Minute))
		if err != nil || !strings.Contains(src.URL, "/Streaming/tracks/201?starttime="+r.drv.clock.ToDevice(st).Format("20060102T150405Z")) {
			t.Fatalf("playback %+v %v", src, err)
		}
		path := src.URL[strings.Index(src.URL, "/Streaming"):]
		if err := provider.RTSPProbe(ctx, "127.0.0.1", rp, "admin", "hik-pass-1", path); err != nil {
			t.Fatalf("playback DESCRIBE: %v", err)
		}
		cancel()
	}
}

func TestISAPIWrongPasswordLocks(t *testing.T) {
	r := setup(t, "wrong", false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := r.drv.Connect(ctx)
	if provider.KindOf(err) != provider.KindAuth || !r.env.locked.Load() {
		t.Fatalf("err %v locked %v", err, r.env.locked.Load())
	}
	fails := r.isapi.AuthFails.Load()
	if _, err := r.drv.Connect(ctx); err != provider.ErrLoginBlocked || r.isapi.AuthFails.Load() != fails {
		t.Fatal("login retried after an authentication failure")
	}
}
