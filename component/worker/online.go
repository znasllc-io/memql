package worker

import "time"

// OnlineWindow is how stale a registration's lastSeenAt may be before the
// machine reads as offline: two heartbeat flushes.
//
// Two rather than one because one is the boundary itself -- a flush that lands
// a few milliseconds late would flap a machine offline and back while nothing
// was wrong. Two means a worker has to miss a whole beat AND the next one
// before the Fleet page calls it gone, which is the smallest window that
// distinguishes "the write was late" from "the laptop is shut".
const OnlineWindow = 2 * HeartbeatBatchInterval

// IsOnline is THE online rule. Every surface that shows a machine as up or
// down answers it with this function or with the one implementation named
// below -- there is no third, and the DSL deliberately does not project
// `online` as a field, because a shape body is a path list and this is a
// predicate over two timestamps and a clock.
//
// A machine is online when all three hold:
//
//	revokedAt is zero      -- a revoked worker is never online, whatever its
//	                          heartbeat says. Revocation is a decision, and a
//	                          machine still beating while revoked is the case
//	                          that most needs to read as gone.
//	lastSeenAt is non-zero -- a registration that has never been heard from is
//	                          offline, not online-since-the-epoch. Without this
//	                          the zero time would sit far outside the window
//	                          and read as offline by accident rather than by
//	                          rule, which is the same answer for the wrong
//	                          reason.
//	now - lastSeenAt <= OnlineWindow
//
// A lastSeenAt in the FUTURE (clock skew between the agent replica that wrote
// it and whoever is asking) yields a negative difference, which is inside the
// window -- online. That is deliberate: a skewed clock should not make a live
// machine disappear.
//
// A SECOND IMPLEMENTATION EXISTS, in clients/portal/src/fleet/online.ts, and
// it exists because the portal decides this per row while rendering and cannot
// ask the engine per row. The two are kept in step by
// TestFleetOnlineWindowMatchesPortal (online_portal_parity_test.go), which
// reads the TypeScript and fails when its window disagrees with this one. If
// you change OnlineWindow -- or HeartbeatBatchInterval, which it is derived
// from -- that test is what will tell you the portal has not been changed too.
func IsOnline(lastSeenAt, revokedAt time.Time, now time.Time) bool {
	if !revokedAt.IsZero() {
		return false
	}
	if lastSeenAt.IsZero() {
		return false
	}
	return now.Sub(lastSeenAt) <= OnlineWindow
}

// IsOnline reports whether this registration is currently online, using the
// rule above. Convenience for callers that already hold the row.
func (r RegistrationRow) IsOnline(now time.Time) bool {
	return IsOnline(r.LastSeenAt, r.RevokedAt, now)
}
