// Command cnm is the Camera Network Monitor service.
//
//	cnm serve        [--data-dir DIR]   run the service (default)
//	cnm display      [--socket PATH]    local display agent (separate service)
//	cnm check-config [--data-dir DIR]   read-only upgrade pre-flight check
//	cnm reset-user   [--data-dir DIR] --username NAME   (password from stdin)
//	cnm version
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cnm/internal/auth"
	"cnm/internal/bridge"
	"cnm/internal/buildinfo"
	"cnm/internal/config"
	"cnm/internal/connreg"
	"cnm/internal/device"
	"cnm/internal/display"
	"cnm/internal/display/agent"
	"cnm/internal/media"
	"cnm/internal/playback"
	"cnm/internal/procutil"
	"cnm/internal/provider"
	"cnm/internal/sdnotify"
	"cnm/internal/secret"
	"cnm/internal/server"
	"cnm/internal/webui"

	_ "cnm/internal/provider/axis"
	_ "cnm/internal/provider/dahua"
	_ "cnm/internal/provider/generic"
	_ "cnm/internal/provider/hikvision"
	_ "cnm/internal/provider/sofia"
	_ "cnm/internal/provider/uniview"
)

func defaultDataDir() string {
	if v := os.Getenv("CNM_DATA_DIR"); v != "" {
		return v
	}
	if runtime.GOOS == "linux" {
		return "/var/lib/camera-network-monitor"
	}
	return "data"
}

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		os.Exit(serve(args))
	case "display":
		os.Exit(runDisplay(args))
	case "check-config":
		os.Exit(checkConfig(args))
	case "reset-user":
		os.Exit(resetUser(args))
	case "version", "--version", "-v":
		fmt.Println("Camera Network Monitor", buildinfo.Version)
	default:
		fmt.Fprintf(os.Stderr, "unbekannter Befehl %q (serve | display | check-config | reset-user | version)\n", cmd)
		os.Exit(2)
	}
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("CNM_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	}
	opts := &slog.HandlerOptions{Level: level}
	if os.Getenv("INVOCATION_ID") != "" {
		// under systemd: journald adds timestamps
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			return a
		}
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

// hasEncryptedSecrets reports whether the config holds values that need the master key.
func hasEncryptedSecrets(c *config.Config) bool {
	for _, d := range c.Devices {
		if d.Credentials.HasPassword() && secret.LooksEncrypted(d.Credentials.Password) {
			return true
		}
	}
	for _, u := range c.Users {
		for _, k := range u.APIKeys {
			if k.Recoverable() {
				return true
			}
		}
	}
	for _, d := range c.Displays {
		if len(d.TokenEnc) > 0 {
			return true
		}
	}
	return false
}

const defaultDisplaySocket = "/run/camera-network-monitor/display.sock"

// runDisplay runs the local display agent (camera-network-monitor-display.service).
func runDisplay(args []string) int {
	fsx := flag.NewFlagSet("display", flag.ContinueOnError)
	socket := fsx.String("socket", envOr("CNM_DISPLAY_SOCKET", defaultDisplaySocket), "CNM display socket")
	stateDir := fsx.String("state-dir", "", "browser profiles (default $STATE_DIRECTORY)")
	sway := fsx.String("sway", envOr("CNM_SWAY", "sway"), "sway binary")
	browser := fsx.String("browser", os.Getenv("CNM_DISPLAY_BROWSER"), "explicit browser binary (optional)")
	if err := fsx.Parse(args); err != nil {
		return 2
	}
	log := newLogger().With("component", "display")
	log.Info("starting display agent", "version", buildinfo.Version, "socket", *socket)
	a := agent.New(agent.Options{Socket: *socket, StateDir: *stateDir, SwayBin: *sway, BrowserBin: *browser, Version: buildinfo.Version, Log: log})
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := a.Run(ctx); err != nil {
		log.Error("display agent failed", "error", err)
		return 1
	}
	log.Info("display agent stopped")
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadKeys(dataDir string, c *config.Config, log *slog.Logger) (*secret.Keyring, error) {
	path := filepath.Join(dataDir, config.KeyFileName)
	keys, err := secret.LoadKeyFile(path)
	if err == nil {
		return keys, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("master.key ist nicht lesbar: %w", err)
	}
	if hasEncryptedSecrets(c) {
		return nil, fmt.Errorf("master.key fehlt, aber config.json enthält verschlüsselte Secrets – master.key aus dem Backup wiederherstellen (config.json und master.key gehören zusammen)")
	}
	log.Info("creating new master key", "path", path)
	return secret.CreateKeyFile(path)
}

func serve(args []string) int {
	fsx := flag.NewFlagSet("serve", flag.ContinueOnError)
	dataDir := fsx.String("data-dir", defaultDataDir(), "data directory")
	listen := fsx.String("listen", os.Getenv("CNM_LISTEN"), "override listen address host:port (testing)")
	displaySocket := fsx.String("display-socket", os.Getenv("CNM_DISPLAY_SOCKET"), "Unix socket for the local display agent (empty = off)")
	displayUser := fsx.String("display-user", envOr("CNM_DISPLAY_USER", "cnm-display"), "system user of the display agent")
	if err := fsx.Parse(args); err != nil {
		return 2
	}
	log := newLogger()
	log.Info("starting Camera Network Monitor", "version", buildinfo.Version, "data_dir", *dataDir)
	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		log.Error("cannot create data directory", "error", err)
		return 1
	}
	store, res, err := config.Open(*dataDir)
	if err != nil {
		log.Error("configuration error", "error", err)
		return 1
	}
	switch {
	case res.Fresh:
		log.Info("new configuration created")
		firstRunMedia(store, log)
	case res.FromBackup:
		log.Warn("config.json was missing or damaged – restored from config.json.bak")
	}
	if res.Archived != "" {
		log.Info("configuration migrated, original archived", "archive", res.Archived)
	}
	if res.Report != nil {
		for _, n := range res.Report.Notes {
			log.Info("config migration", "note", n)
		}
	}
	keys, err := loadKeys(*dataDir, store.Get(), log)
	if err != nil {
		log.Error("secret storage error", "error", err)
		return 1
	}
	migratePlaintextPasswords(store, keys, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conns := connreg.New()
	authSvc := auth.NewService(store, keys, conns)
	br, err := bridge.New(log.With("component", "bridge"))
	if err != nil {
		log.Error("cannot open internal bridge listener", "error", err)
		return 1
	}
	devices := device.NewManager(store, keys, br.URL, log.With("component", "device"))
	br.SetOpener(devices)
	recon := media.NewReconciler(store.Get, devices, log.With("component", "media"))
	proc := media.NewProcess(store.Get, log.With("component", "go2rtc"), recon.Trigger)
	pb := playback.New(store.Get, devices, log.With("component", "playback"))
	srv := server.New(server.Deps{
		Store: store, Keys: keys, Auth: authSvc, Devices: devices, Recon: recon, Process: proc,
		Playback: pb, Conns: conns, Log: log.With("component", "http"), Version: buildinfo.Version, Static: webui.Handler(),
	})

	// local displays: CNM serves desired state + kiosk credentials to the agent
	var baseMu sync.Mutex
	baseURL := "http://127.0.0.1:" + strconv.Itoa(store.Get().System.ListenPort)
	dispHost := display.NewHost(store, keys, srv.Hub.Presence, func() string {
		baseMu.Lock()
		defer baseMu.Unlock()
		return baseURL
	}, log.With("component", "display"))
	dispHost.SysfsRoot = os.Getenv("CNM_DRM_SYSFS")
	dispHost.GPU = os.Getenv("CNM_DISPLAY_GPU") // set by setup.sh when several GPUs exist
	srv.Displays = dispHost

	// ---- event wiring --------------------------------------------------------------------
	store.Subscribe(func(old, next *config.Config) {
		srv.Hub.ScheduleState()
		recon.Trigger()
		proc.Changed()
	})
	var stMu sync.Mutex
	lastState := map[string]device.State{}
	devices.OnStatus(func(st device.Status) {
		srv.Hub.Event(map[string]any{"type": "device", "device": st}, func(p *auth.Principal) bool { return p.IsAdmin() })
		stMu.Lock()
		prev := lastState[st.ID]
		lastState[st.ID] = st.State
		stMu.Unlock()
		if prev != st.State && st.State == device.Connected {
			srv.Hub.ScheduleState() // device reported channel names may have changed
			pb.RecheckDevice(st.ID) // recording availability (storage detected automatically)
		}
	})
	devices.OnSourcesChanged(recon.Trigger)
	pb.OnAvailabilityChange(srv.Hub.ScheduleState)
	recon.OnStatus(func(st media.Status) {
		srv.Hub.EventFunc(func(p *auth.Principal) any {
			s := st
			if p.Kind == auth.KindKiosk {
				s.Errors, s.Version = nil, ""
			}
			return map[string]any{"type": "media", "media": s}
		})
	})
	recon.OnReplaced(func(names []string) {
		c := store.Get()
		srv.Hub.EventFunc(func(p *auth.Principal) any {
			allowed := media.AllowedStreams(c, p.ScopeMonitor())
			var mine []string
			for _, n := range names {
				if allowed[n] {
					mine = append(mine, n)
				}
			}
			if len(mine) == 0 {
				return nil
			}
			return map[string]any{"type": "streamsReplaced", "streams": mine}
		})
	})

	// ---- start ------------------------------------------------------------------------------
	go br.Serve()
	devices.Start()
	go recon.Run(ctx)
	go proc.Run(ctx)
	go pb.RunIndex(ctx)
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				authSvc.Sweep()
			}
		}
	}()

	sys := store.Get().System
	addr := net.JoinHostPort(sys.ListenHost, strconv.Itoa(sys.ListenPort))
	if *listen != "" {
		addr = *listen
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("cannot listen", "addr", addr, "error", err)
		return 1
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		host := "127.0.0.1"
		if !tcp.IP.IsUnspecified() && !tcp.IP.IsLoopback() {
			host = tcp.IP.String()
			if tcp.IP.To4() == nil {
				host = "[" + host + "]"
			}
		}
		baseMu.Lock()
		baseURL = "http://" + host + ":" + strconv.Itoa(tcp.Port)
		baseMu.Unlock()
	}
	// the agent socket opens only now: kiosk URLs need the real listen address
	if *displaySocket != "" {
		allowed := []int{0, os.Getuid()}
		if u, err := user.Lookup(*displayUser); err == nil {
			if uid, err := strconv.Atoi(u.Uid); err == nil {
				allowed = append(allowed, uid)
			}
		}
		if err := dispHost.Serve(*displaySocket, allowed); err != nil {
			log.Warn("display socket not available – local displays disabled", "socket", *displaySocket, "error", err)
		} else {
			log.Info("display socket ready", "socket", *displaySocket)
			defer dispHost.Close()
		}
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String())
	sdnotify.Send("READY=1\nSTATUS=listening on " + ln.Addr().String())
	go watchdog(ctx, ln.Addr().String(), log)

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	case err := <-serveErr:
		log.Error("http server failed", "error", err)
	}
	code := shutdown(log, cancel, httpSrv, srv, devices, br)
	if err := authSvc.SaveSessions(); err != nil {
		log.Warn("sessions not saved", "error", err)
	}
	return code
}

