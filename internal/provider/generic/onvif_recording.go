package generic

import (
	"context"
	"sort"
	"strings"
	"time"

	"cnm/internal/provider"
	"cnm/internal/rtsp"
)

// ONVIF Profile G: recording search (search service) and replay (replay
// service + RTSP with "Require: onvif-replay").
//
//	FindRecordings → SearchToken; GetRecordingSearchResults until the
//	SearchState is Completed (each answer only carries new results);
//	EndSearch when the search is abandoned early
//	GetRecordingJobs (recording service) links recordings to media profiles
//	GetReplayUri(RecordingToken) → RTSP URI; PLAY with
//	"Range: clock=<from>Z-<to>Z" and "Rate-Control: yes"
//
// The search only reports the first and last recorded instant of every
// recording (EarliestRecording/LatestRecording), not the gaps in between
// (SearchKind "span"). ONVIF times are real UTC, so no device clock
// conversion is needed.

// Cache lifetimes of the recording list (one search covers all channels).
const (
	searchMaxAge   = 15 * time.Second
	probeMaxAge    = time.Minute
	playbackMaxAge = 2 * time.Minute // a miss triggers a fresh search
	// permanent answers (no Profile G, access denied) are not repeated sooner
	recsErrTTL = 5 * time.Minute
	// maxSearchPolls bounds GetRecordingSearchResults calls per search
	maxSearchPolls = 50
)

var errNoProfileG = provider.NewError(provider.KindUnsupported, nil, "ONVIF: Gerät bietet keine Aufzeichnungssuche (Profile G)")

// onvifRecording is one recording with its recorded span.
type onvifRecording struct {
	Token       string
	Channel     int // -1 = unknown
	Start, End  time.Time
	sourceID    string
	videoSource string
}

type xRecordingInfo struct {
	Token       string `xml:"RecordingToken"`
	SourceID    string `xml:"Source>SourceId"`
	VideoSource string `xml:"Source>VideoSourceToken"`
	Earliest    string `xml:"EarliestRecording"`
	Latest      string `xml:"LatestRecording"`
	Tracks      []struct {
		Type string `xml:"TrackType"`
		From string `xml:"DataFrom"`
		To   string `xml:"DataTo"`
	} `xml:"Track"`
}

// span returns the recorded span: Earliest/LatestRecording, else the
// union of the video tracks (all tracks when there is no video track).
func (x xRecordingInfo) span() (time.Time, time.Time, bool) {
	s, ok1 := parseONVIFTime(x.Earliest)
	e, ok2 := parseONVIFTime(x.Latest)
	if ok1 && ok2 && e.After(s) {
		return s, e, true
	}
	for _, videoOnly := range []bool{true, false} {
		var from, to time.Time
		for _, t := range x.Tracks {
			if videoOnly && !strings.EqualFold(strings.TrimSpace(t.Type), "Video") {
				continue
			}
			f, okf := parseONVIFTime(t.From)
			e, oke := parseONVIFTime(t.To)
			if !okf || !oke {
				continue
			}
			if from.IsZero() || f.Before(from) {
				from = f
			}
			if e.After(to) {
				to = e
			}
		}
		if !from.IsZero() && to.After(from) {
			return from, to, true
		}
	}
	return time.Time{}, time.Time{}, false
}

// recordings returns all recordings of the device with their channel,
// from the cache when it is younger than maxAge.
func (o *onvifDev) recordings(ctx context.Context, maxAge time.Duration) ([]onvifRecording, error) {
	o.searchMu.Lock()
	defer o.searchMu.Unlock()
	o.mu.Lock()
	recs, rerr, at := o.recs, o.recsErr, o.recsAt
	o.mu.Unlock()
	if !at.IsZero() {
		if rerr == nil && time.Since(at) < maxAge {
			return recs, nil
		}
		if rerr != nil && time.Since(at) < recsErrTTL {
			return nil, rerr
		}
	}
	recs, err := o.searchAll(ctx)
	if err != nil {
		switch provider.KindOf(err) {
		case provider.KindUnsupported, provider.KindAuth:
			o.mu.Lock()
			o.recs, o.recsErr, o.recsAt = nil, err, time.Now()
			o.mu.Unlock()
		}
		return nil, err
	}
	o.mu.Lock()
	o.recs, o.recsErr, o.recsAt = recs, nil, time.Now()
	o.mu.Unlock()
	return recs, nil
}

