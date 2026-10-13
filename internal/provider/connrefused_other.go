//go:build !windows

package provider

import (
	"errors"
	"syscall"
)

func isConnRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
