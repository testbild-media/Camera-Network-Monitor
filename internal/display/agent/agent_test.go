package agent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"cnm/internal/display"
)

// ---- fake compositor -------------------------------------------------------------

type fakeView struct {
	id     int64
	pid    int
	title  string
	output string
	fs     bool
}

type fakeSway struct {
	outs    []swayOutput
	views   []*fakeView
	cmds    []string
	focused string
	nextID  int64
}

var (
	reOutEnable = regexp.MustCompile(`^output "([^"]+)" enable(?: mode (\d+)x(\d+)@(\d+)\.(\d{3})Hz)? transform (\S+) scale 1 pos (\d+) 0$`)
	reOutOff    = regexp.MustCompile(`^output "([^"]+)" disable$`)
	reCon       = regexp.MustCompile(`^\[con_id=(\d+)\] (.+)$`)
	reFocusOut  = regexp.MustCompile(`^focus output "([^"]+)"$`)
)

func (f *fakeSway) out(name string) *swayOutput {
	for i := range f.outs {
		if f.outs[i].Name == name {
			return &f.outs[i]
		}
	}
	return nil
}

func (f *fakeSway) Outputs() ([]swayOutput, error) {
	return append([]swayOutput(nil), f.outs...), nil
}

func (f *fakeSway) Tree() (*swayNode, error) {
	root := &swayNode{Type: "root"}
	for _, o := range f.outs {
		ws := swayNode{Type: "workspace", Name: "1"}
		for _, v := range f.views {
			if v.output == o.Name {
				fm := 0
				if v.fs {
					fm = 1
				}
				ws.Nodes = append(ws.Nodes, swayNode{ID: v.id, Type: "con", PID: v.pid, Name: v.title, FullscreenMode: fm})
			}
		}
		root.Nodes = append(root.Nodes, swayNode{Type: "output", Name: o.Name, Nodes: []swayNode{ws}})
	}
	return root, nil
}

func (f *fakeSway) Command(cmd string) error {
	f.cmds = append(f.cmds, cmd)
	if m := reOutEnable.FindStringSubmatch(cmd); m != nil {
		o := f.out(m[1])
		if o == nil {
			return fmt.Errorf("unknown output %s", m[1])
		}
		o.Active = true
		if m[2] != "" {
			w, _ := strconv.Atoi(m[2])
			h, _ := strconv.Atoi(m[3])
			hz, _ := strconv.Atoi(m[4])
			fr, _ := strconv.Atoi(m[5])
			o.CurrentMode = &swayMode{Width: w, Height: h, Refresh: hz*1000 + fr}
		}
		o.Transform = m[6]
		o.Rect.X, _ = strconv.Atoi(m[7])
		return nil
	}
	if m := reOutOff.FindStringSubmatch(cmd); m != nil {
		if o := f.out(m[1]); o != nil {
			o.Active = false
		}
		return nil
	}
	if m := reFocusOut.FindStringSubmatch(cmd); m != nil {
		f.focused = m[1]
		return nil
	}
	if m := reCon.FindStringSubmatch(cmd); m != nil {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		for _, v := range f.views {
			if v.id != id {
				continue
			}
			switch {
			case strings.HasPrefix(m[2], "move container to output "):
				v.output = strings.Trim(strings.TrimPrefix(m[2], "move container to output "), `"`)
			case m[2] == "fullscreen enable":
				v.fs = true
			case m[2] == "fullscreen disable":
				v.fs = false
			}
		}
		return nil
	}
	return nil
}

// ---- fake browsers --------------------------------------------------------------

type fakeProc struct {
	pid     int
	done    chan struct{}
	stopped bool
	spec    launchSpec
}

func (p *fakeProc) PID() int              { return p.pid }
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Err() string           { return "boom" }
func (p *fakeProc) Stop() {
	if !p.stopped {
		p.stopped = true
		close(p.done)
	}
}
func (p *fakeProc) crash() { p.Stop() }

type fakeLauncher struct {
	sway *fakeSway
	pid  int
	all  []*fakeProc
	// windowOn overrides the output a new window opens on ("" = focused)
	windowOn string
}

