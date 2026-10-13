// Package buildinfo carries the version stamped at build time:
//
//	go build -ldflags "-X cnm/internal/buildinfo.Version=1.0.0"
package buildinfo

// Version of this build.
var Version = "1.0.0-dev"
