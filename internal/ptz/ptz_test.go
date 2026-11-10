package ptz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/device"
	"cnm/internal/provider"
	_ "cnm/internal/provider/reolink"
	"cnm/internal/sim"
)

type env struct{}

func (env) AllowLogin() bool                              { return true }
func (env) ReportAuthFailure(error)                       {}
func (env) LegacyLiveURL(int, provider.StreamKind) string { return "" }

type fakeDevices struct {
	drv     provider.Driver
	st      device.Status
	offline atomic.Bool
}

func (f *fakeDevices) Driver(string) (provider.Driver, device.Status, error) {
	if f.offline.Load() {
		return nil, f.st, errors.New("offline")
	}
	return f.drv, f.st, nil
}
func (f *fakeDevices) Status(string) (device.Status, bool) { return f.st, true }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type rig struct {
	cam *sim.ReolinkServer
	dev *fakeDevices
	cfg *config.Config
	dir string
	clk *clock
}

func setup(t *testing.T) *rig {
	t.Helper()
	cam := &sim.ReolinkServer{User: "admin", Pass: "pw-1234", DualLens: true, PTZ: true, RTSPPort: 554, Loc: time.UTC,
		Clip: func(int, bool) *sim.Clip { return sim.Synthetic(10) }}
	if err := cam.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cam.Close)
	h, p, _ := net.SplitHostPort(cam.Addr())
	port, _ := strconv.Atoi(p)
	prov, model, err := provider.Resolve("reolink")
	if err != nil {
		t.Fatal(err)
	}
	drv, err := prov.NewDriver(provider.DeviceConfig{Model: model, Host: h, Username: "admin", Password: "pw-1234", Ports: map[string]int{"http": port}}, env{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(drv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, err := drv.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Devices: []config.Device{{ID: "d1", Protocol: "reolink", Options: map[string]string{provider.OptPTZReturn: "2"}}},
		Cameras: []config.Camera{{ID: "wide", DeviceID: "d1", Channel: 0, Enabled: true}, {ID: "tele", DeviceID: "d1", Channel: 1, Enabled: true}},
	}
	return &rig{cam: cam, dev: &fakeDevices{drv: drv, st: device.Status{ID: "d1", State: device.Connected, Info: info}}, cfg: cfg,
		dir: t.TempDir(), clk: &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}}
}

