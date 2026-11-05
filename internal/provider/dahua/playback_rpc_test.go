package dahua

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"

	"cnm/internal/provider"
	"cnm/internal/sim"
)

type dhavFrame struct {
	typ  byte
	date time.Time
}

// parseDHAV lists the frames of a DHAV stream (date in device wall clock).
func parseDHAV(t *testing.T, b []byte) []dhavFrame {
	t.Helper()
	var out []dhavFrame
	for len(b) >= dhavHdr {
		if string(b[:4]) != "DHAV" {
			t.Fatalf("garbage in output at %d bytes before the end", len(b))
		}
		n := int(binary.LittleEndian.Uint32(b[12:16]))
		out = append(out, dhavFrame{typ: b[4], date: unpackTime(binary.LittleEndian.Uint32(b[16:20]))})
		b = b[n:]
	}
	return out
}

func wall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

// The cutter starts with the keyframe at or before the start, skips damaged
// bytes, continues across files and stops after the end.
func TestDHAVCut(t *testing.T) {
	clip := sim.Synthetic(10) // 10 fps, a keyframe every second
	t0 := time.Date(2026, 11, 5, 15, 0, 0, 0, time.Local)
	file1 := sim.DHAV(clip, t0, 600)                    // 15:00:00–15:00:59
	file2 := sim.DHAV(clip, t0.Add(5*time.Minute), 600) // 15:05:00–15:05:59
	// damaged bytes at the frame boundary before frame 100 (15:00:10)
	off := 0
	for i := 0; i < 100; i++ {
		off += int(binary.LittleEndian.Uint32(file1[off+12:]))
	}
	damaged := append(append(append([]byte{}, file1[:off]...), []byte("xxDHAVjunk")...), file1[off:]...)

	cut := &dhavCut{from: wall(t0.Add(20500 * time.Millisecond)).Truncate(time.Second), to: wall(t0.Add(5*time.Minute + 10*time.Second))}
	var out bytes.Buffer
	if done, err := cut.copy(bufio.NewReader(bytes.NewReader(damaged)), &out); done || err != nil {
		t.Fatalf("file 1: done=%v err=%v", done, err)
	}
	if done, err := cut.copy(bufio.NewReader(bytes.NewReader(file2)), &out); !done || err != nil {
		t.Fatalf("file 2: done=%v err=%v", done, err)
	}
	frames := parseDHAV(t, out.Bytes())
	if len(frames) == 0 || frames[0].typ != dhavIFrame || !frames[0].date.Equal(wall(t0.Add(20*time.Second))) {
		t.Fatalf("start %+v", frames[:min(len(frames), 1)])
	}
	last := frames[len(frames)-1]
	if !last.date.Equal(wall(t0.Add(5*time.Minute + 10*time.Second))) {
		t.Fatalf("end %v", last.date)
	}
	// 40 s of file 1 and 11 s of file 2 at 10 fps
	if len(frames) != 400+110 {
		t.Fatalf("%d frames", len(frames))
	}
	// a range inside a gap ends without output
	gap := &dhavCut{from: wall(t0.Add(2 * time.Minute)), to: wall(t0.Add(3 * time.Minute))}
	out.Reset()
	if done, _ := gap.copy(bufio.NewReader(bytes.NewReader(file2)), &out); !done || out.Len() != 0 {
		t.Fatalf("gap: done=%v %d bytes", done, out.Len())
	}
}

// startRecorder runs an RPC2 recorder with 4 channels, CGI download and an
// RTSP server that numbers playback channels from 1 or (zeroBased) from 0.
func startRecorder(t *testing.T, zeroBased bool) (*rpcDriver, *sim.RTSPServer) {
	t.Helper()
	clip := sim.Synthetic(10)
	srv := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 4, HasDisk: true, Recordings: sim.QuarterSchedule, DHAVClip: clip}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	r := &sim.RTSPServer{User: "admin", Pass: "pw123456", HasRecording: sim.InQuarterSchedule, ZeroBasedPlayback: zeroBased,
		Clip: func(ch, _ int) *sim.Clip {
			if ch < 1 || ch > 4 {
				return nil
			}
			return clip
		}}
	if err := r.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	h, hp := port(srv.Addr())
	_, rp := port(r.Addr())
	d := newRPCDriver(provider.DeviceConfig{ID: "d", Model: dahuaProto(), Host: h, Username: "admin", Password: "pw123456",
		Ports: map[string]int{"http": hp, "rtsp": rp}}, &env{}, VariantRPC2Classic)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Check(ctx); err != nil { // measures the device clock
		t.Fatal(err)
	}
	return d, r
}

