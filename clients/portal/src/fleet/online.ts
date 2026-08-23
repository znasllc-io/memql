// The derived online rule. THE SERVER'S RULE, restated here because a shape
// body is a path list and cannot compute. Kept in step with
// component/worker/online.go by TestFleetOnlineWindowMatchesPortal, which
// parses the literal below.
//
// ===========================================================================
// WHY THIS IS DERIVED FROM THE ROW AND NOT FROM THE SUBSCRIPTION
// ===========================================================================
// The obvious implementation is to treat the live stream as the truth: a
// machine is online while its replica is talking to us. It is wrong in the
// mesh, which is the only topology that ships.
//
// The stream tells you about ONE replica -- the one holding this browser's
// connection, and the one whose in-memory registry answers "is it connected
// to ME". A machine is online if ANY replica holds its gRPC stream, and the
// row is the only place that fact is written down: `connectedNodeId` names the
// replica and `lastSeenAt` is bumped by the heartbeat wherever it lands. So a
// machine served by the other replica would render offline on half of all page
// loads, non-deterministically, with nothing wrong.
//
// Hence a pure function over two row fields plus a clock. It is testable with
// no cluster, it agrees with itself on every replica, and it is the same
// predicate the router applies server-side.

// The heartbeat cadence is 15s (docs/public/operate/workers-runbook.md), and
// the window is two of them: one missed beat is a network hiccup, two is a
// machine that has gone away. Widening it makes a dead machine look live for
// longer; narrowing it makes a live machine flicker between beats.
//
// THE LITERAL IS PARSED. TestFleetOnlineWindowMatchesPortal extracts this
// number by regexp and fails the build when it disagrees with
// component/worker/online.go. Change it in both places, in one commit, or the
// portal and the router disagree about which machines exist.
export const ONLINE_WINDOW_SECONDS = 30;

// isWorkerOnline answers the one question the machines list is about.
//
// Both timestamps arrive as they sit on the row: RFC3339 strings, empty when
// the field was never written. Empty is meaningful for each of them and means
// the opposite thing -- an empty `revokedAt` is a live registration, an empty
// `lastSeenAt` is a machine that has never checked in -- so neither is treated
// as a missing value to fall back from.
//
// An unparseable `lastSeenAt` is OFFLINE rather than online: a value we cannot
// read is not evidence of a heartbeat, and the failure that matters here is
// telling an operator a machine is reachable when it is not.
//
// A `lastSeenAt` in the FUTURE counts as online. That is clock skew between
// the cluster and the browser, not a machine from the future, and refusing it
// would make the whole list go dark on a laptop whose clock is a minute slow.
export function isWorkerOnline(lastSeenAt: string, revokedAt: string, now: Date): boolean {
  if (revokedAt.trim() !== "") return false;
  if (lastSeenAt.trim() === "") return false;

  const seen = Date.parse(lastSeenAt);
  if (Number.isNaN(seen)) return false;

  const elapsedSeconds = (now.getTime() - seen) / 1000;
  return elapsedSeconds <= ONLINE_WINDOW_SECONDS;
}
