package server

import (
	"strings"

	"cnm/internal/config"
	"cnm/internal/media"
)

// Media and playback sessions are additionally registered per stream and per
// camera. After every configuration change the server closes the sessions
// that are no longer allowed – e.g. all live streams and playbacks of a camera
// that was just disabled, in every browser and kiosk at once. Handlers check
// permission again after registering, so a session that is being set up while
// the camera is disabled cannot slip through between check and registration.

func streamKey(scope, name string) string { return scope + "\x00" + name }

// revalidateSessions closes media and playback sessions that the new
// configuration no longer allows.
func (s *Server) revalidateSessions(c *config.Config) {
	allowed := map[string]map[string]bool{}
	for _, key := range s.streams.Keys() {
		scope, name, _ := strings.Cut(key, "\x00")
		a, ok := allowed[scope]
		if !ok {
			a = media.AllowedStreams(c, scope)
			allowed[scope] = a
		}
		if !c.System.Media.Enabled || !a[name] {
			if n := s.streams.Close(key); n > 0 {
				s.Log.Info("media sessions closed", "stream", name, "count", n, "reason", "stream no longer allowed")
			}
		}
	}
	for _, id := range s.playbacks.Keys() {
		if cam, ok := c.Camera(id); !ok || !cam.Enabled {
			if n := s.playbacks.Close(id); n > 0 {
				s.Log.Info("playback sessions closed", "camera", id, "count", n, "reason", "camera disabled or removed")
			}
		}
	}
}

// streamAllowed re-checks a stream against the current configuration.
func (s *Server) streamAllowed(scope, name string) bool {
	c := s.Store.Get()
	return c.System.Media.Enabled && media.AllowedStreams(c, scope)[name]
}