func (l *fakeLauncher) Launch(spec launchSpec) (proc, error) {
	l.pid++
	p := &fakeProc{pid: 1000 + l.pid, done: make(chan struct{}), spec: spec}
	l.all = append(l.all, p)
	out := l.sway.focused
	if l.windowOn != "" {
		out = l.windowOn
	}
	l.sway.nextID++
	l.sway.views = append(l.sway.views, &fakeView{id: l.sway.nextID, pid: p.pid, output: out, title: "Neuer Tab"})
	return p, nil
}

func (l *fakeLauncher) last() *fakeProc { return l.all[len(l.all)-1] }

func newRig() (*reconciler, *fakeSway, *fakeLauncher, *time.Time) {
	fs := &fakeSway{outs: []swayOutput{
		{Name: "HDMI-A-1", Active: true, Transform: "normal", CurrentMode: &swayMode{1920, 1080, 60000}, Rect: swayRect{Width: 1920, Height: 1080},
			Modes: []swayMode{{3840, 2160, 30000}, {1920, 1080, 60000}, {1920, 1080, 59940}, {1920, 1080, 50000}, {1280, 720, 60000}}},
		{Name: "DP-1", Active: true, Transform: "normal", CurrentMode: &swayMode{2560, 1440, 59951}, Rect: swayRect{X: 1920, Width: 2560, Height: 1440},
			Modes: []swayMode{{2560, 1440, 59951}, {1920, 1080, 60000}}},
	}}
	fl := &fakeLauncher{sway: fs}
	clock := time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local)
	r := newReconciler(fs, fl, display.BrowserInfo{Kind: "chromium", Path: "/usr/bin/chromium"}, nil, "/state", nil)
	r.now = func() time.Time { return clock }
	return r, fs, fl, &clock
}

func dstate(r *reconciler, id string) *dispState { return r.disp[id] }

func TestReconcileOutputsModesAndPlacement(t *testing.T) {
	r, fs, fl, _ := newRig()
	fl.windowOn = "DP-1" // the window opens on the wrong output first
	d := &display.Desired{Rev: 1, Outputs: []display.DesiredOutput{
		{DisplayID: "d1", Connector: "HDMI-A-1", Enabled: true, Mode: "1920x1080@59.940", URL: "http://127.0.0.1:8090/viewer?mon=m1&key=k&display=HDMI-A-1"},
	}}
	if err := r.step(d); err != nil {
		t.Fatal(err)
	}
	hdmi, dp := fs.out("HDMI-A-1"), fs.out("DP-1")
	if dp.Active {
		t.Fatal("unassigned output must be switched off")
	}
	if hdmi.CurrentMode.Refresh != 59940 || hdmi.CurrentMode.Width != 1920 {
		t.Fatalf("exact mode not applied: %+v", hdmi.CurrentMode)
	}
	if st := dstate(r, "d1"); st.state != display.StateStarting || len(fl.all) != 1 {
		t.Fatalf("browser not started: %+v", st)
	}
	if !strings.Contains(fl.last().spec.ProfileDir, filepath.Join("profiles", "d1")) {
		t.Fatalf("profile dir: %s", fl.last().spec.ProfileDir)
	}
	// next pass: window found by pid on DP-1 → moved and fullscreened
	_ = r.step(d)
	v := fs.views[0]
	if v.output != "HDMI-A-1" || !v.fs {
		t.Fatalf("window not placed: %+v (cmds %v)", v, fs.cmds)
	}
	_ = r.step(d)
	if st := dstate(r, "d1"); st.state != display.StateRunning || !st.placed {
		t.Fatalf("not running: %+v", st)
	}
	// stable state: no further commands
	n := len(fs.cmds)
	_ = r.step(d)
	if len(fs.cmds) != n {
		t.Fatalf("idempotency violated: %v", fs.cmds[n:])
	}
	_, ds := r.status()
	if ds[0].Mode != "1920x1080@59.940" || ds[0].State != display.StateRunning {
		t.Fatalf("status: %+v", ds)
	}
}

