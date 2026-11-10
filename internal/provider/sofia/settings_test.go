package sofia

import (
	"context"
	"strings"
	"testing"
	"time"

	"cnm/internal/provider"
	"cnm/internal/sim"
)

func connectedSofia(t *testing.T) (*driver, *sim.SofiaServer) {
	t.Helper()
	srv := startSim(t, false, 0)
	d, _ := newDriver(t, srv.Addr(), "pw123456")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d, srv
}

func TestSofiaReadSettings(t *testing.T) {
	d, _ := connectedSofia(t)
	s, err := d.ReadSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Channels) != 4 || s.Time == nil || s.Time.NTP == nil || s.Time.NTP.Server != "pool.ntp.org" || s.Overwrite == nil || !*s.Overwrite || len(s.Notes) != 0 {
		t.Fatalf("%+v", s)
	}
	c := s.Channels[0]
	if c.Name == nil || *c.Name != "XM CAM 1" || c.Main.Res != "D1" || c.Main.GOP != 2 || c.Main.Options.GOPUnit != "seconds" || !c.Main.Options.FromDevice {
		t.Fatalf("channel %+v main %+v", c, c.Main)
	}
	var res []string
	for _, r := range c.Main.Options.Resolutions {
		res = append(res, r.Value)
	}
	if strings.Join(res, ",") != "D1,HD1,CIF,QCIF" || len(c.Sub.Options.Resolutions) != 2 || strings.Join(c.Main.Options.Codecs, ",") != "H.264" {
		t.Fatalf("options %v %+v", res, c.Sub.Options)
	}
	if c.Record.Mode != provider.RecordContinuous || c.Motion == nil || c.Motion.Enabled || c.Motion.Sensitivity != 3 || c.Motion.SensitivityMax != 6 {
		t.Fatalf("record %+v motion %+v", c.Record, c.Motion)
	}
	if len(s.Disks) != 1 || s.Disks[0].TotalBytes != 0xE8E0D<<20 || s.Disks[0].Oldest != "2026-01-02 03:04:05" || s.Disks[0].Newest != "2026-01-09 10:11:12" || s.Disks[0].State != "" {
		t.Fatalf("disks %+v", s.Disks)
	}
}

