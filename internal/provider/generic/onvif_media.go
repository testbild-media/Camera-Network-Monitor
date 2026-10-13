package generic

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cnm/internal/provider"
)

// ONVIF Profile S cameras and recorders: every video source is a channel in
// GetVideoSources order. The media profiles of a source (GetProfiles, linked
// through VideoSourceConfiguration/SourceToken) provide its streams: the
// first profile is the main stream, the second one the sub stream. Stream
// URIs (GetStreamUri, RTP-Unicast over RTSP) are resolved at Connect and
// cached; LiveSources hands them to go2rtc with the credentials injected.

type onvifChannel struct {
	source          string
	profiles        []string // profile tokens, video profiles first
	main, sub       string   // stream URIs without credentials
	mainTok, subTok string   // profiles of main and sub
}

type onvifMedia struct {
	channels      []onvifChannel
	profileSource map[string]string // profile token → video source token
}

// sourceIndex maps video source tokens to channels.
func (m *onvifMedia) sourceIndex() map[string]int {
	idx := map[string]int{}
	if m != nil {
		for i, c := range m.channels {
			idx[c.source] = i
		}
	}
	return idx
}

func (o *onvifDev) mediaState() *onvifMedia {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.media
}

func (o *onvifDev) clearMedia() {
	o.mu.Lock()
	o.media = nil
	o.mu.Unlock()
}

// resolveMedia reads video sources and profiles and resolves the stream
// URIs of the first maxChannels channels.
func (o *onvifDev) resolveMedia(ctx context.Context, maxChannels int) (*onvifMedia, error) {
	svc, err := o.services(ctx)
	if err != nil {
		return nil, err
	}
	xaddr := svc[nsMedia]
	var vs struct {
		Sources []struct {
			Token string `xml:"token,attr"`
		} `xml:"Body>GetVideoSourcesResponse>VideoSources"`
	}
	if err := o.call(ctx, xaddr, "GetVideoSources", `<trt:GetVideoSources/>`, &vs); err != nil && provider.KindOf(err) != provider.KindUnsupported {
		return nil, err
	}
	profiles, err := o.profiles(ctx, xaddr)
	if err != nil {
		return nil, err
	}
	m := &onvifMedia{profileSource: map[string]string{}}
	idx := map[string]int{}
	addSource := func(tok string) {
		if _, ok := idx[tok]; !ok && tok != "" {
			idx[tok] = len(m.channels)
			m.channels = append(m.channels, onvifChannel{source: tok})
		}
	}
	for _, s := range vs.Sources {
		addSource(strings.TrimSpace(s.Token))
	}
	// profiles with a video encoder first, each group in device order
	for pass := 0; pass < 2; pass++ {
		for _, p := range profiles {
			tok, src := p.token, p.source
			if tok == "" || src == "" || p.video != (pass == 0) {
				continue
			}
			m.profileSource[tok] = src
			addSource(src) // devices without GetVideoSources: order of appearance
			ch := &m.channels[idx[src]]
			ch.profiles = append(ch.profiles, tok)
		}
	}
	if len(m.channels) == 0 {
		return nil, provider.NewError(provider.KindProtocol, nil, "ONVIF: keine Videoquellen gefunden")
	}
	found := 0
	for i := range m.channels {
		if i >= maxChannels {
			break
		}
		ch := &m.channels[i]
		// main = first profile, sub = second; a profile without a usable
		// stream is skipped (at most three attempts per channel)
		for j, tok := range ch.profiles {
			if j > 2 || ch.sub != "" {
				break
			}
			uri, err := o.streamURI(ctx, xaddr, tok)
			if err != nil {
				if provider.KindOf(err) == provider.KindAuth || ctx.Err() != nil {
					return nil, err
				}
				continue
			}
			if ch.main == "" {
				ch.main, ch.mainTok = uri, tok
				found++
			} else {
				ch.sub, ch.subTok = uri, tok
			}
		}
	}
	if found == 0 {
		return nil, provider.NewError(provider.KindProtocol, nil, "ONVIF: keine Stream-URL ermittelt")
	}
	o.mu.Lock()
	o.media = m
	o.mu.Unlock()
	return m, nil
}

type onvifProfile struct {
	token, source string
	video         bool // has a video encoder configuration
}

// profiles lists the media profiles in device order.
func (o *onvifDev) profiles(ctx context.Context, xaddr string) ([]onvifProfile, error) {
	var pr struct {
		Profiles []struct {
			Token   string `xml:"token,attr"`
			Source  string `xml:"VideoSourceConfiguration>SourceToken"`
			Encoder *struct {
				Encoding string `xml:"Encoding"`
			} `xml:"VideoEncoderConfiguration"`
		} `xml:"Body>GetProfilesResponse>Profiles"`
	}
	if err := o.call(ctx, xaddr, "GetProfiles", `<trt:GetProfiles/>`, &pr); err != nil {
		return nil, err
	}
	out := make([]onvifProfile, 0, len(pr.Profiles))
	for _, p := range pr.Profiles {
		out = append(out, onvifProfile{token: strings.TrimSpace(p.Token), source: strings.TrimSpace(p.Source), video: p.Encoder != nil})
	}
	return out, nil
}

