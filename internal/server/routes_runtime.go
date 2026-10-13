package server

import (
	"context"
	"net/http"

	"cnm/internal/auth"
	"strconv"
	"strings"
	"time"

	"cnm/internal/config"
	"cnm/internal/media"
	"cnm/internal/playback"
	"cnm/internal/provider"
)

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"state":     s.publicState(p.ScopeMonitor()),
		"principal": p,
		"media":     s.mediaSummary(p.ScopeMonitor() != ""),
	})
}

func (s *Server) handleMonitors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, monitorList(s.Store.Get(), principal(r).ScopeMonitor()))
}

func (s *Server) handleCameras(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cameraList(s.Store.Get(), principal(r).ScopeMonitor()))
}

// handleMediaStatus serves the browser watchdog: the summary plus runtime
// health for the requested streams (filtered to the caller's scope).
func (s *Server) handleMediaStatus(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	allowed := media.AllowedStreams(s.Store.Get(), p.ScopeMonitor())
	var names []string
	for _, n := range strings.Split(r.URL.Query().Get("streams"), ",") {
		if n = strings.TrimSpace(n); n != "" && allowed[n] {
			names = append(names, n)
		}
		if len(names) >= 128 {
			break
		}
	}
	resp := map[string]any{
		"summary": s.mediaSummary(p.ScopeMonitor() != ""),
		"streams": s.Recon.Health(names),
	}
	if p.IsAdmin() && s.Process != nil {
		resp["process"] = s.Process.Status()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &body); err != nil {
		writeAnyErr(w, err)
		return
	}
	if !s.Devices.Reconnect(body.ID) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "Gerät nicht gefunden")
		return
	}
	s.Log.Info("manual reconnect", "device", body.ID, "by", principal(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleProtocols lists the protocols of the device dialog.
func (s *Server) handleProtocols(w http.ResponseWriter, r *http.Request) {
	out := provider.Protocols()
	for i := range out {
		out[i].Capabilities = out[i].Capabilities.Normalized()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, config.LayoutTemplates)
}

// ---- playback ----------------------------------------------------------------------------

// cameraAllowed decides whether the caller may use playback for a camera.
// Kiosk sessions are live only: no playback at all.
func (s *Server) cameraAllowed(r *http.Request, cameraID string) bool {
	p := principal(r)
	if p == nil || p.Kind != auth.KindUser {
		return false
	}
	_, ok := s.Store.Get().Camera(cameraID)
	return ok
}

func writePlaybackErr(w http.ResponseWriter, e *playback.Error) {
	writeJSON(w, e.Status, map[string]any{"error": e})
}

func (s *Server) handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("cameraId")
	if !s.cameraAllowed(r, id) {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "Wiedergabe für diese Kamera nicht erlaubt")
		return
	}
	info, e := s.Playback.Info(id)
	if e != nil {
		writePlaybackErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// parseTime accepts epoch milliseconds or RFC 3339.
func parseTime(v any) time.Time {
	switch x := v.(type) {
	case float64:
		return time.UnixMilli(int64(x))
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return time.UnixMilli(n)
		}
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (s *Server) handlePlaybackSearch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CameraID string `json:"cameraId"`
		From     any    `json:"from"`
		To       any    `json:"to"`
	}
	if err := readJSON(r, &body); err != nil {
		writeAnyErr(w, err)
		return
	}
	if !s.cameraAllowed(r, body.CameraID) {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "Wiedergabe für diese Kamera nicht erlaubt")
		return
	}
	res, e := s.Playback.Search(r.Context(), body.CameraID, parseTime(body.From), parseTime(body.To))
	if e != nil {
		writePlaybackErr(w, e)
		return
	}
	if len(res.Ranges) > playback.MaxResultRows {
		res.Truncated = true
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handlePlaybackStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("cameraId")
	if !s.cameraAllowed(r, id) {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "Wiedergabe für diese Kamera nicht erlaubt")
		return
	}
	p := principal(r)
	ctx, cancel := contextWithCancel(r)
	defer cancel()
	// terminated on logout / kiosk revoke / shutdown
	removeSess := s.Conns.Add(p.ConnKey(), cancel)
	defer removeSess()
	removeAll := s.all.Add("playback", cancel)
	defer removeAll()
	// registered before Playback re-checks the camera: disabling it closes the stream
	removeCam := s.playbacks.Add(id, cancel)
	defer removeCam()
	if e := s.Playback.Stream(ctx, w, id, parseTime(q.Get("start")), parseTime(q.Get("end"))); e != nil {
		if ctx.Err() == nil {
			writePlaybackErr(w, e)
		}
	}
}

// handlePlaybackDownload exports a recorded range as MP4 file.
func (s *Server) handlePlaybackDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("cameraId")
	if !s.cameraAllowed(r, id) {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "Download für diese Kamera nicht erlaubt")
		return
	}
	p := principal(r)
	ctx, cancel := contextWithCancel(r)
	defer cancel()
	removeSess := s.Conns.Add(p.ConnKey(), cancel)
	defer removeSess()
	removeAll := s.all.Add("playback", cancel)
	defer removeAll()
	removeCam := s.playbacks.Add(id, cancel)
	defer removeCam()
	s.Log.Info("recording download", "camera", id, "by", p.Username)
	if e := s.Playback.Download(ctx, w, id, parseTime(q.Get("start")), parseTime(q.Get("end"))); e != nil {
		if ctx.Err() == nil {
			writePlaybackErr(w, e)
		}
	}
}

func contextWithCancel(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithCancel(r.Context())
}
