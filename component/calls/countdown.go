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

// HostState is what the countdown needs: whether the host is present, when
// they left, and the seat of another key holder, empty when there is none.
type HostState struct {
	HostPresent        bool
	HostLeftAt         time.Time
	OtherKeyHolderSeat string
}

// HostLeft decides what happens after the host leaves. Another key holder
// takes over at once, because a call with someone entitled to run it needs no
// timer. Otherwise the call survives for grace so a dropped connection can
// come back, and ends at the deadline, which is returned so every client
// shows the same countdown. The deadline itself already ends the call.
func HostLeft(s HostState, now time.Time, grace time.Duration) (HostLeftAction, time.Time) {
	if s.HostPresent {
		return HostLeftNone, time.Time{}
	}
	if s.OtherKeyHolderSeat != "" {
		return HostLeftHandOver, time.Time{}
	}
	deadline := s.HostLeftAt.Add(grace)
	if now.Before(deadline) {
		return HostLeftCountdown, deadline
	}
	return HostLeftEnd, deadline
}
