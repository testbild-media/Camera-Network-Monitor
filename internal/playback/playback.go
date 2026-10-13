// Package playback serves historical DVR recordings to browsers.
//
// Sources of different DVRs (RTSP playback URLs, native DHAV byte streams)
// are normalised by FFmpeg into one browser format: fragmented MP4, video
// only, codec copied (no transcoding). Seeking is done by reopening the DVR
// source at an absolute time – no HTTP byte ranges. The FFmpeg process lives
// exactly as long as the client request.
package playback

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"cnm/internal/config"
	"cnm/internal/device"
	"cnm/internal/procutil"
	"cnm/internal/provider"
)

// Limits.
const (
	MaxRange       = 24 * time.Hour
	MaxDownload    = 2 * time.Hour
	DefaultRange   = 15 * time.Minute
	StartupTimeout = 12 * time.Second
	MaxResultRows  = 100
	perDeviceLimit = 3
	globalLimit    = 12
)

// Error codes.
const (
	CodeNoStorage      = "NO_STORAGE"
	CodeNoRecordings   = "NO_RECORDINGS"
	CodeUnsupported    = "PLAYBACK_UNSUPPORTED"
	CodeInvalidRange   = "INVALID_TIME_RANGE"
	CodeRangeTooLarge  = "RANGE_TOO_LARGE"
	CodeStartTimeout   = "PLAYBACK_START_TIMEOUT"
	CodeMediaError     = "PLAYBACK_MEDIA_ERROR"
	CodeDeviceOffline  = "DEVICE_OFFLINE"
	CodeBusy           = "PLAYBACK_BUSY"
	CodeCameraNotFound = "CAMERA_NOT_FOUND"
	CodeSearchFailed   = "SEARCH_FAILED"
)

// Error is a structured playback error.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Range is a recorded time range in epoch milliseconds.
type Range struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Info describes playback availability for a camera.
type Info struct {
	CameraID       string `json:"cameraId"`
	Available      bool   `json:"available"`
	Code           string `json:"code,omitempty"`
	Message        string `json:"message,omitempty"`
	Storage        string `json:"storage"`
	SearchKind     string `json:"searchKind,omitempty"`
	MaxRangeMs     int64  `json:"maxRangeMs"`
	DefaultRangeMs int64  `json:"defaultRangeMs"`
	Recordings     string `json:"recordings"` // yes | no | unknown
}

// SearchResult of a recording search.
type SearchResult struct {
	Ranges      []Range `json:"ranges"`
	Total       int     `json:"total"`
	Truncated   bool    `json:"truncated"`
	Approximate bool    `json:"approximate"`
}

// Service implements playback.
type Service struct {
	cfg     func() *config.Config
	devices *device.Manager
	log     *slog.Logger

	mu     sync.Mutex
	perDev map[string]int
	total  int

	idx index
}

// New creates the service.
func New(cfg func() *config.Config, devices *device.Manager, log *slog.Logger) *Service {
	s := &Service{cfg: cfg, devices: devices, log: log, perDev: map[string]int{}}
	s.idx.init()
	return s
}

type target struct {
	dev   *config.Device
	cam   *config.Camera
	prov  provider.Provider
	model provider.Model
	caps  provider.Capabilities
}

func (s *Service) resolve(cameraID string) (*target, *Error) {
	c := s.cfg()
	d, cam, ok := c.CameraSource(cameraID)
	if !ok || !cam.Enabled {
		return nil, errf(404, CodeCameraNotFound, "Kamera nicht gefunden oder deaktiviert")
	}
	p, m, err := provider.Resolve(d.Protocol)
	if err != nil {
		return nil, errf(422, CodeUnsupported, "Für dieses Gerät ist keine Wiedergabe verfügbar")
	}
	return &target{dev: d, cam: cam, prov: p, model: m, caps: provider.DeviceCapabilities(p, m, d.Variant())}, nil
}

// storageState is detected automatically from the device report.
func (s *Service) storageState(t *target) string {
	if s.devices == nil {
		return string(provider.StorageUnknown)
	}
	if st, ok := s.devices.Status(t.dev.ID); ok && st.Storage != "" {
		return st.Storage
	}
	return string(provider.StorageUnknown)
}

