package reolink

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"slices"
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
	cam  *sim.ReolinkServer
	rtsp *sim.RTSPServer
	drv  *driver
	env  *env
}

const pw = "reo-Pass-1"

func setup(t *testing.T, pass string, mod func(*sim.ReolinkServer)) *rig {
	t.Helper()
	clip := sim.Synthetic(40)
	rs := &sim.RTSPServer{User: "admin", Pass: pw, Route: sim.ReolinkRoute, Clip: func(int, int) *sim.Clip { return clip }}
	if err := rs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rs.Close)
	_, rp := hostPort(rs.Addr())
	cs := &sim.ReolinkServer{User: "admin", Pass: pw, RTSPPort: rp, Loc: time.UTC, Clip: func(int, bool) *sim.Clip { return clip }}
	if mod != nil {
		mod(cs)
	}
	if err := cs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.Close)
	h, hp := hostPort(cs.Addr())
	m, _ := prov{}.Info().FindModel(ProtoReolink)
	e := &env{}
	d, err := prov{}.NewDriver(provider.DeviceConfig{Model: m, Host: h, Username: "admin", Password: pass, Ports: map[string]int{"http": hp}}, e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return &rig{cam: cs, rtsp: rs, drv: d.(*driver), env: e}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A dual-lens PTZ camera with RTSP switched off: CNM logs in over HTTPS,
// switches RTSP and RTMP on, offers both lenses and steers them together.
func TestReolinkConnectDualLens(t *testing.T) {
	r := setup(t, pw, func(s *sim.ReolinkServer) { s.DualLens, s.PTZ, s.RTSPOff = true, true, true })
	ctx := ctxT(t)
	info, err := r.drv.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, hp := hostPort(r.cam.Addr())
	if info.Channels != 2 || info.ChannelNames[0] != "Sim Kamera Weitwinkel" || info.ChannelNames[1] != "Sim Kamera Tele" ||
		info.Variant != "https:"+strconv.Itoa(hp) || info.PTZ[0] != 0 || info.PTZ[1] != 0 || len(info.PTZ) != 2 {
		t.Fatalf("info %+v", info)
	}
	if !strings.Contains(info.Summary, "RTSP und RTMP an der Kamera eingeschaltet") || !strings.Contains(info.Summary, "zwei Objektiven") {
		t.Fatalf("summary %q", info.Summary)
	}
	if rtsp, rtmp := r.cam.PortsEnabled(); !rtsp || !rtmp {
		t.Fatal("ports not switched on")
	}
	rh, rp := hostPort(r.rtsp.Addr())
	main := r.drv.LiveSources(0, provider.Main)
	sub := r.drv.LiveSources(1, provider.Sub)
	if len(main) != 1 || !strings.HasSuffix(main[0], "@"+rh+":"+strconv.Itoa(rp)+"/Preview_01_main#backchannel=0#media=video") ||
		!strings.HasPrefix(main[0], "rtsp://admin:"+pw+"@") || !strings.Contains(sub[0], "/Preview_02_sub#") {
		t.Fatalf("live %v %v", main, sub)
	}
	if code, err := provider.RTSPDescribe(ctx, rh, rp, "admin", pw, "/Preview_02_sub"); err != nil || code != 200 {
		t.Fatalf("rtsp %d %v", code, err)
	}
	h, err := r.drv.Check(ctx)
	if err != nil || h.Storage != provider.StoragePresent || h.ClockOffset == nil || h.Degraded != "" {
		t.Fatalf("health %+v %v", h, err)
	}
}

// Plain HTTP on a custom port is found after HTTPS fails; a wrong password
// costs exactly one rejected login and locks further automatic logins.
func TestReolinkHTTPAndWrongPassword(t *testing.T) {
	r := setup(t, pw, func(s *sim.ReolinkServer) { s.HTTPOnly = true })
	info, err := r.drv.Connect(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	_, hp := hostPort(r.cam.Addr())
	if info.Variant != "http:"+strconv.Itoa(hp) || info.Channels != 1 || info.PTZ != nil || r.cam.LoginFail.Load() != 0 {
		t.Fatalf("info %+v", info)
	}
	if _, err := r.drv.PTZState(ctxT(t), 0); provider.KindOf(err) != provider.KindUnsupported {
		t.Fatalf("ptz on a fixed camera: %v", err)
	}

	bad := setup(t, "falsch", nil)
	_, err = bad.drv.Connect(ctxT(t))
	if provider.KindOf(err) != provider.KindAuth || bad.cam.LoginFail.Load() != 1 || !bad.env.locked.Load() {
		t.Fatalf("wrong password: %v, fails %d", err, bad.cam.LoginFail.Load())
	}
	if _, err := bad.drv.Connect(ctxT(t)); err != provider.ErrLoginBlocked || bad.cam.LoginFail.Load() != 1 {
		t.Fatalf("second attempt: %v", err)
	}
}

// Recordings are searched day by day; the tele lens is addressed as
// logical channel or, when the device refuses that, as channel 1.
func TestReolinkRecordings(t *testing.T) {
	for _, teleAsChannel := range []bool{false, true} {
		r := setup(t, pw, func(s *sim.ReolinkServer) { s.DualLens, s.TeleAsChannel = true, teleAsChannel })
		ctx := ctxT(t)
		if _, err := r.drv.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		to := time.Now().Truncate(15 * time.Minute)
		from := to.Add(-2 * time.Hour)
		for ch := 0; ch < 2; ch++ {
			recs, err := r.drv.SearchRecordings(ctx, ch, from, to)
			if err != nil || len(recs) != 8 {
				t.Fatalf("tele=%v ch %d: %v %v", teleAsChannel, ch, recs, err)
			}
			for _, rc := range recs {
				if rc.End.Sub(rc.Start) != 10*time.Minute || rc.Start.Minute()%15 != 0 {
					t.Fatalf("range %v", rc)
				}
			}
			if ok, err := r.drv.HasRecordings(ctx, ch, from, to); !ok || err != nil {
				t.Fatalf("has %v %v", ok, err)
			}
		}
		// over midnight: two searches, one per device day
		day := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
		recs, err := r.drv.SearchRecordings(ctx, 0, day.Add(-30*time.Minute), day.Add(30*time.Minute))
		if err != nil || len(recs) != 4 {
			t.Fatalf("midnight %v %v", recs, err)
		}
	}
}

// maxPTS returns the highest PES presentation time (90 kHz) in a TS stream.
func maxPTS(ts []byte) (int64, int) {
	var top int64 = -1
	n := 0
	for i := 0; i+188 <= len(ts); i += 188 {
		p := ts[i : i+188]
		if p[0] != 0x47 || p[1]&0x40 == 0 {
			continue
		}
		off := 4
		if p[3]&0x20 != 0 {
			off += 1 + int(p[4])
		}
		if off+14 > 188 || p[off] != 0 || p[off+1] != 0 || p[off+2] != 1 || p[off+3] != 0xE0 {
			continue
		}
		h := p[off+9:]
		pts := int64(h[0]>>1&0x07)<<30 | int64(binary.BigEndian.Uint16(h[1:3])>>1)<<15 | int64(binary.BigEndian.Uint16(h[3:5])>>1)
		top = max(top, pts)
		n++
	}
	return top, n
}

// Playback chains HTTP-FLV files from the requested instant; both kinds of
// timestamps after a seek give the same result.
func TestReolinkPlayback(t *testing.T) {
	for _, zero := range []bool{false, true} {
		r := setup(t, pw, func(s *sim.ReolinkServer) { s.DualLens, s.FLVZeroAtSeek = true, zero })
		ctx := ctxT(t)
		if _, err := r.drv.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		fileStart := time.Now().Add(-time.Hour).Truncate(15 * time.Minute)
		from := fileStart.Add(30 * time.Second)
		to := from.Add(20 * time.Second)
		src, err := r.drv.OpenPlayback(ctx, 1, from, to)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(src.Reader)
		src.Reader.Close()
		if err != nil || src.Format != "mpegts" {
			t.Fatalf("read %v", err)
		}
		top, frames := maxPTS(data)
		if top < 18*90000 || top > 20*90000 || frames < 150 {
			t.Fatalf("zero=%v: last pts %.1f s, %d frames", zero, float64(top)/90000, frames)
		}
		q := r.cam.LastFLV()
		if q.Get("seek") != "30" || q.Get("channel") != "0" || q.Get("type") != "2" || !strings.Contains(q.Get("start"), "RecM02_") {
			t.Fatalf("flv request %v", q)
		}

		// across a file end: the next file follows, the gap is left out
		from = fileStart.Add(10*time.Minute - 5*time.Second)
		to = fileStart.Add(15*time.Minute + 5*time.Second)
		src, err = r.drv.OpenPlayback(ctx, 0, from, to)
		if err != nil {
			t.Fatal(err)
		}
		data, _ = io.ReadAll(src.Reader)
		src.Reader.Close()
		if top, _ := maxPTS(data); top < 9*90000 || top > 11*90000 || r.cam.FLVOpens.Load() != 3 {
			t.Fatalf("chained: last pts %.1f s, opens %d", float64(top)/90000, r.cam.FLVOpens.Load())
		}
	}
}

// PTZ: state with presets, guard and tracking; moves, presets, guard and
// the automatic behaviour are sent to the head of both lenses.
func TestReolinkPTZ(t *testing.T) {
	r := setup(t, pw, func(s *sim.ReolinkServer) { s.DualLens, s.PTZ = true, true })
	ctx := ctxT(t)
	if _, err := r.drv.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := r.drv.PTZState(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	f := st.Features
	if !f.PanTilt || !f.Zoom || !f.Presets || !f.Guard || !f.Tracking || len(st.Presets) != 2 || st.Presets[1].Name != "Hof" ||
		st.Auto.Guard == nil || !*st.Auto.Guard || st.Auto.GuardSeconds != 60 || !st.Auto.GuardSet || st.Auto.Tracking == nil || !*st.Auto.Tracking {
		t.Fatalf("state %+v", st)
	}
	steps := []func() error{
		func() error { return r.drv.PTZMove(ctx, 0, -1, 0, 0) },
		func() error { return r.drv.PTZMove(ctx, 1, 0.5, 0.5, 0) },
		func() error { return r.drv.PTZMove(ctx, 1, 0, 0, 1) },
		func() error { return r.drv.PTZMove(ctx, 1, 0, 0, -0.25) },
		func() error { return r.drv.PTZMove(ctx, 1, 0, 0, 0) },
		func() error { return r.drv.PTZGotoPreset(ctx, 1, 2) },
		func() error { return r.drv.PTZGotoGuard(ctx, 0) },
		func() error { return r.drv.PTZSaveGuard(ctx, 0) },
	}
	for i, s := range steps {
		if err := s(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	want := []string{"Left:64", "RightUp:32", "ZoomInc:64", "ZoomDec:16", "Stop", "ToPos:32:2", "GuardToPos", "GuardSave"}
	if got := r.cam.PTZOps(); !slices.Equal(got, want) {
		t.Fatalf("ops %v", got)
	}
	off, on := false, true
	if err := r.drv.PTZSetAuto(ctx, 0, provider.PTZAuto{Tracking: &off, GuardSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	if en, _, tmo := r.cam.Guard(); !en || tmo != 300 || r.cam.Tracking() {
		t.Fatalf("paused: guard %v %d tracking %v", en, tmo, r.cam.Tracking())
	}
	if err := r.drv.PTZSetAuto(ctx, 0, provider.PTZAuto{Tracking: &on, Guard: &on, GuardSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if en, _, tmo := r.cam.Guard(); !en || tmo != 60 || !r.cam.Tracking() {
		t.Fatalf("restored: guard %v %d tracking %v", en, tmo, r.cam.Tracking())
	}
	// a guard time the camera refuses is reported
	if err := r.drv.PTZSetAuto(ctx, 0, provider.PTZAuto{GuardSeconds: 7200}); err == nil {
		t.Fatal("refused guard time accepted")
	}
}

// FLV with H.265 as codec 12: parameter sets go in front of keyframes.
func TestFLVHEVC(t *testing.T) {
	vps, sps, pps := []byte{0x40, 0x01, 0xAA}, []byte{0x42, 0x01, 0xBB}, []byte{0x44, 0x01, 0xCC}
	var b bytes.Buffer
	b.Write([]byte{'F', 'L', 'V', 1, 1, 0, 0, 0, 9, 0, 0, 0, 0})
	tag := func(ts uint32, body []byte) {
		b.Write([]byte{9, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), byte(ts >> 16), byte(ts >> 8), byte(ts), byte(ts >> 24), 0, 0, 0})
		b.Write(body)
		b.Write([]byte{0, 0, 0, byte(11 + len(body))})
	}
	rec := make([]byte, 22)
	rec[0] = 1
	rec[21] = 0x03
	rec = append(rec, 3)
	for i, n := range [][]byte{vps, sps, pps} {
		rec = append(rec, byte(32+i), 0, 1, 0, byte(len(n)))
		rec = append(rec, n...)
	}
	tag(0, append([]byte{0x1C, 0, 0, 0, 0}, rec...))
	idr := []byte{0x26, 0x01, 0x11}
	tag(40, append([]byte{0x1C, 1, 0, 0, 0, 0, 0, 0, byte(len(idr))}, idr...))
	trail := []byte{0x02, 0x01, 0x22}
	tag(80, append([]byte{0x2C, 1, 0, 0, 0, 0, 0, 0, byte(len(trail))}, trail...))

	rd := newFLVReader(&b)
	f1, err := rd.next()
	if err != nil || !f1.key || !f1.hevc || f1.ms != 40 {
		t.Fatalf("first %+v %v", f1, err)
	}
	want := []byte{0, 0, 0, 1, 0x40, 0x01, 0xAA, 0, 0, 0, 1, 0x42, 0x01, 0xBB, 0, 0, 0, 1, 0x44, 0x01, 0xCC, 0, 0, 0, 1, 0x26, 0x01, 0x11}
	if !bytes.Equal(f1.data, want) {
		t.Fatalf("keyframe % x", f1.data)
	}
	f2, err := rd.next()
	if err != nil || f2.key || !bytes.Equal(f2.data, []byte{0, 0, 0, 1, 0x02, 0x01, 0x22}) || f2.ms != 80 {
		t.Fatalf("second %+v %v", f2, err)
	}
	if _, err := rd.next(); err != io.EOF {
		t.Fatalf("end %v", err)
	}
}
