package display

import "time"

// Protocol between CNM (host) and the display agent. The agent talks HTTP
// over a local Unix socket:
//
//	GET  /v1/desired?rev=N&epoch=E  long poll (≤ 25 s): returns as soon as
//	                        the revision or the CNM instance (epoch) differs
//	POST /v1/status         agent status; the response is the current Desired
//
// Desired contains kiosk credentials and is therefore only served on the
// socket, to the agent user (peer credentials are checked).

// Desired is what CNM wants the local displays to show.
type Desired struct {
	Epoch        string          `json:"epoch"` // changes with every CNM start
	Rev          int64           `json:"rev"`
	Browser      string          `json:"browser"`                // auto | chromium | firefox
	DailyRestart string          `json:"dailyRestart,omitempty"` // "HH:MM" local time, "" = off
	Outputs      []DesiredOutput `json:"outputs"`
}

// DesiredOutput describes one configured connector.
type DesiredOutput struct {
	DisplayID    string `json:"displayId"`
	Connector    string `json:"connector"`
	Enabled      bool   `json:"enabled"`             // false → output switched off
	Mode         string `json:"mode"`                // "" → preferred mode
	Preferred    string `json:"preferred,omitempty"` // preferred mode from EDID (hint)
	Rotation     int    `json:"rotation"`            // 0, 90, 180, 270
	URL          string `json:"url,omitempty"`       // kiosk URL incl. credential (secret)
	MonitorLabel string `json:"monitorLabel,omitempty"`
	ReloadSeq    int    `json:"reloadSeq"`
	// ViewerDownMs is how long the kiosk viewer of this display has not been
	// connected to CNM (0 = connected). The agent restarts a browser whose
	// page stays disconnected although CNM is running.
	ViewerDownMs int64 `json:"viewerDownMs"`
}

// Status is reported by the agent.
type Status struct {
	Version    string           `json:"version"`
	Compositor CompositorStatus `json:"compositor"`
	Browser    BrowserInfo      `json:"browser"`
	// Installed lists the usable kiosk browsers ("firefox", "chromium";
	// snap packages do not count) – the settings offer only these.
	Installed []string        `json:"installed"`
	Outputs   []OutputStatus  `json:"outputs"`
	Displays  []DisplayStatus `json:"displays"`
	Error     string          `json:"error,omitempty"`
}

// CompositorStatus describes the Wayland compositor (sway).
type CompositorStatus struct {
	State   string    `json:"state"` // starting | running | failed | missing
	Version string    `json:"version,omitempty"`
	Error   string    `json:"error,omitempty"`
	Since   time.Time `json:"since"`
}

// BrowserInfo describes the kiosk browser the agent uses.
type BrowserInfo struct {
	Kind    string `json:"kind,omitempty"` // chromium | firefox
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// OutputStatus is a compositor output.
type OutputStatus struct {
	Name     string `json:"name"`
	Make     string `json:"make,omitempty"`
	Model    string `json:"model,omitempty"`
	Serial   string `json:"serial,omitempty"`
	Active   bool   `json:"active"`
	Current  *Mode  `json:"current,omitempty"`
	Modes    []Mode `json:"modes"`
	Rotation int    `json:"rotation"`
}

// Display states reported by the agent.
const (
	StateOff        = "off"        // disabled in CNM
	StateNoOutput   = "no-output"  // connector not connected
	StateStarting   = "starting"   // browser launched, page not yet placed
	StateRunning    = "running"    // browser fullscreen on its output
	StateRestarting = "restarting" // waiting for the next start attempt (backoff)
	StateError      = "error"
)

// DisplayStatus is the kiosk browser state of one configured display.
type DisplayStatus struct {
	DisplayID string    `json:"displayId"`
	Connector string    `json:"connector"`
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
	Restarts  int       `json:"restarts"`
	Error     string    `json:"error,omitempty"`
	Mode      string    `json:"mode,omitempty"` // mode actually applied
}