// check runs the spec order: storage → capability.
func (s *Service) check(t *target) *Error {
	if s.storageState(t) == string(provider.StorageAbsent) {
		return errf(409, CodeNoStorage, "Das Gerät hat keine Festplatte – es sind keine Aufnahmen vorhanden.")
	}
	if !t.caps.Playback.Enabled() {
		return errf(409, CodeUnsupported, "Wiedergabe wird für %s nicht unterstützt.", t.model.Name)
	}
	return nil
}

// Info reports availability.
func (s *Service) Info(cameraID string) (*Info, *Error) {
	t, e := s.resolve(cameraID)
	if e != nil {
		return nil, e
	}
	info := &Info{CameraID: cameraID, Storage: s.storageState(t), SearchKind: t.caps.SearchKind, MaxRangeMs: MaxRange.Milliseconds(), DefaultRangeMs: DefaultRange.Milliseconds()}
	if e := s.check(t); e != nil {
		info.Code, info.Message = e.Code, e.Message
		return info, nil
	}
	info.Recordings = string(s.Availability(cameraID))
	if info.Recordings == string(AvailNo) {
		info.Code, info.Message = CodeNoRecordings, "Für diese Kamera sind auf dem Gerät keine Aufnahmen vorhanden."
		return info, nil
	}
	info.Available = true
	return info, nil
}

// ValidateRange checks a requested time range.
func ValidateRange(from, to time.Time) *Error {
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return errf(400, CodeInvalidRange, "Ungültiger Zeitraum: Start muss vor Ende liegen.")
	}
	if to.Sub(from) > MaxRange {
		return errf(400, CodeRangeTooLarge, "Der Zeitraum darf höchstens 24 Stunden umfassen.")
	}
	if from.After(time.Now().Add(5 * time.Minute)) {
		return errf(400, CodeInvalidRange, "Der Zeitraum liegt in der Zukunft.")
	}
	return nil
}

func (s *Service) driver(t *target) (provider.Driver, *Error) {
	drv, _, err := s.devices.Driver(t.dev.ID)
	if err != nil {
		return nil, errf(503, CodeDeviceOffline, "Das Gerät ist derzeit nicht verbunden.")
	}
	return drv, nil
}

// Search looks up recordings in [from, to].
func (s *Service) Search(ctx context.Context, cameraID string, from, to time.Time) (*SearchResult, *Error) {
	if e := ValidateRange(from, to); e != nil {
		return nil, e
	}
	t, e := s.resolve(cameraID)
	if e != nil {
		return nil, e
	}
	if e := s.check(t); e != nil {
		return nil, e
	}
	drv, e := s.driver(t)
	if e != nil {
		return nil, e
	}
	searcher, ok := drv.(provider.RecordingSearcher)
	if !ok || !t.caps.RecordingSearch.Enabled() {
		// no search: offer the whole range, the stream reports missing data
		return &SearchResult{Ranges: []Range{{from.UnixMilli(), to.UnixMilli()}}, Total: 1, Approximate: true}, nil
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	recs, err := searcher.SearchRecordings(sctx, t.cam.Channel, from, to)
	if err != nil {
		s.log.Info("recording search failed", "camera", cameraID, "error", provider.Redact(err.Error()))
		if provider.KindOf(err) == provider.KindAuth {
			return nil, errf(502, CodeSearchFailed, "Das Gerät lehnt die Anmeldung ab.")
		}
		return nil, errf(502, CodeSearchFailed, "Die Aufnahmesuche ist fehlgeschlagen.")
	}
	if len(recs) == 0 {
		return nil, errf(404, CodeNoRecordings, "Im gewählten Zeitraum wurden keine Aufnahmen gefunden.")
	}
	// "probe" (time probe) and "span" (first/last recording only) are approximate
	res := &SearchResult{Total: len(recs), Approximate: t.caps.SearchKind != "list"}
	for _, r := range recs {
		res.Ranges = append(res.Ranges, Range{r.Start.UnixMilli(), r.End.UnixMilli()})
	}
	return res, nil
}

func (s *Service) acquire(devID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= globalLimit || s.perDev[devID] >= perDeviceLimit {
		return false
	}
	s.total++
	s.perDev[devID]++
	return true
}

func (s *Service) release(devID string) {
	s.mu.Lock()
	s.total--
	s.perDev[devID]--
	if s.perDev[devID] <= 0 {
		delete(s.perDev, devID)
	}
	s.mu.Unlock()
}

// Active returns the number of running playback streams.
func (s *Service) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// FFmpegArgs builds the FFmpeg command line for a source (exported for tests).
func FFmpegArgs(src *provider.PlaybackSource) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-fflags", "+genpts+discardcorrupt", "-analyzeduration", "3000000", "-probesize", "5000000"}
	if src.Reader != nil {
		args = append(args, "-f", src.Format, "-i", "pipe:0")
	} else {
		args = append(args, src.InputArgs...)
		if strings.HasPrefix(src.URL, "rtsp") {
			args = append(args, "-timeout", "10000000")
		}
		args = append(args, "-i", src.URL)
	}
	args = append(args,
		"-map", "0:v:0", "-an", "-sn", "-dn",
		"-c:v", "copy",
		"-f", "mp4",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-frag_duration", "500000",
		"-flush_packets", "1",
		"pipe:1",
	)
	return args
}