// firstRunMedia lets a fresh installation manage the bundled go2rtc
// (CNM_DEFAULT_MANAGE_GO2RTC=auto|1) unless a go2rtc instance already
// answers at the configured API URL. Existing configurations are untouched.
func firstRunMedia(store *config.Store, log *slog.Logger) {
	mode := strings.ToLower(os.Getenv("CNM_DEFAULT_MANAGE_GO2RTC"))
	if mode != "auto" && mode != "1" && mode != "true" {
		return
	}
	c := store.Get()
	if mode == "auto" {
		bin := procutil.Resolve("", "go2rtc")
		if _, err := os.Stat(bin); err != nil {
			return
		}
		hc := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
		if resp, err := hc.Get(c.System.Media.Go2rtcURL + "/api"); err == nil {
			resp.Body.Close()
			log.Info("external go2rtc detected – leaving go2rtc unmanaged", "url", c.System.Media.Go2rtcURL)
			return
		}
	}
	if _, err := store.Update(func(c *config.Config) error { c.System.Media.ManageGo2rtc = true; return nil }); err == nil {
		log.Info("fresh installation: managing the bundled go2rtc")
	}
}

// shutdown tears everything down in parallel. A hard guard guarantees the
// process ends within ~5 s even if a component hangs.
func shutdown(log *slog.Logger, cancel context.CancelFunc, httpSrv *http.Server, srv *server.Server, devices *device.Manager, br *bridge.Bridge) int {
	sdnotify.Send("STOPPING=1")
	guard := time.AfterFunc(5*time.Second, func() {
		log.Warn("shutdown guard expired – forcing exit")
		os.Exit(0)
	})
	defer guard.Stop()
	ctx, done := context.WithTimeout(context.Background(), 4500*time.Millisecond)
	defer done()
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}
	run(func() {
		sctx, c := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer c()
		if err := httpSrv.Shutdown(sctx); err != nil {
			_ = httpSrv.Close()
		}
	})
	run(srv.Hub.Shutdown)
	run(func() {
		time.Sleep(300 * time.Millisecond) // short grace period for media sockets
		n := srv.CloseConnections()
		if n > 0 {
			log.Info("closed media connections", "count", n)
		}
	})
	run(func() { devices.Close(ctx) })
	run(br.Close)
	run(cancel) // stops reconciler, managed go2rtc (SIGTERM → SIGKILL) and sweepers
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		log.Warn("shutdown timed out")
	}
	time.Sleep(100 * time.Millisecond) // let child processes receive their signals
	log.Info("stopped")
	return 0
}

