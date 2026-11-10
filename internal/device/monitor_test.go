package device

import (
	"strings"
	"testing"

	"cnm/internal/provider"
)

func monitorSettings(mode string, disks []provider.DiskInfo, deviceTime string, offset int) *provider.DeviceSettings {
	set := &provider.DeviceSettings{Disks: disks, Time: &provider.TimeSettings{DeviceTime: deviceTime, OffsetSeconds: offset}}
	for ch := 0; ch < 4; ch++ {
		set.Channels = append(set.Channels, provider.ChannelSettings{Channel: ch,
			Main: &provider.StreamSettings{Enabled: true}, Record: &provider.RecordSettings{Mode: mode}})
	}
	return set
}

func TestSettingsProblems(t *testing.T) {
	ok := []provider.DiskInfo{{Name: "Festplatte 1", TotalBytes: 1e12, FreeBytes: 0, Newest: "2026-11-06 10:00:00"}}
	yes := true
	for _, tc := range []struct {
		name  string
		set   *provider.DeviceSettings
		want  []string // substrings, in order
		probe int
	}{
		{"healthy", monitorSettings(provider.RecordContinuous, ok, "2026-11-06 10:30:00", 1), nil, 4},
		{"motion only: nothing to probe", monitorSettings(provider.RecordMotion, ok, "2026-11-06 10:30:00", 0), nil, 0},
		{"disk fault, nothing recorded for years", monitorSettings(provider.RecordContinuous,
			[]provider.DiskInfo{{Name: "Festplatte 1, Partition 1", TotalBytes: 1e12, Error: true, State: "Fehlerstatus 15", Newest: "2022-08-29 23:29:25"}},
			"2026-11-05 20:00:00", 6),
			[]string{"„Festplatte 1, Partition 1“ meldet einen Fehler (Fehlerstatus 15)", "Keine neuen Aufnahmen seit 1528 Tagen (neueste Aufnahme 2022-08-29 23:29:25)"}, 0},
		{"no disk", monitorSettings(provider.RecordContinuous, []provider.DiskInfo{}, "2026-11-06 10:30:00", 0),
			[]string{"Keine Festplatte erkannt"}, 0},
		{"no disk, recording off", monitorSettings(provider.RecordOff, []provider.DiskInfo{}, "2026-11-06 10:30:00", 0), nil, 0},
		{"disk not reported", monitorSettings(provider.RecordContinuous, nil, "2026-11-06 10:30:00", 0), nil, 4},
		{"clock 5 minutes ahead", monitorSettings(provider.RecordContinuous, ok, "2026-11-06 10:30:00", 300),
			[]string{"Uhr des Rekorders geht 5 Minuten vor"}, 4},
		{"clock in 1958", monitorSettings(provider.RecordContinuous, ok, "1958-10-16 11:08:43", -2147483647),
			[]string{"Uhr des Rekorders ist falsch (zeigt 1958-10-16 11:08:43)"}, 4},
	} {
		problems, probe := settingsProblems(tc.set)
		if len(problems) != len(tc.want) || len(probe) != tc.probe {
			t.Errorf("%s: problems %q probe %d", tc.name, problems, len(probe))
			continue
		}
		for i, w := range tc.want {
			if !strings.Contains(problems[i], w) {
				t.Errorf("%s: %q does not contain %q", tc.name, problems[i], w)
			}
		}
	}
	// a full disk that is not overwritten
	full := monitorSettings(provider.RecordContinuous, []provider.DiskInfo{{Name: "sda", TotalBytes: 1e12, FreeBytes: 1e9, Newest: "2026-11-06 10:00:00"}}, "2026-11-06 10:30:00", 0)
	no := false
	full.Overwrite = &no
	if problems, probe := settingsProblems(full); len(problems) != 1 || !strings.Contains(problems[0], "Überschreiben ist ausgeschaltet") || len(probe) != 0 {
		t.Errorf("full disk: %q %d", problems, len(probe))
	}
	full.Overwrite = &yes
	if problems, _ := settingsProblems(full); len(problems) != 0 {
		t.Errorf("full disk, overwritten: %q", problems)
	}
}
