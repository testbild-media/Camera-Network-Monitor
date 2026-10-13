// Package procutil starts and stops helper processes (go2rtc, FFmpeg) so
// that they never outlive CNM and are stopped quickly on request.
package procutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Prepare configures cmd so the child dies with CNM (Linux: PDEATHSIG) and
// ctx cancellation first asks politely, then kills after grace.
func Prepare(cmd *exec.Cmd, grace time.Duration) {
	setAttrs(cmd)
	cmd.Cancel = func() error { return terminate(cmd) }
	cmd.WaitDelay = grace
}

// Resolve finds an executable: explicit path, next to the CNM binary, the
// install dir, then PATH.
func Resolve(configured, name string) string {
	if configured != "" {
		if p, err := exec.LookPath(configured); err == nil {
			return p
		}
		return configured
	}
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name+exeSuffix))
	}
	candidates = append(candidates, filepath.Join("/opt/camera-network-monitor/bin", name))
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}
