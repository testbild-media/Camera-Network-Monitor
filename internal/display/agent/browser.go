package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"cnm/internal/display"
)

// browser candidates; "auto" prefers Firefox (installed by the setup on
// Debian and Ubuntu) and falls back to Chromium/Chrome
var (
	chromiumBins = []string{"chromium", "chromium-browser", "google-chrome-stable", "google-chrome"}
	firefoxBins  = []string{"firefox-esr", "firefox"}
)

// isSnap reports whether a browser binary is (a wrapper around) a snap
// package. Snap confinement rejects the per-display profile directories, so
// such browsers are not used.
func isSnap(path string) bool {
	if real, err := filepath.EvalSymlinks(path); err == nil && strings.HasPrefix(real, "/snap/") {
		return true
	}
	if info, err := os.Stat(path); err == nil && info.Size() < 16<<10 {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), "/snap/bin/") {
			return true
		}
	}
	return false
}

// installedBrowsers lists the browser kinds with a usable (non-snap) binary.
func installedBrowsers() []string {
	out := []string{}
	for _, k := range []struct {
		kind string
		bins []string
	}{{"firefox", firefoxBins}, {"chromium", chromiumBins}} {
		for _, b := range k.bins {
			if p, err := exec.LookPath(b); err == nil && !isSnap(p) {
				out = append(out, k.kind)
				break
			}
		}
	}
	return out
}

// detectBrowser finds the kiosk browser. override (an explicit path) wins.
func detectBrowser(pref, override string) display.BrowserInfo {
	snapOnly := false
	try := func(kind string, bins []string) (display.BrowserInfo, bool) {
		for _, b := range bins {
			if p, err := exec.LookPath(b); err == nil {
				if isSnap(p) {
					snapOnly = true
					continue
				}
				return display.BrowserInfo{Kind: kind, Path: p}, true
			}
		}
		return display.BrowserInfo{}, false
	}
	if override != "" {
		kind := "chromium"
		if strings.Contains(strings.ToLower(filepath.Base(override)), "firefox") {
			kind = "firefox"
		}
		if p, err := exec.LookPath(override); err == nil {
			return withVersion(display.BrowserInfo{Kind: kind, Path: p})
		}
		return display.BrowserInfo{Kind: kind, Path: override, Error: "Browser " + override + " nicht gefunden"}
	}
	order := [][2]any{{"firefox", firefoxBins}, {"chromium", chromiumBins}}
	if pref == "chromium" {
		order[0], order[1] = order[1], order[0]
	}
	for i, o := range order {
		if pref != "auto" && pref != "" && i > 0 {
			break // an explicit choice does not fall back silently
		}
		if info, ok := try(o[0].(string), o[1].([]string)); ok {
			return withVersion(info)
		}
	}
	want := "Firefox oder Chromium"
	if pref == "chromium" || pref == "firefox" {
		want = map[string]string{"chromium": "Chromium", "firefox": "Firefox"}[pref]
	}
	if snapOnly {
		return display.BrowserInfo{Error: "Nur Snap-Browser gefunden – bitte " + want + " als .deb-Paket installieren (siehe docs/OPERATIONS.md)"}
	}
	return display.BrowserInfo{Error: "Kein Browser gefunden – " + want + " installieren"}
}

func withVersion(info display.BrowserInfo) display.BrowserInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, info.Path, "--version").Output()
	if err == nil {
		info.Version = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	}
	return info
}

// launchSpec describes one kiosk browser instance.
type launchSpec struct {
	Browser    display.BrowserInfo
	URL        string
	ProfileDir string
	Connector  string
	Env        []string // compositor environment (WAYLAND_DISPLAY, XDG_RUNTIME_DIR)
}

// proc is a running kiosk browser.
type proc interface {
	PID() int
	Done() <-chan struct{}
	Err() string
	Stop()
}

// launcher starts browsers (replaced by a fake in tests).
type launcher interface {
	Launch(spec launchSpec) (proc, error)
}

type execLauncher struct{}

type execProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	tail *lineTail
}

func (p *execProc) PID() int              { return p.cmd.Process.Pid }
func (p *execProc) Done() <-chan struct{} { return p.done }
func (p *execProc) Err() string           { return p.tail.last() }

// Stop ends the browser and all its helper processes (process group).
func (p *execProc) Stop() {
	select {
	case <-p.done:
		return
	default:
	}
	terminate(p.cmd, true)
	select {
	case <-p.done:
	case <-time.After(4 * time.Second):
		kill(p.cmd, true)
		<-p.done
	}
}

func (execLauncher) Launch(spec launchSpec) (proc, error) {
	if err := os.MkdirAll(spec.ProfileDir, 0o700); err != nil {
		return nil, err
	}
	// The kiosk URL carries the display's credential. It must not appear in
	// the process list, so the browser opens a private start file that
	// forwards to it.
	start, err := writeStartPage(spec.ProfileDir, spec.URL)
	if err != nil {
		return nil, err
	}
	spec.URL = start
	args, env := browserCommand(spec)
	cmd := exec.Command(spec.Browser.Path, args...)
	cmd.Env = env
	cmd.Dir = spec.ProfileDir
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	setProcAttrs(cmd, true)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProc{cmd: cmd, done: make(chan struct{}), tail: newLineTail(10)}
	go p.tail.consume(stderr, nil)
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p, nil
}

