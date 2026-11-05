package dahua

import (
	"context"
	"testing"
	"time"

	"cnm/internal/provider"
	"cnm/internal/sim"
)

func TestParseDahuaTime(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-11-04 15:00:45":  time.Date(2026, 11, 4, 15, 0, 45, 0, time.UTC),
		"2026-11- 4 15: 0:45":  time.Date(2026, 11, 4, 15, 0, 45, 0, time.UTC),
		" 2026-1- 4  9: 5: 7 ": time.Date(2026, 1, 4, 9, 5, 7, 0, time.UTC),
		"2026-11- 4 24: 0: 0":  time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC),
		"2026-11-04T15:00:45":  time.Date(2026, 11, 4, 15, 0, 45, 0, time.UTC),
	} {
		got, err := parseDahuaTime(in)
		if err != nil || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "2026-13-01 00:00:00", "2026-11-04", "04.11.2026 15:00:45", "2026-11-04 15:60:00"} {
		if _, err := parseDahuaTime(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Field case: a 4-channel recorder lists its recordings with space padded
// times and has no storage.getDeviceAllInfo. The timeline must still show
// the recordings and the availability check must find them.
func TestSpacePaddedRecordingList(t *testing.T) {
	srv := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 4, HasDisk: true, SpacePadded: true, NoStorageAPI: true, Recordings: sim.QuarterSchedule}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	d, _ := newRPC(t, srv.Addr(), "pw123456")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := d.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Storage != provider.StorageUnknown || h.ClockOffset == nil {
		t.Fatalf("health: storage %s, clock %v", h.Storage, h.ClockOffset)
	}
	to := time.Now().Truncate(time.Minute)
	from := to.Add(-time.Hour)
	recs, err := d.SearchRecordings(ctx, 0, from, to)
	if err != nil || len(recs) < 3 {
		t.Fatalf("recordings: %d %v", len(recs), err)
	}
	for _, r := range recs {
		if r.Start.Before(from) || r.End.After(to) || !r.End.After(r.Start) {
			t.Fatalf("bad range %v–%v", r.Start, r.End)
		}
	}
	has, err := d.HasRecordings(ctx, 0, from, to)
	if err != nil || !has {
		t.Fatalf("HasRecordings = %v, %v", has, err)
	}
}

// A truthy findNextFile with an empty list means "no recordings".
func TestHasRecordingsEmptyList(t *testing.T) {
	srv := &sim.DahuaServer{User: "admin", Pass: "pw123456", Channels: 4, HasDisk: true, TruthyEmpty: true,
		Recordings: func(int, time.Time, time.Time) [][2]time.Time { return nil }}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	d, _ := newRPC(t, srv.Addr(), "pw123456")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := d.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if has, err := d.HasRecordings(ctx, 0, now.Add(-time.Hour), now); err != nil || has {
		t.Fatalf("HasRecordings = %v, %v", has, err)
	}
}
