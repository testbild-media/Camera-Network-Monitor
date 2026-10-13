package sim

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ONVIFServer simulates the SOAP side of an ONVIF device: Profile S (device
// and media ver10 services) and Profile G (search, replay and recording
// services) with WS-UsernameToken digest authentication. Each service has its
// own path; requests sent to the wrong service are rejected, so clients must
// use the XAddrs from GetServices.
//
// Video is served by an RTSPServer configured with Route = ONVIFRoute and
// ONVIFReplay = (*ONVIFServer).Replay.
type ONVIFServer struct {
	User, Pass string
	// Channels is the number of video sources (1 = camera).
	Channels int
	// SubStreams adds a second (sub stream) profile per video source.
	SubStreams bool
	// RTSPAddr is host:port of the RTSP server announced in stream URIs.
	RTSPAddr string
	// AdvertiseHost replaces the host in announced XAddrs and stream URIs
	// (like a device behind port forwarding announcing its LAN address).
	AdvertiseHost string
	// ClockOffset shifts the device clock; WS-Security "Created" stamps must
	// match the device clock within 5 s.
	ClockOffset time.Duration
	// Recordings of the device (Profile G).
	Recordings []ONVIFRecording
	// NoProfileG omits the search, replay and recording services.
	NoProfileG bool
	// SourceTokens adds Source/VideoSourceToken to search results. Without
	// it the source is an opaque id and the channel follows from GetRecordingJobs.
	SourceTokens bool
	// NoJobs makes GetRecordingJobs return no jobs.
	NoJobs bool

	ln        net.Listener
	srv       *http.Server
	AuthFails atomic.Int32
	Searches  atomic.Int32
	mu        sync.Mutex
	searches  map[string]int // search token → polls answered
	nextID    int
}

// ONVIFRecording is one recording of a video source.
type ONVIFRecording struct {
	Token      string
	Channel    int // video source index (0 based)
	Start, End time.Time
}

// Service paths of the simulator.
const (
	ONVIFDevicePath    = "/onvif/device_service"
	ONVIFMediaPath     = "/onvif/media_service"
	ONVIFSearchPath    = "/onvif/search_service"
	ONVIFReplayPath    = "/onvif/replay_service"
	ONVIFRecordingPath = "/onvif/recording_service"
)

// Listen starts the HTTP server.
func (s *ONVIFServer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if s.Channels <= 0 {
		s.Channels = 1
	}
	s.ln = ln
	s.searches = map[string]int{}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.handle)}
	go s.srv.Serve(ln)
	return nil
}

// Addr returns the listen address.
func (s *ONVIFServer) Addr() string { return s.ln.Addr().String() }

// Close stops the server.
func (s *ONVIFServer) Close() { s.srv.Close() }

// VideoSourceToken and ProfileToken name the simulated media entities
// (channel 0 based).
func VideoSourceToken(ch int) string { return fmt.Sprintf("VideoSource_%d", ch+1) }

// ProfileToken returns the main (sub=false) or sub stream profile token.
func ProfileToken(ch int, sub bool) string {
	if sub {
		return fmt.Sprintf("Profile_%d_sub", ch+1)
	}
	return fmt.Sprintf("Profile_%d_main", ch+1)
}

// ONVIFRoute resolves the RTSP paths announced by ONVIFServer:
// /onvif/live/<ch>/<sub> (channel 1 based, sub 0|1) and /onvif/replay/<ch>.
func ONVIFRoute(u *url.URL, _ *time.Location) (Route, bool) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "onvif" {
		return Route{}, false
	}
	ch, err := strconv.Atoi(parts[2])
	if err != nil || ch < 1 {
		return Route{}, false
	}
	switch parts[1] {
	case "live":
		if len(parts) < 4 {
			return Route{}, false
		}
		sub, err := strconv.Atoi(parts[3])
		return Route{Channel: ch, Subtype: sub}, err == nil && (sub == 0 || sub == 1)
	case "replay":
		return Route{Channel: ch, Playback: true}, true
	}
	return Route{}, false
}

// Replay is the RTSPServer.ONVIFReplay hook: footage exists from the start
// of a recording of the channel (1 based) up to its end.
func (s *ONVIFServer) Replay(channel int, start, end time.Time) (time.Time, bool) {
	for _, r := range s.Recordings {
		if r.Channel != channel-1 || start.Before(r.Start) || !start.Before(r.End) {
			continue
		}
		until := r.End
		if !end.IsZero() && end.Before(until) {
			until = end
		}
		return until, true
	}
	return time.Time{}, false
}

