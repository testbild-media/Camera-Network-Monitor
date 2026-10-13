package tsmux

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cnm/internal/sim"
)

func TestTSDecodableByFFmpeg(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	clip, err := sim.Generate(ff, 320, 180, 10, 3, 0)
	if err != nil {
		t.Skip(err)
	}
	var buf bytes.Buffer
	m := New(&buf, StreamTypeH264)
	for i, au := range clip.AUs {
		if err := m.WriteFrame(au.Data, uint64(90000+i*9000), au.Key); err != nil {
			t.Fatal(err)
		}
	}
	if buf.Len()%188 != 0 {
		t.Fatalf("output is not a multiple of 188 bytes: %d", buf.Len())
	}
	for i := 0; i < buf.Len(); i += 188 {
		if buf.Bytes()[i] != 0x47 {
			t.Fatalf("lost sync at %d", i)
		}
	}
	p := filepath.Join(t.TempDir(), "x.ts")
	os.WriteFile(p, buf.Bytes(), 0o644)
	out, err := exec.Command(ff, "-hide_banner", "-v", "error", "-i", p, "-f", "null", "-").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("ffmpeg reported problems: %v %s", err, out)
	}
	// one framecrc line per packet – independent of the FFmpeg version's progress output
	crc, err := exec.Command(ff, "-hide_banner", "-v", "error", "-i", p, "-map", "0:v", "-c", "copy", "-f", "framecrc", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	packets := 0
	for _, line := range strings.Split(string(crc), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			packets++
		}
	}
	if packets != len(clip.AUs) {
		t.Fatalf("expected %d video packets (one per frame), got %d", len(clip.AUs), packets)
	}
}
