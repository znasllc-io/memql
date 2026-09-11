// The derived online rule, restated for the OS the way
// This file restates the engine's rule for the shell (the portal carried a
// third copy until epic memql#4984): a machine
// is online if ANY replica holds its stream, and the ROW is the only place
// that fact is written (`connectedNodeId` stamped while the stream is live,
// blanked on close; `revokedAt` empty while the registration lives). Deriving
// it from this browser's subscription would answer "connected to the replica I
// happen to be talking to", which renders half the fleet offline at random.
//
// StreamHeld is authoritative for Ask / Fleet reachability: connectedNodeId
// present and not revoked. lastSeenAt alone is deliberately NOT enough (that
// is how Setup / Ask once read ready with no holder). lastSeen gaps during
// transient DB persist failures (roll / slot storm) must NOT flap a held
// stream offline — catalog Online already ignores lastSeen for the same reason.
//
// activeCount is never consulted (idle machines correctly report 0).
//
// THE LITERAL IS PARSED. component/worker/online_client_parity_test.go
// extracts this number by regexp -- from the portal's copy AND from this
// one -- and fails the build when either disagrees with
// component/worker/online.go's OnlineWindow. The heartbeat cadence is 15s;
// the window is two of them. Kept for least-loaded / "recently heard" UIs;
// isWorkerOnline itself does not apply it.
export const ONLINE_WINDOW_SECONDS = 30;

export interface OnlineFacts {
  connectedNodeId?: string;
  lastSeenAt?: string;
  revokedAt?: string;
}

/** Ask / Fleet reachability: stream held (connectedNodeId), not heartbeat freshness. */
export function isWorkerOnline(row: OnlineFacts, _now: Date = new Date()): boolean {
  if (row.revokedAt) return false;
  if (!row.connectedNodeId || !String(row.connectedNodeId).trim()) return false;
  return true;
}
