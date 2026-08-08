// Searching and grouping the concept registry.
//
// Pure functions over the Concept list the cluster publishes (ConceptsListMsg
// -> ConceptInfo). No React, no SDK calls -- so the search semantics are
// pinned by a unit test rather than by clicking around.
//
// ===========================================================================
// NO CONCEPT-SPECIFIC KNOWLEDGE LIVES HERE, and that is enforced.
// ===========================================================================
// Everything below reads FIELDS of a Concept (id / domain / entity /
// description). Nothing enumerates concept names, weights a particular
// domain, or special-cases an id. A concept declared tomorrow is searchable
// and grouped today, which is the same property view-kit enforces one level
// down for rows. portal_render_path_test.go (repo root) fails the build if a
// concept-id literal appears in this file or any other module on the browse
// path.

import type { Concept } from "@znasllc-io/memql-sdk-core/client";

export interface RegistryFilter {
  // Free text. Whitespace-separated terms, ALL of which must match somewhere
  // in the concept -- typing a domain and part of an entity name finds the
  // concept without the operator having to guess the exact spelling or the
  // order the two appear in.
  // An OR would return the whole identity domain for that same input, which
  // is not what someone typing two words means.
  query: string;
  // Exact domain, or "" for every domain. Separate from `query` because it is
  // a different gesture: one is "find me a thing", the other is "show me this
  // area", and collapsing them makes the second unreliable (a domain named
  // `data` substring-matches half the registry).
  domain: string;
}

// haystack is the searchable text of one concept: its id, the parts of that
// id, and its prose. Lower-cased once per call rather than per term.
function haystack(concept: Concept): string {
  return [concept.id, concept.domain, concept.entity, concept.description]
    .join(" ")
    .toLowerCase();
}

export function searchTerms(query: string): string[] {
  return query
    .toLowerCase()
    .split(/\s+/)
    .filter((term) => term !== "");
}

export function conceptMatches(concept: Concept, filter: RegistryFilter): boolean {
  if (filter.domain !== "" && concept.domain !== filter.domain) return false;
  const hay = haystack(concept);
  return searchTerms(filter.query).every((term) => hay.includes(term));
}

// filterConcepts preserves the caller's ordering. useConcepts already sorts by
// id, and re-sorting by a relevance score here would make the list jump around
// under the cursor as someone types -- which is worse than a stable list for a
// registry an operator is scanning rather than picking a single "best" answer
// out of.
export function filterConcepts(
  concepts: readonly Concept[],
  filter: RegistryFilter,
): Concept[] {
  return concepts.filter((concept) => conceptMatches(concept, filter));
}

export interface DomainSummary {
  domain: string;
  count: number;
}

// domainSummaries counts the concepts in each domain, sorted by domain name.
// Computed from the UNFILTERED list by every caller that renders the domain
// picker: a filter that empties a domain must not make that domain disappear
// from the control you would use to clear the filter.
export function domainSummaries(concepts: readonly Concept[]): DomainSummary[] {
  const counts = new Map<string, number>();
  for (const concept of concepts) {
    const domain = concept.domain || "(none)";
    counts.set(domain, (counts.get(domain) ?? 0) + 1);
  }
  return [...counts.entries()]
    .map(([domain, count]) => ({ domain, count }))
    .sort((a, b) => a.domain.localeCompare(b.domain));
}

export interface DomainGroup {
  domain: string;
  concepts: Concept[];
}

// groupByDomain buckets a (usually already filtered) list for rendering.
// Domains sorted by name; concepts keep their incoming order.
export function groupByDomain(concepts: readonly Concept[]): DomainGroup[] {
  const groups = new Map<string, Concept[]>();
  for (const concept of concepts) {
    const domain = concept.domain || "(none)";
    const bucket = groups.get(domain);
    if (bucket) bucket.push(concept);
    else groups.set(domain, [concept]);
  }
  return [...groups.entries()]
    .map(([domain, list]) => ({ domain, concepts: list }))
    .sort((a, b) => a.domain.localeCompare(b.domain));
}
