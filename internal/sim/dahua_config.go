package sim

import (
	"encoding/json"
	"fmt"
	"time"
)

// Dahua RPC2 configuration of the simulator (configManager), shaped like
// the tables of real recorders: three main stream profiles (regular, motion,
// alarm), a sub stream, recording mode and weekly schedule, motion detection
// with detection windows (V3.0), NTP and storage group.

func dahuaVideo(w, h, fps, bitrate int, control string) map[string]any {
	return map[string]any{"BitRate": bitrate, "BitRateControl": control, "Compression": "H.264", "FPS": fps, "GOP": fps,
		"Height": h, "Pack": "DHAV", "Profile": "Main", "Quality": 4, "QualityRange": 6, "Width": w}
}

func dahuaFormat(video map[string]any) map[string]any {
	return map[string]any{"AudioEnable": false, "Audio": map[string]any{"Compression": "G.711A"}, "Video": video, "VideoEnable": true}
}

func dahuaWeek(mask int) []any {
	var week []any
	for d := 0; d < 8; d++ {
		m := mask
		if d == 7 {
			m = 0 // holiday
		}
		day := []any{fmt.Sprintf("%d 00:00:00-24:00:00", m)}
		for i := 1; i < 6; i++ {
			day = append(day, "0 00:00:00-24:00:00")
		}
		week = append(week, day)
	}
	return week
}

// initConfig creates the default configuration.
func (d *DahuaServer) initConfig() {
	var enc, titles, modes, recs, mds, losses, blinds []any
	for ch := 0; ch < d.Channels; ch++ {
		enc = append(enc, map[string]any{
			"MainFormat":  []any{dahuaFormat(dahuaVideo(1920, 1080, 25, 4096, "CBR")), dahuaFormat(dahuaVideo(1920, 1080, 25, 4096, "CBR")), dahuaFormat(dahuaVideo(1280, 720, 25, 2048, "CBR"))},
			"ExtraFormat": []any{dahuaFormat(dahuaVideo(352, 288, 12, 224, "VBR")), dahuaFormat(dahuaVideo(352, 288, 25, 640, "CBR")), dahuaFormat(dahuaVideo(352, 288, 25, 640, "CBR"))},
			"SnapFormat":  []any{map[string]any{"Video": dahuaVideo(352, 288, 1, 32, "CBR"), "VideoEnable": true}},
		})
		titles = append(titles, map[string]any{"Name": fmt.Sprintf("SIM CAM %d", ch+1)})
		modes = append(modes, map[string]any{"Mode": 0, "ModeExtra1": 2})
		recs = append(recs, map[string]any{"HolidayEnable": false, "PreRecord": 4, "Redundancy": false, "TimeSection": dahuaWeek(1)})
		mds = append(mds, map[string]any{"DetectVersion": "V3.0", "Enable": true, "Level": 3,
			"EventHandler":       map[string]any{"RecordChannels": []any{ch}, "RecordEnable": true, "RecordLatch": 10, "TimeSection": dahuaWeek(1)[:7]},
			"MotionDetectWindow": []any{map[string]any{"Id": 0, "Name": "Region1", "Sensitive": 80, "Threshold": 0}}})
		losses = append(losses, map[string]any{"Enable": true,
			"EventHandler": map[string]any{"BeepEnable": false, "RecordChannels": []any{}, "RecordEnable": false, "TimeSection": dahuaWeek(1)[:7]}})
		blinds = append(blinds, map[string]any{"Enable": false, "Level": 3,
			"EventHandler": map[string]any{"BeepEnable": false, "RecordChannels": []any{}, "RecordEnable": false, "TimeSection": dahuaWeek(0)[:7]}})
	}
	d.config = map[string]any{
		"Encode": enc, "ChannelTitle": titles, "RecordMode": modes, "Record": recs, "MotionDetect": mds,
		"NTP":          map[string]any{"Address": "pool.ntp.org", "Enable": false, "Port": 123, "TimeZone": 1, "UpdatePeriod": 60},
		"StorageGroup": []any{map[string]any{"FileHoldTime": 0, "OverWrite": 1}},
		"LossDetect":   losses, "BlindDetect": blinds,
		"StorageNotExist": map[string]any{"Enable": true, "EventHandler": map[string]any{"BeepEnable": true, "TipEnable": true}},
		"StorageFailure":  map[string]any{"Enable": true, "EventHandler": map[string]any{"BeepEnable": true, "TipEnable": true}},
		"StorageLowSpace": map[string]any{"Enable": false, "LowerLimit": 10, "EventHandler": map[string]any{"BeepEnable": false, "TipEnable": true}},
		"General":         map[string]any{"LocalNo": 8, "MachineName": "SIM"},
		"Locales":         map[string]any{"DSTEnable": false, "TimeFormat": "yyyy-MM-dd HH:mm:ss", "VideoStandard": "PAL"},
		"Network": map[string]any{"DefaultInterface": "eth0", "Domain": "local", "Hostname": "SIMDVR",
			"eth0": map[string]any{"IPAddress": "192.0.2.108", "SubnetMask": "255.255.255.0", "DefaultGateway": "192.0.2.1",
				"DnsServers": []any{"192.0.2.1", "198.51.100.53"}, "DhcpEnable": false, "PhysicalAddress": "4c:11:bf:00:00:01", "MTU": 1500}},
		"DVRIP": map[string]any{"TCPPort": 37777, "UDPPort": 37778, "MaxConnections": 10},
		"Web":   map[string]any{"Enable": true, "Port": 80},
		"RTSP":  map[string]any{"Enable": true, "Port": 554},
	}
}

// configCopy returns a deep copy of a configuration table.
func configCopy(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// setConfig stores a table like the device: unknown names are refused,
// encodings above 25 fps refused, bitrates above 6144 kbit/s clamped.
func (d *DahuaServer) setConfig(name string, table any) (ok bool) {
	if _, known := d.config[name]; !known {
		return false
	}
	if name == "Encode" {
		for _, e := range asArr(table) {
			for _, key := range []string{"MainFormat", "ExtraFormat"} {
				for _, f := range asArr(asMap(e)[key]) {
					v := asMap(asMap(f)["Video"])
					if asNum(v["FPS"]) > 25 || asNum(v["Width"]) > 2600 {
						return false
					}
					if asNum(v["BitRate"]) > 6144 {
						v["BitRate"] = 6144
					}
				}
			}
		}
	}
	d.config[name] = configCopy(table)
	d.ConfigWrites.Add(1)
	return true
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asArr(v any) []any          { a, _ := v.([]any); return a }
func asNum(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

// deviceTime is the simulated wall clock (offset by global.setCurrentTime).
func (d *DahuaServer) deviceTime() time.Time {
	return time.Now().Add(d.clockOffset).In(d.Loc)
}
