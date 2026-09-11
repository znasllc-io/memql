// The derived online rule, restated for the OS the way
// This file restates the engine's rule for the shell (the portal carried a
// third copy until epic memql#4984): a machine
// is online if ANY replica holds its stream, and the ROW is the only place
// that fact is written (`connectedNodeId` stamped while the stream is live,
// blanked on close; `revokedAt` empty while the registration lives). Deriving
// it from this browser's subscription would answer "connected to the replica I
// happen to be talking to", which renders half the fleet offline at random.
//
// lastSeenAt alone is deliberately NOT enough: that is "saw a heartbeat
// somewhere" and is how Setup / Ask read ready while no replica held a stream.
// activeCount is never consulted (idle machines correctly report 0).
//
// THE LITERAL IS PARSED. component/worker/online_client_parity_test.go
// extracts this number by regexp -- from the portal's copy AND from this
// one -- and fails the build when either disagrees with
// component/worker/online.go's OnlineWindow. The heartbeat cadence is 15s;
// the window is two of them.
export const ONLINE_WINDOW_SECONDS = 30;

export interface OnlineFacts {
  connectedNodeId?: string;
  lastSeenAt?: string;
  revokedAt?: string;
}

export function isWorkerOnline(row: OnlineFacts, now: Date = new Date()): boolean {
  if (row.revokedAt) return false;
  if (!row.connectedNodeId || !String(row.connectedNodeId).trim()) return false;
  if (!row.lastSeenAt) {
    // Stream held, beat not yet flushed -- still online for Ask / Setup.
    return true;
  }
  const seen = Date.parse(row.lastSeenAt);
  if (Number.isNaN(seen)) return false;
  return now.getTime() - seen <= ONLINE_WINDOW_SECONDS * 1000;
}
