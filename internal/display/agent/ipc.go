// Package agent implements the CNM display agent: it runs the Wayland kiosk
// compositor (sway) on the server's GPU and one kiosk browser per configured
// output, following the desired state CNM publishes on its display socket.
package agent

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"cnm/internal/display"
)

// sway / i3 IPC message types
const (
	ipcRunCommand = 0
	ipcGetOutputs = 3
	ipcGetTree    = 4
	ipcGetVersion = 7
)

var ipcMagic = []byte("i3-ipc")

// IPC is a minimal sway IPC client (one request at a time).
type IPC struct {
	mu   sync.Mutex
	conn net.Conn
}

// DialIPC connects to the sway IPC socket.
func DialIPC(path string, timeout time.Duration) (*IPC, error) {
	c, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	return &IPC{conn: c}, nil
}

// Close closes the connection.
func (c *IPC) Close() error { return c.conn.Close() }

func (c *IPC) call(typ uint32, payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	msg := make([]byte, 14+len(payload))
	copy(msg, ipcMagic)
	binary.NativeEndian.PutUint32(msg[6:], uint32(len(payload)))
	binary.NativeEndian.PutUint32(msg[10:], typ)
	copy(msg[14:], payload)
	if _, err := c.conn.Write(msg); err != nil {
		return nil, err
	}
	hdr := make([]byte, 14)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		return nil, err
	}
	if string(hdr[:6]) != string(ipcMagic) {
		return nil, errors.New("sway ipc: invalid reply")
	}
	n := binary.NativeEndian.Uint32(hdr[6:])
	if n > 16<<20 {
		return nil, errors.New("sway ipc: reply too large")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return nil, err
	}
	if got := binary.NativeEndian.Uint32(hdr[10:]); got != typ {
		return nil, fmt.Errorf("sway ipc: unexpected reply type %d", got)
	}
	return body, nil
}

// Command runs a sway command; any failing sub-command is an error.
func (c *IPC) Command(cmd string) error {
	body, err := c.call(ipcRunCommand, []byte(cmd))
	if err != nil {
		return err
	}
	var res []struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	var errs []string
	for _, r := range res {
		if !r.Success {
			errs = append(errs, r.Error)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("sway: %s (%s)", strings.Join(errs, "; "), cmd)
	}
	return nil
}

// swayMode is a mode as reported by sway (refresh in mHz).
type swayMode struct {
	Width   int `json:"width"`
	Height  int `json:"height"`
	Refresh int `json:"refresh"`
}

func (m swayMode) mode() display.Mode {
	return display.Mode{Width: m.Width, Height: m.Height, RefreshMHz: m.Refresh}
}

type swayRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// swayOutput is an entry of GET_OUTPUTS.
type swayOutput struct {
	Name        string     `json:"name"`
	Make        string     `json:"make"`
	Model       string     `json:"model"`
	Serial      string     `json:"serial"`
	Active      bool       `json:"active"`
	Transform   string     `json:"transform"`
	Rect        swayRect   `json:"rect"`
	Modes       []swayMode `json:"modes"`
	CurrentMode *swayMode  `json:"current_mode"`
}

// Outputs returns all outputs (connected ones, active or not).
func (c *IPC) Outputs() ([]swayOutput, error) {
	body, err := c.call(ipcGetOutputs, nil)
	if err != nil {
		return nil, err
	}
	var out []swayOutput
	err = json.Unmarshal(body, &out)
	return out, err
}

// swayNode is a node of GET_TREE.
type swayNode struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Type           string     `json:"type"`
	PID            int        `json:"pid"`
	AppID          string     `json:"app_id"`
	FullscreenMode int        `json:"fullscreen_mode"`
	Nodes          []swayNode `json:"nodes"`
	FloatingNodes  []swayNode `json:"floating_nodes"`
}

// Tree returns the layout tree.
func (c *IPC) Tree() (*swayNode, error) {
	body, err := c.call(ipcGetTree, nil)
	if err != nil {
		return nil, err
	}
	var n swayNode
	err = json.Unmarshal(body, &n)
	return &n, err
}

// Version returns sway's human readable version.
func (c *IPC) Version() (string, error) {
	body, err := c.call(ipcGetVersion, nil)
	if err != nil {
		return "", err
	}
	var v struct {
		Human string `json:"human_readable"`
	}
	err = json.Unmarshal(body, &v)
	return v.Human, err
}

// view is an application window with the output it is on.
type view struct {
	ID         int64
	PID        int
	Title      string
	Output     string
	Fullscreen bool
}

// views flattens the tree into application windows.
func views(root *swayNode) []view {
	var out []view
	var walk func(n *swayNode, output string)
	walk = func(n *swayNode, output string) {
		if n.Type == "output" {
			output = n.Name
		}
		if (n.Type == "con" || n.Type == "floating_con") && n.PID > 0 {
			out = append(out, view{ID: n.ID, PID: n.PID, Title: n.Name, Output: output, Fullscreen: n.FullscreenMode == 1})
		}
		for i := range n.Nodes {
			walk(&n.Nodes[i], output)
		}
		for i := range n.FloatingNodes {
			walk(&n.FloatingNodes[i], output)
		}
	}
	if root != nil {
		walk(root, "")
	}
	return out
}

// quote quotes an argument for sway commands.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
