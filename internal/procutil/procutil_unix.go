//go:build !windows

package procutil

import (
	"os/exec"
	"syscall"
)

const exeSuffix = ""

func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(syscall.SIGTERM)
}
