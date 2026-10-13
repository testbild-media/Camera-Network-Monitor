package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cnm/internal/config"
	"cnm/internal/provider"
)

// Status is the media layer summary shown to users.
type Status struct {
	State            string        `json:"state"` // ok | warn | offline | disabled | starting
	Desired          int           `json:"desired"`
	Registered       int           `json:"registered"`
	StreamErrors     int           `json:"streamErrors"`
	PreloadErrors    int           `json:"preloadErrors"`
	PreloadSupported bool          `json:"preloadSupported"`
	Errors           []ErrorDetail `json:"errors,omitempty"`
	Version          string        `json:"version,omitempty"`
	Message          string        `json:"message,omitempty"`
	CheckedAt        time.Time     `json:"checkedAt"`
}

// ErrorDetail is a verified, sanitised problem.
type ErrorDetail struct {
	Stream  string `json:"stream"`
	Kind    string `json:"kind"` // stream | preload
	Message string `json:"message"`
}

// StreamHealth is the per-stream runtime view for browser watchdogs.
type StreamHealth struct {
	Registered bool `json:"registered"`
	Online     bool `json:"online"`
	Consumers  int  `json:"consumers"`
}

// Devices is what the reconciler needs from the device manager.
type Devices interface {
	Sources(deviceID string, channel int, kind provider.StreamKind) []string
	Online(deviceID string) bool
}

// Reconciler converges go2rtc towards the desired stream set. Exactly one
// goroutine executes passes, so passes never overlap; triggers are
// coalesced; operations inside a pass run with bounded concurrency.
type Reconciler struct {
	cfg     func() *config.Config
	devices Devices
	log     *slog.Logger

	trigger chan struct{}
	client  atomic.Pointer[Client]

	status atomic.Pointer[Status]
	health atomic.Pointer[map[string]StreamHealth]

	// owned by the run goroutine
	applied map[string]string // stream name → applied source fingerprint

	bytesMu sync.Mutex
	bytes   map[string]bytesSample

	lmu        sync.Mutex
	onStatus   []func(Status)
	onReplaced []func([]string)

	// Concurrency limits the parallel go2rtc operations of one pass.
	Concurrency int
	// OpTimeout bounds every single go2rtc API call (from the moment it is
	// sent: the client sends one request at a time).
	OpTimeout time.Duration
	// WatchInterval is the cadence of the lightweight go2rtc watch.
	WatchInterval time.Duration
	// FastRetry is the schedule used while go2rtc is unreachable.
	FastRetry []time.Duration
}

type bytesSample struct {
	bytes   int64
	changed time.Time
}

// NewReconciler creates a reconciler.
func NewReconciler(cfg func() *config.Config, devices Devices, log *slog.Logger) *Reconciler {
	r := &Reconciler{
		cfg:           cfg,
		devices:       devices,
		log:           log,
		trigger:       make(chan struct{}, 1),
		applied:       map[string]string{},
		bytes:         map[string]bytesSample{},
		Concurrency:   4,
		OpTimeout:     6 * time.Second,
		WatchInterval: 3 * time.Second,
		FastRetry:     []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second},
	}
	r.status.Store(&Status{State: "starting", PreloadSupported: true})
	empty := map[string]StreamHealth{}
	r.health.Store(&empty)
	r.setClient(cfg().System.Media.Go2rtcURL)
	return r
}

func (r *Reconciler) setClient(base string) {
	if old := r.client.Load(); old != nil {
		if old.Base() == strings.TrimRight(base, "/") {
			return
		}
		old.Close()
	}
	r.client.Store(NewClient(base))
}

// Client returns the current go2rtc client.
func (r *Reconciler) Client() *Client { return r.client.Load() }

// OnStatus registers a status change callback.
func (r *Reconciler) OnStatus(fn func(Status)) {
	r.lmu.Lock()
	r.onStatus = append(r.onStatus, fn)
	r.lmu.Unlock()
}

// OnReplaced registers a callback receiving names of streams whose
// definition was replaced (browsers should restart these players).
func (r *Reconciler) OnReplaced(fn func([]string)) {
	r.lmu.Lock()
	r.onReplaced = append(r.onReplaced, fn)
	r.lmu.Unlock()
}

// Trigger requests a reconcile pass soon (coalesced).
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Status returns the latest summary.
func (r *Reconciler) Status() Status { return *r.status.Load() }

