//go:build windows

package config

// Directory fsync is not available on Windows; rename is still atomic (MoveFileEx).
func syncDir(string) {}

func isUnsupported(error) bool { return true }
