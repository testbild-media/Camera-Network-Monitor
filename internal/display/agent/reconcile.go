package agent

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"cnm/internal/display"
)

// Timings of the kiosk supervision.
var (
	placeTimeout   = 60 * time.Second // window must appear within this time
	viewerGrace    = 90 * time.Second // minimum browser runtime before the viewer watchdog acts
	viewerDownMax  = int64(60000)     // ms the viewer may be disconnected from CNM
	stableRuntime  = 5 * time.Minute  // a browser that ran this long resets the backoff
	backoffInitial = 2 * time.Second
	backoffMax     = 60 * time.Second
)

// compositorAPI is the part of the sway IPC the reconciler needs.
type compositorAPI interface {
	Outputs() ([]swayOutput, error)
	Tree() (*swayNode, error)
	Command(cmd string) error
}

// dispState is the runtime state of one configured display.
type dispState struct {
	id, connector string
	p             proc
	url           string
	reloadSeq     int
	startedAt     time.Time
	placed        bool
	state         string
	since         time.Time
	err           string
	restarts      int
	failures      int
	nextStart     time.Time
	mode          string
}

func (s *dispState) set(state, err string, now time.Time) {
	if s.state != state {
		s.since = now
	}
	s.state, s.err = state, err
}

// reconciler drives outputs and kiosk browsers towards the desired state.
// It is used from a single goroutine.
type reconciler struct {
	sway     compositorAPI
	launch   launcher
	browser  display.BrowserInfo
	env      []string // compositor environment for browsers
	stateDir string
	now      func() time.Time
	log      *slog.Logger

	disp    map[string]*dispState
	outputs []swayOutput
	outErr  map[string]string
}

func newReconciler(sway compositorAPI, l launcher, browser display.BrowserInfo, env []string, stateDir string, log *slog.Logger) *reconciler {
	return &reconciler{sway: sway, launch: l, browser: browser, env: env, stateDir: stateDir, now: time.Now, log: log, disp: map[string]*dispState{}, outErr: map[string]string{}}
}