// Health returns runtime health for the given stream names.
func (r *Reconciler) Health(names []string) map[string]StreamHealth {
	all := *r.health.Load()
	out := make(map[string]StreamHealth, len(names))
	for _, n := range names {
		out[n] = all[n]
	}
	return out
}

func (r *Reconciler) setStatus(s Status) {
	prev := r.status.Load()
	s.CheckedAt = time.Now()
	r.status.Store(&s)
	if prev == nil || !sameStatus(*prev, s) {
		r.lmu.Lock()
		fns := append([]func(Status){}, r.onStatus...)
		r.lmu.Unlock()
		for _, fn := range fns {
			fn(s)
		}
	}
}

func sameStatus(a, b Status) bool {
	if a.State != b.State || a.Desired != b.Desired || a.StreamErrors != b.StreamErrors || a.PreloadErrors != b.PreloadErrors || len(a.Errors) != len(b.Errors) || a.Message != b.Message {
		return false
	}
	for i := range a.Errors {
		if a.Errors[i] != b.Errors[i] {
			return false
		}
	}
	return true
}

// Run executes passes until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	go r.watch(ctx)
	fast := 0
	for {
		next := time.Duration(r.cfg().System.Media.ReconcileIntervalMs) * time.Millisecond
		reachable := r.safePass(ctx)
		if ctx.Err() != nil {
			return
		}
		if !reachable && fast < len(r.FastRetry) {
			next = r.FastRetry[fast]
			fast++
		} else if reachable {
			fast = 0
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-r.trigger:
			t.Stop()
			// coalesce bursts of changes (startup, bulk edits)
			select {
			case <-ctx.Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
			select {
			case <-r.trigger:
			default:
			}
			fast = 0
		case <-t.C:
		}
	}
}

func (r *Reconciler) safePass(ctx context.Context) (reachable bool) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("reconcile panic", "panic", p, "stack", string(debug.Stack()))
			reachable = true
		}
	}()
	return r.pass(ctx)
}

// watch polls go2rtc cheaply to keep per-stream health fresh and to detect
// a go2rtc restart (registered streams vanish) without waiting 15 s.
func (r *Reconciler) watch(ctx context.Context) {
	t := time.NewTicker(r.WatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg := r.cfg()
		if !cfg.System.Media.Enabled {
			continue
		}
		c := r.client.Load()
		wctx, cancel := withOpTimeout(ctx, r.OpTimeout)
		streams, err := c.Streams(wctx)
		cancel()
		if err != nil {
			if st := r.Status(); st.State != "offline" {
				r.Trigger() // fast path: run a pass now, which switches to fast retries
			}
			continue
		}
		r.updateHealth(streams)
		desired, _ := Desired(cfg, r.devices.Sources, r.devices.Online)
		for name := range desired {
			if _, ok := streams[name]; !ok {
				r.Trigger()
				break
			}
		}
		if st := r.Status(); st.State == "offline" {
			r.Trigger()
		}
	}
}

const sourceIdle = 12 * time.Second

func (r *Reconciler) updateHealth(streams map[string]StreamInfo) {
	now := time.Now()
	h := make(map[string]StreamHealth, len(streams))
	r.bytesMu.Lock()
	for name, si := range streams {
		if !strings.HasPrefix(name, Prefix) {
			continue
		}
		b := si.BytesRecv()
		prev, ok := r.bytes[name]
		if !ok || b != prev.bytes {
			prev = bytesSample{bytes: b, changed: now}
			r.bytes[name] = prev
		}
		online := si.ActiveProducers() > 0
		// Producers that report byte counters must also make progress; some
		// (e.g. go2rtc's dvrip source) report none – then an active connection counts.
		if online && b > 0 && now.Sub(prev.changed) >= sourceIdle {
			online = false
		}
		h[name] = StreamHealth{Registered: true, Online: online, Consumers: len(si.Consumers)}
	}
	for name := range r.bytes {
		if _, ok := streams[name]; !ok {
			delete(r.bytes, name)
		}
	}
	r.bytesMu.Unlock()
	r.health.Store(&h)
}

type op struct {
	name          string
	desired       *DesiredStream
	put           bool // (re)create stream definition
	dropPreload   bool // remove preload before replacing / deleting
	addPreload    bool
	delete        bool // undesired stream
	repairPreload bool
}