// A disk with an error status is formatted; afterwards it reports none.
func TestSofiaFormatDisk(t *testing.T) {
	d, srv := connectedSofia(t)
	srv.SetDiskError(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := d.ReadSettings(ctx)
	if err != nil || !s.CanFormat || len(s.Disks) != 1 || s.Disks[0].State != "Fehlerstatus 15" || s.Disks[0].Newest != "2022-08-29 23:29:25" {
		t.Fatalf("before: %v %+v", err, s.Disks)
	}
	if err := d.FormatDisk(ctx, 1, 0); err == nil || !strings.Contains(err.Error(), "606") || srv.Formats.Load() != 0 {
		t.Fatalf("wrong disk: %v", err)
	}
	if err := d.FormatDisk(ctx, s.Disks[0].Disk, s.Disks[0].Part); err != nil || srv.Formats.Load() != 1 {
		t.Fatalf("format: %v", err)
	}
	if s, err = d.ReadSettings(ctx); err != nil || s.Disks[0].State != "" || s.Disks[0].Oldest != "" || s.Disks[0].Newest != "" {
		t.Fatalf("after: %v %+v", err, s.Disks)
	}
}

// The device's event log is read page by page, newest first.
func TestSofiaReadLog(t *testing.T) {
	var log []map[string]any
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 300; i++ {
		log = append(log, map[string]any{"Time": base.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:04:05"), "Type": "LogIn", "User": "admin"})
	}
	log[150]["Type"] = "StorageDeviceError"
	d, srv := connectedSofia(t)
	srv.SetLog(log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entries, err := d.ReadLog(ctx, base.Add(-time.Hour), base.Add(400*time.Hour), "all", 1000)
	if err != nil || len(entries) != 300 || entries[0].Time != "2026-03-13 11:00:00" || entries[299].Time != "2026-03-01 00:00:00" || entries[149].Type != "StorageDeviceError" {
		t.Fatalf("%v %d %+v", err, len(entries), entries[:min(2, len(entries))])
	}
	if entries, err = d.ReadLog(ctx, base.Add(-time.Hour), base.Add(400*time.Hour), "all", 10); err != nil || len(entries) != 10 {
		t.Fatalf("limit: %v %d", err, len(entries))
	}
	// the device filters by kind
	if entries, err = d.ReadLog(ctx, base.Add(-time.Hour), base.Add(400*time.Hour), "storage", 100); err != nil || len(entries) != 1 || entries[0].Type != "StorageDeviceError" {
		t.Fatalf("storage: %v %+v", err, entries)
	}
}

func TestSofiaApplySettings(t *testing.T) {
	d, srv := connectedSofia(t)
	ctx := context.Background()
	before, err := d.ReadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	main := *before.Channels[1].Main
	main.Res, main.FPS, main.Bitrate, main.GOP = "CIF", 12, 768, 1
	name := "Einfahrt"
	stop := false
	res, err := d.ApplySettings(ctx, provider.SettingsChange{
		Channels: []provider.ChannelSettings{
			{Channel: 1, Name: &name, Main: &main,
				Record: &provider.RecordSettings{Mode: provider.RecordMotion, From: "20:00", To: "06:30"},
				Motion: &provider.MotionSettings{Enabled: true, Sensitivity: 5}},
			{Channel: 2, Record: &provider.RecordSettings{Mode: provider.RecordContinuous, From: "08:00", To: "18:00"}},
		},
		NTP:       &provider.NTPSettings{Enabled: false, Server: "192.0.2.1", IntervalMin: 60},
		Overwrite: &stop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings %v", res.Warnings)
	}
	c := res.Settings.Channels[1]
	if *c.Name != "Einfahrt" || c.Main.Res != "CIF" || c.Main.FPS != 12 || c.Main.Bitrate != 768 || c.Main.GOP != 1 {
		t.Fatalf("main %+v", c.Main)
	}
	if c.Record.Mode != provider.RecordMotion || c.Record.From != "20:00" || c.Record.To != "06:30" || !c.Motion.Enabled || c.Motion.Sensitivity != 5 {
		t.Fatalf("record %+v motion %+v", c.Record, c.Motion)
	}
	if r := res.Settings.Channels[2].Record; r.Mode != provider.RecordContinuous || r.From != "08:00" || r.To != "18:00" {
		t.Fatalf("channel 3 record %+v", r)
	}
	if *res.Settings.Channels[0].Name != "XM CAM 1" || res.Settings.Channels[0].Record.Mode != provider.RecordContinuous {
		t.Fatal("other channel changed")
	}
	if n := res.Settings.Time.NTP; n.Enabled || n.Server != "192.0.2.1" || *res.Settings.Overwrite {
		t.Fatalf("ntp %+v", n)
	}
	// clamped by the device → warning; refused resolution → error
	main.Bitrate = 6000
	res, err = d.ApplySettings(ctx, provider.SettingsChange{Channels: []provider.ChannelSettings{{Channel: 0, Main: &main}}})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "4096") {
		t.Fatalf("clamp %v %v", res, err)
	}
	// the device decides; its refusal (Ret 606) is explained per channel from
	// its capability tables, and nothing is stored
	writes := srv.ConfigWrites.Load()
	main.Bitrate = 1024
	m := main
	m.Res = "VGA"
	_, err = d.ApplySettings(ctx, provider.SettingsChange{Channels: []provider.ChannelSettings{{Channel: 0, Main: &m}}})
	if err == nil || !strings.Contains(err.Error(), "606") || !strings.Contains(err.Error(), "Haupt VGA@12 (erlaubt: D1, HD1, CIF, QCIF)") {
		t.Fatalf("device refusal: %v", err)
	}
	// QCIF main allows only a QCIF sub stream: CNM adapts the sub stream
	// instead of letting the device refuse the whole encoding
	m = main
	m.Res = "QCIF"
	res, err = d.ApplySettings(ctx, provider.SettingsChange{Channels: []provider.ChannelSettings{{Channel: 0, Main: &m}}})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Kanal 1: Substream-Auflösung von CIF auf QCIF angepasst") {
		t.Fatalf("sub for QCIF: %v %+v", err, res)
	}
	if s := res.Settings.Channels[0]; s.Main.Res != "QCIF" || s.Sub.Res != "QCIF" {
		t.Fatalf("after QCIF: %+v %+v", s.Main, s.Sub)
	}
	if srv.ConfigWrites.Load() != writes+1 {
		t.Fatalf("writes %d→%d", writes, srv.ConfigWrites.Load())
	}
	if srv.ConfigWrites.Load() == 0 {
		t.Fatal("no writes")
	}
}

func TestSofiaTimeAndReboot(t *testing.T) {
	d, srv := connectedSofia(t)
	ctx := context.Background()
	if err := d.SetDeviceTime(ctx, provider.WallNow().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s, _ := d.ReadSettings(ctx)
	if off := s.Time.OffsetSeconds; off < 7190 || off > 7210 {
		t.Fatalf("offset %d", off)
	}
	if err := d.Reboot(ctx); err != nil || srv.Reboots.Load() != 1 {
		t.Fatalf("reboot %v %d", err, srv.Reboots.Load())
	}
}

// CNM does not judge the encoder load: the capacity old DVRs report is no
// real limit, so a load above it is sent and the device takes it.
func TestSofiaHighEncodeLoad(t *testing.T) {
	d, srv := connectedSofia(t)
	ctx := context.Background()
	s, err := d.ReadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// all four channels D1@25 with CIF@12 sub: above the reported capacity
	var change provider.SettingsChange
	for _, c := range s.Channels {
		m := *c.Main
		m.FPS = 25
		change.Channels = append(change.Channels, provider.ChannelSettings{Channel: c.Channel, Main: &m})
	}
	writes := srv.ConfigWrites.Load()
	if res, err := d.ApplySettings(ctx, change); err != nil || len(res.Warnings) != 0 || srv.ConfigWrites.Load() == writes {
		t.Fatalf("high load: %v %v, writes %d→%d", res, err, writes, srv.ConfigWrites.Load())
	}
}

// Video loss, tamper, the buzzer on motion and the disk alarms are read and
// changed; an enabled event without schedule gets one.
func TestSofiaAlarms(t *testing.T) {
	d, _ := connectedSofia(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := d.ReadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Channels[1]
	if c.VideoLoss == nil || !c.VideoLoss.Enabled || c.VideoLoss.Beep == nil || *c.VideoLoss.Beep || c.VideoLoss.Record == nil || *c.VideoLoss.Record ||
		c.Tamper == nil || c.Tamper.Enabled || c.Motion.Beep == nil || *c.Motion.Beep {
		t.Fatalf("before: loss %+v tamper %+v motion %+v", c.VideoLoss, c.Tamper, c.Motion)
	}
	a := s.DiskAlarms
	if a == nil || a.NoDisk == nil || !a.NoDisk.Enabled || a.NoDisk.Record != nil || a.LowSpace == nil || a.LowSpace.Enabled || a.LowSpacePercent != 10 {
		t.Fatalf("disk alarms %+v", a)
	}
	on := true
	m := *c.Motion
	m.Beep = &on
	change := provider.SettingsChange{
		Channels: []provider.ChannelSettings{{Channel: 1, Motion: &m,
			VideoLoss: &provider.EventSettings{Enabled: true, Beep: &on, Record: &on},
			Tamper:    &provider.EventSettings{Enabled: true, Beep: &on}}},
		DiskAlarms: &provider.DiskAlarms{Failure: &provider.EventSettings{Enabled: true, Beep: &on},
			LowSpace: &provider.EventSettings{Enabled: true, Beep: &on}, LowSpacePercent: 15},
	}
	res, err := d.ApplySettings(ctx, change)
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("apply: %v %+v", err, res)
	}
	c = res.Settings.Channels[1]
	if !*c.VideoLoss.Beep || !*c.VideoLoss.Record || !c.Tamper.Enabled || !*c.Tamper.Beep || !*c.Motion.Beep {
		t.Fatalf("after: loss %+v tamper %+v motion %+v", c.VideoLoss, c.Tamper, c.Motion)
	}
	if other := res.Settings.Channels[0]; *other.VideoLoss.Record || *other.VideoLoss.Beep {
		t.Fatalf("channel 1 changed too: %+v", other.VideoLoss)
	}
	a = res.Settings.DiskAlarms
	if !*a.Failure.Beep || !a.LowSpace.Enabled || a.LowSpacePercent != 15 || *a.NoDisk.Beep {
		t.Fatalf("disk alarms after %+v %+v %+v", a.NoDisk, a.Failure, a.LowSpace)
	}
	// the tamper schedule was empty: enabling it activated the schedule
	ctl, _ := d.control()
	v, _ := configGet(ctx, ctl, "Detect.BlindDetect.[1]")
	if !activeSections(cfgArr(cfgMap(cfgMap(v)["EventHandler"])["TimeSection"])) {
		t.Fatal("tamper schedule still inactive")
	}
}

// Device name, video standard and maintenance restart are read and changed;
// the network settings are read for display.
func TestSofiaSystem(t *testing.T) {
	d, _ := connectedSofia(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := d.ReadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sy := s.System
	if sy == nil || sy.Name == nil || *sy.Name != "LocalHost" || sy.VideoStandard != "PAL" || sy.AutoReboot == nil || *sy.AutoReboot != (provider.AutoReboot{Day: "tue", Hour: 1}) {
		t.Fatalf("system %+v", sy)
	}
	n := s.Network
	if n == nil || n.IP != "192.0.2.11" || n.Mask != "255.255.255.0" || n.Gateway != "192.0.2.1" || strings.Join(n.DNS, ",") != "192.0.2.1,198.51.100.53" ||
		n.DHCP == nil || *n.DHCP || n.Ports["TCP"] != 34567 || n.Ports["HTTP"] != 80 || n.MAC == "" {
		t.Fatalf("network %+v", n)
	}
	name := "Werkstatt"
	res, err := d.ApplySettings(ctx, provider.SettingsChange{System: &provider.SystemSettings{Name: &name, VideoStandard: "NTSC",
		AutoReboot: &provider.AutoReboot{Day: "never", Hour: 3}}})
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("apply: %v %+v", err, res)
	}
	sy = res.Settings.System
	if *sy.Name != "Werkstatt" || sy.VideoStandard != "NTSC" || *sy.AutoReboot != (provider.AutoReboot{Day: "never", Hour: 3}) {
		t.Fatalf("after %+v %+v", sy, sy.AutoReboot)
	}
	if _, err := d.ApplySettings(ctx, provider.SettingsChange{System: &provider.SystemSettings{VideoStandard: "SECAM"}}); provider.KindOf(err) != provider.KindConfig {
		t.Fatalf("invalid standard: %v", err)
	}
}

// Users are listed; a password change is checked with a second login, and
// the old password is restored when that login fails.
func TestSofiaPassword(t *testing.T) {
	d, srv := connectedSofia(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	users, err := d.Users(ctx)
	if err != nil || len(users) != 2 || users[0].Name != "admin" || !users[0].Self || users[1].Self {
		t.Fatalf("users %v %+v", err, users)
	}
	if err := d.ChangePassword(ctx, "default", "", "x"); provider.KindOf(err) != provider.KindConfig || srv.PasswordChanges.Load() != 0 {
		t.Fatalf("other user without old password: %v", err)
	}
	if err := d.ChangePassword(ctx, "admin", "", "neu-Pass-1"); err != nil {
		t.Fatal(err)
	}
	host, port := hostPort(t, srv.Addr())
	c, err := DialControl(ctx, host, port, "admin", "neu-Pass-1")
	if err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	c.Close()
	if _, err := DialControl(ctx, host, port, "admin", "pw123456"); provider.KindOf(err) != provider.KindAuth {
		t.Fatalf("old password still valid: %v", err)
	}

	// the check login fails: the old password is restored
	d2, srv2 := connectedSofia(t)
	srv2.RefuseNewPassword = true
	err = d2.ChangePassword(ctx, "admin", "", "anders-2")
	if err == nil || !strings.Contains(err.Error(), "alte Passwort wiederhergestellt") || srv2.PasswordChanges.Load() != 2 {
		t.Fatalf("restore: %v (changes %d)", err, srv2.PasswordChanges.Load())
	}
	host, port = hostPort(t, srv2.Addr())
	if c, err := DialControl(ctx, host, port, "admin", "pw123456"); err != nil {
		t.Fatalf("old password after restore: %v", err)
	} else {
		c.Close()
	}
}
