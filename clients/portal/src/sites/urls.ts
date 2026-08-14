// Addresses inside the sites surface (memql#3717). Mirrors
// integrations/urls.ts: every destination is a URL, not component state
// (#3316), and paths are written WITHOUT the /portal prefix since the
// router's basename already carries it (see App / Vite's `base`).

export const SITES_ROOT = "/sites";

export function sitesPath(): string {
  return SITES_ROOT;
}

// sitePath addresses one site's detail + actions panel. Site ids are plain
// shortIds (newShortId(), no colon -- see docs/public/concepts/identifiers.md
// on the bare-ids client contract), so a plain encodeURIComponent is enough;
// unlike a concept row id there is no `concept:id` punctuation to preserve
// through the round trip.
export function sitePath(siteId: string): string {
  return `${SITES_ROOT}/${encodeURIComponent(siteId)}`;
}
