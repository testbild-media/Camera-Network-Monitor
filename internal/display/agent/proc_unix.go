//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setProcAttrs: children die with the agent (Linux) and browsers get their
// own process group so that all their helper processes can be stopped.
func setProcAttrs(cmd *exec.Cmd, group bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: group}
	setPdeathsig(cmd.SysProcAttr)
}

func signal(cmd *exec.Cmd, group bool, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	if group {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
		return
	}
	_ = cmd.Process.Signal(sig)
}

func terminate(cmd *exec.Cmd, group bool) { signal(cmd, group, syscall.SIGTERM) }
func kill(cmd *exec.Cmd, group bool)      { signal(cmd, group, syscall.SIGKILL) }
