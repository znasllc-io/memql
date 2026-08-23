// Addresses inside the machines surface (memql#4363). Mirrors sites/urls.ts:
// every destination is a URL rather than component state (#3316), and paths
// are written WITHOUT the /portal prefix since the router's basename already
// carries it.

export const MACHINES_ROOT = "/machines";

export function machinesPath(): string {
  return MACHINES_ROOT;
}

// sessionPath addresses one app session's live transcript. Session ids are
// canonical (v1:worker:appSession:<shortId>) because the row is minted
// server-side and the id travels on the task, so the colons need encoding to
// survive the address bar intact.
export function sessionPath(sessionId: string): string {
  return `${MACHINES_ROOT}/sessions/${encodeURIComponent(sessionId)}`;
}

export const SESSION_ROUTE_PATTERN = "sessions/:sessionId";