// Stream runs the playback pipeline and writes fragmented MP4 to w. Errors
// before the first byte are returned (and written as JSON by the caller).
func (s *Service) Stream(ctx context.Context, w http.ResponseWriter, cameraID string, from, to time.Time) *Error {
	return s.run(ctx, w, cameraID, from, to, "")
}

// Download writes [from, to] as an MP4 attachment (the playback pipeline
// with Content-Disposition). Sources that the device only delivers in real
// time take as long as the range.
func (s *Service) Download(ctx context.Context, w http.ResponseWriter, cameraID string, from, to time.Time) *Error {
	if to.Sub(from) > MaxDownload {
		return errf(400, CodeRangeTooLarge, "Ein Download darf höchstens 2 Stunden umfassen.")
	}
	t, e := s.resolve(cameraID)
	if e != nil {
		return e
	}
	if !t.caps.Download.Enabled() {
		return errf(409, CodeUnsupported, "Download wird für %s nicht unterstützt.", t.model.Name)
	}
	name := t.cam.Label
	if name == "" {
		name = fmt.Sprintf("%s-Kanal-%d", t.dev.Label, t.cam.Channel+1)
	}
	return s.run(ctx, w, cameraID, from, to, fileName(name, from))
}

// fileName builds a safe download name: "<camera>_<YYYY-MM-DD_HH-MM-SS>.mp4".
func fileName(camera string, from time.Time) string {
	var b strings.Builder
	for _, r := range camera {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.',
			strings.ContainsRune("äöüÄÖÜß", r):
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		}
	}
	n := strings.Trim(b.String(), "._")
	if n == "" {
		n = "Aufnahme"
	}
	return n + "_" + from.In(time.Local).Format("2006-01-02_15-04-05") + ".mp4"
}

func (s *Service) run(ctx context.Context, w http.ResponseWriter, cameraID string, from, to time.Time, attachment string) *Error {
	if e := ValidateRange(from, to); e != nil {
		return e
	}
	t, e := s.resolve(cameraID)
	if e != nil {
		return e
	}
	if e := s.check(t); e != nil {
		return e
	}
	drv, e := s.driver(t)
	if e != nil {
		return e
	}
	opener, ok := drv.(provider.PlaybackOpener)
	if !ok {
		return errf(409, CodeUnsupported, "Wiedergabe wird für dieses Gerät nicht unterstützt.")
	}
	if !s.acquire(t.dev.ID) {
		return errf(429, CodeBusy, "Zu viele gleichzeitige Wiedergaben für dieses Gerät.")
	}
	defer s.release(t.dev.ID)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	startCtx, startCancel := context.WithTimeout(ctx, StartupTimeout)
	defer startCancel()

	src, err := opener.OpenPlayback(startCtx, t.cam.Channel, from, to)
	if err != nil {
		if startCtx.Err() == context.DeadlineExceeded {
			return errf(504, CodeStartTimeout, "Die Wiedergabe startet nicht (Zeitüberschreitung).")
		}
		if provider.KindOf(err) == provider.KindNotFound {
			return errf(404, CodeNoRecordings, "Für den gewählten Zeitpunkt liefert das Gerät keine Aufnahme.")
		}
		s.log.Info("open playback failed", "camera", cameraID, "error", provider.Redact(err.Error()))
		return errf(502, CodeMediaError, "Die Aufnahme konnte am Gerät nicht geöffnet werden.")
	}
	if src.Reader != nil {
		defer src.Reader.Close()
	}

	return s.pipe(ctx, startCtx, w, cameraID, src, attachment)
}

