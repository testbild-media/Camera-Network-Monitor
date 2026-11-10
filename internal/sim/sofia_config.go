package sim

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// DVRIP configuration of the simulator, shaped like old 8-channel DVRs:
// Simplify.Encode with D1-class resolutions, Record and
// Detect.MotionDetect per channel with weekly schedule and mask, NTP and
// the general settings. Main streams accept D1/HD1/CIF/QCIF, extra streams
// CIF/QCIF; bitrates above 4096 kbit/s are clamped like real devices do.

func sofiaVideo(res string, fps, bitrate int) map[string]any {
	return map[string]any{"BitRate": bitrate, "BitRateControl": "VBR", "Compression": "H.264", "FPS": fps, "GOP": 2, "Quality": 4, "Resolution": res}
}

func sofiaWeek(on bool, mask string) (sections, masks []any) {
	for d := 0; d < 7; d++ {
		first := "0 00:00:00-24:00:00"
		m := "0x00000000"
		if on {
			first, m = "1 00:00:00-24:00:00", mask
		}
		sec := []any{first}
		ms := []any{m}
		for i := 1; i < 6; i++ {
			sec = append(sec, "0 00:00:00-24:00:00")
			ms = append(ms, "0x00000000")
		}
		sections = append(sections, sec)
		masks = append(masks, ms)
	}
	return
}

func (s *SofiaServer) initConfig() {
	var enc, recs, mds, losses, blinds []any
	for ch := 0; ch < s.Channels; ch++ {
		enc = append(enc, map[string]any{
			"MainFormat":  map[string]any{"AudioEnable": false, "Video": sofiaVideo("D1", 12, 1024), "VideoEnable": true},
			"ExtraFormat": map[string]any{"AudioEnable": false, "Video": sofiaVideo("CIF", 12, 256), "VideoEnable": true},
		})
		sec, ms := sofiaWeek(true, "0x00000007")
		recs = append(recs, map[string]any{"Mask": ms, "PacketLength": 60, "PreRecord": 5, "RecordMode": "ManualRecord", "Redundancy": false, "TimeSection": sec})
		mdSec, _ := sofiaWeek(true, "")
		mds = append(mds, map[string]any{"Enable": false, "Level": 3, "EventHandler": map[string]any{"RecordEnable": true, "RecordLatch": 10,
			"RecordMask": "0x00000000", "BeepEnable": false, "TimeSection": mdSec}})
		lossSec, _ := sofiaWeek(true, "")
		losses = append(losses, map[string]any{"Enable": true, "EventHandler": map[string]any{"RecordEnable": false, "RecordMask": "0x00000000",
			"BeepEnable": false, "TimeSection": lossSec}})
		blindSec, _ := sofiaWeek(false, "")
		blinds = append(blinds, map[string]any{"Enable": false, "Level": 3, "EventHandler": map[string]any{"RecordEnable": false, "RecordMask": "0x00000000",
			"BeepEnable": false, "TimeSection": blindSec}})
	}
	titles := []any{}
	for i := 0; i < s.Channels; i++ {
		titles = append(titles, fmt.Sprintf("XM CAM %d", i+1))
	}
	s.config = map[string]any{
		"Simplify.Encode": enc, "Record": recs, "Detect.MotionDetect": mds, "ChannelTitle": titles,
		"NetWork.NetNTP":       map[string]any{"Enable": true, "Server": map[string]any{"Address": "0x00000000", "Name": "pool.ntp.org", "Port": 123}, "TimeZone": 1, "UpdatePeriod": 10},
		"General.General":      map[string]any{"LocalNo": 0, "MachineName": "LocalHost", "OverWrite": "OverWrite"},
		"General.Location":     map[string]any{"VideoFormat": "PAL", "Language": "German"},
		"General.AutoMaintain": map[string]any{"AutoDeleteFilesDays": 0, "AutoRebootDay": "Tuesday", "AutoRebootHour": 1},
		"NetWork.NetCommon": map[string]any{"HostIP": "0x0B0200C0", "Submask": "0x00FFFFFF", "GateWay": "0x010200C0", "HostName": "LocalHost",
			"MAC": "00:12:41:aa:bb:cc", "HttpPort": 80, "TCPPort": 34567, "UDPPort": 34568, "MaxBps": 0, "MonMode": "TCP"},
		"NetWork.NetDNS":    map[string]any{"Address": "0x010200C0", "SpareAddress": "0x356433C6"},
		"NetWork.NetDHCP":   []any{map[string]any{"Enable": false, "Interface": "eth0"}},
		"Detect.LossDetect": losses, "Detect.BlindDetect": blinds,
		"Storage.StorageNotExist": map[string]any{"Enable": true, "EventHandler": map[string]any{"BeepEnable": false, "ShowInfo": true}},
		"Storage.StorageFailure":  map[string]any{"Enable": true, "EventHandler": map[string]any{"BeepEnable": false, "ShowInfo": true}},
		"Storage.StorageLowSpace": map[string]any{"Enable": false, "LowerLimit": 10, "EventHandler": map[string]any{"BeepEnable": false, "ShowInfo": true}},
	}
}

