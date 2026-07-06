// Knowledge-domain id helpers.
//
// An agent's `capabilities.domains` array carries TWO kinds of ids:
//
//  1. User-picked domains -- ids the user chose in a Knowledge /
//     Training surface: "business_administration", "math_algebra",
//     "design_ux", etc. These are the ones a user expects to see.
//
//  2. Synthetic auto-attached domains -- ids the engine's knowledge
//     pipeline appends after the fact for retrieval plumbing:
//
//       - `bridge-{16hex}` -- a cross-domain bridge corpus the engine
//         mints (integrations/knowledge/seed_bridge.go) when an agent
//         has 2+ domains, hash-keyed by (roleSlug, sortedDomainIds).
//         It is attached to capabilities.domains so retrieval pulls
//         bridge chunks alongside per-domain chunks -- a retrieval
//         implementation detail, NOT something the user picked.
//
// The `bridge-` prefix is an ENGINE convention (seed_bridge.go mints
// it; integrations/agent/replier.go consumes it), so these predicates
// are client-agnostic and belong in the runtime core. Any SDK consumer
// that renders capabilities.domains needs to hide synthetic ids from
// user-facing lists while the underlying array keeps carrying them so
// retrieval still works.

/**
 * True for ids the engine's knowledge pipeline auto-attaches to
 * capabilities.domains for plumbing reasons (bridges and similar
 * retrieval-only entries). False for ids the user picked.
 *
 * Centralised so a future synthetic prefix (per-language bridges,
 * etc.) only needs this predicate updated -- call sites keep using
 * displayDomainIds(...) and pick it up automatically.
 */
export function isSyntheticDomainId(id: string): boolean {
  if (!id) return false;
  return id.startsWith("bridge-");
}

/**
 * Filter an array of domain ids to those a human should see in the
 * UI -- drops bridges and any other synthetic-prefix ids. Use at every
 * render site that shows an agent's capabilities.domains.
 */
export function displayDomainIds<T extends string>(ids: readonly T[]): T[] {
  return ids.filter((id) => !isSyntheticDomainId(id));
}
