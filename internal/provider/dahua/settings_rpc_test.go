package dahua

import (
	"context"
	"strings"
	"testing"
	"time"

	"cnm/internal/provider"
	"cnm/internal/sim"
)

func connectedRPC(t *testing.T) (*rpcDriver, *sim.DahuaServer) {
	t.Helper()
	srv := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 4, HasDisk: true, Recordings: sim.QuarterSchedule}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	d, _ := newRPC(t, srv.Addr(), "pw123456")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	return d, srv
}

func TestDahuaReadSettings(t *testing.T) {
	d, _ := connectedRPC(t)
	s, err := d.ReadSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Channels) != 4 || !s.CanReboot || s.Time == nil || s.Time.NTP == nil || s.Overwrite == nil || !*s.Overwrite {
		t.Fatalf("%+v", s)
	}
	c := s.Channels[1]
	if c.Name == nil || *c.Name != "SIM CAM 2" || c.Main == nil || c.Main.Res != "1920x1080" || c.Main.Bitrate != 4096 || c.Main.Control != "CBR" ||
		c.Main.Options == nil || c.Main.Options.GOPUnit != "frames" || c.Sub == nil || c.Sub.Res != "352x288" {
		t.Fatalf("channel %+v main %+v sub %+v", c, c.Main, c.Sub)
	}
	if c.Record == nil || c.Record.Mode != provider.RecordContinuous || c.Record.From != "" {
		t.Fatalf("record %+v", c.Record)
	}
	if c.Motion == nil || !c.Motion.Enabled || c.Motion.Sensitivity != 80 || c.Motion.SensitivityMax != 100 {
		t.Fatalf("motion %+v", c.Motion)
	}
	if len(s.Disks) != 1 || s.Disks[0].TotalBytes != 1e12 || s.Disks[0].FreeBytes != 5e11 {
		t.Fatalf("disks %+v", s.Disks)
	}
	if s.Time.OffsetSeconds < -2 || s.Time.OffsetSeconds > 2 {
		t.Fatalf("time %+v", s.Time)
	}
}

// Firmware without storage.getDeviceAllInfo: the disk is estimated from the
// recording files of all channels.
func TestDahuaDiskFromRecordings(t *testing.T) {
	now := time.Now()
	day0 := now.Add(-50 * time.Hour).Truncate(time.Hour)
	hourly := func(_ int, from, to time.Time) [][2]time.Time {
		var out [][2]time.Time
		for s := day0; s.Before(now.Truncate(time.Hour)); s = s.Add(time.Hour) {
			if e := s.Add(time.Hour); e.After(from) && s.Before(to) {
				out = append(out, [2]time.Time{s, e})
			}
		}
		return out
	}
	srv := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 4, HasDisk: true, NoStorageAPI: true, Recordings: hourly}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	d, _ := newRPC(t, srv.Addr(), "pw123456")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := d.ReadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hours := int64(now.Truncate(time.Hour).Sub(day0) / time.Hour)
	if len(s.Disks) != 1 || !s.Disks[0].Estimated || s.Disks[0].TotalBytes != 4*hours*3600*125000 ||
		s.Disks[0].Oldest != day0.Format(dahuaTime) || s.Disks[0].Newest != now.Truncate(time.Hour).Format(dahuaTime) {
		t.Fatalf("disks %+v (hours %d)", s.Disks, hours)
	}
}

