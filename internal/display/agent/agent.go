package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"cnm/internal/display"
)

// Options configure the agent.
type Options struct {
	Socket     string // CNM display socket
	StateDir   string // browser profiles
	RuntimeDir string // XDG_RUNTIME_DIR of the display session
	SwayBin    string
	BrowserBin string // explicit browser path (optional)
	Version    string
	Log        *slog.Logger
}

// Agent runs the local kiosk displays.
type Agent struct {
	opt    Options
	log    *slog.Logger
	client *http.Client
	kick   chan struct{}

	mu       sync.Mutex
	desired  *display.Desired
	cnmErr   string
	snapshot display.Status
}

// New creates an agent.
func New(opt Options) *Agent {
	if opt.SwayBin == "" {
		opt.SwayBin = "sway"
	}
	if opt.RuntimeDir == "" {
		opt.RuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	}
	if opt.StateDir == "" {
		if d := os.Getenv("STATE_DIRECTORY"); d != "" {
			opt.StateDir = d
		} else if home, err := os.UserHomeDir(); err == nil {
			opt.StateDir = filepath.Join(home, ".local", "state", "cnm-display")
		}
	}
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	sock := opt.Socket
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Agent{opt: opt, log: log, client: &http.Client{Transport: tr}, kick: make(chan struct{}, 1)}
}

func (a *Agent) poke() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *Agent) setDesired(d *display.Desired) {
	a.mu.Lock()
	// a new CNM instance (epoch) restarts its revision counter
	newer := a.desired == nil || d.Epoch != a.desired.Epoch || d.Rev >= a.desired.Rev
	changed := a.desired == nil || d.Epoch != a.desired.Epoch || d.Rev != a.desired.Rev
	if newer {
		a.desired = d
	}
	a.cnmErr = ""
	a.mu.Unlock()
	if changed {
		a.poke()
	}
}

func (a *Agent) currentDesired() (*display.Desired, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.desired, a.cnmErr
}

