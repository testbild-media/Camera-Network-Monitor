package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"cnm/internal/auth"
	"cnm/internal/config"
	"cnm/internal/display"
	"cnm/internal/secret"
)

// Local displays: the server's own GPU outputs show multiviewers as kiosks.

type displayModeView struct {
	Value     string `json:"value"`
	Label     string `json:"label"`
	Preferred bool   `json:"preferred,omitempty"`
}

type displayConfigView struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Enabled   bool   `json:"enabled"`
	MonitorID string `json:"monitorId"`
	Mode      string `json:"mode"`
	Rotation  int    `json:"rotation"`
}

type displayPortView struct {
	Connector string                 `json:"connector"`
	Card      string                 `json:"card,omitempty"`
	Detected  bool                   `json:"detected"`
	Connected bool                   `json:"connected"`
	Screen    string                 `json:"screen,omitempty"` // monitor name from EDID / compositor
	Vendor    string                 `json:"vendor,omitempty"`
	Serial    string                 `json:"serial,omitempty"`
	Modes     []displayModeView      `json:"modes"`
	Preferred string                 `json:"preferred,omitempty"`
	Display   *displayConfigView     `json:"display"`
	Output    *display.OutputStatus  `json:"output,omitempty"`
	Status    *display.DisplayStatus `json:"status,omitempty"`
	Viewer    bool                   `json:"viewerConnected"`
}

func (s *Server) handleDisplays(w http.ResponseWriter, r *http.Request) {
	c := s.Store.Get()
	resp := map[string]any{
		"supported": false,
		"options":   c.System.Display,
		"agent":     map[string]any{"connected": false},
		"ports":     []displayPortView{},
	}
	mons := []map[string]any{}
	for _, m := range c.Monitors {
		mons = append(mons, map[string]any{"id": m.ID, "label": m.Label, "enabled": m.Enabled})
	}
	resp["monitors"] = mons
	if s.Displays == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	conns, err := s.Displays.Connectors()
	if err != nil {
		s.Log.Warn("display scan failed", "error", err)
	}
	st, at := s.Displays.Agent()
	agentOK := s.Displays.AgentConnected()
	agent := map[string]any{"connected": agentOK}
	if !at.IsZero() {
		agent["lastSeen"] = at
	}
	outputs := map[string]*display.OutputStatus{}
	statuses := map[string]*display.DisplayStatus{}
	if st != nil {
		agent["version"] = st.Version
		agent["compositor"] = st.Compositor
		agent["browser"] = st.Browser
		agent["installed"] = st.Installed
		agent["error"] = st.Error
		if agentOK {
			for i := range st.Outputs {
				outputs[st.Outputs[i].Name] = &st.Outputs[i]
			}
			for i := range st.Displays {
				statuses[st.Displays[i].DisplayID] = &st.Displays[i]
			}
		}
	}
	resp["agent"] = agent
	resp["supported"] = len(conns) > 0 || agentOK || len(c.Displays) > 0
	if s.Displays.GPU != "" {
		resp["gpu"] = display.NormalizePCI(s.Displays.GPU)
	}

	ports := map[string]*displayPortView{}
	port := func(name string) *displayPortView {
		p := ports[name]
		if p == nil {
			p = &displayPortView{Connector: name, Modes: []displayModeView{}}
			ports[name] = p
		}
		return p
	}
	sysModes := map[string][]display.Mode{}
	for _, cn := range conns {
		p := port(cn.Name)
		p.Card, p.Detected, p.Connected = cn.Card, true, cn.Connected
		if cn.EDID != nil {
			p.Screen, p.Vendor, p.Serial = cn.EDID.Name, cn.EDID.Vendor, cn.EDID.Serial
		}
		sysModes[cn.Name] = cn.Modes
		for _, m := range cn.Modes {
			if m.Preferred {
				p.Preferred = m.String()
			}
		}
	}
	for name, o := range outputs {
		p := port(name)
		p.Detected, p.Connected, p.Output = true, true, o
		if p.Screen == "" {
			p.Screen = strings.TrimSpace(o.Make + " " + o.Model)
		}
	}
	for i := range c.Displays {
		d := c.Displays[i]
		p := port(d.Connector)
		p.Display = &displayConfigView{ID: d.ID, Label: d.Label, Enabled: d.Enabled, MonitorID: d.MonitorID, Mode: d.Mode, Rotation: d.Rotation}
		p.Status = statuses[d.ID]
		if s.Hub != nil {
			n, _ := s.Hub.Presence(config.DisplayKeyPrefix + d.ID)
			p.Viewer = n > 0
		}
	}
	list := make([]displayPortView, 0, len(ports))
	for name, p := range ports {
		// exact compositor modes win; EDID/kernel modes otherwise
		modes := sysModes[name]
		if p.Output != nil && len(p.Output.Modes) > 0 {
			modes = p.Output.Modes
		}
		pref, _ := display.ParseMode(p.Preferred)
		for _, m := range display.SortModes(modes) {
			isPref := pref.Width > 0 && m.Width == pref.Width && m.Height == pref.Height && (pref.RefreshMHz == 0 || m.RefreshMHz == pref.RefreshMHz)
			p.Modes = append(p.Modes, displayModeView{Value: m.String(), Label: m.Label(), Preferred: isPref})
		}
		list = append(list, *p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Connected != list[j].Connected {
			return list[i].Connected
		}
		return naturalLess(list[i].Connector, list[j].Connector)
	})
	resp["ports"] = list
	writeJSON(w, http.StatusOK, resp)
}

var natRe = regexp.MustCompile(`\d+|\D+`)