func (s *ONVIFServer) now() time.Time { return time.Now().UTC().Add(s.ClockOffset) }

var (
	reONVIFOp        = regexp.MustCompile(`<(?:[\w-]+:)?Body[^>]*>\s*<(?:[\w-]+:)?(\w+)`)
	reONVIFUser      = regexp.MustCompile(`<(?:[\w-]+:)?Username(?:\s[^>]*)?>([^<]*)<`)
	reONVIFPassword  = regexp.MustCompile(`<(?:[\w-]+:)?Password(?:\s[^>]*)?>([^<]*)<`)
	reONVIFNonce     = regexp.MustCompile(`<(?:[\w-]+:)?Nonce(?:\s[^>]*)?>([^<]*)<`)
	reONVIFCreated   = regexp.MustCompile(`<(?:[\w-]+:)?Created(?:\s[^>]*)?>([^<]*)<`)
	reONVIFProfile   = regexp.MustCompile(`<(?:[\w-]+:)?ProfileToken>([^<]*)<`)
	reONVIFRecording = regexp.MustCompile(`<(?:[\w-]+:)?RecordingToken>([^<]*)<`)
	reONVIFSearch    = regexp.MustCompile(`<(?:[\w-]+:)?SearchToken>([^<]*)<`)
	reONVIFProtocol  = regexp.MustCompile(`<(?:[\w-]+:)?Protocol>([^<]*)<`)
)

func onvifMatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return html.UnescapeString(strings.TrimSpace(m[1]))
	}
	return ""
}

// authorized validates the WS-UsernameToken password digest
// base64(sha1(nonce + created + password)) and the Created stamp.
func (s *ONVIFServer) authorized(body string) bool {
	nonce, err := base64.StdEncoding.DecodeString(onvifMatch(reONVIFNonce, body))
	created := onvifMatch(reONVIFCreated, body)
	if err != nil || onvifMatch(reONVIFUser, body) != s.User {
		return false
	}
	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(s.Pass))
	if onvifMatch(reONVIFPassword, body) != base64.StdEncoding.EncodeToString(h.Sum(nil)) {
		return false
	}
	c, err := time.Parse(time.RFC3339, created)
	return err == nil && c.Sub(s.now()).Abs() <= 5*time.Second
}

const onvifEnvelope = `<?xml version="1.0" encoding="UTF-8"?><env:Envelope xmlns:env="http://www.w3.org/2003/05/soap-envelope"` +
	` xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:trt="http://www.onvif.org/ver10/media/wsdl"` +
	` xmlns:tse="http://www.onvif.org/ver10/search/wsdl" xmlns:trp="http://www.onvif.org/ver10/replay/wsdl"` +
	` xmlns:trc="http://www.onvif.org/ver10/recording/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"` +
	` xmlns:ter="http://www.onvif.org/ver10/error"><env:Body>%s</env:Body></env:Envelope>`

func onvifReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, onvifEnvelope, body)
}

func onvifFault(w http.ResponseWriter, code, reason string) {
	onvifReply(w, http.StatusBadRequest, `<env:Fault><env:Code><env:Value>env:Sender</env:Value><env:Subcode><env:Value>`+code+
		`</env:Value></env:Subcode></env:Code><env:Reason><env:Text xml:lang="en">`+reason+`</env:Text></env:Reason></env:Fault>`)
}

// onvifServicePath maps an operation to the service that implements it.
var onvifServicePath = map[string]string{
	"GetSystemDateAndTime": ONVIFDevicePath, "GetDeviceInformation": ONVIFDevicePath, "GetServices": ONVIFDevicePath, "GetCapabilities": ONVIFDevicePath,
	"GetVideoSources": ONVIFMediaPath, "GetProfiles": ONVIFMediaPath, "GetStreamUri": ONVIFMediaPath,
	"FindRecordings": ONVIFSearchPath, "GetRecordingSearchResults": ONVIFSearchPath, "EndSearch": ONVIFSearchPath,
	"GetReplayUri": ONVIFReplayPath, "GetRecordingJobs": ONVIFRecordingPath,
}