// pass performs one reconcile pass. It returns false when go2rtc is unreachable.
func (r *Reconciler) pass(ctx context.Context) bool {
	cfg := r.cfg()
	r.setClient(cfg.System.Media.Go2rtcURL)
	if !cfg.System.Media.Enabled {
		r.setStatus(Status{State: "disabled", PreloadSupported: true})
		return true
	}
	c := r.client.Load()
	desired, expected := Desired(cfg, r.devices.Sources, r.devices.Online)

	lctx, cancel := withOpTimeout(ctx, r.OpTimeout)
	actual, err := c.Streams(lctx)
	cancel()
	if err != nil {
		r.log.Debug("go2rtc unreachable", "error", err)
		r.setStatus(Status{State: "offline", Desired: expected, PreloadSupported: r.Status().PreloadSupported, Message: "go2rtc nicht erreichbar"})
		// go2rtc lost all state: forget what we applied
		r.applied = map[string]string{}
		return false
	}
	pctx, cancel := withOpTimeout(ctx, r.OpTimeout)
	preloads, preloadSupported, perr := c.Preloads(pctx)
	cancel()
	if perr != nil {
		preloads = map[string]bool{}
	}
	vctx, cancel := withOpTimeout(ctx, r.OpTimeout)
	ver, _ := c.Version(vctx)
	cancel()
	r.updateHealth(actual)

	var ops []*op
	var replaced []string
	for name, d := range desired {
		o := &op{name: name, desired: d}
		si, exists := actual[name]
		fp := d.fingerprint()
		switch {
		case !exists:
			o.put = true
		case r.applied[name] == fp:
		case sourcesMatch(si, d.Sources):
			r.applied[name] = fp // adopt an identical definition (e.g. after CNM restart)
		default:
			o.put = true
			replaced = append(replaced, name)
		}
		if o.put && preloads[name] {
			o.dropPreload = true
		}
		if preloadSupported {
			wantPreload := d.Preload
			has := preloads[name]
			switch {
			case wantPreload && (o.put || !has):
				o.addPreload = true
			case wantPreload && has && exists && (!si.HasPreloadConsumer() || si.ActiveProducers() == 0):
				// stale preload: entry exists but is attached to a replaced
				// stream object or holds no producer
				o.repairPreload = true
			case !wantPreload && has:
				o.dropPreload = true
			}
		}
		if o.put || o.addPreload || o.repairPreload || o.dropPreload {
			ops = append(ops, o)
		}
	}
	for name := range actual {
		if !strings.HasPrefix(name, Prefix) {
			continue
		}
		if _, ok := desired[name]; !ok {
			ops = append(ops, &op{name: name, delete: true, dropPreload: preloads[name]})
		}
	}
	for name := range preloads {
		if strings.HasPrefix(name, Prefix) {
			if _, ok := desired[name]; !ok {
				if _, inActual := actual[name]; !inActual {
					ops = append(ops, &op{name: name, dropPreload: true})
				}
			}
		}
	}

	errs := r.execute(ctx, c, ops)
	if ctx.Err() != nil {
		return true
	}
	st := Status{State: "ok", Desired: expected, PreloadSupported: preloadSupported, Version: ver, Registered: len(desired)}
	for _, e := range errs {
		switch e.Kind {
		case "stream":
			st.StreamErrors++
		case "preload":
			st.PreloadErrors++
		}
	}
	if len(errs) > 0 {
		st.State = "warn"
		sort.Slice(errs, func(i, j int) bool {
			if errs[i].Stream == errs[j].Stream {
				return errs[i].Kind < errs[j].Kind
			}
			return errs[i].Stream < errs[j].Stream
		})
		st.Errors = errs
	}
	r.setStatus(st)
	if len(replaced) > 0 {
		r.lmu.Lock()
		fns := append([]func([]string){}, r.onReplaced...)
		r.lmu.Unlock()
		for _, fn := range fns {
			fn(replaced)
		}
	}
	return true
}

// execute runs per-stream operation chains with bounded concurrency.
func (r *Reconciler) execute(ctx context.Context, c *Client, ops []*op) []ErrorDetail {
	if len(ops) == 0 {
		return nil
	}
	sem := make(chan struct{}, max(1, r.Concurrency))
	var mu sync.Mutex
	var errs []ErrorDetail
	var applied []struct{ name, fp string }
	var wg sync.WaitGroup
	for _, o := range ops {
		wg.Add(1)
		go func(o *op) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			e, fp := r.runOp(ctx, c, o)
			mu.Lock()
			errs = append(errs, e...)
			if fp != "" {
				applied = append(applied, struct{ name, fp string }{o.name, fp})
			}
			mu.Unlock()
		}(o)
	}
	wg.Wait()
	for _, o := range ops {
		if o.delete {
			delete(r.applied, o.name)
		} else if o.put {
			delete(r.applied, o.name)
		}
	}
	for _, a := range applied {
		r.applied[a.name] = a.fp
	}
	return errs
}