func TestPickModeFallbacks(t *testing.T) {
	o := &swayOutput{Modes: []swayMode{{1920, 1080, 60000}, {1920, 1080, 59940}, {1280, 720, 60000}, {3840, 2160, 30000}}}
	cases := []struct {
		mode, pref, want string
		note             bool
	}{
		{"1920x1080@60.000", "", "1920x1080@60.000", false},
		{"1920x1080@59.940", "", "1920x1080@59.940", false},
		{"1920x1080", "", "1920x1080@60.000", false}, // no refresh → highest
		{"", "1280x720@60.000", "1280x720@60.000", false},
		{"2560x1440@60.000", "1920x1080@60.000", "1920x1080@60.000", true},
		{"", "", "3840x2160@30.000", false}, // largest
	}
	for _, c := range cases {
		m, note, custom := pickMode(o, display.DesiredOutput{Mode: c.mode, Preferred: c.pref})
		if m.String() != c.want || (note != "") != c.note || custom {
			t.Fatalf("%q/%q → %s (%q), want %s", c.mode, c.pref, m, note, c.want)
		}
	}
	// virtual outputs without a mode list take the configured mode as custom mode
	virt := &swayOutput{CurrentMode: &swayMode{1920, 1080, 60000}}
	if m, _, custom := pickMode(virt, display.DesiredOutput{Mode: "1280x720"}); !custom || m.String() != "1280x720" {
		t.Fatalf("custom mode: %s %v", m, custom)
	}
	if m, _, custom := pickMode(virt, display.DesiredOutput{}); custom || m.String() != "1920x1080@60.000" {
		t.Fatalf("virtual output without wish keeps its mode: %s", m)
	}
	if !sameMode(display.Mode{Width: 1280, Height: 720, RefreshMHz: 60000}, display.Mode{Width: 1280, Height: 720}) {
		t.Fatal("refresh wildcard")
	}
}

func TestRotationAndPositions(t *testing.T) {
	r, fs, _, _ := newRig()
	d := &display.Desired{Outputs: []display.DesiredOutput{
		{DisplayID: "a", Connector: "DP-1", Enabled: true, Rotation: 90, URL: "u-a"},
		{DisplayID: "b", Connector: "HDMI-A-1", Enabled: true, URL: "u-b"},
	}}
	_ = r.step(d)
	dp, hdmi := fs.out("DP-1"), fs.out("HDMI-A-1")
	// natural order: DP-1 before HDMI-A-1; DP-1 rotated → logical width 1440
	if dp.Transform != "90" || dp.Rect.X != 0 || hdmi.Rect.X != 1440 {
		t.Fatalf("positions/rotation: dp=%+v hdmi=%+v", dp.Rect, hdmi.Rect)
	}
	if dp.CurrentMode.Width != 2560 { // no mode configured → largest
		t.Fatalf("dp mode %+v", dp.CurrentMode)
	}
}

func TestBrowserCrashBackoffAndRestarts(t *testing.T) {
	r, _, fl, clock := newRig()
	d := &display.Desired{Outputs: []display.DesiredOutput{{DisplayID: "d1", Connector: "HDMI-A-1", Enabled: true, URL: "u1"}}}
	_ = r.step(d)
	_ = r.step(d)
	fl.last().crash()
	_ = r.step(d)
	st := dstate(r, "d1")
	if st.state != display.StateRestarting || st.restarts != 1 || len(fl.all) != 1 {
		t.Fatalf("crash not handled: %+v", st)
	}
	if !strings.Contains(st.err, "boom") {
		t.Fatalf("error text: %q", st.err)
	}
	_ = r.step(d) // still in backoff
	if len(fl.all) != 1 {
		t.Fatal("restarted before backoff elapsed")
	}
	*clock = clock.Add(3 * time.Second)
	_ = r.step(d)
	if len(fl.all) != 2 || dstate(r, "d1").state != display.StateStarting {
		t.Fatalf("not restarted after backoff: %d", len(fl.all))
	}
	// second crash right away → longer backoff
	fl.last().crash()
	_ = r.step(d)
	if got := st.nextStart.Sub(*clock); got != 4*time.Second {
		t.Fatalf("backoff %v", got)
	}
}

