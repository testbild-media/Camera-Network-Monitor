package reolink

import (
	"context"
	"encoding/json"
	"math"

	"cnm/internal/provider"
)

// head returns the device channel that moves a camera channel.
func (d *driver) head(channel int) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if channel < 0 || channel >= max(d.channels, 1) {
		return 0, provider.NewError(provider.KindConfig, nil, "Kanal %d gibt es nicht", channel+1)
	}
	h := channel
	if d.dualLens {
		h = 0
	}
	if ptzType(d.abilities, h) == 0 {
		return 0, provider.NewError(provider.KindUnsupported, nil, "Diese Kamera lässt sich nicht schwenken oder zoomen")
	}
	return h, nil
}

func (d *driver) features(h int) provider.PTZFeatures {
	d.mu.RLock()
	a := d.abilities
	d.mu.RUnlock()
	t := ptzType(a, h)
	f := provider.PTZFeatures{
		PanTilt: t == 2 || t == 3 || t == 5 || t == 6 || t == 7,
		Zoom:    t == 1 || t == 2 || t == 5 || a.ver(h, "supportDigitalZoom") > 0,
	}
	f.Presets = f.PanTilt && a.ver(h, "ptzPreset") > 0
	f.Guard = f.PanTilt && a.ver(h, "GetPtzGuard") > 0
	f.Tracking = a.ver(h, "aiTrack") > 0
	return f
}

type guardCfg struct {
	BEnable   int `json:"benable"`
	BExistPos int `json:"bexistPos"`
	Timeout   int `json:"timeout"`
}

func (d *driver) PTZState(ctx context.Context, channel int) (*provider.PTZState, error) {
	h, err := d.head(channel)
	if err != nil {
		return nil, err
	}
	st := &provider.PTZState{Features: d.features(h), Presets: []provider.PTZPreset{}}
	var cmds []cmd
	if st.Features.Presets {
		cmds = append(cmds, cmd{Cmd: "GetPtzPreset", Param: map[string]any{"channel": h}})
	}
	if st.Features.Guard {
		cmds = append(cmds, cmd{Cmd: "GetPtzGuard", Param: map[string]any{"channel": h}})
	}
	if st.Features.Tracking {
		cmds = append(cmds, cmd{Cmd: "GetAiCfg", Param: map[string]any{"channel": h}})
	}
	if len(cmds) == 0 {
		return st, nil
	}
	res, err := d.api.do(ctx, cmds...)
	if err != nil {
		return nil, err
	}
	for _, r := range res {
		if r.Code != 0 {
			continue
		}
		switch r.Cmd {
		case "GetPtzPreset":
			var v struct {
				PtzPreset []struct {
					ID     int    `json:"id"`
					Name   string `json:"name"`
					Enable int    `json:"enable"`
				} `json:"PtzPreset"`
			}
			if json.Unmarshal(r.Value, &v) == nil {
				for _, p := range v.PtzPreset {
					if p.Enable == 1 {
						st.Presets = append(st.Presets, provider.PTZPreset{ID: p.ID, Name: p.Name})
					}
				}
			}
		case "GetPtzGuard":
			var v struct {
				PtzGuard guardCfg `json:"PtzGuard"`
			}
			if json.Unmarshal(r.Value, &v) == nil {
				on := v.PtzGuard.BEnable == 1
				st.Auto.Guard = &on
				st.Auto.GuardSeconds = v.PtzGuard.Timeout
				st.Auto.GuardSet = v.PtzGuard.BExistPos == 1
			}
		case "GetAiCfg":
			var v map[string]any
			if json.Unmarshal(r.Value, &v) == nil {
				on, mode := trackingOf(v)
				st.Auto.Tracking = &on
				st.Auto.TrackingMode = mode
			}
		}
	}
	return st, nil
}

