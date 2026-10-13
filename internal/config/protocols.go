package config

import (
	"strconv"
	"strings"
)

// Protocol ids (Device.Protocol). They match the provider registrations
// (checked by the provider tests); the admin chooses only the protocol,
// variants and channels are detected and stored in Device.Detected.
const (
	ProtoDahua     = "dahua"
	ProtoSofia     = "sofia"
	ProtoHikvision = "hikvision"
	ProtoAxis      = "axis"
	ProtoUniview   = "uniview"
	ProtoONVIF     = "onvif"
	ProtoRTSP      = "rtsp"
	ProtoRTMP      = "rtmp"
	ProtoHTTP      = "http"
	ProtoTCP       = "tcp"
)

// Detection is what CNM found out about a device at its first connection
// (written by CNM, never by the admin).
type Detection struct {
	// Variant is handed back to the driver so it connects the same way again
	// (e.g. "rpc2-dahua3", "native", "dvrip").
	Variant string `json:"variant"`
	// Summary describes it for the admin ("RPC2 über HTTP 80 · …").
	Summary  string `json:"summary,omitempty"`
	Channels int    `json:"channels,omitempty"`
}

// Variant returns the detected variant ("" = not detected yet).
func (d *Device) Variant() string {
	if d.Detected == nil {
		return ""
	}
	return d.Detected.Variant
}

// legacyProtocol maps the manufacturer/model pair of configurations up to
// schema 3 onto a protocol. Where the old model fixed a login variant or a
// channel count, it is returned so migrated devices connect exactly as
// before (no new detection). Unknown pairs return "".
func legacyProtocol(manufacturer, model string) (protocol, variant string, channels int) {
	man := strings.ToLower(strings.TrimSpace(manufacturer))
	mod := strings.ToLower(strings.NewReplacer(" ", "", "_", "", "-", "").Replace(strings.TrimSpace(model)))
	switch man {
	case "lupus", "lupus-dahua", "lupus electronics", "dahua", "amcrest":
		switch {
		case strings.Contains(mod, "nano") || strings.HasPrefix(mod, "le800"):
			return ProtoDahua, "native", 4
		case strings.HasPrefix(mod, "le808"):
			return ProtoDahua, "rpc2-dahua3", 8
		case strings.HasPrefix(mod, "le820"):
			return ProtoDahua, "rpc2-form", 8
		case mod == "dahua":
			return ProtoDahua, "rpc2-web3", 0
		}
		return ProtoDahua, "", 0
	case "netsurveillance", "sofia", "xmeye", "dvrip", "xm":
		for _, n := range []int{16, 8, 4} {
			if strings.HasSuffix(mod, strconv.Itoa(n)+"ch") || strings.HasSuffix(mod, "dvrip"+strconv.Itoa(n)) {
				return ProtoSofia, "dvrip", n
			}
		}
		return ProtoSofia, "", 0
	case "hikvision", "hiwatch":
		if strings.Contains(mod, "ipc") {
			return ProtoHikvision, "isapi", 1
		}
		return ProtoHikvision, "", 0
	case "uniview", "unv":
		if strings.Contains(mod, "ipc") {
			return ProtoUniview, "rtsp", 1
		}
		return ProtoUniview, "", 0
	case "axis":
		return ProtoAxis, "", 0
	case "generic", "":
		switch mod {
		case "onvif", "onvifnvr":
			return ProtoONVIF, "", 0
		case "rtsp", "rtspnvr":
			return ProtoRTSP, "", 0
		case "rtmp":
			return ProtoRTMP, "", 0
		case "http":
			return ProtoHTTP, "", 0
		case "tcp":
			return ProtoTCP, "", 0
		}
	}
	// model names without a known manufacturer
	switch {
	case strings.HasPrefix(mod, "le808"):
		return ProtoDahua, "rpc2-dahua3", 8
	case strings.HasPrefix(mod, "le820"):
		return ProtoDahua, "rpc2-form", 8
	case strings.Contains(mod, "nano"):
		return ProtoDahua, "native", 4
	case strings.HasPrefix(mod, "dvrip"):
		return legacyProtocol("sofia", model)
	}
	return "", "", 0
}