// pipe runs FFmpeg for an opened source and streams fragmented MP4 to w
// (as a file download when attachment names the file).
func (s *Service) pipe(ctx, startCtx context.Context, w http.ResponseWriter, cameraID string, src *provider.PlaybackSource, attachment string) *Error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bin := procutil.Resolve(s.cfg().System.Media.FFmpegBin, "ffmpeg")
	cmd := exec.CommandContext(ctx, bin, FFmpegArgs(src)...)
	procutil.Prepare(cmd, time.Second)
	if src.Reader != nil {
		cmd.Stdin = src.Reader
	}
	stderr := &limitedWriter{w: &bytes.Buffer{}, n: 16 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errf(500, CodeMediaError, "FFmpeg konnte nicht gestartet werden.")
	}
	if err := cmd.Start(); err != nil {
		s.log.Error("ffmpeg start failed", "binary", bin, "error", err)
		return errf(500, CodeMediaError, "FFmpeg ist nicht verfügbar.")
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		cancel() // kills FFmpeg (SIGTERM, then SIGKILL after 1 s)
		<-exited
	}()

	rd := bufio.NewReaderSize(stdout, 256<<10)
	type initRes struct {
		data []byte
		err  error
	}
	initCh := make(chan initRes, 1)
	go func() {
		d, err := readInitSegment(rd)
		initCh <- initRes{d, err}
	}()
	var init []byte
	select {
	case r := <-initCh:
		if r.err != nil {
			cancel()
			<-exited // stderr is complete once Wait returned
			return s.classifyFailure(cameraID, stderr.String())
		}
		init = r.data
	case <-startCtx.Done():
		if ctx.Err() != nil {
			return errf(499, CodeMediaError, "abgebrochen")
		}
		s.log.Info("playback startup timeout", "camera", cameraID)
		return errf(504, CodeStartTimeout, "Die Wiedergabe startet nicht (Zeitüberschreitung).")
	}
	codec, err := CodecString(init)
	if err != nil {
		return errf(415, CodeMediaError, "Das Videoformat der Aufnahme wird nicht unterstützt.")
	}
	h := w.Header()
	h.Set("Content-Type", fmt.Sprintf(`video/mp4; codecs="%s"`, codec))
	if attachment != "" {
		h.Set("Content-Type", "video/mp4")
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, asciiName(attachment), url.PathEscape(attachment)))
	}
	h.Set("X-CNM-Codec", codec)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(init); err != nil {
		return nil
	}
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := rd.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil // client went away → deferred cancel kills FFmpeg
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				s.log.Info("playback ended with error", "camera", cameraID, "error", provider.Redact(stderr.String()))
			}
			return nil
		}
	}
}

// status codes as whole words – URLs in FFmpeg messages contain ports and
// timestamps such as "T140100Z" or ":54012"
var (
	reStatus401 = regexp.MustCompile(`\b401\b`)
	reStatus404 = regexp.MustCompile(`\b404\b`)
)

func (s *Service) classifyFailure(cameraID, stderr string) *Error {
	msg := provider.Redact(strings.TrimSpace(stderr))
	s.log.Info("playback start failed", "camera", cameraID, "ffmpeg", msg)
	l := strings.ToLower(msg)
	switch {
	case reStatus401.MatchString(l) || strings.Contains(l, "unauthorized"):
		return errf(502, CodeMediaError, "Das Gerät verweigert die Wiedergabe (Anmeldung abgelehnt).")
	case reStatus404.MatchString(l) || strings.Contains(l, "not found"):
		return errf(404, CodeNoRecordings, "Für den gewählten Zeitpunkt liefert das Gerät keine Aufnahme.")
	case strings.Contains(l, "invalid data") || strings.Contains(l, "could not find codec"):
		return errf(502, CodeMediaError, "Die Aufnahme liefert keine lesbaren Videodaten.")
	}
	return errf(502, CodeMediaError, "Die Wiedergabe konnte nicht gestartet werden.")
}

// asciiName replaces non-ASCII letters for the plain filename parameter.
func asciiName(s string) string {
	r := strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss")
	return r.Replace(s)
}

type limitedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
	n  int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := l.n - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}

func (l *limitedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.String()
}