func TestDahuaApplySettings(t *testing.T) {
	d, srv := connectedRPC(t)
	ctx := context.Background()
	before, err := d.ReadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	main := *before.Channels[2].Main
	main.Control, main.Bitrate, main.FPS, main.Quality, main.GOP, main.Res = "VBR", 1536, 15, 5, 30, "1280x960"
	sub := *before.Channels[2].Sub
	sub.Bitrate = 384
	name := "Hof"
	ntpOff := false
	res, err := d.ApplySettings(ctx, provider.SettingsChange{
		Channels: []provider.ChannelSettings{
			{Channel: 2, Name: &name, Main: &main, Sub: &sub,
				Record: &provider.RecordSettings{Mode: provider.RecordMotion, From: "18:00", To: "06:00"},
				Motion: &provider.MotionSettings{Enabled: true, Sensitivity: 60}},
			{Channel: 3, Record: &provider.RecordSettings{Mode: provider.RecordOff}},
		},
		NTP:       &provider.NTPSettings{Enabled: true, Server: "192.0.2.1", Port: 123, IntervalMin: 30},
		Overwrite: &ntpOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings %v", res.Warnings)
	}
	c := res.Settings.Channels[2]
	if *c.Name != "Hof" || c.Main.Control != "VBR" || c.Main.Bitrate != 1536 || c.Main.FPS != 15 || c.Main.GOP != 30 || c.Main.Res != "1280x960" || c.Sub.Bitrate != 384 {
		t.Fatalf("channel %+v main %+v sub %+v", c, c.Main, c.Sub)
	}
	if c.Record.Mode != provider.RecordMotion || c.Record.From != "18:00" || c.Record.To != "06:00" || c.Motion.Sensitivity != 60 {
		t.Fatalf("record %+v motion %+v", c.Record, c.Motion)
	}
	if res.Settings.Channels[3].Record.Mode != provider.RecordOff || res.Settings.Channels[1].Record.Mode != provider.RecordContinuous {
		t.Fatal("other channels changed")
	}
	if n := res.Settings.Time.NTP; !n.Enabled || n.Server != "192.0.2.1" || n.IntervalMin != 30 || *res.Settings.Overwrite {
		t.Fatalf("ntp %+v overwrite %v", n, *res.Settings.Overwrite)
	}
	// all three main stream profiles follow; the unchanged sub stream fields stay
	if c.Sub.FPS != before.Channels[2].Sub.FPS || c.Sub.Res != before.Channels[2].Sub.Res {
		t.Fatalf("sub changed: %+v", c.Sub)
	}
	writes := srv.ConfigWrites.Load()

	// the device clamps the bitrate: CNM reports it
	main.Bitrate = 8000
	res, err = d.ApplySettings(ctx, provider.SettingsChange{Channels: []provider.ChannelSettings{{Channel: 0, Main: &main}}})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Bitrate") || !strings.Contains(res.Warnings[0], "6144") {
		t.Fatalf("clamp: %v %v", res, err)
	}
	// the device decides: values it cannot encode are refused, nothing is stored
	w1 := srv.ConfigWrites.Load()
	main.Bitrate = 4096
	main.FPS = 30
	if _, err := d.ApplySettings(ctx, provider.SettingsChange{Channels: []provider.ChannelSettings{{Channel: 0, Main: &main}}}); err == nil || !strings.Contains(err.Error(), "Encode") || srv.ConfigWrites.Load() != w1 {
		t.Fatalf("fps: %v", err)
	}
	main.FPS, main.GOP = 15, 400 // obviously wrong values are refused before sending
	if _, err := d.ApplySettings(ctx, provider.SettingsChange{Channels: []provider.ChannelSettings{{Channel: 0, Main: &main}}}); provider.KindOf(err) != provider.KindConfig || srv.ConfigWrites.Load() != w1 {
		t.Fatalf("gop: %v", err)
	}
	main.GOP, main.Res = 30, "3840x2160"
	if _, err := d.ApplySettings(ctx, provider.SettingsChange{Channels: []provider.ChannelSettings{{Channel: 0, Main: &main}}}); err == nil || !strings.Contains(err.Error(), "Encode") {
		t.Fatalf("refused: %v", err)
	}
	// unchanged values cause no write
	after, _ := d.ReadSettings(ctx)
	w0 := srv.ConfigWrites.Load()
	if _, err := d.ApplySettings(ctx, provider.SettingsChange{Channels: []provider.ChannelSettings{{Channel: 1, Name: after.Channels[1].Name}}}); err != nil || srv.ConfigWrites.Load() != w0 {
		t.Fatalf("unchanged name written: %v", err)
	}
	if writes == 0 {
		t.Fatal("no writes")
	}
}

