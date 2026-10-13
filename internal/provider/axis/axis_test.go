package axis

import (
	"context"
	"io"
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

func setup(t *testing.T, pass string) (*sim.AxisServer, *sim.RTSPServer, *driver, *env) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	ax := &sim.AxisServer{User: "root", Pass: "axis-pass-1", Sources: 2, Recordings: []sim.AxisRecording{
		{ID: "20261009_A", Source: 1, Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour)},
		{ID: "20261009_B", Source: 1, Start: now.Add(-90 * time.Minute), End: now.Add(time.Hour)}, // still recording
		{ID: "20261009_C", Source: 2, Start: now.Add(-time.Hour), End: now.Add(-30 * time.Minute)},
	}}
	if err := ax.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ax.Close)
	clip := sim.Synthetic(20)
	rs := &sim.RTSPServer{User: "root", Pass: "axis-pass-1", Route: ax.Route, ONVIFReplay: ax.Replay, ReplayNoRequire: true,
		Clip: func(int, int) *sim.Clip { return clip }}
	if err := rs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rs.Close)
	h, hp := hostPort(ax.Addr())
	_, rp := hostPort(rs.Addr())
	m, _ := prov{}.Info().FindModel(ProtoAxis)
	e := &env{}
	d, _ := prov{}.NewDriver(provider.DeviceConfig{Model: m, Host: h, Username: "root", Password: pass, Ports: map[string]int{"http": hp, "rtsp": rp}}, e)
	t.Cleanup(d.Close)
	return ax, rs, d.(*driver), e
}

func TestVAPIXDeviceRecordingsAndReplay(t *testing.T) {
	_, rs, d, _ := setup(t, "axis-pass-1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := d.Connect(ctx)
	if err != nil || info.Serial != "ACCC8E000001" || !strings.Contains(info.Model, "AXIS SIM") || info.Channels != 2 {
		t.Fatalf("info %+v %v", info, err)
	}
	h, err := d.Check(ctx)
	if err != nil || h.Storage != provider.StoragePresent || h.ClockOffset == nil {
		t.Fatalf("health %+v %v", h, err)
	}
	now := time.Now()
	recs, err := d.SearchRecordings(ctx, 0, now.Add(-4*time.Hour), now)
	if err != nil || len(recs) != 2 {
		t.Fatalf("source 1: %+v %v", recs, err)
	}
	if recs, _ := d.SearchRecordings(ctx, 1, now.Add(-4*time.Hour), now); len(recs) != 1 {
		t.Fatalf("source 2: %+v", recs)
	}
	if src := d.LiveSources(1, provider.Sub)[0]; !strings.Contains(src, "camera=2") || !strings.Contains(src, "resolution=640x360") {
		t.Fatal(src)
	}
	// 3 s of the ongoing recording, replayed with "Range: clock="
	from := now.Add(-10 * time.Minute).Truncate(time.Second) // whole seconds: "Range: clock" has no fractions
	src, err := d.OpenPlayback(ctx, 0, from, from.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := io.ReadAll(src.Reader)
	src.Reader.Close()
	if err != nil || src.Format != "mpegts" {
		t.Fatalf("%v %s", err, src.Format)
	}
	frames := 0
	for i := 0; i+188 <= len(ts); i += 188 {
		p := ts[i : i+188]
		if p[0] == 0x47 && int(p[1]&0x1F)<<8|int(p[2]) == 0x0100 && p[1]&0x40 != 0 {
			frames++
		}
	}
	if frames < 25 || frames > 35 { // 3 s at 10 fps
		t.Fatalf("%d frames for 3 s", frames)
	}
	// nothing recorded on source 2 at that time
	if _, err := d.OpenPlayback(ctx, 1, from, from.Add(time.Minute)); provider.KindOf(err) != provider.KindNotFound {
		t.Fatalf("want not found, got %v", err)
	}
	for i := 0; i < 100 && rs.Sessions.Load() > 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if rs.Sessions.Load() != 0 {
		t.Fatal("replay session not closed")
	}
}

func TestVAPIXWrongPasswordLocks(t *testing.T) {
	ax, _, d, e := setup(t, "wrong")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); provider.KindOf(err) != provider.KindAuth || !e.locked.Load() {
		t.Fatalf("%v", err)
	}
	fails := ax.AuthFails.Load()
	if _, err := d.Connect(ctx); err != provider.ErrLoginBlocked || ax.AuthFails.Load() != fails {
		t.Fatal("login retried after an authentication failure")
	}
}
