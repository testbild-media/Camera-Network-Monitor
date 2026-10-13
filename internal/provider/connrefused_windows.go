package provider

import (
	"errors"
	"syscall"
)

// Windows reports a closed port as WSAECONNREFUSED, not syscall.ECONNREFUSED.
const wsaeConnRefused syscall.Errno = 10061

func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, wsaeConnRefused)
}
