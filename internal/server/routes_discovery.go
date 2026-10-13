package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"cnm/internal/discovery"
)

var scanner discovery.Scanner

// handleDiscovery runs a manual network search (admin only, one at a time)
// and marks devices that are already configured.
func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Range string `json:"range"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &in); err != nil {
			writeAnyErr(w, err)
			return
		}
	}
	if _, err := discovery.ParseRange(in.Range); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_RANGE", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := scanner.Scan(ctx, discovery.Options{Range: in.Range})
	if err != nil {
		if errors.Is(err, discovery.ErrBusy) {
			writeErr(w, http.StatusConflict, "SCAN_BUSY", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "SCAN_FAILED", err.Error())
		return
	}
	hosts := map[string][2]string{}
	for _, d := range s.Store.Get().Devices {
		hosts[d.Host] = [2]string{d.ID, d.Label}
	}
	discovery.MarkAdded(res.Devices, hosts)
	s.Log.Info("network scan", "by", principal(r).Username, "range", in.Range, "found", len(res.Devices), "ms", res.DurationMs)
	writeJSON(w, http.StatusOK, res)
}