func TestDahuaTimeAndReboot(t *testing.T) {
	d, srv := connectedRPC(t)
	ctx := context.Background()
	target := provider.WallNow().Add(-90 * time.Minute)
	if err := d.SetDeviceTime(ctx, target); err != nil {
		t.Fatal(err)
	}
	s, _ := d.ReadSettings(ctx)
	if off := s.Time.OffsetSeconds; off > -5390 || off < -5410 {
		t.Fatalf("offset %d", off)
	}
	if err := d.Reboot(ctx); err != nil || srv.Reboots.Load() != 1 {
		t.Fatalf("reboot %v %d", err, srv.Reboots.Load())
	}
}

func TestSettingsValidation(t *testing.T) {
	bad := []provider.SettingsChange{
		{Channels: []provider.ChannelSettings{{Channel: 0, Main: &provider.StreamSettings{Codec: "MJPEG", Control: "CBR", FPS: 10, Bitrate: 512, Quality: 4, GOP: 10, Res: "704x576"}}}},
		{Channels: []provider.ChannelSettings{{Channel: 0, Main: &provider.StreamSettings{Codec: "H.264", Control: "CBR", FPS: 0, Bitrate: 512, Quality: 4, GOP: 10, Res: "704x576"}}}},
		{Channels: []provider.ChannelSettings{{Channel: 0, Record: &provider.RecordSettings{Mode: "motion", From: "18:00"}}}},
		{Channels: []provider.ChannelSettings{{Channel: 0, Record: &provider.RecordSettings{Mode: "custom"}}}},
		{Channels: []provider.ChannelSettings{{Channel: 0}, {Channel: 0}}},
		{NTP: &provider.NTPSettings{Enabled: true}},
	}
	for i, c := range bad {
		if c.Validate() == nil {
			t.Errorf("change %d accepted", i)
		}
	}
	if got := provider.DaySlots("18:00", "06:00"); strings.Join(got, ",") != "00:00:00-06:00:00,18:00:00-24:00:00" {
		t.Fatal(got)
	}
	if f, to, ok := provider.WindowFromSlots([]string{"18:00:00-24:00:00", "00:00:00-06:00:00"}); !ok || f != "18:00" || to != "06:00" {
		t.Fatal(f, to, ok)
	}
}

// Video loss, tamper, the buzzer on motion and the disk alarms.
func TestDahuaAlarms(t *testing.T) {
	d, _ := connectedRPC(t)
	ctx := context.Background()
	s, err := d.ReadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Channels[2]
	if c.VideoLoss == nil || !c.VideoLoss.Enabled || c.VideoLoss.Record == nil || *c.VideoLoss.Record || c.Tamper == nil || c.Tamper.Enabled {
		t.Fatalf("before: loss %+v tamper %+v", c.VideoLoss, c.Tamper)
	}
	a := s.DiskAlarms
	if a == nil || a.Failure == nil || !a.Failure.Enabled || a.Failure.Beep == nil || !*a.Failure.Beep || a.LowSpacePercent != 10 {
		t.Fatalf("disk alarms %+v", a)
	}
	on, off := true, false
	change := provider.SettingsChange{
		Channels: []provider.ChannelSettings{{Channel: 2,
			VideoLoss: &provider.EventSettings{Enabled: true, Beep: &on, Record: &on},
			Tamper:    &provider.EventSettings{Enabled: true, Record: &on}}},
		DiskAlarms: &provider.DiskAlarms{NoDisk: &provider.EventSettings{Enabled: true, Beep: &off},
			LowSpace: &provider.EventSettings{Enabled: true, Beep: &on}, LowSpacePercent: 5},
	}
	res, err := d.ApplySettings(ctx, change)
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("apply: %v %+v", err, res)
	}
	c = res.Settings.Channels[2]
	if !*c.VideoLoss.Beep || !*c.VideoLoss.Record || !c.Tamper.Enabled || !*c.Tamper.Record {
		t.Fatalf("after: loss %+v tamper %+v", c.VideoLoss, c.Tamper)
	}
	if other := res.Settings.Channels[1]; *other.VideoLoss.Record {
		t.Fatalf("channel 2 changed too: %+v", other.VideoLoss)
	}
	a = res.Settings.DiskAlarms
	if *a.NoDisk.Beep || !a.LowSpace.Enabled || a.LowSpacePercent != 5 || !*a.Failure.Beep {
		t.Fatalf("disk alarms after %+v %+v %+v", a.NoDisk, a.Failure, a.LowSpace)
	}
	// recording on video loss of channel 3 off again
	change = provider.SettingsChange{Channels: []provider.ChannelSettings{{Channel: 2, VideoLoss: &provider.EventSettings{Enabled: true, Record: &off}}}}
	if res, err = d.ApplySettings(ctx, change); err != nil || *res.Settings.Channels[2].VideoLoss.Record {
		t.Fatalf("record off: %v", err)
	}
}

