package calls

import (
	"testing"
	"time"
)

func TestHostLeftCountdown(t *testing.T) {
	left := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	grace := 5 * time.Minute
	cases := []struct {
		name string
		s    HostState
		now  time.Time
		want HostLeftAction
		dl   time.Time
	}{
		{"present", HostState{HostPresent: true}, left.Add(time.Hour), HostLeftNone, time.Time{}},
		{"other key holder", HostState{HostLeftAt: left, OtherKeyHolderSeat: "seat1"}, left, HostLeftHandOver, time.Time{}},
		{"within grace", HostState{HostLeftAt: left}, left.Add(4 * time.Minute), HostLeftCountdown, left.Add(grace)},
		{"at deadline ends", HostState{HostLeftAt: left}, left.Add(grace), HostLeftEnd, left.Add(grace)},
		{"after grace", HostState{HostLeftAt: left}, left.Add(6 * time.Minute), HostLeftEnd, left.Add(grace)},
		// A countdown that has not begun -- a booked call's runs from its booked start, or
		// from its booked end for a host who never came -- is none yet, and says no deadline.
		{"not begun", HostState{HostLeftAt: left}, left.Add(-time.Second), HostLeftNone, time.Time{}},
		{"not begun, a key holder in the call", HostState{HostLeftAt: left, OtherKeyHolderSeat: "seat1"}, left.Add(-time.Hour), HostLeftHandOver, time.Time{}},
		{"just begun", HostState{HostLeftAt: left}, left, HostLeftCountdown, left.Add(grace)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, dl := HostLeft(c.s, c.now, grace)
			if got != c.want || !dl.Equal(c.dl) {
				t.Fatalf("got %s %v, want %s %v", got, dl, c.want, c.dl)
			}
		})
	}
}