func TestAssignmentChangeReloadAndViewerWatchdog(t *testing.T) {
	r, _, fl, clock := newRig()
	out := display.DesiredOutput{DisplayID: "d1", Connector: "HDMI-A-1", Enabled: true, URL: "u1"}
	d := &display.Desired{Outputs: []display.DesiredOutput{out}}
	_ = r.step(d)
	_ = r.step(d)
	_ = r.step(d)
	first := fl.last()
	// new multiviewer → immediate restart with the new URL
	out.URL = "u2"
	d = &display.Desired{Outputs: []display.DesiredOutput{out}}
	_ = r.step(d)
	if !first.stopped || fl.last().spec.URL != "u2" {
		t.Fatal("assignment change did not restart the browser")
	}
	// manual reload
	out.ReloadSeq = 1
	d = &display.Desired{Outputs: []display.DesiredOutput{out}}
	_ = r.step(d)
	if len(fl.all) != 3 {
		t.Fatalf("reload ignored: %d", len(fl.all))
	}
	_ = r.step(d)
	_ = r.step(d)
	// viewer disconnected from CNM: tolerated during the grace time …
	out.ViewerDownMs = 120000
	d = &display.Desired{Outputs: []display.DesiredOutput{out}}
	_ = r.step(d)
	if len(fl.all) != 3 {
		t.Fatal("watchdog fired during grace time")
	}
	// … then the browser is restarted
	*clock = clock.Add(2 * time.Minute)
	_ = r.step(d)
	st := dstate(r, "d1")
	if st.state != display.StateRestarting || !strings.Contains(st.err, "nicht mit CNM verbunden") {
		t.Fatalf("watchdog: %+v", st)
	}
}

func TestOutputGoneAndDisabled(t *testing.T) {
	r, fs, fl, _ := newRig()
	out := display.DesiredOutput{DisplayID: "d1", Connector: "HDMI-A-1", Enabled: true, URL: "u1"}
	d := &display.Desired{Outputs: []display.DesiredOutput{out}}
	_ = r.step(d)
	p := fl.last()
	// unplugged
	saved := fs.outs
	fs.outs = fs.outs[1:]
	_ = r.step(d)
	if !p.stopped || dstate(r, "d1").state != display.StateNoOutput {
		t.Fatalf("unplug: %+v", dstate(r, "d1"))
	}
	fs.outs = saved
	_ = r.step(d)
	if len(fl.all) != 2 {
		t.Fatal("not restarted after replug")
	}
	// disabled in CNM → browser stopped, output off
	out.Enabled, out.URL = false, ""
	_ = r.step(&display.Desired{Outputs: []display.DesiredOutput{out}})
	if !fl.last().stopped || fs.out("HDMI-A-1").Active || dstate(r, "d1").state != display.StateOff {
		t.Fatal("disable not applied")
	}
	// removed from config → state dropped
	_ = r.step(&display.Desired{})
	if len(r.disp) != 0 {
		t.Fatal("removed display still tracked")
	}
}

func TestWindowNeverAppears(t *testing.T) {
	r, fs, fl, clock := newRig()
	d := &display.Desired{Outputs: []display.DesiredOutput{{DisplayID: "d1", Connector: "HDMI-A-1", Enabled: true, URL: "u1"}}}
	_ = r.step(d)
	fs.views = nil // the window never shows up
	*clock = clock.Add(61 * time.Second)
	_ = r.step(d)
	if !fl.last().stopped || dstate(r, "d1").state != display.StateRestarting {
		t.Fatalf("missing window not handled: %+v", dstate(r, "d1"))
	}
}

func TestDailyRestart(t *testing.T) {
	loc := time.Local
	start := time.Date(2026, 10, 7, 3, 0, 0, 0, loc)
	if dailyDue("04:00", start, time.Date(2026, 10, 7, 3, 59, 0, 0, loc)) {
		t.Fatal("too early")
	}
	if !dailyDue("04:00", start, time.Date(2026, 10, 7, 4, 0, 0, 0, loc)) {
		t.Fatal("not due at 04:00")
	}
	if dailyDue("04:00", time.Date(2026, 10, 7, 4, 0, 1, 0, loc), time.Date(2026, 10, 8, 3, 0, 0, 0, loc)) {
		t.Fatal("due twice")
	}
	if dailyDue("", start, start.Add(48*time.Hour)) {
		t.Fatal("disabled but due")
	}
}

func TestBrowserMissing(t *testing.T) {
	r, _, fl, _ := newRig()
	r.browser = display.BrowserInfo{Error: "Kein Browser gefunden"}
	_ = r.step(&display.Desired{Outputs: []display.DesiredOutput{{DisplayID: "d1", Connector: "HDMI-A-1", Enabled: true, URL: "u1"}}})
	if len(fl.all) != 0 || dstate(r, "d1").state != display.StateError {
		t.Fatal("missing browser not reported")
	}
}