// browserCommand returns arguments and environment for a kiosk browser on
// the compositor. It also prepares the profile directory.
func browserCommand(spec launchSpec) ([]string, []string) {
	env := append(filterEnv(os.Environ(), "DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "LANGUAGE"), spec.Env...)
	// German browser UI: a German page is then not "foreign" (no translation offer)
	env = append(env, "XDG_SESSION_TYPE=wayland", "LANGUAGE=de")
	if spec.Browser.Kind == "firefox" {
		prepareFirefoxProfile(spec.ProfileDir)
		env = append(env, "MOZ_ENABLE_WAYLAND=1")
		return append(append([]string{"--kiosk", "--no-remote", "--new-instance", "--profile", spec.ProfileDir}, extraArgs()...), spec.URL), env
	}
	// a crashed Chromium leaves locks that would block the next start
	for _, f := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		_ = os.Remove(filepath.Join(spec.ProfileDir, f))
	}
	return append(append([]string{
		"--ozone-platform=wayland",
		"--enable-features=UseOzonePlatform",
		"--kiosk",
		"--user-data-dir=" + spec.ProfileDir,
		"--class=cnm-display",
		"--no-first-run",
		"--no-default-browser-check",
		"--noerrdialogs",
		"--disable-infobars",
		"--disable-session-crashed-bubble",
		"--disable-restore-session-state",
		"--lang=de",
		"--disable-features=Translate,TranslateUI,MediaRouter,InfiniteSessionRestore",
		"--disable-sync",
		"--disable-component-update",
		"--disable-pinch",
		"--overscroll-history-navigation=0",
		"--check-for-update-interval=31536000",
		"--autoplay-policy=no-user-gesture-required",
		"--password-store=basic",
	}, extraArgs()...), spec.URL), env
}

// extraArgs returns additional browser arguments from CNM_DISPLAY_BROWSER_ARGS
// (troubleshooting, e.g. "--disable-gpu").
func extraArgs() []string {
	return strings.Fields(os.Getenv("CNM_DISPLAY_BROWSER_ARGS"))
}

const firefoxPrefs = `// generated by Camera Network Monitor
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.sessionstore.resume_from_crash", false);
user_pref("browser.sessionstore.max_resumed_crashes", 0);
user_pref("browser.startup.homepage_override.mstone", "ignore");
user_pref("browser.aboutwelcome.enabled", false);
user_pref("trailhead.firstrun.didSeeAboutWelcome", true);
user_pref("datareporting.policy.dataSubmissionEnabled", false);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
user_pref("app.update.auto", false);
user_pref("app.update.enabled", false);
user_pref("media.autoplay.default", 0);
user_pref("browser.tabs.warnOnClose", false);
user_pref("toolkit.cosmeticAnimations.enabled", false);
user_pref("browser.translations.enable", false);
user_pref("browser.translations.automaticallyPopup", false);
user_pref("intl.accept_languages", "de-DE, de");
`

// writeStartPage writes a forwarding page (mode 0600) and returns its file URL.
func writeStartPage(dir, target string) (string, error) {
	esc := html.EscapeString(target)
	js, _ := json.Marshal(target)
	page := "<!doctype html><meta charset=\"utf-8\"><meta http-equiv=\"refresh\" content=\"0;url=" + esc + "\">" +
		"<title>CNM-DISPLAY</title><style>html{background:#000}</style><script>location.replace(" + string(js) + ")</script>"
	path := filepath.Join(dir, "cnm-start.html")
	if err := os.WriteFile(path, []byte(page), 0o600); err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String(), nil
}

func prepareFirefoxProfile(dir string) {
	_ = os.WriteFile(filepath.Join(dir, "user.js"), []byte(firefoxPrefs), 0o600)
	for _, f := range []string{"lock", ".parentlock"} {
		_ = os.Remove(filepath.Join(dir, f))
	}
}

// displayTitle is the window title prefix the CNM viewer sets for a display
// (used to find the window when the process id does not match, e.g. snaps).
func displayTitle(connector string) string { return "CNM-DISPLAY " + connector }

func titleMatches(title, connector string) bool {
	p := displayTitle(connector)
	return title == p || strings.HasPrefix(title, p+" ")
}

func customModeCommand(m display.Mode) string {
	if m.RefreshMHz <= 0 {
		return fmt.Sprintf("%dx%d", m.Width, m.Height)
	}
	return modeCommand(m)
}

func modeCommand(m display.Mode) string {
	return fmt.Sprintf("%dx%d@%d.%03dHz", m.Width, m.Height, m.RefreshMHz/1000, m.RefreshMHz%1000)
}

func lookPath(bin string) (string, error) { return exec.LookPath(bin) }