func backoff(failures int) time.Duration {
	d := backoffInitial
	for i := 1; i < failures && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

func transformName(rot int) string {
	switch rot {
	case 90, 180, 270:
		return strconv.Itoa(rot)
	}
	return "normal"
}

// pickMode chooses the exact compositor mode: the configured mode (nearest
// refresh), else the EDID preferred mode, else the largest mode. Outputs
// without a mode list (virtual/headless outputs) get the configured mode as
// a custom mode.
func pickMode(o *swayOutput, want display.DesiredOutput) (mode display.Mode, note string, custom bool) {
	modes := make([]display.Mode, 0, len(o.Modes))
	for _, m := range o.Modes {
		modes = append(modes, m.mode())
	}
	if len(modes) == 0 {
		if w, ok := display.ParseMode(want.Mode); ok && w.Width > 0 {
			return w, "", true
		}
	}
	if w, ok := display.ParseMode(want.Mode); ok && w.Width > 0 {
		if m, ok := display.Best(modes, w); ok {
			return m, "", false
		}
		note = "Auflösung " + w.Label() + " wird vom Display nicht angeboten – bevorzugte Auflösung aktiv"
	}
	if w, ok := display.ParseMode(want.Preferred); ok && w.Width > 0 {
		if m, ok := display.Best(modes, w); ok {
			return m, note, false
		}
	}
	if m, ok := display.Largest(modes); ok {
		return m, note, false
	}
	if o.CurrentMode != nil {
		return o.CurrentMode.mode(), note, false
	}
	return display.Mode{}, note, false
}

// sameMode compares modes; an unspecified refresh matches any refresh.
func sameMode(cur, target display.Mode) bool {
	if cur.Width != target.Width || cur.Height != target.Height {
		return false
	}
	return target.RefreshMHz == 0 || cur.RefreshMHz == target.RefreshMHz
}

func dailyDue(hhmm string, since, now time.Time) bool {
	h, m, ok := strings.Cut(hhmm, ":")
	if !ok {
		return false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil {
		return false
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	if t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return since.Before(t)
}

func (r *reconciler) state(id, connector string) *dispState {
	st := r.disp[id]
	if st == nil {
		st = &dispState{id: id, connector: connector, since: r.now()}
		r.disp[id] = st
	}
	st.connector = connector
	return st
}

func (r *reconciler) stopBrowser(st *dispState) {
	if st.p != nil {
		st.p.Stop()
		st.p = nil
	}
	st.placed = false
}

// stopAll stops every browser (compositor restart, shutdown).
func (r *reconciler) stopAll() {
	for _, st := range r.disp {
		r.stopBrowser(st)
	}
}

func (r *reconciler) fail(st *dispState, msg string, now time.Time) {
	st.failures++
	st.restarts++
	st.nextStart = now.Add(backoff(st.failures))
	st.set(display.StateRestarting, msg, now)
}

// step performs one reconcile pass. An error means the compositor is not
// usable (IPC failed) and must be restarted.
func (r *reconciler) step(d *display.Desired) error {
	if d == nil {
		d = &display.Desired{}
	}
	outs, err := r.sway.Outputs()
	if err != nil {
		return err
	}
	r.outputs = outs
	now := r.now()
	present := map[string]*swayOutput{}
	names := make([]string, 0, len(outs))
	for i := range outs {
		present[outs[i].Name] = &outs[i]
		names = append(names, outs[i].Name)
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	want := map[string]display.DesiredOutput{}
	for _, o := range d.Outputs {
		want[o.Connector] = o
	}

	// 1. outputs: enable with the exact mode, rotation and a non-overlapping
	//    position; everything not assigned is switched off
	r.outErr = map[string]string{}
	modeOf := map[string]string{}
	x := 0
	for _, name := range names {
		o := present[name]
		dout, ok := want[name]
		if !ok || !dout.Enabled {
			if o.Active {
				r.command("output "+quote(name)+" disable", name)
			}
			continue
		}
		target, note, custom := pickMode(o, dout)
		tr := transformName(dout.Rotation)
		w := target.Width
		if dout.Rotation == 90 || dout.Rotation == 270 {
			w = target.Height
		}
		cur := display.Mode{}
		if o.CurrentMode != nil {
			cur = o.CurrentMode.mode()
		}
		if !o.Active || (target.Width > 0 && !sameMode(cur, target)) || o.Transform != tr || o.Rect.X != x || o.Rect.Y != 0 {
			cmd := "output " + quote(name) + " enable"
			switch {
			case custom:
				cmd += " mode --custom " + customModeCommand(target)
			case target.Width > 0:
				cmd += " mode " + modeCommand(target)
			}
			cmd += fmt.Sprintf(" transform %s scale 1 pos %d 0", tr, x)
			r.command(cmd, name)
		}
		if note != "" && r.outErr[name] == "" {
			r.outErr[name] = note
		}
		modeOf[name] = target.String()
		if w == 0 && o.Rect.Width > 0 {
			w = o.Rect.Width
		}
		x += w
	}

	// 2. kiosk browsers
	tree, err := r.sway.Tree()
	if err != nil {
		return err
	}
	vs := views(tree)
	seen := map[string]bool{}
	for _, dout := range d.Outputs {
		seen[dout.DisplayID] = true
		st := r.state(dout.DisplayID, dout.Connector)
		st.mode = modeOf[dout.Connector]
		if !dout.Enabled || dout.URL == "" {
			r.stopBrowser(st)
			st.failures = 0
			st.set(display.StateOff, "", now)
			continue
		}
		if present[dout.Connector] == nil {
			r.stopBrowser(st)
			st.set(display.StateNoOutput, "", now)
			continue
		}
		if r.browser.Path == "" || r.browser.Error != "" {
			st.set(display.StateError, r.browser.Error, now)
			continue
		}
		// browser exited on its own?
		if st.p != nil {
			select {
			case <-st.p.Done():
				msg := "Browser beendet"
				if e := st.p.Err(); e != "" {
					msg += ": " + e
				}
				if now.Sub(st.startedAt) > stableRuntime {
					st.failures = 0
				}
				st.p, st.placed = nil, false
				r.fail(st, msg, now)
				r.logInfo("kiosk browser exited", st)
			default:
			}
		}
		if st.p != nil {
			switch {
			case st.url != dout.URL:
				r.logInfo("display assignment changed – restarting browser", st)
				r.stopBrowser(st)
				st.failures, st.nextStart = 0, time.Time{}
			case st.reloadSeq != dout.ReloadSeq:
				r.logInfo("reload requested", st)
				r.stopBrowser(st)
				st.restarts++
				st.failures, st.nextStart = 0, time.Time{}
			case st.placed && now.Sub(st.startedAt) > viewerGrace && dout.ViewerDownMs > viewerDownMax:
				r.logInfo("viewer not connected to CNM – restarting browser", st)
				r.stopBrowser(st)
				r.fail(st, "Anzeige war nicht mit CNM verbunden – Browser neu gestartet", now)
			case !st.placed && now.Sub(st.startedAt) > placeTimeout:
				r.logInfo("browser window did not appear – restarting", st)
				r.stopBrowser(st)
				r.fail(st, "Browserfenster erschien nicht", now)
			case d.DailyRestart != "" && dailyDue(d.DailyRestart, st.startedAt, now):
				r.logInfo("daily refresh", st)
				r.stopBrowser(st)
				st.restarts++
				st.failures, st.nextStart = 0, time.Time{}
			}
		}
		if st.p == nil {
			if now.Before(st.nextStart) {
				st.set(display.StateRestarting, st.err, now)
				continue
			}
			_ = r.sway.Command("focus output " + quote(dout.Connector))
			p, err := r.launch.Launch(launchSpec{
				Browser: r.browser, URL: dout.URL, Connector: dout.Connector,
				ProfileDir: filepath.Join(r.stateDir, "profiles", dout.DisplayID), Env: r.env,
			})
			if err != nil {
				r.fail(st, "Browserstart fehlgeschlagen: "+err.Error(), now)
				st.set(display.StateError, st.err, now)
				continue
			}
			st.p, st.url, st.reloadSeq, st.startedAt, st.placed = p, dout.URL, dout.ReloadSeq, now, false
			st.set(display.StateStarting, "", now)
			r.logInfo("kiosk browser started", st)
			continue
		}
		// 3. placement: fullscreen on its own output
		v := findView(vs, st.p.PID(), dout.Connector)
		if v == nil {
			continue
		}
		if v.Output == dout.Connector && v.Fullscreen {
			if !st.placed {
				st.placed = true
			}
			st.set(display.StateRunning, r.outErr[dout.Connector], now)
			continue
		}
		con := fmt.Sprintf("[con_id=%d] ", v.ID)
		if v.Output != dout.Connector {
			if v.Fullscreen {
				_ = r.sway.Command(con + "fullscreen disable")
			}
			_ = r.sway.Command(con + "move container to output " + quote(dout.Connector))
			_ = r.sway.Command(con + "focus")
		}
		if err := r.sway.Command(con + "fullscreen enable"); err != nil {
			st.err = err.Error()
		}
	}
	for id, st := range r.disp {
		if !seen[id] {
			r.stopBrowser(st)
			delete(r.disp, id)
		}
	}
	return nil
}

func (r *reconciler) command(cmd, output string) {
	if err := r.sway.Command(cmd); err != nil {
		r.outErr[output] = err.Error()
		if r.log != nil {
			r.log.Warn("compositor command failed", "output", output, "error", err)
		}
	}
}

func (r *reconciler) logInfo(msg string, st *dispState) {
	if r.log != nil {
		r.log.Info(msg, "display", st.connector, "restarts", st.restarts)
	}
}

func findView(vs []view, pid int, connector string) *view {
	for i := range vs {
		if vs[i].PID == pid {
			return &vs[i]
		}
	}
	for i := range vs {
		if titleMatches(vs[i].Title, connector) {
			return &vs[i]
		}
	}
	return nil
}

// status renders the agent's view for CNM.
func (r *reconciler) status() ([]display.OutputStatus, []display.DisplayStatus) {
	outs := make([]display.OutputStatus, 0, len(r.outputs))
	for _, o := range r.outputs {
		ost := display.OutputStatus{Name: o.Name, Make: o.Make, Model: o.Model, Serial: o.Serial, Active: o.Active, Modes: []display.Mode{}}
		for _, m := range o.Modes {
			ost.Modes = append(ost.Modes, m.mode())
		}
		ost.Modes = display.SortModes(ost.Modes)
		if o.CurrentMode != nil && o.Active {
			m := o.CurrentMode.mode()
			ost.Current = &m
		}
		if n, err := strconv.Atoi(o.Transform); err == nil {
			ost.Rotation = n
		}
		outs = append(outs, ost)
	}
	ds := make([]display.DisplayStatus, 0, len(r.disp))
	for _, st := range r.disp {
		e := st.err
		if e == "" && st.state != display.StateOff {
			e = r.outErr[st.connector]
		}
		ds = append(ds, display.DisplayStatus{DisplayID: st.id, Connector: st.connector, State: st.state, Since: st.since, Restarts: st.restarts, Error: e, Mode: st.mode})
	}
	sort.Slice(ds, func(i, j int) bool { return naturalLess(ds[i].Connector, ds[j].Connector) })
	return outs, ds
}

// naturalLess orders "HDMI-A-2" before "HDMI-A-10".
func naturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ca, cb := a[i], b[j]
		if isDigit(ca) && isDigit(cb) {
			si, sj := i, j
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na, _ := strconv.Atoi(a[si:i])
			nb, _ := strconv.Atoi(b[sj:j])
			if na != nb {
				return na < nb
			}
			continue
		}
		if ca != cb {
			return ca < cb
		}
		i++
		j++
	}
	return len(a)-i < len(b)-j
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