// watchdog notifies systemd only while the HTTP server answers requests, so
// a deadlocked process gets restarted.
func watchdog(ctx context.Context, addr string, log *slog.Logger) {
	interval := sdnotify.WatchdogInterval()
	if interval <= 0 {
		return
	}
	host, port, _ := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/api/health"
	client := &http.Client{Timeout: interval / 2, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		resp, err := client.Get(url)
		if err != nil {
			log.Warn("watchdog self check failed", "error", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			sdnotify.Send("WATCHDOG=1")
		}
	}
}

// migratePlaintextPasswords encrypts DVR passwords that a legacy config
// stored in clear text.
func migratePlaintextPasswords(store *config.Store, keys *secret.Keyring, log *slog.Logger) {
	var todo []string
	for _, d := range store.Get().Devices {
		if d.Credentials.HasPassword() && !secret.LooksEncrypted(d.Credentials.Password) {
			if _, err := keys.Decrypt(d.Credentials.Password); err != nil {
				todo = append(todo, d.ID)
			}
		}
	}
	if len(todo) == 0 {
		return
	}
	_, err := store.Update(func(c *config.Config) error {
		for _, id := range todo {
			d, ok := c.Device(id)
			if !ok {
				continue
			}
			var plain string
			if err := jsonUnmarshal(d.Credentials.Password, &plain); err != nil {
				continue
			}
			enc, err := keys.Encrypt(plain)
			if err != nil {
				return err
			}
			d.Credentials.Password = jsonMarshal(enc)
		}
		return nil
	})
	if err != nil {
		log.Error("encrypting legacy plaintext passwords failed", "error", err)
		return
	}
	log.Info("legacy plaintext DVR passwords encrypted", "devices", strings.Join(todo, ","))
}

// ---- check-config ------------------------------------------------------------------------------

func checkConfig(args []string) int {
	fsx := flag.NewFlagSet("check-config", flag.ContinueOnError)
	dataDir := fsx.String("data-dir", defaultDataDir(), "data directory")
	if err := fsx.Parse(args); err != nil {
		return 2
	}
	path := filepath.Join(*dataDir, config.FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Println("OK: keine bestehende Konfiguration (Erstinstallation)")
		return 0
	}
	if err != nil {
		fmt.Println("FEHLER:", err)
		return 1
	}
	c, rep, err := config.Parse(data, *dataDir)
	if err != nil {
		fmt.Println("FEHLER:", err)
		return 1
	}
	problems := 0
	fmt.Printf("Konfiguration: Schema %d, %d Geräte, %d Monitore, %d Benutzer\n", rep.SourceVersion, len(c.Devices), len(c.Monitors), len(c.Users))
	for _, n := range rep.Notes {
		fmt.Println("  Hinweis:", n)
	}
	keys, kerr := secret.LoadKeyFile(filepath.Join(*dataDir, config.KeyFileName))
	if kerr != nil {
		if hasEncryptedSecrets(c) {
			fmt.Println("FEHLER: master.key fehlt oder ist unlesbar:", kerr)
			problems++
		} else {
			fmt.Println("Hinweis: kein master.key vorhanden – wird beim Start erzeugt")
		}
	}
	for _, d := range c.Devices {
		if _, _, err := provider.Resolve(d.Protocol); err != nil {
			fmt.Printf("WARNUNG: Gerät %s: %v\n", d.ID, err)
		}
		if !d.Credentials.HasPassword() || keys == nil {
			continue
		}
		if _, err := keys.Decrypt(d.Credentials.Password); err != nil {
			if secret.LooksEncrypted(d.Credentials.Password) {
				fmt.Printf("FEHLER: Gerät %s: DVR-Passwort nicht entschlüsselbar\n", d.ID)
				problems++
			} else {
				fmt.Printf("Hinweis: Gerät %s: Klartext-Passwort wird beim Start verschlüsselt\n", d.ID)
			}
		}
	}
	for _, u := range c.Users {
		if !secret.HasUsableHash(u.PasswordHash) {
			fmt.Printf("WARNUNG: Benutzer %s: Passwort-Hash nicht lesbar (Passwort neu setzen)\n", u.Username)
		}
		for _, k := range u.APIKeys {
			if k.Recoverable() && keys != nil {
				tok, err := keys.Decrypt(k.TokenEnc)
				if err != nil || !secret.VerifyTokenHash(k.TokenHash, tok) {
					fmt.Printf("WARNUNG: Kiosk-Key %s (%s): URL nicht wiederherstellbar, Key selbst bleibt gültig\n", k.ID, k.Label)
				}
			}
		}
	}
	if problems > 0 {
		fmt.Printf("%d Problem(e) gefunden\n", problems)
		return 1
	}
	fmt.Println("OK: Konfiguration und master.key sind kompatibel")
	return 0
}

// ---- reset-user --------------------------------------------------------------------------------

func resetUser(args []string) int {
	fsx := flag.NewFlagSet("reset-user", flag.ContinueOnError)
	dataDir := fsx.String("data-dir", defaultDataDir(), "data directory")
	username := fsx.String("username", "", "user to reset or create (admin)")
	if err := fsx.Parse(args); err != nil || *username == "" {
		fmt.Fprintln(os.Stderr, "Verwendung: cnm reset-user --username NAME < passwort (Dienst vorher stoppen)")
		return 2
	}
	fmt.Fprint(os.Stderr, "Neues Passwort: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	pw := strings.TrimRight(line, "\r\n")
	if err := secret.ValidatePasswordPolicy(pw); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store, _, err := config.Open(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	h, err := secret.HashPassword(pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_, err = store.Update(func(c *config.Config) error {
		if u, ok := c.UserByName(*username); ok {
			u.PasswordHash = jsonMarshal(h)
			u.Enabled = true
			u.Role = config.RoleAdmin
			return nil
		}
		c.Users = append(c.Users, config.User{ID: config.NewID("u_"), Username: *username, Role: config.RoleAdmin, Enabled: true, PasswordHash: jsonMarshal(h), APIKeys: []config.APIKey{}, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "\nAdministrator %q gesetzt.\n", *username)
	return 0
}