var reIndexed = regexp.MustCompile(`^(.+)\.\[(\d+)\]$`)

// getConfig resolves "Name" and "Name.[n]".
func (s *SofiaServer) getConfig(name string) (any, bool) {
	if v, ok := s.config[name]; ok {
		return configCopy(v), true
	}
	if m := reIndexed.FindStringSubmatch(name); m != nil {
		i, _ := strconv.Atoi(m[2])
		arr := asArr(s.config[m[1]])
		if i < len(arr) {
			return configCopy(arr[i]), true
		}
	}
	return nil, false
}

var sofiaMainRes = map[string]bool{"D1": true, "HD1": true, "CIF": true, "QCIF": true}
var sofiaExtraRes = map[string]bool{"CIF": true, "QCIF": true}

// encoder capacity like old 8-channel DVRs report it (pixels per second);
// like the real ones, the simulator runs above it without complaint
const (
	sofiaMaxEncodePower = 42172416
	sofiaMaxBitrate     = 32768
)

// sub stream resolutions allowed with a main resolution (QCIF main: QCIF sub only)
var sofiaExtraForMain = map[string]map[string]bool{"D1": sofiaExtraRes, "HD1": sofiaExtraRes, "CIF": sofiaExtraRes, "QCIF": {"QCIF": true}}

// setConfig stores a value; it returns a DVRIP Ret code (606 = refused).
func (s *SofiaServer) setConfig(name string, v any) int {
	if name == "Simplify.Encode" {
		for _, e := range asArr(v) {
			mainRes, _ := asMap(asMap(asMap(e)["MainFormat"])["Video"])["Resolution"].(string)
			subRes, _ := asMap(asMap(asMap(e)["ExtraFormat"])["Video"])["Resolution"].(string)
			if allowed := sofiaExtraForMain[mainRes]; allowed != nil && !allowed[subRes] {
				return 606
			}
			for key, allowed := range map[string]map[string]bool{"MainFormat": sofiaMainRes, "ExtraFormat": sofiaExtraRes} {
				f := asMap(asMap(e)[key])
				vid := asMap(f["Video"])
				res, _ := vid["Resolution"].(string)
				if !allowed[res] {
					return 606
				}
				if asNum(vid["BitRate"]) > 4096 {
					vid["BitRate"] = 4096
				}
			}
		}
	}
	if _, ok := s.config[name]; ok {
		s.config[name] = configCopy(v)
		s.ConfigWrites.Add(1)
		return 100
	}
	if m := reIndexed.FindStringSubmatch(name); m != nil {
		i, _ := strconv.Atoi(m[2])
		arr := asArr(s.config[m[1]])
		if i < len(arr) {
			arr[i] = configCopy(v)
			s.ConfigWrites.Add(1)
			return 100
		}
	}
	return 607
}

func (s *SofiaServer) encodeCapability() map[string]any {
	ex := []any{"0x00000018", "0x00000018", "0x00000000", "0x00000018", "0x00000010"} // per main resolution D1, HD1, BCIF, CIF, QCIF
	return map[string]any{"MaxEncodePower": sofiaMaxEncodePower, "MaxBitrate": sofiaMaxBitrate,
		"MaxEncodePowerPerChannel": []any{"0x00C15C00", "0x00C15C00", "0x00C15C00", "0x00C15C00"},
		"ExImageSizePerChannelEx":  []any{ex, ex, ex, ex}, "EncodeInfo": []any{
			map[string]any{"StreamType": "MainStream", "Enable": true, "CompressionMask": "0x00000080", "ResolutionMask": "0x0000001B"},
			map[string]any{"StreamType": "ExtraStream2", "Enable": true, "CompressionMask": "0x00000080", "ResolutionMask": "0x00000018"},
		}}
}

// sofiaNow is the device wall clock including a set offset.
func (s *SofiaServer) sofiaNow() time.Time {
	return DeviceNow().Add(s.clockOffset)
}
