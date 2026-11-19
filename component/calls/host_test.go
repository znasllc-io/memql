package calls

import (
	"slices"
	"testing"
	"time"
)

// TestHostActionsNameTheirSeatAndTheirSources: every host control is recognised and
// nothing else is; the four done to a seat say so, and the three track controls act on
// exactly the sources a person's pass may publish them on -- mute on the microphone,
// camera off on the camera, stop share on the screen share and its audio.
func TestHostActionsNameTheirSeatAndTheirSources(t *testing.T) {
	for _, c := range []struct {
		action   string
		seat     bool
		sources  []string
		isTracks bool
	}{
		{HostMute, true, []string{"microphone"}, true},
		{HostCameraOff, true, []string{"camera"}, true},
		{HostStopShare, true, []string{"screen_share", "screen_share_audio"}, true},
		{HostRemove, true, nil, false},
		{HostAdmit, true, nil, false},
		{HostLock, false, nil, false},
		{HostUnlock, false, nil, false},
		{HostEnd, false, nil, false},
	} {
		if !IsHostAction(c.action) {
			t.Errorf("%q is not a host action", c.action)
		}
		if got := HostActionNamesSeat(c.action); got != c.seat {
			t.Errorf("%q names a seat: %v, want %v", c.action, got, c.seat)
		}
		sources, isTracks := HostActionSources(c.action)
		if isTracks != c.isTracks || !slices.Equal(sources, c.sources) {
			t.Errorf("%q acts on %v (%v), want %v (%v)", c.action, sources, isTracks, c.sources, c.isTracks)
		}
	}
	for _, unknown := range []string{"", "MUTE", "kick", "ban", "unmute"} {
		if IsHostAction(unknown) || HostActionNamesSeat(unknown) {
			t.Errorf("%q was taken for a host action", unknown)
		}
		if _, isTracks := HostActionSources(unknown); isTracks {
			t.Errorf("%q was taken for a track control", unknown)
		}
	}
	// Every source a track control names is one a person's pass may publish.
	person, err := GrantFor(KindPerson)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{HostMute, HostCameraOff, HostStopShare} {
		sources, _ := HostActionSources(action)
		for _, source := range sources {
			if !slices.Contains(person.Sources, source) {
				t.Errorf("%q acts on %q, which no pass publishes", action, source)
			}
		}
	}
	// The table is not shared: a caller that changes its answer changes nobody else's.
	sources, _ := HostActionSources(HostStopShare)
	if len(sources) == 0 {
		t.Fatal("stop_share acts on no source")
	}
	sources[0] = "changed"
	if again, _ := HostActionSources(HostStopShare); len(again) == 0 || again[0] != "screen_share" {
		t.Fatal("HostActionSources hands out a shared slice")
	}
}

// TestSeatAbsent: a seat the media server does not show on a pass is closed only once it has
// been MISSING for the grace, counted from the first pass that found it missing (or from a
// later sign of life), never from the age of its last stamp: a seat seen two minutes ago
// and missed now has only just gone missing, so the first pass that misses it records that
// and closes nothing. It closes at its last sign of life -- the later of when it was last
// seen and when it was let in -- and a mark from before its latest admission is the stint
// before's, so a seat let in again is judged from its new stint.
func TestSeatAbsent(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	grace := 2 * time.Minute
	for _, c := range []struct {
		name                           string
		lastSeen, joined, missing, now time.Time
		want                           SeatAbsence
	}{
		// The stamp is as old as the grace, which is what a present seat's routinely is: the
		// first pass to miss it marks it, and closes nothing.
		{"first missed, its stamp a grace old", base, base.Add(-time.Hour), time.Time{}, base.Add(grace),
			SeatAbsence{Mark: true, At: base}},
		{"first missed, its stamp long old", base, base.Add(-time.Hour), time.Time{}, base.Add(time.Hour),
			SeatAbsence{Mark: true, At: base}},
		{"missing, inside the grace", base, base.Add(-time.Hour), base.Add(time.Hour), base.Add(time.Hour + grace - time.Second),
			SeatAbsence{At: base}},
		{"missing for the grace", base, base.Add(-time.Hour), base.Add(time.Hour), base.Add(time.Hour + grace),
			SeatAbsence{Gone: true, At: base}},
		// A sign of life after the mark -- a media server's event about the seat -- restarts
		// the grace from it.
		{"seen again after the mark", base.Add(time.Hour + time.Minute), base.Add(-time.Hour), base.Add(time.Hour), base.Add(time.Hour + grace),
			SeatAbsence{At: base.Add(time.Hour + time.Minute)}},
		{"never seen, just let in", time.Time{}, base, time.Time{}, base.Add(time.Second),
			SeatAbsence{Mark: true, At: base}},
		{"never seen, missing for the grace", time.Time{}, base, base.Add(time.Second), base.Add(time.Second + grace),
			SeatAbsence{Gone: true, At: base}},
		{"a mark from the stint before", base.Add(-time.Hour), base, base.Add(-30 * time.Minute), base.Add(time.Minute),
			SeatAbsence{Mark: true, At: base}},
		{"let in again, missing for the grace since", base.Add(-time.Hour), base, base.Add(time.Second), base.Add(time.Second + grace),
			SeatAbsence{Gone: true, At: base}},
		{"no sign of life at all, missing for the grace", time.Time{}, time.Time{}, base, base.Add(grace),
			SeatAbsence{Gone: true, At: base}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := SeatAbsent(c.lastSeen, c.joined, c.missing, c.now, grace)
			if got.Mark != c.want.Mark || got.Gone != c.want.Gone || !got.At.Equal(c.want.At) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