func (s *ONVIFServer) handle(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	body := string(b)
	op := onvifMatch(reONVIFOp, body)
	path := onvifServicePath[op]
	profileG := path == ONVIFSearchPath || path == ONVIFReplayPath || path == ONVIFRecordingPath
	if path == "" || path != r.URL.Path || (profileG && s.NoProfileG) {
		onvifFault(w, "ter:ActionNotSupported", "action not supported by this service")
		return
	}
	if op == "GetSystemDateAndTime" { // unauthenticated (pre-auth)
		t := s.now()
		onvifReply(w, http.StatusOK, fmt.Sprintf(`<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime><tt:DateTimeType>NTP</tt:DateTimeType>`+
			`<tt:UTCDateTime><tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time>`+
			`<tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date></tt:UTCDateTime>`+
			`</tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`, t.Hour(), t.Minute(), t.Second(), t.Year(), t.Month(), t.Day()))
		return
	}
	if !s.authorized(body) {
		s.AuthFails.Add(1)
		onvifFault(w, "ter:NotAuthorized", "Sender not Authorized")
		return
	}
	host := r.Host
	if s.AdvertiseHost != "" {
		_, port, _ := net.SplitHostPort(r.Host)
		host = net.JoinHostPort(s.AdvertiseHost, port)
	}
	switch op {
	case "GetDeviceInformation":
		onvifReply(w, http.StatusOK, `<tds:GetDeviceInformationResponse><tds:Manufacturer>CNM</tds:Manufacturer><tds:Model>SIM-ONVIF</tds:Model>`+
			`<tds:FirmwareVersion>1.0</tds:FirmwareVersion><tds:SerialNumber>ONVIF-SIM-1</tds:SerialNumber><tds:HardwareId>sim</tds:HardwareId></tds:GetDeviceInformationResponse>`)
	case "GetCapabilities": // ONVIF Core (Profile S); go2rtc's onvif:// source starts with it
		onvifReply(w, http.StatusOK, fmt.Sprintf(`<tds:GetCapabilitiesResponse><tds:Capabilities>`+
			`<tt:Device><tt:XAddr>http://%[1]s%[2]s</tt:XAddr></tt:Device>`+
			`<tt:Events><tt:XAddr>http://%[1]s/onvif/event_service</tt:XAddr></tt:Events>`+
			`<tt:Media><tt:XAddr>http://%[1]s%[3]s</tt:XAddr><tt:StreamingCapabilities><tt:RTPMulticast>false</tt:RTPMulticast>`+
			`<tt:RTP_TCP>true</tt:RTP_TCP><tt:RTP_RTSP_TCP>true</tt:RTP_RTSP_TCP></tt:StreamingCapabilities></tt:Media>`+
			`</tds:Capabilities></tds:GetCapabilitiesResponse>`, host, ONVIFDevicePath, ONVIFMediaPath))
	case "GetServices":
		var b strings.Builder
		b.WriteString(`<tds:GetServicesResponse>`)
		svc := func(ns, path string) {
			fmt.Fprintf(&b, `<tds:Service><tds:Namespace>%s</tds:Namespace><tds:XAddr>http://%s%s</tds:XAddr><tds:Version><tt:Major>2</tt:Major><tt:Minor>60</tt:Minor></tds:Version></tds:Service>`, ns, host, path)
		}
		svc("http://www.onvif.org/ver10/device/wsdl", ONVIFDevicePath)
		svc("http://www.onvif.org/ver10/media/wsdl", ONVIFMediaPath)
		svc("http://www.onvif.org/ver10/events/wsdl", "/onvif/event_service")
		if !s.NoProfileG {
			svc("http://www.onvif.org/ver10/recording/wsdl", ONVIFRecordingPath)
			svc("http://www.onvif.org/ver10/search/wsdl", ONVIFSearchPath)
			svc("http://www.onvif.org/ver10/replay/wsdl", ONVIFReplayPath)
		}
		b.WriteString(`</tds:GetServicesResponse>`)
		onvifReply(w, http.StatusOK, b.String())
	case "GetVideoSources":
		var b strings.Builder
		b.WriteString(`<trt:GetVideoSourcesResponse>`)
		for ch := 0; ch < s.Channels; ch++ {
			fmt.Fprintf(&b, `<trt:VideoSources token="%s"><tt:Framerate>25</tt:Framerate><tt:Resolution><tt:Width>1280</tt:Width><tt:Height>720</tt:Height></tt:Resolution></trt:VideoSources>`, VideoSourceToken(ch))
		}
		b.WriteString(`</trt:GetVideoSourcesResponse>`)
		onvifReply(w, http.StatusOK, b.String())
	case "GetProfiles":
		// listed in reverse channel order: the channel order must come from
		// GetVideoSources, the main/sub order from the profile order per source
		var b strings.Builder
		b.WriteString(`<trt:GetProfilesResponse>`)
		profile := func(ch int, sub bool) {
			fmt.Fprintf(&b, `<trt:Profiles token="%s" fixed="true"><tt:Name>%s</tt:Name>`+
				`<tt:VideoSourceConfiguration token="VSC_%d"><tt:Name>VSC</tt:Name><tt:UseCount>2</tt:UseCount><tt:SourceToken>%s</tt:SourceToken><tt:Bounds x="0" y="0" width="1280" height="720"/></tt:VideoSourceConfiguration>`+
				`<tt:VideoEncoderConfiguration token="VEC_%s"><tt:Name>VEC</tt:Name><tt:UseCount>1</tt:UseCount><tt:Encoding>H264</tt:Encoding></tt:VideoEncoderConfiguration></trt:Profiles>`,
				ProfileToken(ch, sub), ProfileToken(ch, sub), ch+1, VideoSourceToken(ch), ProfileToken(ch, sub))
		}
		// an audio/metadata only profile without video source
		b.WriteString(`<trt:Profiles token="Profile_meta" fixed="true"><tt:Name>Metadata</tt:Name></trt:Profiles>`)
		for ch := s.Channels - 1; ch >= 0; ch-- {
			profile(ch, false)
		}
		if s.SubStreams {
			for ch := s.Channels - 1; ch >= 0; ch-- {
				profile(ch, true)
			}
		}
		b.WriteString(`</trt:GetProfilesResponse>`)
		onvifReply(w, http.StatusOK, b.String())
	case "GetStreamUri":
		tok := onvifMatch(reONVIFProfile, body)
		var ch, sub int
		var kind string
		if n, _ := fmt.Sscanf(tok, "Profile_%d_%s", &ch, &kind); n != 2 || ch < 1 || ch > s.Channels || (kind != "main" && kind != "sub") || (kind == "sub" && !s.SubStreams) {
			onvifFault(w, "ter:InvalidArgVal/ter:NoProfile", "no such profile")
			return
		}
		if onvifMatch(reONVIFProtocol, body) != "RTSP" {
			onvifFault(w, "ter:InvalidArgVal/ter:InvalidStreamSetup", "unsupported transport")
			return
		}
		if kind == "sub" {
			sub = 1
		}
		onvifReply(w, http.StatusOK, fmt.Sprintf(`<trt:GetStreamUriResponse><trt:MediaUri><tt:Uri>rtsp://%s/onvif/live/%d/%d</tt:Uri>`+
			`<tt:InvalidAfterConnect>false</tt:InvalidAfterConnect><tt:InvalidAfterReboot>false</tt:InvalidAfterReboot><tt:Timeout>PT0S</tt:Timeout></trt:MediaUri></trt:GetStreamUriResponse>`, s.rtspHost(), ch, sub))
	case "FindRecordings":
		s.Searches.Add(1)
		s.mu.Lock()
		s.nextID++
		tok := fmt.Sprintf("search-%d", s.nextID)
		s.searches[tok] = 0
		s.mu.Unlock()
		onvifReply(w, http.StatusOK, `<tse:FindRecordingsResponse><tse:SearchToken>`+tok+`</tse:SearchToken></tse:FindRecordingsResponse>`)
	case "GetRecordingSearchResults":
		// first poll: state Searching with the first half, second poll: the rest
		tok := onvifMatch(reONVIFSearch, body)
		s.mu.Lock()
		polls, ok := s.searches[tok]
		s.searches[tok] = polls + 1
		if polls >= 1 {
			delete(s.searches, tok)
		}
		s.mu.Unlock()
		if !ok {
			onvifFault(w, "ter:InvalidArgVal/ter:InvalidToken", "unknown search")
			return
		}
		half := (len(s.Recordings) + 1) / 2
		recs, state := s.Recordings[:half], "Searching"
		if polls >= 1 {
			recs, state = s.Recordings[half:], "Completed"
		}
		var b strings.Builder
		b.WriteString(`<tse:GetRecordingSearchResultsResponse><tse:ResultList><tt:SearchState>` + state + `</tt:SearchState>`)
		for _, rec := range recs {
			s.writeRecording(&b, rec)
		}
		b.WriteString(`</tse:ResultList></tse:GetRecordingSearchResultsResponse>`)
		onvifReply(w, http.StatusOK, b.String())
	case "EndSearch":
		s.mu.Lock()
		delete(s.searches, onvifMatch(reONVIFSearch, body))
		s.mu.Unlock()
		onvifReply(w, http.StatusOK, `<tse:EndSearchResponse><tse:Endpoint>`+s.now().Format(time.RFC3339)+`</tse:Endpoint></tse:EndSearchResponse>`)
	case "GetRecordingJobs":
		var b strings.Builder
		b.WriteString(`<trc:GetRecordingJobsResponse>`)
		if !s.NoJobs {
			for i, rec := range s.Recordings {
				fmt.Fprintf(&b, `<trc:JobItem><tt:JobToken>Job_%d</tt:JobToken><tt:JobConfiguration><tt:ScheduleToken>Always</tt:ScheduleToken>`+
					`<tt:RecordingToken>%s</tt:RecordingToken><tt:Mode>Active</tt:Mode><tt:Priority>1</tt:Priority>`+
					`<tt:Source><tt:SourceToken Type="http://www.onvif.org/ver10/schema/Profile"><tt:Token>%s</tt:Token></tt:SourceToken>`+
					`<tt:Tracks><tt:SourceTag>VIDEO</tt:SourceTag><tt:Destination>VIDEO001</tt:Destination></tt:Tracks></tt:Source></tt:JobConfiguration></trc:JobItem>`,
					i+1, rec.Token, ProfileToken(rec.Channel, false))
			}
		}
		b.WriteString(`</trc:GetRecordingJobsResponse>`)
		onvifReply(w, http.StatusOK, b.String())
	case "GetReplayUri":
		tok := onvifMatch(reONVIFRecording, body)
		for _, rec := range s.Recordings {
			if rec.Token == tok {
				onvifReply(w, http.StatusOK, fmt.Sprintf(`<trp:GetReplayUriResponse><trp:Uri>rtsp://%s/onvif/replay/%d</trp:Uri></trp:GetReplayUriResponse>`, s.rtspHost(), rec.Channel+1))
				return
			}
		}
		onvifFault(w, "ter:InvalidArgVal/ter:NoRecording", "no such recording")
	}
}

