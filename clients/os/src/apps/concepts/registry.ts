import type { Concept } from "@znasllc-io/memql-sdk-core/client";

// Reading the concept registry: search, grouping, and the badge a
// concept's data-origins declaration earns it.
//
// Pure functions over the descriptors the engine sends, so the whole of
// this app's list behaviour is testable without React, a connection or a
// cluster -- the split `src/system/` uses for the shell's own state
// machines.

/** One domain's concepts, in the order the list renders them. */
export interface ConceptGroup {
  domain: string;
  concepts: Concept[];
}

/**
 * Match a concept against a free-text query.
 *
 * AND of terms, each matched case-insensitively against the id, domain,
 * entity and description. AND rather than OR because the question a
 * person types here is a narrowing one -- "shopify order" means both
 * words, and OR would answer it with every concept in the shopify domain
 * plus every concept anywhere with "order" in its description.
 */
export function conceptMatches(concept: Concept, query: string): boolean {
  const terms = query.toLowerCase().split(/\s+/).filter((t) => t !== "");
  if (terms.length === 0) return true;
  const haystack = [concept.id, concept.domain, concept.entity, concept.description]
    .join(" ")
    .toLowerCase();
  return terms.every((term) => haystack.includes(term));
}

/**
 * The domains present in a registry, alphabetically, each with the
 * number of concepts it holds.
 *
 * Counted over the UNFILTERED registry deliberately: a facet that
 * renumbered itself as you typed would answer "how many are there" with
 * "how many are left", and the count is what tells somebody the domain
 * is worth opening.
 */
export function domainCounts(concepts: readonly Concept[]): { domain: string; count: number }[] {
  const counts = new Map<string, number>();
  for (const concept of concepts) {
    const domain = concept.domain || "(no domain)";
    counts.set(domain, (counts.get(domain) ?? 0) + 1);
  }
  return [...counts.entries()]
    .map(([domain, count]) => ({ domain, count }))
    .sort((a, b) => a.domain.localeCompare(b.domain));
}

/**
 * The list, filtered and grouped for rendering.
 *
 * An empty `domain` means every domain; a domain naming nothing yields no
 * groups rather than every group, which is the fail-closed reading of a
 * facet whose value went stale when the registry changed under it.
 */
export function groupConcepts(
  concepts: readonly Concept[],
  query: string,
  domain: string,
): ConceptGroup[] {
  const wanted = domain.trim();
  const matched = concepts.filter(
    (c) => (wanted === "" || (c.domain || "(no domain)") === wanted) && conceptMatches(c, query),
  );
  const byDomain = new Map<string, Concept[]>();
  for (const concept of matched) {
    const key = concept.domain || "(no domain)";
    const bucket = byDomain.get(key);
    if (bucket) bucket.push(concept);
    else byDomain.set(key, [concept]);
  }
  return [...byDomain.entries()]
    .map(([d, list]) => ({
      domain: d,
      concepts: [...list].sort((a, b) => a.entity.localeCompare(b.entity)),
    }))
    .sort((a, b) => a.domain.localeCompare(b.domain));
}

// ---- the origin badge --------------------------------------------------

/**
 * What a concept's data-origins declaration earns it in a list.
 *
 * THREE STATES, AND ONLY TWO OF THEM ARE A BADGE. `native` is the default
 * -- MemQL owns the data and nothing else has a copy -- and it is what
 * most of the registry is. Badging it would put a mark on almost every
 * row, which tells a reader scanning the list nothing and hides the two
 * marks that matter. So native earns no badge, and its absence is the
 * statement.
 *
 * `mirror` is the one that changes what a caller may DO: an external
 * system owns the data and the engine refuses every write that does not
 * come from that system's connector. `origin` means MemQL owns it and
 * pushes copies out.
 */
export type OriginBadge =
  | { kind: "none" }
  | { kind: "mirror"; origin: string }
  | { kind: "origin"; targets: string[] };

export function originBadgeFor(concept: Concept): OriginBadge {
  const state = concept.dataState ?? "";
  if (state === "mirror") {
    // The origin is never "" on a server that carries the field, but a
    // descriptor from one that predates it would be -- and "Mirror of"
    // with nothing after it is worse than no badge.
    const origin = (concept.dataOrigin ?? "").trim();
    if (origin === "") return { kind: "none" };
    return { kind: "mirror", origin };
  }
  if (state === "origin") {
    const targets = (concept.dataMirroredTo ?? []).filter((t) => t.trim() !== "");
    if (targets.length === 0) return { kind: "none" };
    return { kind: "origin", targets };
  }
  return { kind: "none" };
}

/** The badge's words. One spelling, so the list and the detail agree. */
export function originBadgeLabel(badge: OriginBadge): string {
  if (badge.kind === "mirror") return `Mirror of ${badge.origin}`;
  if (badge.kind === "origin") return `Origin, mirrored to ${badge.targets.join(", ")}`;
  return "";
}
