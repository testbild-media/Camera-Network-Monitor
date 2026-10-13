// Package dahua implements the Dahua protocol family as used by Dahua and
// many OEM recorders: the JSON control protocol "RPC2" over HTTP (several
// web-client login variants) and the older native binary protocol on TCP
// 37777. Live video is RTSP in both cases. The admin only chooses "Dahua";
// which variant a device speaks is detected at Connect (see family.go).
package dahua

import (
	"net/url"
	"strconv"

	"cnm/internal/provider"
)

// ProviderID of the Dahua protocol family.
const ProviderID = "dahua"

// ProtoDahua is the protocol id stored in the configuration.
const ProtoDahua = "dahua"

// Detected variants (stored per device, see DeviceConfig.Variant).
const (
	VariantRPC2Classic = "rpc2-dahua3" // RPC2, web client "Dahua3.0-Web3.0" (verified)
	VariantRPC2Form    = "rpc2-form"   // RPC2, web client "Web3.0", form encoded (verified)
	VariantRPC2Direct  = "rpc2-web3"   // RPC2, current web client, loginType Direct
	VariantNative      = "native"      // native binary protocol (recordings), RTSP live
)

// rpcVariants in detection order: verified variants first.
var rpcVariants = []string{VariantRPC2Classic, VariantRPC2Direct, VariantRPC2Form}

type prov struct{}

func init() { provider.Register(prov{}) }

var rpcCaps = provider.Capabilities{
	Discovery: provider.Experimental, ChannelList: provider.Verified,
	LiveMain: provider.Verified, LiveSub: provider.Verified,
	RecordingSearch: provider.Verified, SearchKind: "list",
	Playback:    provider.Verified,
	DeviceTime:  provider.Verified,
	StorageInfo: provider.Verified,
}

var nativeCaps = provider.Capabilities{
	Discovery: provider.Experimental, ChannelList: provider.Experimental,
	LiveMain: provider.Verified, LiveSub: provider.Verified,
	RecordingSearch: provider.Experimental, SearchKind: "list",
	Playback:    provider.Verified,
	DeviceTime:  provider.Unsupported,
	StorageInfo: provider.Unsupported,
}

func (prov) Info() provider.Info {
	return provider.Info{
		ID:    ProviderID,
		Name:  "Dahua",
		Order: 10,
		Models: []provider.Model{{
			ID: ProtoDahua, Name: "Dahua", AutoDetect: true,
			Ports: []provider.PortSpec{
				{Key: "http", Label: "HTTP-Port (RPC2)", Default: 80},
				{Key: "rtsp", Label: "RTSP", Default: 554},
				{Key: "native", Label: "Nativer Port (TCP)", Default: 37777},
			},
			Options:      []provider.OptionSpec{provider.ChannelOverride()},
			Capabilities: rpcCaps,
			Auth:         "RPC2-Login (Dahua-Digest) bzw. nativer Login · RTSP Digest",
			Notes: "CNM erkennt beim Verbinden selbst, ob das Gerät die Steuerung über HTTP (RPC2) oder nur das native Protokoll " +
				"(TCP 37777) spricht, und welche Login-Variante es erwartet; die Kanalzahl liest CNM vom Gerät. Live immer über RTSP. " +
				"Beim nativen Protokoll gibt es keine Gerätezeit und keinen Speicherstatus.",
		}},
	}
}

// VariantCapabilities: the native protocol offers less than RPC2.
func (prov) VariantCapabilities(_ provider.Model, variant string) (provider.Capabilities, bool) {
	switch variant {
	case VariantNative:
		return nativeCaps, true
	case VariantRPC2Direct:
		c := rpcCaps // not yet confirmed on hardware
		c.RecordingSearch, c.Playback, c.DeviceTime, c.StorageInfo, c.ChannelList = provider.Experimental, provider.Experimental, provider.Experimental, provider.Experimental, provider.Experimental
		return c, true
	case VariantRPC2Classic, VariantRPC2Form:
		return rpcCaps, true
	}
	return provider.Capabilities{}, false
}

func (prov) NewDriver(cfg provider.DeviceConfig, env provider.Env) (provider.Driver, error) {
	if cfg.Host == "" {
		return nil, provider.NewError(provider.KindConfig, nil, "Host fehlt")
	}
	return newFamilyDriver(cfg, env), nil
}

// liveURL builds the Dahua style RTSP live URL. channel is zero based.
func liveURL(cfg provider.DeviceConfig, channel int, kind provider.StreamKind) string {
	subtype := "0"
	if kind == provider.Sub {
		subtype = "1"
	}
	q := url.Values{"channel": {strconv.Itoa(channel + 1)}, "subtype": {subtype}}
	return provider.CredURL("rtsp", cfg.Username, cfg.Password, cfg.Host, cfg.Port("rtsp"), "/cam/realmonitor", q, "backchannel=0#media=video")
}