func (s *ONVIFServer) rtspHost() string {
	if s.AdvertiseHost == "" {
		return s.RTSPAddr
	}
	_, port, _ := net.SplitHostPort(s.RTSPAddr)
	return net.JoinHostPort(s.AdvertiseHost, port)
}

func (s *ONVIFServer) writeRecording(b *strings.Builder, rec ONVIFRecording) {
	ts := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
	fmt.Fprintf(b, `<tt:RecordingInformation><tt:RecordingToken>%s</tt:RecordingToken><tt:Source><tt:SourceId>urn:cnm-sim:source:%s</tt:SourceId>`+
		`<tt:Name>Kamera %d</tt:Name><tt:Location>Sim</tt:Location><tt:Description/><tt:Address>rtsp://%s/onvif/live/%d/0</tt:Address>`,
		rec.Token, rec.Token, rec.Channel+1, s.rtspHost(), rec.Channel+1)
	if s.SourceTokens {
		fmt.Fprintf(b, `<tt:VideoSourceToken>%s</tt:VideoSourceToken>`, VideoSourceToken(rec.Channel))
	}
	fmt.Fprintf(b, `</tt:Source><tt:EarliestRecording>%s</tt:EarliestRecording><tt:LatestRecording>%s</tt:LatestRecording><tt:Content>Sim</tt:Content>`+
		`<tt:Track><tt:TrackToken>VIDEO001</tt:TrackToken><tt:TrackType>Video</tt:TrackType><tt:Description/><tt:DataFrom>%s</tt:DataFrom><tt:DataTo>%s</tt:DataTo></tt:Track>`+
		`<tt:RecordingStatus>Recording</tt:RecordingStatus></tt:RecordingInformation>`, ts(rec.Start), ts(rec.End), ts(rec.Start), ts(rec.End))
}

