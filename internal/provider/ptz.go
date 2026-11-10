package provider

import (
	"context"
	"strconv"
)

// PTZ: pan, tilt and zoom of motorised cameras. A device reports in
// DeviceInfo.PTZ which of its camera channels can be steered and which
// mechanical head moves them (both lenses of a dual-lens camera share one
// head). CNM pauses the camera's own automatic behaviour (tracking, the
// return to its guard position) while a user steers and restores it
// afterwards.

// PTZFeatures of one camera channel.
type PTZFeatures struct {
	PanTilt bool `json:"panTilt"`
	Zoom    bool `json:"zoom"`
	Presets bool `json:"presets"`
	// Guard: the camera has a guard position it returns to by itself.
	Guard bool `json:"guard"`
	// Tracking: the camera follows people or vehicles by itself.
	Tracking bool `json:"tracking"`
}

// PTZPreset is a stored camera position.
type PTZPreset struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// PTZAuto is the automatic behaviour of a camera. Nil fields are not
// supported (in a reading) or not changed (when writing).
type PTZAuto struct {
	Tracking *bool `json:"tracking,omitempty"`
	// TrackingMode is the device's own tracking method, kept to restore it
	// exactly (0 = unknown or the device has none).
	TrackingMode int `json:"trackingMode,omitempty"`
	// Guard: the camera returns to its guard position after GuardSeconds
	// without movement (after tracking or manual control).
	Guard        *bool `json:"guard,omitempty"`
	GuardSeconds int   `json:"guardSeconds,omitempty"`
	// GuardSet: a guard position is stored (read only).
	GuardSet bool `json:"guardSet"`
}

// PTZState of a camera channel.
type PTZState struct {
	Features PTZFeatures `json:"features"`
	Presets  []PTZPreset `json:"presets"`
	Auto     PTZAuto     `json:"auto"`
}

// PTZController is implemented by drivers with PTZ control. channel is the
// camera channel (as in LiveSources).
type PTZController interface {
	PTZState(ctx context.Context, channel int) (*PTZState, error)
	// PTZMove starts a continuous movement: pan (right +), tilt (up +) and
	// zoom (in +) in -1..1, the magnitude is the speed. All zero stops.
	PTZMove(ctx context.Context, channel int, pan, tilt, zoom float64) error
	PTZGotoPreset(ctx context.Context, channel, id int) error
	// PTZGotoGuard moves to the guard position.
	PTZGotoGuard(ctx context.Context, channel int) error
	// PTZSaveGuard stores the current position as guard position.
	PTZSaveGuard(ctx context.Context, channel int) error
	// PTZSetAuto changes the automatic behaviour (only non-nil fields;
	// GuardSeconds only when > 0).
	PTZSetAuto(ctx context.Context, channel int, auto PTZAuto) error
}

// OptPTZReturn is the device option with the minutes after which CNM ends
// a manual PTZ control without input and the camera returns.
const OptPTZReturn = "ptzReturn"

// DefaultPTZReturn is the default of OptPTZReturn in minutes.
const DefaultPTZReturn = 5

// PTZReturnOption is the device option for protocols with PTZ.
func PTZReturnOption() OptionSpec {
	return OptionSpec{Key: OptPTZReturn, Label: "PTZ: Rückkehr nach (Minuten)", Type: OptionNumber, Min: 1, Max: 120,
		Default: strconv.Itoa(DefaultPTZReturn), Placeholder: strconv.Itoa(DefaultPTZReturn),
		Help: "Wie lange eine Kamera nach der letzten Eingabe in der Hand des Benutzers bleibt, bevor sie in ihre feste Position zurückfährt und wieder selbst verfolgt"}
}

// PTZReturnMinutes returns the configured return time of a device.
func PTZReturnMinutes(options map[string]string) int {
	if n, err := strconv.Atoi(options[OptPTZReturn]); err == nil && n > 0 {
		return min(n, 120)
	}
	return DefaultPTZReturn
}
