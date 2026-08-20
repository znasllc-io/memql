// Addresses for the Modules surface (memql#4191). A module is keyed by
// (kind, name) -- the pair the inventory rows carry -- and both segments are
// plain slugs (kind is a closed set; names are registry identifiers with no
// colon), so unlike concept ids they need no encoding.

export const MODULES_ROOT = "/modules";
export const MODULES_ROUTE_PATTERN = "modules";

export function modulePath(kind: string, name: string): string {
  return `${MODULES_ROOT}/${encodeURIComponent(kind)}/${encodeURIComponent(name)}`;
}
