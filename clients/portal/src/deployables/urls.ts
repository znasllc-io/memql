// Addresses inside the deployables surface (memql#4346). Mirrors
// artifacts/urls.ts: every destination is a URL, not component state (#3316),
// and paths are written WITHOUT the /portal prefix since the router's basename
// already carries it (see App / Vite's `base`).

export const DEPLOYABLES_ROOT = "/deployables";

// The address this surface used to live at. Kept as a named constant rather
// than a literal in the route table because two places have to agree on it:
// the redirect route, and the test that proves a bookmark still lands.
export const RETIRED_SITES_ROOT = "/sites";

export function deployablesPath(): string {
  return DEPLOYABLES_ROOT;
}

// deployablePath addresses one deployable's detail + actions panel. Site ids
// are plain shortIds (newShortId(), no colon -- see
// docs/public/concepts/identifiers.md on the bare-ids client contract), so a
// plain encodeURIComponent is enough; unlike a concept row id there is no
// `concept:id` punctuation to preserve through the round trip.
export function deployablePath(siteId: string): string {
  return `${DEPLOYABLES_ROOT}/${encodeURIComponent(siteId)}`;
}

// liveUrlFor is where the thing actually IS -- the single most useful link on
// this whole surface, and the one the Sites screen never rendered.
//
// ALWAYS https, never the portal's own protocol. Locally the front door
// terminates TLS with an mkcert wildcard and in the cloud with a real
// certificate, so a hosted site is https in both; deriving the scheme from
// window.location would only ever be a way to be wrong on a dev server.
//
// Returns "" for a blank hostname so a caller renders nothing rather than a
// link to "https:///".
export function liveUrlFor(hostname: string): string {
  const host = hostname.trim();
  return host === "" ? "" : `https://${host}/`;
}
