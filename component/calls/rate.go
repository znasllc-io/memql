package calls

import "time"

// JoinWindowLength is the window a room's join attempts are counted in. The
// policy's RoomJoinsPerMinute is per minute.
const JoinWindowLength = time.Minute

// JoinWindow is a room's count of join attempts in its current window, as the
// room row stores it. A zero StartedAt is a room that has counted nothing.
type JoinWindow struct {
	StartedAt time.Time
	Count     int
}

// CountJoinAttempt decides one join attempt against a FIXED window: a window
// opens at the first attempt after the last one ended and admits at most limit
// attempts until it ends. A fixed window rather than a sliding one because its
// whole state is two values on the room row, which is what lets every replica
// count the same attempts under the room's lock; the price is that a burst
// straddling two windows can reach twice the limit in one moving minute, which
// a guard against a leaked link can afford.
//
// An admitted attempt returns the window with this attempt counted. A refused
// one returns the window unchanged, so a caller that writes only what it is
// handed writes nothing, and the time until the window ends, which is when an
// attempt can next succeed. A clock behind the window's start (another
// replica's clock ran ahead when it opened the window) counts in that window,
// and the wait is capped at one window. A limit of zero or less admits
// nothing: a limit fails closed.
func CountJoinAttempt(w JoinWindow, now time.Time, limit int, length time.Duration) (JoinWindow, bool, time.Duration) {
	if w.StartedAt.IsZero() || !now.Before(w.StartedAt.Add(length)) {
		w = JoinWindow{StartedAt: now}
	}
	if w.Count >= limit {
		return w, false, min(w.StartedAt.Add(length).Sub(now), length)
	}
	w.Count++
	return w, true, 0
}