// ---- RTSP replay (ONVIF Streaming Specification §6) -----------------------------------------

func requiresReplay(r *rtspReq) bool {
	for _, v := range strings.Split(r.headers["require"], ",") {
		if strings.EqualFold(strings.TrimSpace(v), "onvif-replay") {
			return true
		}
	}
	return false
}

const onvifClock = "20060102T150405Z"

// parseClockRange parses "clock=20260101T120000Z-[20260101T121500Z]"
// (optional fractional seconds).
func parseClockRange(v string) (start, end time.Time, ok bool) {
	v, found := strings.CutPrefix(strings.TrimSpace(v), "clock=")
	if !found {
		return start, end, false
	}
	a, b, _ := strings.Cut(v, "-")
	parse := func(s string) (time.Time, error) {
		if i := strings.IndexByte(s, '.'); i >= 0 {
			s = s[:i] + "Z"
		}
		return time.Parse(onvifClock, s)
	}
	start, err := parse(a)
	if err != nil {
		return start, end, false
	}
	if b = strings.TrimSpace(b); b != "" {
		if end, err = parse(b); err != nil || !end.After(start) {
			return start, end, false
		}
	}
	return start, end, true
}

// onvifPlay answers PLAY of an ONVIF replay session: "Require: onvif-replay"
// and "Range: clock=…" are mandatory, "Rate-Control: no" streams as fast as
// possible. Every access unit carries the replay header extension with its
// recording time; at the end of the footage the server sends RTCP BYE and
// stops sending (the connection stays open, like real recorders).
func (s *RTSPServer) onvifPlay(r *rtspReq, channel int, clip *Clip, reply func(status string, hdr map[string]string), write func([]byte) error, stop <-chan struct{}) {
	if !s.ReplayNoRequire && !requiresReplay(r) {
		reply("551 Option not supported", map[string]string{"Unsupported": "onvif-replay"})
		return
	}
	start, end, ok := parseClockRange(r.headers["range"])
	if !ok {
		reply("457 Invalid Range", map[string]string{})
		return
	}
	until, ok := s.ONVIFReplay(channel, start, end)
	if !ok || !until.After(start) {
		reply("457 Invalid Range", map[string]string{})
		return
	}
	pace := !strings.EqualFold(strings.TrimSpace(r.headers["rate-control"]), "no")
	reply("200 OK", map[string]string{"Range": "clock=" + start.UTC().Format(onvifClock) + "-" + until.UTC().Format(onvifClock), "Rate-Control": map[bool]string{true: "yes", false: "no"}[pace]})
	s.Sessions.Add(1)
	go func() {
		defer s.Sessions.Add(-1)
		streamReplay(clip, write, stop, start, until, pace)
	}()
}

