package uniview

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

func TestUniviewURLsAgainstSimulator(t *testing.T) {
	clip := sim.Synthetic(20)
	rs := &sim.RTSPServer{User: "admin", Pass: "unv-pass-1", Route: sim.UniviewRoute, Loc: time.UTC,
		Clip: func(ch, sub int) *sim.Clip {
			if ch > 8 {
				return nil // an 8 channel recorder: the channel count is detected
			}
			return clip
		},
		HasRecording: func(ch int, st time.Time) bool { return true }}
	if err := rs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	h, ps, _ := net.SplitHostPort(rs.Addr())
	p, _ := strconv.Atoi(ps)
	m, _ := prov{}.Info().FindModel(ProtoUniview)
	e := &env{}
	d, _ := prov{}.NewDriver(provider.DeviceConfig{Model: m, Host: h, Username: "admin", Password: "unv-pass-1", Ports: map[string]int{"rtsp": p}}, e)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := d.Connect(ctx)
	if err != nil || info.Channels != 8 {
		t.Fatalf("%+v %v", info, err)
	}
	if src := d.LiveSources(2, provider.Sub)[0]; !strings.Contains(src, "/unicast/c3/s1/live") {
		t.Fatal(src)
	}
	from := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	pb, _ := d.(provider.PlaybackOpener).OpenPlayback(ctx, 0, from, from.Add(time.Minute))
	path := pb.URL[strings.Index(pb.URL, "/c1/"):]
	if path != "/c1/b"+strconv.FormatInt(from.Unix(), 10)+"/e"+strconv.FormatInt(from.Add(time.Minute).Unix(), 10)+"/replay/" {
		t.Fatal(path)
	}
	if err := provider.RTSPProbe(ctx, h, p, "admin", "unv-pass-1", path); err != nil {
		t.Fatalf("replay DESCRIBE: %v", err)
	}
	// wrong password: one attempt, then locked
	d2, _ := prov{}.NewDriver(provider.DeviceConfig{Model: m, Host: h, Username: "admin", Password: "x", Ports: map[string]int{"rtsp": p}}, e)
	if _, err := d2.Connect(ctx); provider.KindOf(err) != provider.KindAuth || !e.locked.Load() {
		t.Fatalf("%v", err)
	}
}

// Without a recording list the availability is asked per range with a
// DESCRIBE of the replay URL.
func TestUniviewHasRecordings(t *testing.T) {
	clip := sim.Synthetic(20)
	rs := &sim.RTSPServer{User: "admin", Pass: "unv-pass-1", Route: sim.UniviewRoute, Loc: time.UTC,
		Clip:         func(ch, sub int) *sim.Clip { return clip },
		HasRecording: func(ch int, st time.Time) bool { return ch == 1 }}
	if err := rs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	h, ps, _ := net.SplitHostPort(rs.Addr())
	p, _ := strconv.Atoi(ps)
	m, _ := prov{}.Info().FindModel(ProtoUniview)
	d, _ := prov{}.NewDriver(provider.DeviceConfig{Model: m, Host: h, Username: "admin", Password: "unv-pass-1", Ports: map[string]int{"rtsp": p}, Channels: 2}, &env{})
	pr := d.(provider.RecordingProber)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	to := time.Now().Add(-10 * time.Minute)
	if has, err := pr.HasRecordings(ctx, 0, to.Add(-5*time.Minute), to); err != nil || !has {
		t.Fatalf("channel 1: %v %v", has, err)
	}
	if has, err := pr.HasRecordings(ctx, 1, to.Add(-5*time.Minute), to); err != nil || has {
		t.Fatalf("channel 2: %v %v", has, err)
	}
	e := &env{}
	bad, _ := prov{}.NewDriver(provider.DeviceConfig{Model: m, Host: h, Username: "admin", Password: "x", Ports: map[string]int{"rtsp": p}, Channels: 2}, e)
	if _, err := bad.(provider.RecordingProber).HasRecordings(ctx, 0, to.Add(-5*time.Minute), to); provider.KindOf(err) != provider.KindAuth || !e.locked.Load() {
		t.Fatalf("wrong password: %v", err)
	}
}
