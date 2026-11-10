package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnm/internal/config"
	"cnm/internal/device"
	"cnm/internal/provider"
)

// Device settings: CNM reads and changes the configuration of a connected
// device (encoding, names, time, recording, motion detection) with the
// stored credentials. Admins only; one operation per device at a time.

var devConfigLocks sync.Map // device id → *sync.Mutex

func devConfigLock(id string) *sync.Mutex {
	m, _ := devConfigLocks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// configurator returns the device's driver as Configurator or writes the
// reason why there is none.
func (s *Server) configurator(w http.ResponseWriter, id string) (provider.Configurator, bool) {
	drv, _, err := s.Devices.Driver(id)
	switch {
	case errors.Is(err, device.ErrNotConnected):
		writeErr(w, http.StatusConflict, "NOT_CONNECTED", "Das Gerät ist nicht verbunden. Geräteeinstellungen gibt es nur bei verbundenen Geräten.")
		return nil, false
	case err != nil:
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "Gerät nicht gefunden")
		return nil, false
	}
	c, ok := drv.(provider.Configurator)
	if !ok {
		writeErr(w, http.StatusConflict, "UNSUPPORTED", "Für dieses Protokoll kann CNM keine Geräteeinstellungen ändern.")
		return nil, false
	}
	return c, true
}

// writeDeviceErr maps driver errors onto responses.
func writeDeviceErr(w http.ResponseWriter, err error) {
	msg := provider.Redact(err.Error())
	switch provider.KindOf(err) {
	case provider.KindConfig:
		writeErr(w, http.StatusBadRequest, "VALIDATION", msg)
	case provider.KindUnsupported:
		writeErr(w, http.StatusConflict, "UNSUPPORTED", msg)
	case provider.KindAuth:
		writeErr(w, http.StatusBadGateway, "DEVICE_AUTH", "Das Gerät lehnt die Anmeldung ab.")
	default:
		if errors.Is(err, context.DeadlineExceeded) {
			msg = "Das Gerät antwortet nicht (Zeitüberschreitung)."
		}
		writeErr(w, http.StatusBadGateway, "DEVICE_ERROR", msg)
	}
}

func (s *Server) handleDeviceConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, ok := s.configurator(w, id)
	if !ok {
		return
	}
	mu := devConfigLock(id)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	settings, err := c.ReadSettings(ctx)
	if err != nil {
		s.Log.Info("device settings read failed", "device", id, "error", provider.Redact(err.Error()))
		writeDeviceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

func (s *Server) handleSaveDeviceConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var change provider.SettingsChange
	if err := readJSON(r, &change); err != nil {
		writeAnyErr(w, err)
		return
	}
	if err := change.Validate(); err != nil {
		writeDeviceErr(w, err)
		return
	}
	if len(change.Channels) == 0 && change.NTP == nil && change.Overwrite == nil && change.DiskAlarms == nil && change.System == nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "Keine Änderung angegeben")
		return
	}
	c, ok := s.configurator(w, id)
	if !ok {
		return
	}
	mu := devConfigLock(id)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	user := principal(r).Username
	// Channels are saved one after another: many recorders refuse a whole
	// block when one channel in it has a value they do not accept, and some
	// only apply one channel at a time reliably.
	steps := splitChange(change)
	res := &provider.SettingsResult{}
	for _, step := range steps {
		r1, err := c.ApplySettings(ctx, step)
		if err != nil {
			s.Log.Info("device settings change failed", "device", id, "by", user, "error", provider.Redact(err.Error()), "values", provider.ChangeDetails(step))
			if len(steps) == 1 || provider.KindOf(err) == provider.KindNetwork || provider.KindOf(err) == provider.KindAuth || ctx.Err() != nil {
				if res.Settings == nil {
					writeDeviceErr(w, err)
					return
				}
				res.Failures = append(res.Failures, stepLabel(step)+": "+provider.Redact(err.Error()))
				break
			}
			res.Failures = append(res.Failures, stepLabel(step)+": "+provider.Redact(err.Error()))
			continue
		}
		s.Log.Info("device settings changed", "device", id, "by", user, "sections", changeSummary(step), "warnings", strings.Join(r1.Warnings, " | "))
		res.Settings = r1.Settings
		res.Warnings = append(res.Warnings, r1.Warnings...)
		res.Restart = res.Restart || r1.Restart
	}
	if res.Settings == nil {
		settings, err := c.ReadSettings(ctx)
		if err != nil {
			writeDeviceErr(w, err)
			return
		}
		res.Settings = settings
	}
	writeJSON(w, http.StatusOK, res)
}

// splitChange turns a change into one step per channel plus one for the
// device wide settings.
func splitChange(c provider.SettingsChange) []provider.SettingsChange {
	var out []provider.SettingsChange
	for _, ch := range c.Channels {
		out = append(out, provider.SettingsChange{Channels: []provider.ChannelSettings{ch}})
	}
	if c.NTP != nil || c.Overwrite != nil || c.DiskAlarms != nil || c.System != nil {
		out = append(out, provider.SettingsChange{NTP: c.NTP, Overwrite: c.Overwrite, DiskAlarms: c.DiskAlarms, System: c.System})
	}
	return out
}