// a finished recording of the quarter schedule (10 of every 15 minutes)
func pastSegment() time.Time {
	return time.Now().Add(-40 * time.Minute).Truncate(15 * time.Minute)
}

func TestPlaybackNumberingStandard(t *testing.T) {
	d, _ := startRecorder(t, false)
	ctx := context.Background()
	s := pastSegment()
	src, err := d.OpenPlayback(ctx, 2, s.Add(time.Minute), s.Add(2*time.Minute))
	if err != nil || !strings.Contains(src.URL, "/cam/playback?channel=3&") || src.Reader != nil {
		t.Fatalf("%+v %v", src, err)
	}
	if d.pbBase != pbFromOne {
		t.Fatalf("numbering %d", d.pbBase)
	}
}

// Field case: 4-channel firmware counts playback channels from 0 and refuses
// channel 0 over RTSP. Cameras 2–4 use the shifted RTSP number, camera 1 the
// CGI download, cut to the requested range.
func TestPlaybackNumberingFromZero(t *testing.T) {
	d, rtsp := startRecorder(t, true)
	ctx := context.Background()
	s := pastSegment()
	src, err := d.OpenPlayback(ctx, 2, s.Add(time.Minute), s.Add(2*time.Minute))
	if err != nil || !strings.Contains(src.URL, "/cam/playback?channel=2&") {
		t.Fatalf("%+v %v", src, err)
	}
	if d.pbBase != pbFromZero || rtsp.AuthFails.Load() != 0 {
		t.Fatalf("numbering %d, RTSP auth failures %d", d.pbBase, rtsp.AuthFails.Load())
	}

	// camera 1 inside one recording: from 2:30.4 to 2:50 of the segment
	from, to := s.Add(150*time.Second+400*time.Millisecond), s.Add(170*time.Second)
	frames := readPlayback(t, d, from, to)
	if frames[0].typ != dhavIFrame || !frames[0].date.Equal(wall(s.Add(150*time.Second))) {
		t.Fatalf("start %+v", frames[0])
	}
	if last := frames[len(frames)-1]; !last.date.Equal(wall(to)) || len(frames) != 210 {
		t.Fatalf("end %v, %d frames", last.date, len(frames))
	}

	// across the gap into the next recording (9:50–15:20)
	frames = readPlayback(t, d, s.Add(9*time.Minute+50*time.Second), s.Add(15*time.Minute+20*time.Second))
	first, last := frames[0].date, frames[len(frames)-1].date
	if !first.Equal(wall(s.Add(9*time.Minute+50*time.Second))) || !last.Equal(wall(s.Add(15*time.Minute+20*time.Second))) || len(frames) != 100+210 {
		t.Fatalf("across files: %v – %v, %d frames", first, last, len(frames))
	}

	// nothing recorded in the range
	if _, err := d.OpenPlayback(ctx, 0, s.Add(11*time.Minute), s.Add(13*time.Minute)); provider.KindOf(err) != provider.KindNotFound {
		t.Fatalf("gap: %v", err)
	}
}

func readPlayback(t *testing.T, d *rpcDriver, from, to time.Time) []dhavFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src, err := d.OpenPlayback(ctx, 0, from, to)
	if err != nil || src.Reader == nil || src.Format != "dhav" || src.StartupTimeout <= 0 {
		t.Fatalf("%+v %v", src, err)
	}
	defer src.Reader.Close()
	b, err := io.ReadAll(src.Reader)
	if err != nil {
		t.Fatal(err)
	}
	frames := parseDHAV(t, b)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	return frames
}
