//go:build windows

package agent

import "os/exec"

// The display agent only runs on Linux; these stubs keep the package
// buildable (and its logic testable) on Windows development machines.
func setProcAttrs(*exec.Cmd, bool) {}

func terminate(cmd *exec.Cmd, _ bool) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func kill(cmd *exec.Cmd, group bool) { terminate(cmd, group) }