func stepLabel(c provider.SettingsChange) string {
	if len(c.Channels) == 1 {
		return fmt.Sprintf("Kanal %d", c.Channels[0].Channel+1)
	}
	return "Gerät"
}

// changeSummary names the changed sections for the log (no values).
func changeSummary(c provider.SettingsChange) string {
	var parts []string
	for _, ch := range c.Channels {
		var what []string
		if ch.Name != nil {
			what = append(what, "Name")
		}
		if ch.Main != nil {
			what = append(what, "Hauptstream")
		}
		if ch.Sub != nil {
			what = append(what, "Substream")
		}
		if ch.Record != nil {
			what = append(what, "Aufnahme")
		}
		if ch.Motion != nil {
			what = append(what, "Bewegung")
		}
		if ch.VideoLoss != nil {
			what = append(what, "Videoverlust")
		}
		if ch.Tamper != nil {
			what = append(what, "Sabotage")
		}
		parts = append(parts, fmt.Sprintf("K%d:%s", ch.Channel+1, strings.Join(what, "+")))
	}
	if c.NTP != nil {
		parts = append(parts, "NTP")
	}
	if c.DiskAlarms != nil {
		parts = append(parts, "Festplatten-Alarme")
	}
	if c.System != nil {
		parts = append(parts, "System")
	}
	if c.Overwrite != nil {
		parts = append(parts, "Überschreiben")
	}
	return strings.Join(parts, " ")
}

// handleDeviceTime sets the device clock to the server's wall clock.
func (s *Server) handleDeviceTime(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, ok := s.configurator(w, id)
	if !ok {
		return
	}
	mu := devConfigLock(id)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := c.SetDeviceTime(ctx, provider.WallNow()); err != nil {
		writeDeviceErr(w, err)
		return
	}
	s.Log.Info("device clock set", "device", id, "by", principal(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeviceFormat formats a disk partition of the device. The caller
// confirms with the device's name; all recordings on it are deleted.
func (s *Server) handleDeviceFormat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, ok := s.configurator(w, id)
	if !ok {
		return
	}
	f, ok := c.(provider.DiskFormatter)
	if !ok {
		writeErr(w, http.StatusConflict, "UNSUPPORTED", "Die Festplatte dieses Geräts kann CNM nicht formatieren.")
		return
	}
	var in struct {
		Disk    int    `json:"disk"`
		Part    int    `json:"part"`
		Confirm string `json:"confirm"`
	}
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	label := ""
	for _, d := range s.Store.Get().Devices {
		if d.ID == id {
			label = d.Label
		}
	}
	if in.Disk < 0 || in.Part < 0 || label == "" || strings.TrimSpace(in.Confirm) != strings.TrimSpace(label) {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "Zum Bestätigen den Namen des Geräts genau eintippen.")
		return
	}
	mu := devConfigLock(id)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()
	user := principal(r).Username
	s.Log.Info("device disk format requested", "device", id, "by", user, "disk", in.Disk, "part", in.Part)
	if err := f.FormatDisk(ctx, in.Disk, in.Part); err != nil {
		s.Log.Info("device disk format failed", "device", id, "by", user, "error", provider.Redact(err.Error()))
		writeDeviceErr(w, err)
		return
	}
	s.Log.Info("device disk formatted", "device", id, "by", user, "disk", in.Disk, "part", in.Part)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeviceStatuses returns the runtime status of all devices, including
// the last recorder check (the top bar shows recorders with problems).
func (s *Server) handleDeviceStatuses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"devices": s.Devices.Statuses()})
}

