//go:build !linux

package display

import "net"

// Peer credentials are only checked on Linux (the only supported platform
// for local displays); elsewhere the socket is a development aid.
const peerCredSupported = false

func peerUID(net.Conn) (int, bool) { return -1, false }
