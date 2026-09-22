package worker

import (
	"strings"
	"time"
)

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
// A SECOND IMPLEMENTATION EXISTS, in clients/os/src/apps/fleet/online.ts, and
// it exists because the shell decides this per row while rendering and cannot
// ask the engine per row. (There were THREE until epic memql#4984 retired the
// portal's copy; the count is load-bearing, because the whole point of the
// gate below is that every copy is found.) The two are kept in step by
// TestFleetOnlineWindowMatchesTheClients (online_client_parity_test.go), which
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

// StreamHeld is the stream-affinity liveness test: a replica currently holds
// this machine's WorkerService stream.
//
// ===========================================================================
// WHY THIS IS NOT IsOnline
// ===========================================================================
// IsOnline answers "we heard a heartbeat recently". That is useful for least-
// loaded rationing and for a page that wants to say a laptop was recently
// awake. It is the WRONG answer for Ask / fleet dispatch / Setup readiness:
// a call can only land on the replica named by connectedNodeId, and that
// field is blanked the moment the stream closes (ClearConnectedNode) while
// lastSeenAt deliberately is not. Treating a fresh lastSeenAt with an empty
// connectedNodeId as "ready" is how Ask hit a sibling replica with no stream
// and reported "no eligible machine" / "this replica no longer holds a
// stream" for a machine the user could see was paired.
//
// activeCount is never consulted here. Zero in-flight calls is the idle
// steady state of a healthy machine, not evidence it is offline; using it as
// a readiness stub would flash every idle fleet as not set up.
//
// Revocation still wins: a revoked registration is never held, whatever the
// node id column says.
func StreamHeld(connectedNodeId string, revokedAt time.Time) bool {
	if !revokedAt.IsZero() {
		return false
	}
	return strings.TrimSpace(connectedNodeId) != ""
}

// StreamHeld reports whether this registration currently has a holding replica.
func (r RegistrationRow) StreamHeld() bool {
	return StreamHeld(r.ConnectedNodeId, r.RevokedAt)
}

// StaleHoldWindow is how far behind `lastSeenAt` may fall before the sweep
// treats a registration's connectedNodeId as a stamp nothing is holding
// (epic memql#5327, design D7).
//
// It is the online window plus one flush interval of slack, and both terms are
// load-bearing. A heartbeat arrives THROUGH the stream on the holding pod, so a
// pod that has gone cannot be refreshing lastSeenAt -- the window is therefore
// the whole signal, and the sweep needs no second read of which nodes are
// alive. The slack is because the flush is THROTTLED: a row can legitimately
// sit one HeartbeatBatchInterval behind a perfectly healthy stream, and a sweep
// that cleared on the online window alone would race the machine's own next
// write and blank a live hold.
const StaleHoldWindow = OnlineWindow + HeartbeatBatchInterval

// HoldIsStale reports whether a registration's connectedNodeId names a replica
// that is no longer holding its stream.
//
// A registration with NO stamp is not stale -- there is nothing to clear, and
// answering true would make the sweep write to every disconnected machine in
// the cluster every two minutes. A registration that has never been heard from
// but carries a stamp IS stale: the stamp can only have come from a register
// that never reached its first heartbeat flush, which is a pod that died inside
// one interval.
func HoldIsStale(connectedNodeId string, lastSeenAt time.Time, now time.Time) bool {
	if strings.TrimSpace(connectedNodeId) == "" {
		return false
	}
	return now.Sub(lastSeenAt) > StaleHoldWindow
}