func TestTitleMatchAndNaturalOrder(t *testing.T) {
	if !titleMatches("CNM-DISPLAY HDMI-A-1 – Zentrale", "HDMI-A-1") || titleMatches("CNM-DISPLAY HDMI-A-10", "HDMI-A-1") {
		t.Fatal("title matching")
	}
	if !naturalLess("HDMI-A-2", "HDMI-A-10") || naturalLess("HDMI-A-10", "HDMI-A-2") || !naturalLess("DP-1", "HDMI-A-1") {
		t.Fatal("natural order")
	}
	if backoff(1) != 2*time.Second || backoff(3) != 8*time.Second || backoff(20) != 60*time.Second {
		t.Fatal("backoff")
	}
}

func TestBrowserCommandLine(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "SingletonLock"), nil, 0o600)
	args, env := browserCommand(launchSpec{Browser: display.BrowserInfo{Kind: "chromium"}, URL: "http://x/viewer", ProfileDir: dir, Env: []string{"WAYLAND_DISPLAY=wayland-1"}})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--kiosk", "--ozone-platform=wayland", "--user-data-dir=" + dir, "--lang=de", "http://x/viewer"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "SingletonLock")); err == nil {
		t.Fatal("stale chromium lock not removed")
	}
	if !strings.Contains(strings.Join(env, "\n"), "WAYLAND_DISPLAY=wayland-1") {
		t.Fatal("wayland env missing")
	}
	args, env = browserCommand(launchSpec{Browser: display.BrowserInfo{Kind: "firefox"}, URL: "http://x/viewer", ProfileDir: dir})
	if args[0] != "--kiosk" || !strings.Contains(strings.Join(env, "\n"), "MOZ_ENABLE_WAYLAND=1") {
		t.Fatalf("firefox args %v", args)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "user.js")); !strings.Contains(string(b), "resume_from_crash") || !strings.Contains(string(b), `"browser.translations.enable", false`) {
		t.Fatal("firefox prefs not written (or translations not disabled)")
	}
	if !strings.Contains(strings.Join(env, "\n"), "LANGUAGE=de") {
		t.Fatal("browser language not German")
	}
}

// ---- IPC framing against a fake sway socket ---------------------------------------

func TestIPCProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the display agent runs on Linux; AF_UNIX dialing is unreliable on Windows")
	}
	dir, err := os.MkdirTemp("", "ipc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		for {
			hdr := make([]byte, 14)
			if _, err := io.ReadFull(c, hdr); err != nil {
				return
			}
			n := binary.NativeEndian.Uint32(hdr[6:])
			typ := binary.NativeEndian.Uint32(hdr[10:])
			payload := make([]byte, n)
			_, _ = io.ReadFull(c, payload)
			var reply any
			switch typ {
			case ipcRunCommand:
				if strings.Contains(string(payload), "bogus") {
					reply = []map[string]any{{"success": true}, {"success": false, "error": "Unknown command"}}
				} else {
					reply = []map[string]any{{"success": true}}
				}
			case ipcGetOutputs:
				reply = []map[string]any{{"name": "HDMI-A-1", "active": true, "make": "Sim", "modes": []map[string]int{{"width": 1920, "height": 1080, "refresh": 60000}}, "current_mode": map[string]int{"width": 1920, "height": 1080, "refresh": 60000}}}
			case ipcGetTree:
				reply = map[string]any{"type": "root", "nodes": []any{map[string]any{"type": "output", "name": "HDMI-A-1", "nodes": []any{map[string]any{"type": "workspace", "nodes": []any{map[string]any{"type": "con", "id": 7, "pid": 42, "name": "CNM-DISPLAY HDMI-A-1", "fullscreen_mode": 1}}}}}}}
			case ipcGetVersion:
				reply = map[string]any{"human_readable": "1.9"}
			}
			b, _ := json.Marshal(reply)
			out := make([]byte, 14+len(b))
			copy(out, ipcMagic)
			binary.NativeEndian.PutUint32(out[6:], uint32(len(b)))
			binary.NativeEndian.PutUint32(out[10:], typ)
			copy(out[14:], b)
			_, _ = c.Write(out)
		}
	}()
	c, err := DialIPC(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if v, err := c.Version(); err != nil || v != "1.9" {
		t.Fatalf("version %q %v", v, err)
	}
	if err := c.Command(`output "HDMI-A-1" enable`); err != nil {
		t.Fatal(err)
	}
	if err := c.Command("bogus"); err == nil || !strings.Contains(err.Error(), "Unknown command") {
		t.Fatalf("command error not reported: %v", err)
	}
	outs, err := c.Outputs()
	if err != nil || len(outs) != 1 || outs[0].CurrentMode.Refresh != 60000 || outs[0].Make != "Sim" {
		t.Fatalf("outputs %+v %v", outs, err)
	}
	tree, err := c.Tree()
	if err != nil {
		t.Fatal(err)
	}
	vs := views(tree)
	if len(vs) != 1 || vs[0].Output != "HDMI-A-1" || vs[0].PID != 42 || !vs[0].Fullscreen {
		t.Fatalf("views %+v", vs)
	}
	if quote(`a"b`) != `"a\"b"` {
		t.Fatal("quote")
	}
}

