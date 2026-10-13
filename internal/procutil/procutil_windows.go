//go:build windows

package procutil

import "os/exec"

const exeSuffix = ".exe"

func setAttrs(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