// trackingOf reads GetAiCfg: newer firmware switches with "bSmartTrack" and
// keeps the method in "aiTrack"; older firmware has only "aiTrack" (0 = off,
// otherwise the method).
func trackingOf(v map[string]any) (on bool, mode int) {
	ai := numOf(v["aiTrack"])
	if s, ok := v["bSmartTrack"]; ok {
		return numOf(s) == 1, ai
	}
	return ai != 0, ai
}

// ops of PtzCtrl by direction (pan, tilt signs).
var moveOps = map[[2]int]string{
	{-1, 0}: "Left", {1, 0}: "Right", {0, 1}: "Up", {0, -1}: "Down",
	{-1, 1}: "LeftUp", {-1, -1}: "LeftDown", {1, 1}: "RightUp", {1, -1}: "RightDown",
}

func sign(v float64) int {
	switch {
	case v > 0.05:
		return 1
	case v < -0.05:
		return -1
	}
	return 0
}

func (d *driver) PTZMove(ctx context.Context, channel int, pan, tilt, zoom float64) error {
	h, err := d.head(channel)
	if err != nil {
		return err
	}
	p := map[string]any{"channel": h, "op": "Stop"}
	if op, ok := moveOps[[2]int{sign(pan), sign(tilt)}]; ok {
		p["op"] = op
		speed := int(math.Round(math.Max(math.Abs(pan), math.Abs(tilt)) * 64))
		p["speed"] = min(max(speed, 1), 64)
	} else if z := sign(zoom); z != 0 {
		p["op"] = "ZoomInc"
		if z < 0 {
			p["op"] = "ZoomDec"
		}
		p["speed"] = min(max(int(math.Round(math.Abs(zoom)*64)), 1), 64)
	}
	return d.api.call(ctx, "PtzCtrl", p, nil)
}

func (d *driver) PTZGotoPreset(ctx context.Context, channel, id int) error {
	h, err := d.head(channel)
	if err != nil {
		return err
	}
	return d.api.call(ctx, "PtzCtrl", map[string]any{"channel": h, "op": "ToPos", "id": id, "speed": 32}, nil)
}

func (d *driver) PTZGotoGuard(ctx context.Context, channel int) error {
	h, err := d.head(channel)
	if err != nil {
		return err
	}
	return d.api.call(ctx, "SetPtzGuard", map[string]any{"PtzGuard": map[string]any{"channel": h, "cmdStr": "toPos"}}, nil)
}

func (d *driver) PTZSaveGuard(ctx context.Context, channel int) error {
	h, err := d.head(channel)
	if err != nil {
		return err
	}
	return d.api.call(ctx, "SetPtzGuard", map[string]any{"PtzGuard": map[string]any{"channel": h, "cmdStr": "setPos", "bSaveCurrentPos": 1}}, nil)
}

func (d *driver) PTZSetAuto(ctx context.Context, channel int, auto provider.PTZAuto) error {
	h, err := d.head(channel)
	if err != nil {
		return err
	}
	if auto.Guard != nil || auto.GuardSeconds > 0 {
		g := map[string]any{"channel": h, "cmdStr": "setPos"}
		if auto.Guard != nil {
			g["benable"] = boolInt(*auto.Guard)
		}
		if auto.GuardSeconds > 0 {
			g["timeout"] = auto.GuardSeconds
		}
		if err := d.api.call(ctx, "SetPtzGuard", map[string]any{"PtzGuard": g}, nil); err != nil {
			return err
		}
	}
	if auto.Tracking != nil {
		var cur map[string]any
		if err := d.api.call(ctx, "GetAiCfg", map[string]any{"channel": h}, &cur); err != nil {
			return err
		}
		p := map[string]any{"channel": h}
		_, hasSwitch := cur["bSmartTrack"]
		switch {
		case hasSwitch:
			p["bSmartTrack"] = boolInt(*auto.Tracking)
		case *auto.Tracking:
			mode := auto.TrackingMode
			if mode <= 0 {
				mode = 1
			}
			p["aiTrack"] = mode
		default:
			p["aiTrack"] = 0
		}
		if err := d.api.call(ctx, "SetAiCfg", p, nil); err != nil {
			return err
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
