//go:build linux

package procutil

import (
	"os/exec"
	"syscall"
)

func setAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}