func naturalLess(a, b string) bool {
	pa, pb := natRe.FindAllString(a, -1), natRe.FindAllString(b, -1)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		if len(pa[i]) > 0 && len(pb[i]) > 0 && pa[i][0] >= '0' && pa[i][0] <= '9' && pb[i][0] >= '0' && pb[i][0] <= '9' {
			if len(pa[i]) != len(pb[i]) {
				return len(pa[i]) < len(pb[i])
			}
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}

type displayInput struct {
	Connector string `json:"connector"`
	Label     string `json:"label"`
	Enabled   bool   `json:"enabled"`
	MonitorID string `json:"monitorId"`
	Mode      string `json:"mode"`
	Rotation  int    `json:"rotation"`
}

func (s *Server) handleSaveDisplay(w http.ResponseWriter, r *http.Request) {
	var in displayInput
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	in.Connector = strings.TrimSpace(in.Connector)
	in.Label = strings.TrimSpace(in.Label)
	if !display.ValidConnectorName(in.Connector) {
		writeErr(w, 400, "VALIDATION", "Ungültiger Anschluss")
		return
	}
	if len([]rune(in.Label)) > 80 {
		writeErr(w, 400, "VALIDATION", "Bezeichnung ist zu lang")
		return
	}
	mode, ok := display.ParseMode(strings.TrimSpace(in.Mode))
	if !ok {
		writeErr(w, 400, "VALIDATION", "Ungültige Auflösung")
		return
	}
	switch in.Rotation {
	case 0, 90, 180, 270:
	default:
		writeErr(w, 400, "VALIDATION", "Ungültige Drehung")
		return
	}
	if in.Enabled && in.MonitorID == "" {
		writeErr(w, 400, "VALIDATION", "Bitte einen Multiviewer auswählen")
		return
	}
	// every display signs in with its own kiosk credential
	token := secret.GenerateToken()
	enc, err := s.Keys.Encrypt(token)
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	encJSON, _ := json.Marshal(enc)
	var saved config.Display
	_, err = s.Store.Update(func(c *config.Config) error {
		if in.MonitorID != "" {
			if _, ok := c.Monitor(in.MonitorID); !ok {
				return config.Invalid("Multiviewer nicht gefunden")
			}
		}
		d, ok := c.DisplayByConnector(in.Connector)
		if !ok {
			c.Displays = append(c.Displays, config.Display{ID: config.NewUUID(), Connector: in.Connector})
			d = &c.Displays[len(c.Displays)-1]
		}
		if d.TokenHash == "" || len(d.TokenEnc) == 0 {
			d.TokenHash, d.TokenEnc = secret.HashToken(token), encJSON
		}
		d.Label, d.Enabled, d.MonitorID, d.Mode, d.Rotation = in.Label, in.Enabled, in.MonitorID, mode.String(), in.Rotation
		saved = *d
		return nil
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	s.Log.Info("display saved", "connector", saved.Connector, "enabled", saved.Enabled, "monitor", saved.MonitorID, "mode", saved.Mode)
	writeJSON(w, http.StatusOK, displayConfigView{ID: saved.ID, Label: saved.Label, Enabled: saved.Enabled, MonitorID: saved.MonitorID, Mode: saved.Mode, Rotation: saved.Rotation})
}

func (s *Server) handleDeleteDisplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.Store.Update(func(c *config.Config) error {
		for i := range c.Displays {
			if c.Displays[i].ID == id {
				c.Displays = append(c.Displays[:i], c.Displays[i+1:]...)
				return nil
			}
		}
		return config.Invalid("Display nicht gefunden")
	})
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDisplayOptions(w http.ResponseWriter, r *http.Request) {
	var in config.DisplaySettings
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	in.DailyRestart = strings.TrimSpace(in.DailyRestart)
	if in.DailyRestart != "" && !regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`).MatchString(in.DailyRestart) {
		writeErr(w, 400, "VALIDATION", "Uhrzeit im Format HH:MM angeben")
		return
	}
	switch in.Browser {
	case config.BrowserAuto, config.BrowserChromium, config.BrowserFirefox:
	default:
		writeErr(w, 400, "VALIDATION", "Unbekannter Browser")
		return
	}
	c, err := s.Store.Update(func(c *config.Config) error { c.System.Display = in; return nil })
	if err != nil {
		writeAnyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c.System.Display)
}

func (s *Server) displayByID(w http.ResponseWriter, r *http.Request) (*config.Display, bool) {
	d, ok := s.Store.Get().Display(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "Display nicht gefunden")
	}
	return d, ok
}

// handleIdentifyDisplay shows the port name on the physical screen.
func (s *Server) handleIdentifyDisplay(w http.ResponseWriter, r *http.Request) {
	d, ok := s.displayByID(w, r)
	if !ok {
		return
	}
	keyID := config.DisplayKeyPrefix + d.ID
	if n, _ := s.Hub.Presence(keyID); n == 0 {
		writeErr(w, http.StatusConflict, "NOT_CONNECTED", "Die Anzeige an "+d.Connector+" ist derzeit nicht verbunden")
		return
	}
	text := d.Connector
	if d.Label != "" {
		text = d.Label + " · " + d.Connector
	}
	s.Hub.Event(map[string]any{"type": "identify", "text": text, "ms": 10000}, func(p *auth.Principal) bool { return p.KeyID == keyID })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReloadDisplay(w http.ResponseWriter, r *http.Request) {
	d, ok := s.displayByID(w, r)
	if !ok {
		return
	}
	if s.Displays == nil || !s.Displays.AgentConnected() {
		writeErr(w, http.StatusConflict, "AGENT_OFFLINE", "Der Display-Dienst ist nicht verbunden")
		return
	}
	s.Displays.Reload(d.ID)
	s.Log.Info("display reload requested", "connector", d.Connector, "at", time.Now().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