// Device name and video standard; the network settings for display.
func TestDahuaSystem(t *testing.T) {
	d, _ := connectedRPC(t)
	ctx := context.Background()
	s, err := d.ReadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sy := s.System; sy == nil || sy.Name == nil || *sy.Name != "SIM" || sy.VideoStandard != "PAL" || sy.AutoReboot != nil {
		t.Fatalf("system %+v", sy)
	}
	n := s.Network
	if n == nil || n.IP != "192.0.2.108" || n.Gateway != "192.0.2.1" || len(n.DNS) != 2 || n.DHCP == nil || *n.DHCP ||
		n.Ports["TCP"] != 37777 || n.Ports["HTTP"] != 80 || n.Ports["RTSP"] != 554 || n.Hostname != "SIMDVR" {
		t.Fatalf("network %+v", n)
	}
	name := "Garage"
	res, err := d.ApplySettings(ctx, provider.SettingsChange{System: &provider.SystemSettings{Name: &name, VideoStandard: "NTSC"}})
	if err != nil || len(res.Warnings) != 0 || *res.Settings.System.Name != "Garage" || res.Settings.System.VideoStandard != "NTSC" {
		t.Fatalf("apply: %v %+v", err, res)
	}
}

// Users are listed; a password change is checked with a second login, and
// the old password is restored when that login fails.
func TestDahuaPassword(t *testing.T) {
	d, srv := connectedRPC(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	users, err := d.Users(ctx)
	if err != nil || len(users) != 2 || !users[0].Self || users[1].Self {
		t.Fatalf("users %v %+v", err, users)
	}
	if err := d.ChangePassword(ctx, "admin", "", "neu-Pass-1"); err != nil {
		t.Fatal(err)
	}
	test := d.rpc.clone("admin", "neu-Pass-1", quietEnv{d.env})
	if err := test.login(ctx); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	old := d.rpc.clone("admin", "pw123456", quietEnv{d.env})
	if err := old.login(ctx); provider.KindOf(err) != provider.KindAuth {
		t.Fatalf("old password still valid: %v", err)
	}

	// the check login fails: the old password is restored
	d2, srv2 := connectedRPC(t)
	srv2.RefuseNewPassword = true
	err = d2.ChangePassword(ctx, "admin", "", "anders-2")
	if err == nil || !strings.Contains(err.Error(), "alte Passwort wiederhergestellt") || srv2.PasswordChanges.Load() != 2 {
		t.Fatalf("restore: %v (changes %d)", err, srv2.PasswordChanges.Load())
	}
	back := d2.rpc.clone("admin", "pw123456", quietEnv{d2.env})
	if err := back.login(ctx); err != nil {
		t.Fatalf("old password after restore: %v", err)
	}
	_ = srv
}