func TestStartPageHidesCredential(t *testing.T) {
	dir := t.TempDir()
	target := "http://127.0.0.1:8090/viewer?mon=m1&key=SECRET_kiosk&display=HDMI-A-1"
	u, err := writeStartPage(dir, target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u, "SECRET") || !strings.HasPrefix(u, "file://") {
		t.Fatalf("start url leaks the credential: %s", u)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "cnm-start.html"))
	if !strings.Contains(string(b), `location.replace("http://127.0.0.1:8090/viewer?mon=m1\u0026key=SECRET_kiosk\u0026display=HDMI-A-1")`) || !strings.Contains(string(b), "url=http://127.0.0.1:8090/viewer?mon=m1&amp;key=SECRET_kiosk") {
		t.Fatalf("start page: %s", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, "cnm-start.html")); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
	}
}

func TestSnapBrowserDetection(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "firefox")
	_ = os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /snap/bin/firefox \"$@\"\n"), 0o755)
	if !isSnap(wrapper) {
		t.Fatal("snap wrapper not detected")
	}
	real := filepath.Join(dir, "chromium")
	_ = os.WriteFile(real, make([]byte, 20<<10), 0o755)
	if isSnap(real) {
		t.Fatal("regular binary flagged as snap")
	}
}

func TestSwayArgsNvidia(t *testing.T) {
	args, env := swayArgs("/run/c.conf", false)
	if strings.Join(args, " ") != "-c /run/c.conf" || len(env) != 0 {
		t.Fatalf("plain: %v %v", args, env)
	}
	args, env = swayArgs("/run/c.conf", true)
	if !strings.Contains(strings.Join(args, " "), "--unsupported-gpu") || env[0] != "WLR_NO_HARDWARE_CURSORS=1" {
		t.Fatalf("nvidia: %v %v", args, env)
	}
}

// "auto" prefers Firefox (installed by the setup) and falls back to
// Chromium/Chrome; an explicit choice never falls back silently.
func TestBrowserPreference(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs executable scripts in PATH")
	}
	dir := t.TempDir()
	fake := func(name string) {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho "+name+" 1.0\n"), 0o755)
	}
	t.Setenv("PATH", dir)
	fake("google-chrome-stable")
	if b := detectBrowser("auto", ""); b.Kind != "chromium" || b.Error != "" {
		t.Fatalf("only Chrome: %+v", b)
	}
	if b := detectBrowser("firefox", ""); b.Error == "" {
		t.Fatalf("explicit Firefox fell back: %+v", b)
	}
	fake("firefox-esr")
	if b := detectBrowser("auto", ""); b.Kind != "firefox" || !strings.HasSuffix(b.Path, "firefox-esr") || b.Version != "firefox-esr 1.0" {
		t.Fatalf("auto with Firefox: %+v", b)
	}
	if b := detectBrowser("chromium", ""); b.Kind != "chromium" {
		t.Fatalf("explicit Chromium: %+v", b)
	}
	if got := strings.Join(installedBrowsers(), ","); got != "firefox,chromium" {
		t.Fatalf("installed %q", got)
	}
}