// handleDeviceCheck checks a recorder now (disk, recording, clock).
func (s *Server) handleDeviceCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := s.Devices.Monitor(ctx, id)
	switch {
	case errors.Is(err, device.ErrNotConnected):
		writeErr(w, http.StatusConflict, "NOT_CONNECTED", "Das Gerät ist nicht verbunden.")
	case errors.Is(err, device.ErrNoMonitor):
		writeErr(w, http.StatusConflict, "UNSUPPORTED", err.Error())
	case err != nil && err.Error() == "Gerät unbekannt":
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "Gerät nicht gefunden")
	case err != nil:
		writeDeviceErr(w, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// handleDeviceUsers lists the recorder's user accounts (no passwords).
func (s *Server) handleDeviceUsers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, ok := s.configurator(w, id)
	if !ok {
		return
	}
	um, ok := c.(provider.UserManager)
	if !ok {
		writeErr(w, http.StatusConflict, "UNSUPPORTED", "Die Benutzer dieses Geräts kann CNM nicht verwalten.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	users, err := um.Users(ctx)
	if err != nil {
		writeDeviceErr(w, err)
		return
	}
	if users == nil {
		users = []provider.DeviceUser{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// passwordChange changes a password on one device. For the login CNM
// uses, the new password is stored in CNM afterwards (the device connects
// again with it).
func (s *Server) passwordChange(ctx context.Context, id, user, oldPass, newPass, by string) error {
	drv, _, err := s.Devices.Driver(id)
	if err != nil {
		return err
	}
	um, ok := drv.(provider.UserManager)
	if !ok {
		return provider.NewError(provider.KindUnsupported, nil, "Die Benutzer dieses Geräts kann CNM nicht verwalten.")
	}
	self := false
	for _, d := range s.Store.Get().Devices {
		if d.ID == id {
			self = d.Credentials.Username == user
		}
	}
	if self {
		oldPass = "" // the driver knows it
	}
	mu := devConfigLock(id)
	mu.Lock()
	defer mu.Unlock()
	if err := um.ChangePassword(ctx, user, oldPass, newPass); err != nil {
		s.Log.Info("device password change failed", "device", id, "by", by, "user", user, "error", provider.Redact(err.Error()))
		return err
	}
	s.Log.Info("device password changed", "device", id, "by", by, "user", user, "cnm_login", self)
	if !self {
		return nil
	}
	enc, err := s.Keys.Encrypt(newPass)
	if err != nil {
		return fmt.Errorf("Das Passwort wurde im Rekorder geändert, CNM konnte es aber nicht speichern (%v) – bitte im Gerätedialog eintragen", err)
	}
	raw, _ := json.Marshal(enc)
	if _, err := s.Store.Update(func(c *config.Config) error {
		for i := range c.Devices {
			if c.Devices[i].ID == id {
				c.Devices[i].Credentials.Password = raw
				return nil
			}
		}
		return config.Invalid("Gerät existiert nicht mehr")
	}); err != nil {
		return fmt.Errorf("Das Passwort wurde im Rekorder geändert, CNM konnte es aber nicht speichern (%v) – bitte im Gerätedialog eintragen", err)
	}
	return nil
}

func validDevicePassword(p string) error {
	if p == "" || len(p) > 32 || strings.TrimSpace(p) != p {
		return config.Invalid("Das neue Passwort muss 1–32 Zeichen lang sein (ohne Leerzeichen am Anfang oder Ende).")
	}
	return nil
}

// handleDevicePassword changes one user's password on a recorder.
func (s *Server) handleDevicePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		User        string `json:"user"`
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	if err := validDevicePassword(in.NewPassword); err != nil || in.User == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "Benutzer und neues Passwort (1–32 Zeichen) angeben.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := s.passwordChange(ctx, r.PathValue("id"), in.User, in.OldPassword, in.NewPassword, principal(r).Username); err != nil {
		if errors.Is(err, device.ErrNotConnected) {
			writeErr(w, http.StatusConflict, "NOT_CONNECTED", "Das Gerät ist nicht verbunden.")
			return
		}
		writeDeviceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDevicesPassword sets one new password for CNM's login on several
// recorders, one after another.
func (s *Server) handleDevicesPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DeviceIDs   []string `json:"deviceIds"`
		NewPassword string   `json:"newPassword"`
	}
	if err := readJSON(r, &in); err != nil {
		writeAnyErr(w, err)
		return
	}
	if err := validDevicePassword(in.NewPassword); err != nil || len(in.DeviceIDs) == 0 || len(in.DeviceIDs) > 64 {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "Geräte und neues Passwort (1–32 Zeichen) angeben.")
		return
	}
	type result struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	var out []result
	for _, id := range in.DeviceIDs {
		res := result{ID: id}
		user := ""
		for _, d := range s.Store.Get().Devices {
			if d.ID == id {
				res.Label, user = d.Label, d.Credentials.Username
			}
		}
		var err error
		if user == "" {
			err = errors.New("Gerät unbekannt oder ohne Benutzername")
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			err = s.passwordChange(ctx, id, user, "", in.NewPassword, principal(r).Username)
			cancel()
		}
		if err != nil {
			res.Error = provider.Redact(err.Error())
			if errors.Is(err, device.ErrNotConnected) {
				res.Error = "nicht verbunden"
			}
		} else {
			res.OK = true
		}
		out = append(out, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// handleDeviceLog returns the device's own event log, newest first.
func (s *Server) handleDeviceLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, ok := s.configurator(w, id)
	if !ok {
		return
	}
	lr, ok := c.(provider.LogReader)
	if !ok {
		writeErr(w, http.StatusConflict, "UNSUPPORTED", "Das Protokoll dieses Geräts kann CNM nicht lesen.")
		return
	}
	days := 30
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v >= 1 && v <= 3650 {
		days = v
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	now := time.Now()
	entries, err := lr.ReadLog(ctx, now.AddDate(0, 0, -days), now.Add(24*time.Hour), r.URL.Query().Get("kind"), 2000)
	if err != nil {
		writeDeviceErr(w, err)
		return
	}
	if entries == nil {
		entries = []provider.LogEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleDeviceReboot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, ok := s.configurator(w, id)
	if !ok {
		return
	}
	mu := devConfigLock(id)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := c.Reboot(ctx); err != nil {
		writeDeviceErr(w, err)
		return
	}
	s.Log.Info("device reboot requested", "device", id, "by", principal(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
