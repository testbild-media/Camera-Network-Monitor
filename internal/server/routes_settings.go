package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cnm/internal/auth"
	"cnm/internal/config"
	"cnm/internal/device"
	"cnm/internal/provider"
	"cnm/internal/secret"
)

// ---- views (never contain passwords, hashes or token material) --------------------------------

type deviceView struct {
	ID           string            `json:"id"`
	Label        string            `json:"label"`
	Protocol     string            `json:"protocol"`
	Detected     *config.Detection `json:"detected,omitempty"`
	Host         string            `json:"host"`
	Username     string            `json:"username"`
	PasswordSet  bool              `json:"passwordSet"`
	Ports        map[string]int    `json:"ports"`
	ChannelCount int               `json:"channelCount"`
	Options      map[string]string `json:"options"`
	Status       *device.Status    `json:"status,omitempty"`
}

type cameraView struct {
	config.Camera
	DeviceLabel string   `json:"deviceLabel"`
	DeviceName  string   `json:"deviceName,omitempty"` // name reported by the device
	Groups      []string `json:"groups"`
	Recordings  string   `json:"recordings"` // yes | no | unknown
	Playback    bool     `json:"playbackCapable"`
	Multiview   string   `json:"multiview,omitempty"`
	Preview     string   `json:"preview,omitempty"` // sub-only stream for the settings preview
}

type keyView struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	MonitorID   string `json:"monitorId"`
	CreatedAt   string `json:"createdAt"`
	Enabled     bool   `json:"enabled"`
	RevokedAt   string `json:"revokedAt,omitempty"`
	Recoverable bool   `json:"recoverable"`
}