// streamURI asks for the RTSP URI of a media profile (RTP unicast over RTSP).
func (o *onvifDev) streamURI(ctx context.Context, xaddr, profile string) (string, error) {
	var r struct {
		URI string `xml:"Body>GetStreamUriResponse>MediaUri>Uri"`
	}
	body := `<trt:GetStreamUri><trt:StreamSetup><tt:Stream>RTP-Unicast</tt:Stream><tt:Transport><tt:Protocol>RTSP</tt:Protocol></tt:Transport></trt:StreamSetup>` +
		`<trt:ProfileToken>` + xmlText(profile) + `</trt:ProfileToken></trt:GetStreamUri>`
	if err := o.call(ctx, xaddr, "GetStreamUri", body, &r); err != nil {
		return "", err
	}
	uri := o.fixStreamURI(r.URI)
	if uri == "" {
		return "", provider.NewError(provider.KindProtocol, nil, "ONVIF GetStreamUri: keine RTSP-URL")
	}
	return uri, nil
}

// liveSource returns the go2rtc source of a channel ("" while the streams
// are not resolved or the channel has no such stream).
func (o *onvifDev) liveSource(channel int, kind provider.StreamKind) string {
	m := o.mediaState()
	if m == nil || channel < 0 || channel >= len(m.channels) {
		return ""
	}
	uri := m.channels[channel].main
	if kind == provider.Sub {
		uri = m.channels[channel].sub
	}
	if uri == "" {
		return ""
	}
	return o.withCredentials(uri) + "#backchannel=0#media=video"
}

// ---- driver integration -----------------------------------------------------------------------

// onvifSources returns the go2rtc sources of a channel: the stream URI CNM
// resolved (video profiles only, reachable host) and, as fallback, go2rtc's
// own ONVIF client with the exact profile token. Without a resolution
// (device answered GetProfiles/GetStreamUri unusably) go2rtc resolves the
// first (main) or second (sub) profile itself.
func (d *driver) onvifSources(channel int, kind provider.StreamKind) []string {
	m := d.ov.mediaState()
	if m == nil || len(m.channels) == 0 {
		if channel > 0 {
			return nil
		}
		if kind == provider.Sub {
			return []string{d.go2rtcONVIF("1")}
		}
		return []string{d.go2rtcONVIF("0")}
	}
	if channel < 0 || channel >= len(m.channels) {
		return nil
	}
	ch := m.channels[channel]
	uri, tok := ch.main, ch.mainTok
	if kind == provider.Sub {
		uri, tok = ch.sub, ch.subTok
	}
	if uri == "" {
		return nil
	}
	return []string{d.ov.withCredentials(uri) + "#backchannel=0#media=video", d.go2rtcONVIF(tok)}
}

// go2rtcONVIF builds a go2rtc onvif:// source (subtype = profile number or token).
func (d *driver) go2rtcONVIF(subtype string) string {
	c := d.cfg
	u := url.URL{Scheme: "onvif", Host: provider.HostPort(c.Host, c.Port("http")), RawQuery: "subtype=" + url.QueryEscape(subtype)}
	if c.Username != "" || c.Password != "" {
		u.User = url.UserPassword(c.Username, c.Password)
	}
	return u.String() + "#backchannel=0#media=video"
}

// onvifConnect logs in (GetDeviceInformation), reads the service addresses
// and resolves channels (video sources) and stream URIs. A device whose
// media answers are unusable stays usable live through go2rtc's ONVIF client.
func (d *driver) onvifConnect(ctx context.Context) (*provider.DeviceInfo, error) {
	c := d.cfg
	if !d.env.AllowLogin() {
		return nil, provider.ErrLoginBlocked
	}
	di, off, err := onvifDeviceInfo(ctx, c.Host, c.Port("http"), c.Username, c.Password)
	if err != nil {
		return nil, err
	}
	d.ov.setClock(off)
	override, _ := strconv.Atoi(strings.TrimSpace(c.Options[provider.OptChannels]))
	n := 64
	if override > 0 {
		n = override
	}
	mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	di.Variant = "onvif"
	di.Summary = fmt.Sprintf("ONVIF über HTTP %d", c.Port("http"))
	m, err := d.ov.resolveMedia(mctx, n)
	switch {
	case err == nil:
		di.Channels = min(len(m.channels), n)
	case provider.KindOf(err) == provider.KindAuth || ctx.Err() != nil:
		return nil, err
	default:
		// unusable media answers: go2rtc resolves the profiles itself
		d.ov.clearMedia()
		di.Channels = 1
		di.Summary += " · Streams über go2rtcs ONVIF-Client"
	}
	if override > 0 {
		di.Channels = override
	}
	return di, nil
}

// onvifCheck repeats the login check and reports the device clock offset.
// ONVIF devices report real UTC, so the offset is the clock error.
func (d *driver) onvifCheck(ctx context.Context) (*provider.Health, error) {
	c := d.cfg
	if !d.env.AllowLogin() {
		return nil, provider.ErrLoginBlocked
	}
	_, off, err := onvifDeviceInfo(ctx, c.Host, c.Port("http"), c.Username, c.Password)
	if err != nil {
		if provider.KindOf(err) == provider.KindAuth {
			d.env.ReportAuthFailure(err)
		}
		return nil, err
	}
	d.ov.setClock(off)
	return &provider.Health{Storage: provider.StorageUnknown, ClockOffset: off}, nil
}
