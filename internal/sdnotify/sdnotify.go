// Package sdnotify implements the systemd notify protocol (READY, STOPPING,
// WATCHDOG) without external dependencies. Outside systemd all calls are no-ops.
package sdnotify

import (
	"net"
	"os"
	"strconv"
	"time"
)

// Send writes a state string to $NOTIFY_SOCKET.
func Send(state string) bool {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return false
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return false
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err == nil
}

// WatchdogInterval returns the interval at which WATCHDOG=1 must be sent (half
// of WatchdogSec), or 0 when the watchdog is not enabled for this process.
func WatchdogInterval() time.Duration {
	us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || us <= 0 {
		return 0
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(us) * time.Microsecond / 2
}