// streamReplay sends the clip as recorded footage from start to until.
func streamReplay(clip *Clip, write func([]byte) error, stop <-chan struct{}, start, until time.Time, pace bool) {
	seq := uint16(rand.Intn(65535))
	ssrc := rand.Uint32()
	frameDur := time.Second / time.Duration(clip.FPS)
	t0 := time.Now()
	interleaved := func(ch byte, pkt []byte) error {
		f := []byte{'$', ch, 0, 0}
		binary.BigEndian.PutUint16(f[2:], uint16(len(pkt)))
		return write(append(f, pkt...))
	}
	for i := 0; ; i++ {
		rec := start.Add(time.Duration(i) * frameDur)
		if !rec.Before(until) {
			break
		}
		select {
		case <-stop:
			return
		default:
		}
		au := clip.AUs[i%len(clip.AUs)]
		ts := uint32(i * 90000 / clip.FPS)
		nals := splitNALs(au.Data)
		first := true
		for ni, n := range nals {
			for _, p := range packetize(n, ni == len(nals)-1) {
				pkt := make([]byte, 12, 28+len(p.payload))
				pkt[0] = 0x80
				pkt[1] = 96
				if p.marker {
					pkt[1] |= 0x80
				}
				binary.BigEndian.PutUint16(pkt[2:], seq)
				binary.BigEndian.PutUint32(pkt[4:], ts)
				binary.BigEndian.PutUint32(pkt[8:], ssrc)
				seq++
				if first {
					// replay header extension: NTP time, flags (C = clean point, D = discontinuity)
					pkt[0] |= 0x10
					ext := make([]byte, 16)
					binary.BigEndian.PutUint16(ext, 0xABAC)
					binary.BigEndian.PutUint16(ext[2:], 3)
					binary.BigEndian.PutUint32(ext[4:], uint32(rec.Unix()+2208988800))
					binary.BigEndian.PutUint32(ext[8:], uint32(uint64(rec.Nanosecond())<<32/1e9))
					if au.Key {
						ext[12] |= 0x80
					}
					if i == 0 {
						ext[12] |= 0x20
					}
					pkt = append(pkt, ext...)
					first = false
				}
				pkt = append(pkt, p.payload...)
				if err := interleaved(0, pkt); err != nil {
					return
				}
			}
		}
		if pace {
			select {
			case <-stop:
				return
			case <-time.After(time.Until(t0.Add(time.Duration(i+1) * frameDur))):
			}
		}
	}
	bye := []byte{0x81, 203, 0, 1, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(bye[4:], ssrc)
	_ = interleaved(1, bye)
}