// pollDesired long-polls CNM for changes.
func (a *Agent) pollDesired(ctx context.Context) {
	fails := 0
	for ctx.Err() == nil {
		var rev int64
		epoch := ""
		if d, _ := a.currentDesired(); d != nil {
			rev, epoch = d.Rev, d.Epoch
		}
		rctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		req, _ := http.NewRequestWithContext(rctx, "GET", "http://cnm/v1/desired?rev="+strconv.FormatInt(rev, 10)+"&epoch="+url.QueryEscape(epoch), nil)
		var d display.Desired
		err := a.do(req, &d)
		cancel()
		if err == nil {
			fails = 0
			a.setDesired(&d)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		fails++
		a.mu.Lock()
		a.cnmErr = "CNM nicht erreichbar: " + err.Error()
		a.mu.Unlock()
		if fails == 1 || fails%30 == 0 {
			a.log.Warn("CNM display socket not reachable", "socket", a.opt.Socket, "error", err)
		}
		sleep(ctx, min(time.Duration(fails)*time.Second, 10*time.Second))
	}
}

// reportLoop sends the agent status every few seconds; the reply carries the
// current desired state (incl. viewer presence for the watchdog).
func (a *Agent) reportLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		a.mu.Lock()
		st := a.snapshot
		if a.cnmErr != "" && st.Error == "" {
			st.Error = a.cnmErr
		}
		a.mu.Unlock()
		body, _ := json.Marshal(st)
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, _ := http.NewRequestWithContext(rctx, "POST", "http://cnm/v1/status", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		var d display.Desired
		if err := a.do(req, &d); err == nil {
			a.setDesired(&d)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Agent) do(req *http.Request, v any) error {
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Run blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if a.opt.StateDir != "" {
		_ = os.MkdirAll(a.opt.StateDir, 0o700)
	}
	go a.pollDesired(ctx)
	go a.reportLoop(ctx)

	var (
		comp       *Compositor
		rec        *reconciler
		compStatus = display.CompositorStatus{State: "starting", Since: time.Now()}
		compFails  int
		nextComp   time.Time
		failRev    int64 = -1 // desired revision of the last failed series
		browser    display.BrowserInfo
		browserFor = "\x00"
		vt         = newVTSwitcher()
	)
	setComp := func(state, errMsg string) {
		if compStatus.State != state {
			compStatus.Since = time.Now()
		}
		compStatus.State, compStatus.Error = state, errMsg
	}
	stopComp := func() {
		if rec != nil {
			rec.stopAll()
		}
		if comp != nil {
			comp.Stop()
		}
		comp, rec = nil, nil
		vt.release() // never leave the monitor on an empty kiosk console
	}
	defer stopComp()
	// compFailed schedules the next attempt. After repeated failures the agent
	// pauses (15 min or until the configuration changes) instead of taking
	// the console over again and again.
	compFailed := func(rev int64) {
		compFails++
		nextComp = time.Now().Add(backoff(compFails))
		if compFails >= 3 {
			nextComp = time.Now().Add(15 * time.Minute)
			failRev = rev
		}
	}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		desired, _ := a.currentDesired()
		pref := "auto"
		if desired != nil && desired.Browser != "" {
			pref = desired.Browser
		}
		if pref != browserFor {
			browser = detectBrowser(pref, a.opt.BrowserBin)
			browserFor = pref
			if browser.Error != "" {
				a.log.Warn("kiosk browser", "error", browser.Error)
			} else {
				a.log.Info("kiosk browser", "kind", browser.Kind, "path", browser.Path, "version", browser.Version)
			}
			if rec != nil {
				rec.stopAll()
				rec.browser = browser
			}
		}
		var rev int64
		if desired != nil {
			rev = desired.Rev
		}
		// the compositor (and with it the kiosk console) only runs while there
		// is something to show – otherwise the monitor keeps the normal console
		show := hasOutputs(desired) && browser.Error == "" && browser.Path != ""
		if comp != nil && !show {
			a.log.Info("nothing to show – stopping the compositor, console released")
			stopComp()
		}
		if comp == nil && !show {
			switch {
			case browser.Error != "" && hasOutputs(desired):
				setComp("idle", browser.Error)
			case compStatus.State != "failed" && compStatus.State != "missing":
				setComp("idle", "")
			}
		}
		if compFails >= 3 && rev != failRev {
			nextComp = time.Time{} // configuration changed: try again right away
		}
		if comp == nil && show && !time.Now().Before(nextComp) {
			if err := vt.activate(); err != nil {
				a.log.Warn("cannot switch console", "error", err)
			}
			c := &Compositor{Bin: a.opt.SwayBin, RuntimeDir: a.opt.RuntimeDir, Log: a.log}
			if err := c.Start(); err != nil {
				vt.release()
				compFailed(rev)
				state := "failed"
				if _, lerr := lookPath(a.opt.SwayBin); lerr != nil {
					state = "missing"
				}
				msg := err.Error()
				if compFails >= 3 {
					msg += " – weitere Versuche in 15 min oder nach einer Änderung"
				}
				setComp(state, msg)
				a.log.Error("compositor start failed", "error", err, "retry_at", nextComp.Format("15:04:05"))
			} else {
				compFails, failRev = 0, -1
				comp = c
				compStatus.Version = c.version
				setComp("running", "")
				env := []string{"WAYLAND_DISPLAY=" + c.wayland, "XDG_RUNTIME_DIR=" + a.opt.RuntimeDir}
				rec = newReconciler(c.ipc, execLauncher{}, browser, env, a.opt.StateDir, a.log)
				a.log.Info("compositor running", "version", c.version, "wayland", c.wayland)
			}
		}
		if rec != nil {
			if err := rec.step(desired); err != nil {
				a.log.Error("compositor not responding – restarting it", "error", err)
				setComp("failed", err.Error())
				stopComp()
				compFailed(rev)
			}
		}
		a.publish(compStatus, browser, rec)

		var exited <-chan struct{}
		if comp != nil {
			exited = comp.Exited()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-a.kick:
		case <-tick.C:
		case <-exited:
			msg := "sway wurde beendet"
			if e := comp.LastError(); e != "" {
				msg += ": " + e
			}
			a.log.Error("compositor exited", "last", comp.LastError())
			setComp("failed", msg)
			stopComp()
			compFailed(rev)
		}
	}
}

// hasOutputs reports whether at least one display should show a multiviewer.
func hasOutputs(d *display.Desired) bool {
	if d == nil {
		return false
	}
	for _, o := range d.Outputs {
		if o.Enabled && o.URL != "" {
			return true
		}
	}
	return false
}

func (a *Agent) publish(comp display.CompositorStatus, browser display.BrowserInfo, rec *reconciler) {
	st := display.Status{Version: a.opt.Version, Compositor: comp, Browser: browser, Installed: installedBrowsers(), Outputs: []display.OutputStatus{}, Displays: []display.DisplayStatus{}}
	if rec != nil {
		st.Outputs, st.Displays = rec.status()
	}
	a.mu.Lock()
	a.snapshot = st
	a.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
