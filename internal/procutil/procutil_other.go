//go:build !linux && !windows

package procutil

import "os/exec"

func setAttrs(*exec.Cmd) {}