func (o *onvifDev) searchAll(ctx context.Context) ([]onvifRecording, error) {
	svc, err := o.services(ctx)
	if err != nil {
		return nil, err
	}
	if svc[nsSearch] == "" {
		return nil, errNoProfileG
	}
	infos, err := o.findRecordings(ctx, svc[nsSearch])
	if err != nil {
		if provider.KindOf(err) == provider.KindUnsupported {
			return nil, errNoProfileG
		}
		return nil, err
	}
	var recs []onvifRecording
	for _, x := range infos {
		s, e, ok := x.span()
		if !ok || strings.TrimSpace(x.Token) == "" {
			continue // empty recording
		}
		recs = append(recs, onvifRecording{Token: strings.TrimSpace(x.Token), Channel: -1, Start: s, End: e,
			sourceID: strings.TrimSpace(x.SourceID), videoSource: strings.TrimSpace(x.VideoSource)})
	}
	o.mapChannels(ctx, svc, recs)
	return recs, nil
}

// findRecordings runs one search over all recordings.
func (o *onvifDev) findRecordings(ctx context.Context, xaddr string) ([]xRecordingInfo, error) {
	var fr struct {
		Token string `xml:"Body>FindRecordingsResponse>SearchToken"`
	}
	if err := o.call(ctx, xaddr, "FindRecordings", `<tse:FindRecordings><tse:Scope/><tse:MaxMatches>100</tse:MaxMatches><tse:KeepAliveTime>PT10S</tse:KeepAliveTime></tse:FindRecordings>`, &fr); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(fr.Token)
	if token == "" {
		return nil, provider.NewError(provider.KindProtocol, nil, "ONVIF FindRecordings: keine Suchkennung")
	}
	completed := false
	defer func() {
		if !completed { // free the search session on the device (best effort)
			ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			_ = o.call(ectx, xaddr, "EndSearch", `<tse:EndSearch><tse:SearchToken>`+xmlText(token)+`</tse:SearchToken></tse:EndSearch>`, nil)
		}
	}()
	byToken := map[string]int{}
	var out []xRecordingInfo
	for poll := 0; poll < maxSearchPolls; poll++ {
		var r struct {
			State string           `xml:"Body>GetRecordingSearchResultsResponse>ResultList>SearchState"`
			Infos []xRecordingInfo `xml:"Body>GetRecordingSearchResultsResponse>ResultList>RecordingInformation"`
		}
		body := `<tse:GetRecordingSearchResults><tse:SearchToken>` + xmlText(token) + `</tse:SearchToken><tse:MinResults>1</tse:MinResults>` +
			`<tse:MaxResults>100</tse:MaxResults><tse:WaitTime>PT5S</tse:WaitTime></tse:GetRecordingSearchResults>`
		if err := o.call(ctx, xaddr, "GetRecordingSearchResults", body, &r); err != nil {
			return nil, err
		}
		for _, x := range r.Infos {
			if i, ok := byToken[x.Token]; ok {
				out[i] = x
				continue
			}
			byToken[x.Token] = len(out)
			out = append(out, x)
		}
		if strings.EqualFold(strings.TrimSpace(r.State), "Completed") {
			completed = true
			return out, nil
		}
		if len(r.Infos) == 0 { // device answered without waiting
			select {
			case <-ctx.Done():
				return nil, provider.ClassifyNetErr(ctx.Err(), "ONVIF-Aufnahmesuche")
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return nil, provider.NewError(provider.KindProtocol, nil, "ONVIF-Aufnahmesuche wird nicht abgeschlossen")
}

// mapChannels assigns recordings to channels: cameras have one channel; for
// recorders the order of preference is Source/VideoSourceToken, the
// recording jobs (source = media profile → its video source), a source id
// equal to a known token, and finally the order of the search results.
func (o *onvifDev) mapChannels(ctx context.Context, svc map[string]string, recs []onvifRecording) {
	media := o.mediaState()
	if media == nil || len(media.channels) <= 1 {
		for i := range recs { // a camera: all recordings belong to its channel
			recs[i].Channel = 0
		}
		return
	}
	sources := media.sourceIndex()
	var profiles map[string]string
	if media != nil {
		profiles = media.profileSource
	}
	byToken := func(tok string) int {
		if ch, ok := sources[tok]; ok {
			return ch
		}
		if src, ok := profiles[tok]; ok {
			if ch, ok := sources[src]; ok {
				return ch
			}
		}
		return -1
	}
	unmapped := func() bool {
		for _, r := range recs {
			if r.Channel < 0 {
				return true
			}
		}
		return false
	}
	for i := range recs {
		if recs[i].videoSource != "" {
			recs[i].Channel = byToken(recs[i].videoSource)
		}
	}
	if unmapped() && svc[nsRecording] != "" && len(sources) > 0 {
		jobs := o.recordingJobs(ctx, svc[nsRecording])
		for i := range recs {
			if recs[i].Channel < 0 {
				if tok, ok := jobs[recs[i].Token]; ok {
					recs[i].Channel = byToken(tok)
				}
			}
		}
	}
	for i := range recs {
		if recs[i].Channel < 0 && recs[i].sourceID != "" {
			recs[i].Channel = byToken(recs[i].sourceID)
		}
	}
	if !unmapped() {
		return
	}
	used := map[int]bool{}
	for _, r := range recs {
		if r.Channel >= 0 {
			used[r.Channel] = true
		}
	}
	next := 0
	for i := range recs {
		if recs[i].Channel >= 0 {
			continue
		}
		for used[next] {
			next++
		}
		recs[i].Channel = next
		used[next] = true
	}
}

// recordingJobs maps recording tokens to the source token of their job
// (best effort: errors leave the map empty).
func (o *onvifDev) recordingJobs(ctx context.Context, xaddr string) map[string]string {
	var r struct {
		Jobs []struct {
			Recording string `xml:"JobConfiguration>RecordingToken"`
			Sources   []struct {
				Type  string `xml:"Type,attr"`
				Token string `xml:"Token"`
			} `xml:"JobConfiguration>Source>SourceToken"`
		} `xml:"Body>GetRecordingJobsResponse>JobItem"`
	}
	out := map[string]string{}
	if err := o.call(ctx, xaddr, "GetRecordingJobs", `<trc:GetRecordingJobs/>`, &r); err != nil {
		return out
	}
	for _, j := range r.Jobs {
		for _, s := range j.Sources {
			typ := strings.TrimSpace(s.Type)
			if tok := strings.TrimSpace(s.Token); tok != "" && (typ == "" || strings.HasSuffix(typ, "/Profile")) {
				out[strings.TrimSpace(j.Recording)] = tok
				break
			}
		}
	}
	return out
}

// ---- driver integration -----------------------------------------------------------------------

// onvifSession returns the ONVIF state for search/playback calls.
// Authorization failures of these calls are returned as KindAuth without
// locking the device: the login check (GetDeviceInformation) passed with
// the same credentials, so they mean missing replay rights, and locking
// would also stop the live view. Real credential errors are caught by the
// next health check, which locks the device.
func (d *driver) onvifSession() (*onvifDev, error) {
	if d.ov == nil {
		return nil, provider.NewError(provider.KindUnsupported, nil, "Aufnahmen werden für dieses Modell nicht unterstützt")
	}
	if !d.env.AllowLogin() {
		return nil, provider.ErrLoginBlocked
	}
	return d.ov, nil
}

// SearchRecordings returns the recorded span of a channel clipped to [from, to].
func (d *driver) SearchRecordings(ctx context.Context, channel int, from, to time.Time) ([]provider.Recording, error) {
	o, err := d.onvifSession()
	if err != nil {
		return nil, err
	}
	recs, err := o.recordings(ctx, searchMaxAge)
	if err != nil {
		return nil, err
	}
	var out []provider.Recording
	for _, r := range recs {
		if r.Channel == channel {
			out = append(out, provider.Recording{Start: r.Start, End: r.End})
		}
	}
	return provider.MergeRecordings(out, from, to, 0), nil
}

// HasRecordings answers from the shared (cached) search; devices without
// Profile G have no recordings.
func (d *driver) HasRecordings(ctx context.Context, channel int, from, to time.Time) (bool, error) {
	o, err := d.onvifSession()
	if err != nil {
		return false, err
	}
	recs, err := o.recordings(ctx, probeMaxAge)
	if err != nil {
		if provider.KindOf(err) == provider.KindUnsupported {
			return false, nil
		}
		return false, err
	}
	for _, r := range recs {
		if r.Channel == channel && r.End.After(from) && r.Start.Before(to) {
			return true, nil
		}
	}
	return false, nil
}

// OpenPlayback replays [from, to] of a channel as MPEG-TS.
func (d *driver) OpenPlayback(ctx context.Context, channel int, from, to time.Time) (*provider.PlaybackSource, error) {
	o, err := d.onvifSession()
	if err != nil {
		return nil, err
	}
	recs, err := o.recordings(ctx, playbackMaxAge)
	if err != nil {
		return nil, err
	}
	rec, ok := pickRecording(recs, channel, from, to)
	if !ok {
		// the cached spans may end before footage recorded since: search again
		if recs, err = o.recordings(ctx, 0); err != nil {
			return nil, err
		}
		if rec, ok = pickRecording(recs, channel, from, to); !ok {
			return nil, provider.NewError(provider.KindNotFound, nil, "keine Aufnahme im gewählten Zeitraum")
		}
	}
	svc, err := o.services(ctx)
	if err != nil {
		return nil, err
	}
	if svc[nsReplay] == "" {
		return nil, provider.NewError(provider.KindUnsupported, nil, "ONVIF: Gerät bietet keine Wiedergabe (Replay-Dienst fehlt)")
	}
	uri, err := o.replayURI(ctx, svc[nsReplay], rec.Token)
	if err != nil {
		return nil, err
	}
	start := from
	if start.Before(rec.Start) {
		start = rec.Start
	}
	rd, err := rtsp.OpenReplay(ctx, uri, rtsp.Options{User: o.user, Pass: o.pass, SetupHeader: map[string]string{"Require": "onvif-replay"}},
		map[string]string{
			"Require":      "onvif-replay",
			"Range":        "clock=" + rtsp.ClockStamp(start, false) + "-" + rtsp.ClockStamp(to, true),
			"Rate-Control": "yes",
		}, start, to)
	if err != nil {
		return nil, err
	}
	return &provider.PlaybackSource{Reader: rd, Format: "mpegts"}, nil
}

// pickRecording selects the recording of a channel that contains from, or
// else the earliest one overlapping [from, to].
func pickRecording(recs []onvifRecording, channel int, from, to time.Time) (onvifRecording, bool) {
	var cand []onvifRecording
	for _, r := range recs {
		if r.Channel == channel && r.End.After(from) && r.Start.Before(to) {
			cand = append(cand, r)
		}
	}
	if len(cand) == 0 {
		return onvifRecording{}, false
	}
	sort.SliceStable(cand, func(i, j int) bool { return cand[i].Start.Before(cand[j].Start) })
	for _, r := range cand {
		if !r.Start.After(from) {
			return r, true
		}
	}
	return cand[0], true
}

func (o *onvifDev) replayURI(ctx context.Context, xaddr, token string) (string, error) {
	var r struct {
		URI string `xml:"Body>GetReplayUriResponse>Uri"`
	}
	body := `<trp:GetReplayUri><trp:StreamSetup><tt:Stream>RTP-Unicast</tt:Stream><tt:Transport><tt:Protocol>RTSP</tt:Protocol></tt:Transport></trp:StreamSetup>` +
		`<trp:RecordingToken>` + xmlText(token) + `</trp:RecordingToken></trp:GetReplayUri>`
	if err := o.call(ctx, xaddr, "GetReplayUri", body, &r); err != nil {
		return "", err
	}
	uri := o.fixStreamURI(r.URI)
	if uri == "" {
		return "", provider.NewError(provider.KindProtocol, nil, "ONVIF GetReplayUri: keine RTSP-URL")
	}
	return uri, nil
}
