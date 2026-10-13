//go:build !linux && !windows

package agent

import "syscall"

func setPdeathsig(*syscall.SysProcAttr) {}