// retry runs fn up to 2 times; on final failure verify decides whether the
// desired state was reached anyway (a timeout does not mean go2rtc did not
// execute the request).
// retry runs fn; after a failure it first verifies the effect and repeats
// fn only when the effect is missing. go2rtc answers PUT /api/streams with
// 400 when it cannot write the stream into its config file although the
// stream was created – repeating the PUT would replace the stream object and
// orphan every consumer that attached in between (with its own camera
// connection).
func (r *Reconciler) retry(ctx context.Context, fn func(context.Context) error, verify func(context.Context) bool) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		octx, cancel := withOpTimeout(ctx, r.OpTimeout)
		err = fn(octx)
		cancel()
		if err == nil || ctx.Err() != nil {
			return err
		}
		if verify != nil {
			vctx, cancel := withOpTimeout(ctx, r.OpTimeout)
			ok := verify(vctx)
			cancel()
			if ok {
				return nil
			}
		}
		if attempt == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
	return err
}

func cleanErr(err error) string {
	if err == nil {
		return ""
	}
	msg := provider.Redact(err.Error())
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

func (r *Reconciler) runOp(ctx context.Context, c *Client, o *op) (errs []ErrorDetail, appliedFP string) {
	if o.dropPreload || o.repairPreload {
		err := r.retry(ctx, func(cx context.Context) error {
			err := c.DeletePreload(cx, o.name)
			var ae *apiError
			if errors.As(err, &ae) && ae.Status == 500 && strings.Contains(ae.Body, "not found") {
				return nil
			}
			return err
		}, func(cx context.Context) bool {
			p, _, err := c.Preloads(cx)
			return err == nil && !p[o.name]
		})
		if err != nil && !o.delete && o.desired != nil {
			r.log.Debug("drop preload failed", "stream", o.name, "error", cleanErr(err))
		}
	}
	if o.delete {
		if err := r.retry(ctx, func(cx context.Context) error { return c.DeleteStream(cx, o.name) }, nil); err != nil {
			// not counted: failures on unwanted streams must not inflate active errors
			r.log.Info("delete of obsolete stream failed", "stream", o.name, "error", cleanErr(err))
		}
		return nil, ""
	}
	if o.desired == nil {
		return nil, ""
	}
	if o.put {
		d := o.desired
		err := r.retry(ctx, func(cx context.Context) error { return c.PutStream(cx, d.Name, d.Sources) }, func(cx context.Context) bool {
			all, err := c.Streams(cx)
			if err != nil {
				return false
			}
			si, ok := all[d.Name]
			return ok && sourcesMatch(si, d.Sources)
		})
		if err != nil {
			r.log.Warn("stream registration failed", "stream", d.Name, "error", cleanErr(err))
			return []ErrorDetail{{Stream: d.Name, Kind: "stream", Message: "Registrierung fehlgeschlagen: " + cleanErr(err)}}, ""
		}
		appliedFP = d.fingerprint()
	}
	if o.addPreload || o.repairPreload {
		if o.repairPreload {
			r.log.Info("repairing stale preload", "stream", o.name)
		}
		err := r.retry(ctx, func(cx context.Context) error { return c.PutPreload(cx, o.name) }, func(cx context.Context) bool {
			p, _, err := c.Preloads(cx)
			if err != nil || !p[o.name] {
				return false
			}
			all, err := c.Streams(cx)
			if err != nil {
				return false
			}
			si, ok := all[o.name]
			return ok && si.HasPreloadConsumer()
		})
		if err != nil {
			errs = append(errs, ErrorDetail{Stream: o.name, Kind: "preload", Message: "Preload fehlgeschlagen: " + cleanErr(err)})
		}
	}
	return errs, appliedFP
}

// String helps debugging without leaking sources.
func (d *DesiredStream) String() string {
	return fmt.Sprintf("%s(%d sources, preload=%v)", d.Name, len(d.Sources), d.Preload)
}
