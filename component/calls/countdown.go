package calls

import "time"

// HostLeftAction is what a call should do about a host who is not there.
type HostLeftAction string

const (
	HostLeftNone      HostLeftAction = "none"
	HostLeftHandOver  HostLeftAction = "hand_over"
	HostLeftCountdown HostLeftAction = "countdown"
	HostLeftEnd       HostLeftAction = "end"
)

// HostState is what the countdown needs: whether the host is present, when the countdown
// counts from (HostLeftAt: when the host left, or a booked call's own start or end, which
// the calls service decides), and the seat of another key holder, empty when there is
// none.
type HostState struct {
	HostPresent        bool
	HostLeftAt         time.Time
	OtherKeyHolderSeat string
}

// HostLeft decides what happens after the host leaves. Another key holder takes over at
// once, because a call with someone entitled to run it needs no timer. Otherwise the call
// survives for grace so a dropped connection can come back, and ends at the deadline,
// which is returned so every client shows the same countdown. The deadline itself already
// ends the call.
//
// A COUNTDOWN THAT HAS NOT BEGUN IS NONE. HostLeftAt can be ahead of now: a booked call
// counts from its booked start at the earliest, and, when its host never came, from its
// booked end (the calls service's host.go hostLeft). Before that moment nothing counts
// down and no deadline is said, so no client shows a countdown for a call that is still
// inside the window its key holder booked; a key holder in the call takes it over all the
// same, since a hand-over needs no timer.
func HostLeft(s HostState, now time.Time, grace time.Duration) (HostLeftAction, time.Time) {
	if s.HostPresent {
		return HostLeftNone, time.Time{}
	}
	if s.OtherKeyHolderSeat != "" {
		return HostLeftHandOver, time.Time{}
	}
	if now.Before(s.HostLeftAt) {
		return HostLeftNone, time.Time{}
	}
	deadline := s.HostLeftAt.Add(grace)
	if now.Before(deadline) {
		return HostLeftCountdown, deadline
	}
	return HostLeftEnd, deadline
}
