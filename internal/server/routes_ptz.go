package server

import (
	"context"
	"net/http"
	"time"

	"cnm/internal/auth"
	"cnm/internal/ptz"
)

// PTZ control of motorised cameras (users only, never kiosk sessions).

// ptzUser returns the steering user, or writes an error.
func (s *Server) ptzUser(w http.ResponseWriter, r *http.Request) (ptz.User, string, bool) {
	id := r.PathValue("id")
	p := principal(r)
	if s.PTZ == nil || p == nil || p.Kind != auth.KindUser || !s.cameraAllowed(r, id) {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "PTZ-Steuerung für diese Kamera nicht erlaubt")
		return ptz.User{}, "", false
	}
	return ptz.User{ID: p.UserID, Name: p.Username, Admin: p.IsAdmin()}, id, true
}

func writePTZ(w http.ResponseWriter, e *ptz.Error, ok any) {
	switch {
	case e == nil:
		writeJSON(w, http.StatusOK, ok)
	case e.Status == http.StatusAccepted:
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "pending": true, "message": e.Message})
	default:
		writeErr(w, e.Status, e.Code, e.Message)
	}
}

func ptzCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 20*time.Second)
}

// handlePTZ returns features, presets and who steers (reads the camera).
func (s *Server) handlePTZ(w http.ResponseWriter, r *http.Request) {
	u, id, ok := s.ptzUser(w, r)
	if !ok {
		return
	}
	ctx, cancel := ptzCtx(r)
	defer cancel()
	v, e := s.PTZ.View(ctx, id, u)
	writePTZ(w, e, v)
}

// handlePTZStatus returns who steers (no camera I/O, polled by the view).
func (s *Server) handlePTZStatus(w http.ResponseWriter, r *http.Request) {
	u, id, ok := s.ptzUser(w, r)
	if !ok {
		return
	}
	c, e := s.PTZ.Status(id, u)
	writePTZ(w, e, map[string]any{"controller": c})
}

func (s *Server) handlePTZControl(w http.ResponseWriter, r *http.Request) {
	u, id, ok := s.ptzUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Take  bool `json:"take"`
		Force bool `json:"force"`
	}
	if err := readJSON(r, &body); err != nil {
		writeAnyErr(w, err)
		return
	}
	ctx, cancel := ptzCtx(r)
	defer cancel()
	if body.Take {
		c, e := s.PTZ.Take(ctx, id, u, body.Force)
		writePTZ(w, e, map[string]any{"controller": c})
		return
	}
	writePTZ(w, s.PTZ.Release(ctx, id, u), map[string]any{"ok": true})
}

func (s *Server) handlePTZMove(w http.ResponseWriter, r *http.Request) {
	u, id, ok := s.ptzUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Pan  float64 `json:"pan"`
		Tilt float64 `json:"tilt"`
		Zoom float64 `json:"zoom"`
	}
	if err := readJSON(r, &body); err != nil {
		writeAnyErr(w, err)
		return
	}
	ctx, cancel := ptzCtx(r)
	defer cancel()
	writePTZ(w, s.PTZ.Move(ctx, id, u, body.Pan, body.Tilt, body.Zoom), map[string]any{"ok": true})
}

func (s *Server) handlePTZPreset(w http.ResponseWriter, r *http.Request) {
	u, id, ok := s.ptzUser(w, r)
	if !ok {
		return
	}
	var body struct {
		ID int `json:"id"`
	}
	if err := readJSON(r, &body); err != nil {
		writeAnyErr(w, err)
		return
	}
	ctx, cancel := ptzCtx(r)
	defer cancel()
	writePTZ(w, s.PTZ.GotoPreset(ctx, id, u, body.ID), map[string]any{"ok": true})
}

// handlePTZGuard: "goto" moves to the guard position, "save" stores the
// current position as guard position (admins).
func (s *Server) handlePTZGuard(w http.ResponseWriter, r *http.Request) {
	u, id, ok := s.ptzUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := readJSON(r, &body); err != nil {
		writeAnyErr(w, err)
		return
	}
	ctx, cancel := ptzCtx(r)
	defer cancel()
	switch body.Action {
	case "goto":
		writePTZ(w, s.PTZ.GotoGuard(ctx, id, u), map[string]any{"ok": true})
	case "save":
		writePTZ(w, s.PTZ.SaveGuard(ctx, id, u), map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusBadRequest, "VALIDATION", "Unbekannte Aktion")
	}
}
