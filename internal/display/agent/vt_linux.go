//go:build linux

package agent

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Linux VT ioctls (linux/vt.h)
const (
	vtGetState   = 0x5603
	vtActivate   = 0x5606
	vtWaitActive = 0x5607
)

// vtSwitcher brings the agent's own virtual terminal (the tty of its login
// session, stdin) to the foreground right before the compositor starts and
// gives the console back when there is nothing to show. The kernel allows
// this without root because the VT is the agent's controlling terminal.
type vtSwitcher struct {
	own      int // own VT number, 0 = not running on a VT (tests, ssh)
	previous int // VT that was active before we took over
}

func newVTSwitcher() *vtSwitcher {
	link, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		return &vtSwitcher{}
	}
	var n int
	if _, err := fmt.Sscanf(link, "/dev/tty%d", &n); err != nil || n <= 0 {
		return &vtSwitcher{}
	}
	return &vtSwitcher{own: n}
}

func vtIoctl(req, arg uintptr) syscall.Errno {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, 0, req, arg)
	return e
}

func (v *vtSwitcher) activeVT() (int, error) {
	var st [3]uint16 // struct vt_stat { v_active, v_signal, v_state }
	// pointer conversion directly in the Syscall call (see unsafe.Pointer rules)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, 0, vtGetState, uintptr(unsafe.Pointer(&st))); e != 0 {
		return 0, e
	}
	return int(st[0]), nil
}

// activate shows the agent's VT (the compositor needs an active session).
func (v *vtSwitcher) activate() error {
	if v.own == 0 {
		return nil
	}
	cur, err := v.activeVT()
	if err == nil && cur == v.own {
		return nil
	}
	if err == nil {
		v.previous = cur
	}
	if e := vtIoctl(vtActivate, uintptr(v.own)); e != 0 {
		return fmt.Errorf("Konsole tty%d kann nicht aktiviert werden: %v", v.own, e)
	}
	done := make(chan struct{})
	go func() { vtIoctl(vtWaitActive, uintptr(v.own)); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// release switches back to the previous console (tty1 if unknown) – only if
// our VT is still in the foreground, i.e. the user has not switched away.
func (v *vtSwitcher) release() {
	if v.own == 0 {
		return
	}
	prev := v.previous
	v.previous = 0
	if cur, err := v.activeVT(); err != nil || cur != v.own {
		return
	}
	if prev == 0 || prev == v.own {
		prev = 1
	}
	vtIoctl(vtActivate, uintptr(prev))
}