type userView struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	Role        string    `json:"role"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   string    `json:"createdAt"`
	PasswordSet bool      `json:"passwordSet"`
	APIKeys     []keyView `json:"apiKeys"`
}

func (s *Server) deviceViews(c *config.Config) []deviceView {
	out := []deviceView{}
	for _, d := range c.Devices {
		v := deviceView{
			ID: d.ID, Label: d.Label, Protocol: d.Protocol, Detected: d.Detected, Host: d.Host,
			Username: d.Credentials.Username, PasswordSet: d.Credentials.HasPassword(),
			Ports: d.Ports, ChannelCount: d.ChannelCount, Options: d.Options,
		}
		if st, ok := s.Devices.Status(d.ID); ok {
			v.Status = &st
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) cameraViews(c *config.Config) []cameraView {
	streams := cameraStreams(c)
	out := []cameraView{}
	for _, cam := range c.Cameras {
		d, ok := c.Device(cam.DeviceID)
		if !ok {
			continue
		}
		v := cameraView{Camera: cam, DeviceLabel: d.Label, Groups: c.CameraGroups(cam.ID), Recordings: string(s.Playback.Availability(cam.ID))}
		if st, ok := s.Devices.Status(d.ID); ok && st.Info != nil {
			v.DeviceName = st.Info.ChannelNames[cam.Channel]
		}
		if p, m, err := provider.Resolve(d.Protocol); err == nil {
			v.Playback = provider.DeviceCapabilities(p, m, d.Variant()).Playback.Enabled()
		}
		if cs := streams[cam.ID]; cs != nil {
			v.Multiview = cs.Multiview
			v.Preview = cs.Preview
		}
		out = append(out, v)
	}
	return out
}

func userViews(c *config.Config) []userView {
	out := []userView{}
	for _, u := range c.Users {
		v := userView{ID: u.ID, Username: u.Username, Role: u.Role, Enabled: u.Enabled, CreatedAt: u.CreatedAt, PasswordSet: secret.HasUsableHash(u.PasswordHash), APIKeys: []keyView{}}
		for _, k := range u.APIKeys {
			v.APIKeys = append(v.APIKeys, keyView{ID: k.ID, Label: k.Label, MonitorID: k.MonitorID, CreatedAt: k.CreatedAt, Enabled: k.Enabled, RevokedAt: k.RevokedAt, Recoverable: k.Recoverable()})
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	c := s.Store.Get()
	resp := map[string]any{
		"system":   c.System,
		"devices":  s.deviceViews(c),
		"cameras":  s.cameraViews(c),
		"groups":   c.Groups,
		"monitors": c.Monitors,
		"users":    userViews(c),
		"self":     principal(r).UserID,
		"media":    s.Recon.Status(),
	}
	if s.Process != nil {
		resp["process"] = s.Process.Status()
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- system -------------------------------------------------------------------------------

func validListen(addr string) bool {
	if addr == "" {
		return true
	}
	_, port, err := net.SplitHostPort(addr)
	return err == nil && port != ""
}

func (s *Server) handleSaveSystem(w http.ResponseWriter, r *http.Request) {
	var in config.System
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "" || len([]rune(in.Name)) > 80:
		writeErr(w, 400, "VALIDATION", "Systemname fehlt oder ist zu lang")
		return
	case net.ParseIP(strings.Trim(in.ListenHost, "[]")) == nil && in.ListenHost != "localhost":
		writeErr(w, 400, "VALIDATION", "Listen Host muss eine IP-Adresse sein (z. B. 0.0.0.0)")
		return
	case in.ListenPort < 1 || in.ListenPort > 65535:
		writeErr(w, 400, "VALIDATION", "Listen Port muss zwischen 1 und 65535 liegen")
		return
	case in.HealthIntervalMs < 5000 || in.HealthIntervalMs > 3600000:
		writeErr(w, 400, "VALIDATION", "Healthcheck-Intervall muss zwischen 5 s und 1 h liegen")
		return
	case in.ReconnectMinMs < 1000 || in.ReconnectMaxMs < in.ReconnectMinMs || in.ReconnectMaxMs > 3600000:
		writeErr(w, 400, "VALIDATION", "Reconnect: Minimum ≥ 1 s und Maximum ≥ Minimum")
		return
	case in.Media.ReconcileIntervalMs < 2000 || in.Media.ReconcileIntervalMs > 600000:
		writeErr(w, 400, "VALIDATION", "Reconcile-Intervall muss zwischen 2 s und 10 min liegen")
		return
	}
	if u, err := url.Parse(in.Media.Go2rtcURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		writeErr(w, 400, "VALIDATION", "go2rtc API URL ist ungültig")
		return
	}
	for _, l := range []string{in.Media.Go2rtcAPIListen, in.Media.Go2rtcRTSPListen, in.Media.Go2rtcWebRTCListen} {
		if !validListen(l) {
			writeErr(w, 400, "VALIDATION", "go2rtc Listen-Adressen müssen die Form host:port haben")
			return
		}
	}
	prev := s.Store.Get().System
	next, err := s.Store.Update(func(c *config.Config) error {
		// keep values not editable in the UI
		in.SessionTTLHours = c.System.SessionTTLHours
		in.KioskSessionTTLDays = c.System.KioskSessionTTLDays
		in.Display = c.System.Display // edited in the displays tab
		if in.Media.Go2rtcRestartDelayMs == 0 {
			in.Media.Go2rtcRestartDelayMs = c.System.Media.Go2rtcRestartDelayMs
		}
		c.System = in
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	s.Log.Info("system settings saved", "by", principal(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{
		"system":          next.System,
		"restartRequired": prev.ListenHost != next.System.ListenHost || prev.ListenPort != next.System.ListenPort,
	})
}

// ---- devices -------------------------------------------------------------------------------

type deviceInput struct {
	ID       string            `json:"id"` // empty = new device (a UUID is assigned)
	Label    string            `json:"label"`
	Protocol string            `json:"protocol"`
	Host     string            `json:"host"`
	Ports    map[string]int    `json:"ports"`
	Options  map[string]string `json:"options"`
	Username string            `json:"username"`
	Password string            `json:"password"` // empty = keep
}

// validateDevice builds a normalised device from the admin input.
func validateDevice(in *deviceInput) (*config.Device, provider.Model, error) {
	d := &config.Device{
		ID:    in.ID,
		Label: strings.TrimSpace(in.Label),
		Host:  strings.TrimSpace(in.Host),
	}
	if d.Host == "" || strings.ContainsAny(d.Host, "/@ ?#") {
		return nil, provider.Model{}, config.Invalid("IP-Adresse oder Hostname fehlt bzw. ist ungültig")
	}
	if d.Label == "" {
		d.Label = d.Host // the address is a fine name until the admin chooses one
	}
	if len([]rune(d.Label)) > 80 {
		return nil, provider.Model{}, config.Invalid("Bezeichnung ist zu lang")
	}
	_, model, err := provider.Resolve(in.Protocol)
	if err != nil {
		return nil, provider.Model{}, config.Invalid("%s", err.Error())
	}
	d.Protocol = model.ID
	d.Ports = map[string]int{}
	for _, ps := range model.Ports {
		v := in.Ports[ps.Key]
		if v == 0 {
			v = ps.Default
		}
		if v < 1 || v > 65535 {
			return nil, model, config.Invalid("Port %s muss zwischen 1 und 65535 liegen", ps.Label)
		}
		d.Ports[ps.Key] = v
	}
	d.Options = map[string]string{}
	for _, o := range model.Options {
		v, set := in.Options[o.Key]
		if !set {
			v = o.Default
		}
		v = strings.TrimSpace(v)
		switch o.Type {
		case provider.OptionText:
			if len(v) > 300 || strings.ContainsAny(v, " \t\r\n") {
				return nil, model, config.Invalid("%s: ungültiger Wert", o.Label)
			}
		case provider.OptionNumber:
			if v == "" && o.Default == "" {
				break // optional (e.g. a channel override)
			}
			n, err := strconv.Atoi(v)
			if err != nil || (o.Max > 0 && (n < o.Min || n > o.Max)) {
				return nil, model, config.Invalid("%s muss eine Zahl zwischen %d und %d sein", o.Label, o.Min, o.Max)
			}
		default:
			valid := false
			for _, ch := range o.Choices {
				if ch.Value == v {
					valid = true
				}
			}
			if !valid {
				v = o.Default
			}
		}
		d.Options[o.Key] = v
	}
	if model.AutoDetect {
		// detected at the first connection; an override counts right away
		d.ChannelCount, _ = strconv.Atoi(d.Options[provider.OptChannels])
	} else {
		d.ChannelCount = provider.ChannelCount(model, d.Options)
	}
	d.Credentials.Username = strings.TrimSpace(in.Username)
	return d, model, nil
}

func (s *Server) handleSaveDevice(w http.ResponseWriter, r *http.Request) {
	var in deviceInput
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	d, model, err := validateDevice(&in)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	var encPw []byte
	if in.Password != "" {
		enc, err := s.Keys.Encrypt(in.Password)
		if err != nil {
			writeAnyErr(w, err)
			return
		}
		encPw, _ = json.Marshal(enc)
	}
	next, err := s.Store.Update(func(c *config.Config) error {
		if d.ID == "" {
			d.ID = config.NewUUID()
			d.Credentials.Password = encPw
			c.Devices = append(c.Devices, *d)
			return nil
		}
		ex, ok := c.Device(d.ID)
		if !ok {
			return config.Invalid("Gerät existiert nicht mehr")
		}
		if encPw != nil {
			d.Credentials.Password = encPw
		} else {
			d.Credentials.Password = ex.Credentials.Password // empty field = unchanged
		}
		d.Metadata = ex.Metadata
		// the detection stays valid unless the admin pointed the entry at
		// another device or protocol; the camera count stays until then
		if ex.Protocol == d.Protocol && ex.Host == d.Host && portsEqual(ex.Ports, d.Ports) {
			d.Detected = ex.Detected
		}
		if model.AutoDetect && d.ChannelCount == 0 {
			d.ChannelCount = ex.ChannelCount
		}
		*ex = *d
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	s.Log.Info("device saved", "device", d.ID, "label", d.Label, "by", principal(r).Username)
	if s.Playback != nil {
		s.Playback.RecheckDevice(d.ID)
	}
	if dd, ok := next.Device(d.ID); ok {
		writeJSON(w, http.StatusOK, s.deviceViews(&config.Config{Devices: []config.Device{*dd}})[0])
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTestDevice(w http.ResponseWriter, r *http.Request) {
	var in deviceInput
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	d, _, err := validateDevice(&in)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	if d.ID == "" {
		d.ID = "test"
	}
	password := in.Password
	if password == "" && in.ID != "" {
		if ex, ok := s.Store.Get().Device(in.ID); ok && ex.Credentials.HasPassword() {
			pw, err := s.Keys.Decrypt(ex.Credentials.Password)
			if err != nil {
				writeErr(w, 400, "PASSWORD_UNREADABLE", "Gespeichertes Passwort ist nicht entschlüsselbar – bitte neu eingeben")
				return
			}
			password = pw
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	res, err := device.TestConnection(ctx, d, password)
	if err != nil {
		kind := provider.KindOf(err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "kind": kind, "message": provider.Redact(err.Error())})
		return
	}
	channels := d.ChannelCount
	if res.Info != nil && res.Info.Channels > 0 {
		channels = res.Info.Channels
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": res.Message, "info": res.Info, "liveMode": res.LiveMode, "channels": channels})
}

func portsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// handleDeleteDevice removes a device; its cameras (and their group / monitor
// assignments) are removed by the store normalisation.
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.Store.Update(func(c *config.Config) error {
		for i := range c.Devices {
			if c.Devices[i].ID == id {
				c.Devices = append(c.Devices[:i], c.Devices[i+1:]...)
				return nil
			}
		}
		return config.Invalid("Gerät nicht gefunden")
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	s.Log.Info("device deleted", "device", id, "by", principal(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- cameras & groups ------------------------------------------------------------------------

type cameraInput struct {
	ID       string                `json:"id"`
	Label    string                `json:"label"`
	Enabled  bool                  `json:"enabled"`
	Streams  config.ChannelStreams `json:"streams"`
	View     config.ChannelView    `json:"view"`
	GroupIDs *[]string             `json:"groupIds"` // nil = unchanged
}

func setMembership(c *config.Config, cameraID string, groupIDs []string) error {
	want := map[string]bool{}
	for _, g := range groupIDs {
		if _, ok := c.Group(g); !ok {
			return config.Invalid("Gruppe nicht gefunden")
		}
		want[g] = true
	}
	for gi := range c.Groups {
		g := &c.Groups[gi]
		ids := []string{}
		for _, id := range g.CameraIDs {
			if id != cameraID {
				ids = append(ids, id)
			}
		}
		if want[g.ID] {
			ids = append(ids, cameraID)
		}
		g.CameraIDs = ids
	}
	return nil
}

func (s *Server) handleSaveCamera(w http.ResponseWriter, r *http.Request) {
	var in cameraInput
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	in.Label = strings.TrimSpace(in.Label)
	if len([]rune(in.Label)) > 80 {
		writeErr(w, 400, "VALIDATION", "Name ist zu lang")
		return
	}
	_, err := s.Store.Update(func(c *config.Config) error {
		cam, ok := c.Camera(in.ID)
		if !ok {
			return config.Invalid("Kamera nicht gefunden")
		}
		cam.Label = in.Label
		cam.Enabled = in.Enabled
		cam.Streams = in.Streams
		cam.View = in.View
		if in.GroupIDs != nil {
			return setMembership(c, cam.ID, *in.GroupIDs)
		}
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type cameraBulkInput struct {
	IDs         []string `json:"ids"`
	Enabled     *bool    `json:"enabled"`
	AspectRatio string   `json:"aspectRatio"`
	ObjectFit   string   `json:"objectFit"`
	AddGroup    string   `json:"addGroup"`
	RemoveGroup string   `json:"removeGroup"`
}

func (s *Server) handleBulkCameras(w http.ResponseWriter, r *http.Request) {
	var in cameraBulkInput
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	if len(in.IDs) == 0 {
		writeErr(w, 400, "VALIDATION", "Keine Kameras ausgewählt")
		return
	}
	_, err := s.Store.Update(func(c *config.Config) error {
		sel := map[string]bool{}
		for _, id := range in.IDs {
			if _, ok := c.Camera(id); !ok {
				return config.Invalid("Kamera nicht gefunden")
			}
			sel[id] = true
		}
		for i := range c.Cameras {
			cam := &c.Cameras[i]
			if !sel[cam.ID] {
				continue
			}
			if in.Enabled != nil {
				cam.Enabled = *in.Enabled
			}
			if in.AspectRatio != "" {
				cam.View.AspectRatio = config.NormalizeAspect(in.AspectRatio)
			}
			if in.ObjectFit != "" {
				cam.View.ObjectFit = config.NormalizeFit(in.ObjectFit)
			}
		}
		for _, gid := range []string{in.AddGroup, in.RemoveGroup} {
			if gid == "" {
				continue
			}
			if _, ok := c.Group(gid); !ok {
				return config.Invalid("Gruppe nicht gefunden")
			}
		}
		for gi := range c.Groups {
			g := &c.Groups[gi]
			switch g.ID {
			case in.AddGroup:
				have := map[string]bool{}
				for _, id := range g.CameraIDs {
					have[id] = true
				}
				for _, id := range in.IDs {
					if !have[id] {
						g.CameraIDs = append(g.CameraIDs, id)
					}
				}
			case in.RemoveGroup:
				ids := []string{}
				for _, id := range g.CameraIDs {
					if !sel[id] {
						ids = append(ids, id)
					}
				}
				g.CameraIDs = ids
			}
		}
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSaveGroup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID        string    `json:"id"`
		Label     string    `json:"label"`
		CameraIDs *[]string `json:"cameraIds"`
	}
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	in.Label = strings.TrimSpace(in.Label)
	if in.Label == "" || len([]rune(in.Label)) > 64 {
		writeErr(w, 400, "VALIDATION", "Gruppenname fehlt oder ist zu lang")
		return
	}
	var savedID string
	next, err := s.Store.Update(func(c *config.Config) error {
		for _, g := range c.Groups {
			if strings.EqualFold(g.Label, in.Label) && g.ID != in.ID {
				return config.Invalid("Gruppe %q existiert bereits", in.Label)
			}
		}
		if in.ID == "" {
			g := config.Group{ID: config.NewID("g_"), Label: in.Label, CameraIDs: []string{}}
			if in.CameraIDs != nil {
				g.CameraIDs = *in.CameraIDs
			}
			c.Groups = append(c.Groups, g)
			savedID = g.ID
			return nil
		}
		g, ok := c.Group(in.ID)
		if !ok {
			return config.Invalid("Gruppe nicht gefunden")
		}
		g.Label = in.Label
		if in.CameraIDs != nil {
			g.CameraIDs = *in.CameraIDs
		}
		savedID = g.ID
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	if g, ok := next.Group(savedID); ok {
		writeJSON(w, http.StatusOK, g)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.Store.Update(func(c *config.Config) error {
		for i := range c.Groups {
			if c.Groups[i].ID == id {
				c.Groups = append(c.Groups[:i], c.Groups[i+1:]...)
				return nil
			}
		}
		return config.Invalid("Gruppe nicht gefunden")
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- monitors ------------------------------------------------------------------------------

func (s *Server) handleSaveMonitor(w http.ResponseWriter, r *http.Request) {
	var m config.Monitor
	if err := readJSON(r, &m); err != nil {
		writeAnyErr(w, err)
		return
	}
	m.Label = strings.TrimSpace(m.Label)
	if err := config.ValidateMonitorInput(&m); err != nil {
		writeAnyErr(w, err)
		return
	}
	if m.ID != "" && !config.ValidID(m.ID) {
		writeErr(w, 400, "VALIDATION", "Ungültige Monitor-ID")
		return
	}
	var savedID string
	_, err := s.Store.Update(func(c *config.Config) error {
		// a disabled camera keeps its existing assignments but must not be
		// assigned anew: per monitor its number of slots may not grow
		before := map[string]int{}
		if m.ID != "" {
			if ex, ok := c.Monitor(m.ID); ok {
				for _, row := range ex.Layout.Rows {
					for _, sl := range row.Slots {
						if sl.CameraID != "" {
							before[sl.CameraID]++
						}
					}
				}
			}
		}
		after := map[string]int{}
		for _, row := range m.Layout.Rows {
			for _, sl := range row.Slots {
				if sl.CameraID == "" {
					continue
				}
				cam, ok := c.Camera(sl.CameraID)
				if !ok {
					return config.Invalid("Eine zugewiesene Kamera existiert nicht mehr – bitte den Editor neu öffnen")
				}
				after[cam.ID]++
				if !cam.Enabled && after[cam.ID] > before[cam.ID] {
					return config.Invalid("Die Kamera „%s“ ist deaktiviert und kann nicht neu zugewiesen werden", cameraTitle(c, cam))
				}
			}
		}
		if m.ID == "" {
			m.ID = config.NewID("m_")
			c.Monitors = append(c.Monitors, m)
		} else if ex, ok := c.Monitor(m.ID); ok {
			*ex = m
		} else {
			c.Monitors = append(c.Monitors, m)
		}
		savedID = m.ID
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	if mm, ok := s.Store.Get().Monitor(savedID); ok {
		writeJSON(w, http.StatusOK, mm)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.Store.Update(func(c *config.Config) error {
		for i := range c.Monitors {
			if c.Monitors[i].ID == id {
				c.Monitors = append(c.Monitors[:i], c.Monitors[i+1:]...)
				return nil // Normalize recreates "Monitor 1" when the last one is gone
			}
		}
		return config.Invalid("Monitor nicht gefunden")
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- users ---------------------------------------------------------------------------------

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, userViews(s.Store.Get()))
}

type userInput struct {
	Username *string `json:"username"`
	Password *string `json:"password"`
	Role     *string `json:"role"`
	Enabled  *bool   `json:"enabled"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var in userInput
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	if in.Username == nil || in.Password == nil {
		writeErr(w, 400, "VALIDATION", "Benutzername und Passwort sind erforderlich")
		return
	}
	name := strings.TrimSpace(*in.Username)
	if err := auth.ValidateUsername(name); err != nil {
		writeAnyErr(w, err)
		return
	}
	if err := secret.ValidatePasswordPolicy(*in.Password); err != nil {
		writeErr(w, 400, "VALIDATION", err.Error())
		return
	}
	hash, err := secret.HashPassword(*in.Password)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	role := config.RoleViewer
	if in.Role != nil && *in.Role == config.RoleAdmin {
		role = config.RoleAdmin
	}
	enabled := in.Enabled == nil || *in.Enabled
	hb, _ := json.Marshal(hash)
	var created config.User
	_, err = s.Store.Update(func(c *config.Config) error {
		if _, exists := c.UserByName(name); exists {
			return config.Invalid("Benutzer %q existiert bereits", name)
		}
		created = config.User{ID: config.NewID("u_"), Username: name, Role: role, Enabled: enabled, PasswordHash: hb, APIKeys: []config.APIKey{}, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		c.Users = append(c.Users, created)
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	s.Log.Info("user created", "user", name, "role", role, "by", principal(r).Username)
	writeJSON(w, http.StatusOK, userViews(&config.Config{Users: []config.User{created}})[0])
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in userInput
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	var hb []byte
	if in.Password != nil && *in.Password != "" {
		if err := secret.ValidatePasswordPolicy(*in.Password); err != nil {
			writeErr(w, 400, "VALIDATION", err.Error())
			return
		}
		hash, err := secret.HashPassword(*in.Password)
		if err != nil {
			writeAnyErr(w, err)
			return
		}
		hb, _ = json.Marshal(hash)
	}
	_, err := s.Store.Update(func(c *config.Config) error {
		u, ok := c.User(id)
		if !ok {
			return config.Invalid("Benutzer nicht gefunden")
		}
		if in.Username != nil {
			name := strings.TrimSpace(*in.Username)
			if err := auth.ValidateUsername(name); err != nil {
				return err
			}
			if o, exists := c.UserByName(name); exists && o.ID != id {
				return config.Invalid("Benutzer %q existiert bereits", name)
			}
			u.Username = name
		}
		if in.Role != nil {
			if *in.Role == config.RoleAdmin {
				u.Role = config.RoleAdmin
			} else {
				u.Role = config.RoleViewer
			}
		}
		if in.Enabled != nil {
			u.Enabled = *in.Enabled
		}
		if hb != nil {
			u.PasswordHash = hb
		}
		if c.ActiveAdmins() == 0 {
			return config.Invalid("Der letzte aktive Administrator kann nicht deaktiviert oder zum Viewer gemacht werden")
		}
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	s.Log.Info("user updated", "user", id, "by", principal(r).Username)
	if u, ok := s.Store.Get().User(id); ok {
		writeJSON(w, http.StatusOK, userViews(&config.Config{Users: []config.User{*u}})[0])
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == principal(r).UserID {
		writeErr(w, 400, "VALIDATION", "Der eigene Account kann nicht gelöscht werden")
		return
	}
	_, err := s.Store.Update(func(c *config.Config) error {
		for i := range c.Users {
			if c.Users[i].ID == id {
				c.Users = append(c.Users[:i], c.Users[i+1:]...)
				if c.ActiveAdmins() == 0 {
					return config.Invalid("Der letzte aktive Administrator kann nicht gelöscht werden")
				}
				return nil
			}
		}
		return config.Invalid("Benutzer nicht gefunden")
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	s.Log.Info("user deleted", "user", id, "by", principal(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- kiosk keys -----------------------------------------------------------------------------

func kioskPath(monitorID, token string) string {
	return "/viewer?mon=" + url.QueryEscape(monitorID) + "&key=" + url.QueryEscape(token)
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("id")
	var in struct {
		Label     string `json:"label"`
		MonitorID string `json:"monitorId"`
	}
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	in.Label = strings.TrimSpace(in.Label)
	if in.Label == "" {
		in.Label = "Kiosk"
	}
	if len([]rune(in.Label)) > 64 {
		writeErr(w, 400, "VALIDATION", "Label ist zu lang")
		return
	}
	token := secret.GenerateToken()
	enc, err := s.Keys.Encrypt(token)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	encJSON, _ := json.Marshal(enc)
	var key config.APIKey
	_, err = s.Store.Update(func(c *config.Config) error {
		u, ok := c.User(uid)
		if !ok {
			return config.Invalid("Benutzer nicht gefunden")
		}
		if _, ok := c.Monitor(in.MonitorID); !ok {
			return config.Invalid("Monitor nicht gefunden")
		}
		key = config.APIKey{ID: config.NewID("k_"), Label: in.Label, MonitorID: in.MonitorID, CreatedAt: time.Now().UTC().Format(time.RFC3339), Enabled: true, TokenHash: secret.HashToken(token), TokenEnc: encJSON}
		u.APIKeys = append(u.APIKeys, key)
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	s.Log.Info("kiosk key created", "user", uid, "key", key.ID, "monitor", in.MonitorID, "by", principal(r).Username)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"key":   keyView{ID: key.ID, Label: key.Label, MonitorID: key.MonitorID, CreatedAt: key.CreatedAt, Enabled: true, Recoverable: true},
		"token": token,
		"path":  kioskPath(in.MonitorID, token),
	})
}

func findKey(c *config.Config, uid, kid string) (*config.User, *config.APIKey) {
	u, ok := c.User(uid)
	if !ok {
		return nil, nil
	}
	for i := range u.APIKeys {
		if u.APIKeys[i].ID == kid {
			return u, &u.APIKeys[i]
		}
	}
	return u, nil
}

func (s *Server) handleRevealKey(w http.ResponseWriter, r *http.Request) {
	_, k := findKey(s.Store.Get(), r.PathValue("userId"), r.PathValue("keyId"))
	if k == nil {
		writeErr(w, 404, "NOT_FOUND", "Key nicht gefunden")
		return
	}
	if !k.Recoverable() {
		writeErr(w, 409, "NOT_RECOVERABLE", "Dieser Key wurde nur als Hash gespeichert und kann nicht erneut angezeigt werden")
		return
	}
	token, err := s.Keys.Decrypt(k.TokenEnc)
	if err != nil || !secret.VerifyTokenHash(k.TokenHash, token) {
		writeErr(w, 409, "INTEGRITY", "Key kann nicht wiederhergestellt werden (Integritätsprüfung fehlgeschlagen)")
		return
	}
	s.Log.Info("kiosk key revealed", "key", k.ID, "by", principal(r).Username)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "path": kioskPath(k.MonitorID, token)})
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	uid, kid := r.PathValue("userId"), r.PathValue("keyId")
	purge := r.URL.Query().Get("purge") == "1"
	_, err := s.Store.Update(func(c *config.Config) error {
		u, k := findKey(c, uid, kid)
		if k == nil {
			return config.Invalid("Key nicht gefunden")
		}
		if purge {
			for i := range u.APIKeys {
				if u.APIKeys[i].ID == kid {
					u.APIKeys = append(u.APIKeys[:i], u.APIKeys[i+1:]...)
					break
				}
			}
			return nil
		}
		k.Enabled = false
		k.RevokedAt = time.Now().UTC().Format(time.RFC3339)
		k.TokenEnc = nil // a revoked key never needs to be revealed again
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	// Sessions of the key are invalidated by the auth service (config
	// subscription) which also closes their WebSockets, media sockets and
	// playback streams. Shared go2rtc stream definitions stay untouched.
	s.Log.Info("kiosk key revoked", "key", kid, "purge", purge, "by", principal(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// cameraTitle names a camera in messages: label, else "device · Kanal n".
func cameraTitle(c *config.Config, cam *config.Camera) string {
	if cam.Label != "" {
		return cam.Label
	}
	label := cam.DeviceID
	if d, ok := c.Device(cam.DeviceID); ok {
		label = d.Label
	}
	return fmt.Sprintf("%s · Kanal %d", label, cam.Channel+1)
}