func (r *rig) manager() *Manager {
	m := New(r.dev, func() *config.Config { return r.cfg }, r.dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = r.clk.now
	return m
}

var (
	anna  = User{ID: "u1", Name: "anna"}
	ben   = User{ID: "u2", Name: "ben"}
	admin = User{ID: "u3", Name: "chef", Admin: true}
)

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Taking control pauses tracking and postpones the guard return; both
// lenses belong to one control; ending it returns the camera and restores
// the behaviour.
func TestTakeSteerRelease(t *testing.T) {
	r := setup(t)
	m := r.manager()
	ctx := ctxT(t)
	v, e := m.View(ctx, "tele", anna)
	if e != nil || !v.Features.PanTilt || len(v.Presets) != 2 || v.Controller != nil || v.ReturnMinutes != 2 || !v.GuardSet {
		t.Fatalf("view %+v %v", v, e)
	}
	if e := m.Move(ctx, "wide", anna, 1, 0, 0); e == nil || e.Code != CodeNotOwner {
		t.Fatalf("move without control: %v", e)
	}
	c, e := m.Take(ctx, "wide", anna, false)
	if e != nil || !c.Self || !c.ReturnAt.Equal(r.clk.now().Add(2*time.Minute)) {
		t.Fatalf("take %+v %v", c, e)
	}
	if en, _, tmo := r.cam.Guard(); !en || tmo != 120 || r.cam.Tracking() {
		t.Fatalf("paused: guard %v/%d tracking %v", en, tmo, r.cam.Tracking())
	}
	if _, err := os.Stat(filepath.Join(r.dir, FileName)); err != nil {
		t.Fatal("session not stored")
	}
	// the other lens is the same head
	if _, e := m.Take(ctx, "tele", ben, false); e == nil || e.Code != CodeBusy {
		t.Fatalf("second user: %v", e)
	}
	if e := m.Move(ctx, "tele", ben, 1, 0, 0); e == nil || e.Code != CodeBusy {
		t.Fatalf("move of other user: %v", e)
	}
	if e := m.Move(ctx, "tele", anna, 0, 1, 0); e != nil {
		t.Fatal(e)
	}
	if e := m.Move(ctx, "tele", anna, 0, 0, 0); e != nil {
		t.Fatal(e)
	}
	if e := m.GotoPreset(ctx, "wide", anna, 1); e != nil {
		t.Fatal(e)
	}
	if e := m.SaveGuard(ctx, "wide", anna); e == nil || e.Code != CodeForbidden {
		t.Fatalf("save guard as viewer: %v", e)
	}
	v, _ = m.View(ctx, "tele", ben)
	if v.Controller == nil || v.Controller.Self || v.Controller.Username != "anna" || v.Tracking == nil || !*v.Tracking {
		t.Fatalf("view of others %+v", v.Controller)
	}
	if e := m.Release(ctx, "tele", ben); e == nil || e.Code != CodeBusy {
		t.Fatalf("release by other: %v", e)
	}
	if e := m.Release(ctx, "tele", anna); e != nil {
		t.Fatal(e)
	}
	if en, _, tmo := r.cam.Guard(); !en || tmo != 60 || !r.cam.Tracking() || m.Pending() != 0 {
		t.Fatalf("restored: guard %v/%d tracking %v pending %d", en, tmo, r.cam.Tracking(), m.Pending())
	}
	want := []string{"Up:64", "Stop", "ToPos:32:1", "Stop", "GuardToPos"}
	if got := r.cam.PTZOps(); !slices.Equal(got, want) {
		t.Fatalf("ops %v", got)
	}
	if _, err := os.Stat(filepath.Join(r.dir, FileName)); !os.IsNotExist(err) {
		t.Fatal("session file left behind")
	}
}

// Without input the control ends after the return time; a forced takeover
// keeps the behaviour saved first; a move that is not renewed stops.
func TestIdleReturnAndTakeover(t *testing.T) {
	r := setup(t)
	m := r.manager()
	ctx := ctxT(t)
	if _, e := m.Take(ctx, "wide", anna, false); e != nil {
		t.Fatal(e)
	}
	if e := m.Move(ctx, "wide", anna, -1, 0, 0); e != nil {
		t.Fatal(e)
	}
	r.clk.add(10 * time.Second)
	m.Tick()
	m.wg.Wait()
	if got := r.cam.PTZOps(); !slices.Equal(got, []string{"Left:64", "Stop"}) {
		t.Fatalf("move not stopped: %v", got)
	}
	if c, e := m.Take(ctx, "tele", ben, true); e != nil || !c.Self || c.Username != "ben" {
		t.Fatalf("forced takeover %+v %v", c, e)
	}
	r.clk.add(119 * time.Second)
	m.Tick()
	m.wg.Wait()
	if m.Pending() != 1 || r.cam.Tracking() {
		t.Fatal("ended too early")
	}
	r.clk.add(2 * time.Second)
	m.Tick()
	m.wg.Wait()
	if m.Pending() != 0 || !r.cam.Tracking() {
		t.Fatalf("idle control not ended: pending %d tracking %v", m.Pending(), r.cam.Tracking())
	}
	if en, _, tmo := r.cam.Guard(); !en || tmo != 60 {
		t.Fatalf("guard %v/%d", en, tmo)
	}
}

// A restart of CNM during a control restores the camera; while it is
// offline the restore is retried.
func TestRestoreAfterRestartAndOffline(t *testing.T) {
	r := setup(t)
	m := r.manager()
	ctx := ctxT(t)
	if _, e := m.Take(ctx, "wide", anna, false); e != nil {
		t.Fatal(e)
	}
	m.Close() // CNM stops, the session stays stored

	r.dev.offline.Store(true)
	m2 := r.manager()
	if m2.Pending() != 1 {
		t.Fatal("stored session not loaded")
	}
	m2.Tick()
	m2.wg.Wait()
	if m2.Pending() != 1 || r.cam.Tracking() {
		t.Fatal("restore while offline")
	}
	r.dev.offline.Store(false)
	r.clk.add(10 * time.Second)
	m2.Tick() // retry interval not over
	m2.wg.Wait()
	if m2.Pending() != 1 {
		t.Fatal("retried too early")
	}
	r.clk.add(30 * time.Second)
	m2.Tick()
	m2.wg.Wait()
	if m2.Pending() != 0 || !r.cam.Tracking() {
		t.Fatalf("not restored after restart: pending %d", m2.Pending())
	}
	if en, _, tmo := r.cam.Guard(); !en || tmo != 60 {
		t.Fatalf("guard %v/%d", en, tmo)
	}
	if got := r.cam.PTZOps(); !slices.Contains(got, "GuardToPos") {
		t.Fatalf("no return to the guard position: %v", got)
	}
}

// A camera that refuses CNM's return time gets its guard return switched
// off during the control instead; admins may store the guard position and
// end the control of others.
func TestGuardFallbackAndAdmin(t *testing.T) {
	r := setup(t)
	r.cfg.Devices[0].Options[provider.OptPTZReturn] = "90" // 5400 s: refused by the simulated camera
	m := r.manager()
	ctx := ctxT(t)
	if _, e := m.Take(ctx, "wide", admin, false); e != nil {
		t.Fatal(e)
	}
	if en, _, tmo := r.cam.Guard(); en || tmo != 60 || r.cam.Tracking() {
		t.Fatalf("fallback: guard %v/%d", en, tmo)
	}
	if e := m.SaveGuard(ctx, "wide", admin); e != nil {
		t.Fatal(e)
	}
	if e := m.Release(ctx, "wide", admin); e != nil {
		t.Fatal(e)
	}
	if en, _, _ := r.cam.Guard(); !en || !r.cam.Tracking() {
		t.Fatal("not restored")
	}
	if _, e := m.Take(ctx, "wide", anna, false); e != nil {
		t.Fatal(e)
	}
	if e := m.Release(ctx, "wide", admin); e != nil || m.Pending() != 0 {
		t.Fatalf("admin release: %v", e)
	}
}

// The pause runs detached from the browser request: a request that is
// already cancelled still pauses completely.
func TestTakeWithCancelledRequest(t *testing.T) {
	r := setup(t)
	m := r.manager()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := m.Take(ctx, "wide", anna, false); e != nil {
		t.Fatal(e)
	}
	if en, _, tmo := r.cam.Guard(); !en || tmo != 120 || r.cam.Tracking() {
		t.Fatalf("paused: guard %v/%d tracking %v", en, tmo, r.cam.Tracking())
	}
	if e := m.Release(ctxT(t), "wide", anna); e != nil || !r.cam.Tracking() {
		t.Fatalf("release %v", e)
	}
}

// A failed return to the guard position does not keep tracking off.
func TestRestoreWithoutGuardPosition(t *testing.T) {
	r := setup(t)
	m := r.manager()
	ctx := ctxT(t)
	if _, e := m.Take(ctx, "wide", anna, false); e != nil {
		t.Fatal(e)
	}
	r.cam.FailGuardGoto.Store(true)
	if e := m.Release(ctx, "wide", anna); e != nil {
		t.Fatal(e)
	}
	if en, _, tmo := r.cam.Guard(); !en || tmo != 60 || !r.cam.Tracking() || m.Pending() != 0 {
		t.Fatalf("restored: guard %v/%d tracking %v pending %d", en, tmo, r.cam.Tracking(), m.Pending())
	}
}

// A stop that does not reach the camera is repeated by the watchdog.
func TestStopRepeatedAfterOutage(t *testing.T) {
	r := setup(t)
	m := r.manager()
	ctx := ctxT(t)
	if _, e := m.Take(ctx, "wide", anna, false); e != nil {
		t.Fatal(e)
	}
	if e := m.Move(ctx, "wide", anna, 0, 0, 1); e != nil {
		t.Fatal(e)
	}
	r.dev.offline.Store(true)
	if e := m.Move(ctx, "wide", anna, 0, 0, 0); e == nil || e.Code != CodeOffline {
		t.Fatalf("stop while offline: %v", e)
	}
	r.dev.offline.Store(false)
	r.clk.add(9 * time.Second)
	m.Tick()
	m.wg.Wait()
	if got := r.cam.PTZOps(); !slices.Equal(got, []string{"ZoomInc:64", "Stop"}) {
		t.Fatalf("ops %v", got)
	}
	m.Tick()
	m.wg.Wait()
	if got := r.cam.PTZOps(); len(got) != 2 {
		t.Fatalf("stopped twice: %v", got)
	}
}
