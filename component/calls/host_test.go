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

// TestSeatGone: a seat the media server no longer shows is gone once its last sign of
// life -- the later of when it was last seen and when it was let in -- is a grace old,
// and it left at that sign. A seat let in again after leaving is judged from its new
// admission, never from the stint before.
func TestSeatGone(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	grace := 2 * time.Minute
	for _, c := range []struct {
		name                  string
		lastSeen, joined, now time.Time
		gone                  bool
		at                    time.Time
	}{
		{"seen, inside the grace", base, base.Add(-time.Hour), base.Add(grace - time.Second), false, base},
		{"seen, at the grace", base, base.Add(-time.Hour), base.Add(grace), true, base},
		{"seen long ago", base, base.Add(-time.Hour), base.Add(time.Hour), true, base},
		{"never seen, just let in", time.Time{}, base, base.Add(time.Second), false, base},
		{"never seen, its pass long spent", time.Time{}, base, base.Add(grace + time.Minute), true, base},
		{"let in again after an old stint", base.Add(-time.Hour), base, base.Add(time.Minute), false, base},
		{"let in again, never seen since", base.Add(-time.Hour), base, base.Add(grace), true, base},
		{"no sign of life at all", time.Time{}, time.Time{}, base, true, base},
	} {
		t.Run(c.name, func(t *testing.T) {
			gone, at := SeatGone(c.lastSeen, c.joined, c.now, grace)
			if gone != c.gone || !at.Equal(c.at) {
				t.Fatalf("got %v at %v, want %v at %v", gone, at, c.gone, c.at)
			}
		})
	}
}
