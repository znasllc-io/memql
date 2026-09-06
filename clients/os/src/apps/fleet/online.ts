// The derived online rule, restated for the OS the way
// This file restates the engine's rule for the shell (the portal carried a
// third copy until epic memql#4984): a machine
// is online if ANY replica holds its stream, and the ROW is the only place
// that fact is written (`lastSeenAt` bumped by the heartbeat wherever it
// lands, `revokedAt` empty while the registration lives). Deriving it from
// this browser's subscription would answer "connected to the replica I
// happen to be talking to", which renders half the fleet offline at random.
//
// THE LITERAL IS PARSED. component/worker/online_portal_parity_test.go
// extracts this number by regexp -- from the portal's copy AND from this
// one -- and fails the build when either disagrees with
// component/worker/online.go's OnlineWindow. The heartbeat cadence is 15s;
// the window is two of them.
export const ONLINE_WINDOW_SECONDS = 30;

export interface OnlineFacts {
  lastSeenAt?: string;
  revokedAt?: string;
}

export function isWorkerOnline(row: OnlineFacts, now: Date = new Date()): boolean {
  if (row.revokedAt) return false;
  if (!row.lastSeenAt) return false;
  const seen = Date.parse(row.lastSeenAt);
  if (Number.isNaN(seen)) return false;
  return now.getTime() - seen <= ONLINE_WINDOW_SECONDS * 1000;
}
