package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"cnm/internal/config"
	"cnm/internal/sim"
)

// startRealGo2rtc runs the go2rtc binary named by CNM_GO2RTC_BIN (skipped
// otherwise) the way CNM does and returns its API base URL, output and
// process; its working directory is a fresh temporary directory.
func startRealGo2rtc(t *testing.T) (string, *bytes.Buffer, *exec.Cmd) {
	t.Helper()
	bin := os.Getenv("CNM_GO2RTC_BIN")
	if bin == "" {
		t.Skip("CNM_GO2RTC_BIN not set")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	dir := t.TempDir()
	cfg := config.Default(dir)
	cfg.System.Media.Go2rtcAPIListen = addr
	cfg.System.Media.Go2rtcRTSPListen = ""
	var out bytes.Buffer
	cmd := exec.Command(bin, "-config", string(GenerateConfig(cfg)))
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	base := "http://" + addr
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(base + "/api"); err == nil {
			resp.Body.Close()
			return base, &out, cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("go2rtc did not start: %s", out.String())
	return "", nil, nil
}

// Many cameras registered at once (CNM start, configuration import) must not
// crash or confuse go2rtc, and go2rtc must not write the stream sources (with
// DVR credentials) to disk.
func TestRealGo2rtcParallelRegistration(t *testing.T) {
	base, out, cmd := startRealGo2rtc(t)
	c := NewClient(base)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			octx, cancel := withOpTimeout(ctx, 6*time.Second)
			defer cancel()
			if err := c.PutStream(octx, fmt.Sprintf("cnm_t%02d_ch01_sub", i), []string{fmt.Sprintf("rtsp://admin:secret@127.0.0.1:9/%d", i)}); err != nil {
				t.Errorf("put %d: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			_, _ = c.Streams(ctx)
		}()
	}
	wg.Wait()
	streams, err := c.Streams(ctx)
	if err != nil || len(streams) != n {
		t.Fatalf("%d streams, %v\n%s", len(streams), err, out.String())
	}
	for i := 0; i < n; i += 2 {
		if err := c.DeleteStream(ctx, fmt.Sprintf("cnm_t%02d_ch01_sub", i)); err != nil {
			t.Fatal(err)
		}
	}
	if streams, _ := c.Streams(ctx); len(streams) != n/2 {
		t.Fatalf("%d streams after deleting half", len(streams))
	}
	if cmd.ProcessState != nil || strings.Contains(out.String(), "panic") || strings.Contains(out.String(), "fatal error") {
		t.Fatalf("go2rtc failed:\n%s", out.String())
	}
	if ents, _ := os.ReadDir(cmd.Dir); len(ents) != 0 {
		t.Fatalf("go2rtc wrote %s", ents[0].Name())
	}
}

// The ONVIF camera model's fallback source is go2rtc's own ONVIF client
// with the exact profile token: go2rtc resolves it (GetCapabilities,
// GetProfiles, GetStreamUri) and then pulls RTSP.
func TestRealGo2rtcONVIFSource(t *testing.T) {
	base, out, _ := startRealGo2rtc(t)
	clip := sim.Synthetic(20)
	rs := &sim.RTSPServer{User: "admin", Pass: "onvif-pass-1", Route: sim.ONVIFRoute, Clip: func(int, int) *sim.Clip { return clip }}
	if err := rs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	ov := &sim.ONVIFServer{User: "admin", Pass: "onvif-pass-1", Channels: 1, SubStreams: true, RTSPAddr: rs.Addr()}
	if err := ov.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer ov.Close()
	c := NewClient(base)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, sub := range []string{sim.ProfileToken(0, false), sim.ProfileToken(0, true)} {
		name := "cnm_onvif_ch01_" + sub
		// go2rtc answers its API before all modules registered their source
		// schemes ("source not supported" right after the start)
		var err error
		for i := 0; i < 40; i++ {
			if err = c.PutStream(ctx, name, []string{"onvif://admin:onvif-pass-1@" + ov.Addr() + "?subtype=" + sub + "#backchannel=0#media=video"}); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		// a probe consumer makes go2rtc dial the source and report its media
		resp, err := http.Get(base + "/api/streams?" + url.Values{"src": {name}, "video": {"all"}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), "H264") || !strings.Contains(string(b), "rtsp://") {
			t.Fatalf("subtype %s: %d %s\n%s", sub, resp.StatusCode, b, out.String())
		}
	}
}
