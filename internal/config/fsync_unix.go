//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"
)

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

func isUnsupported(err error) bool { return errors.Is(err, syscall.ENOTSUP) }
