// Generic concept browsing -- the TypeScript mirror of sdk/go's
// concept_browser.go.
//
// This bypasses the named-primitive surface deliberately, and it is the ONLY
// sanctioned reason to do so. A concept browser iterates the registry from
// listConcepts and lets an operator click into any concept's rows without
// knowing its name at compile time; that use case is concept-name-agnostic by
// definition, so no equivalent named primitive can exist. Every other caller
// uses a generated typed method.

import type { QueryClient, QueryCallOptions } from "./query.js";
import type { Row } from "./types.js";

// A concept registry can hold far more than the engine's implicit
// unmarked-list backstop (MEMQL_MEMORY_ENGINE_DEFAULT_LIST_CAP, 50 rows), so the
// browse query MUST declare sort + paginate to opt into a keyset window and a
// continuation cursor -- otherwise it silently truncates at the backstop with
// no way to page past it (memql#2008).
export const DEFAULT_CONCEPT_BROWSE_PAGE_SIZE = 200;

export interface ConceptPage {
  // rows preserve the full nested wire shape (payload / metadata / provenance
  // / intrinsics), exactly like Result.rawNodes(). A generic inspector needs
  // the intrinsics that flattening would drop.
  rows: Row[];
  // nextCursor is the opaque continuation token for the following page, or ""
  // when the set is exhausted. It is bound to this query's sort ordering and
  // carries no server session state, so it resolves on any replica.
  nextCursor: string;
}

export interface ConceptBrowseOptions extends QueryCallOptions {
  /**
   * Walk direction. The default "asc" is the canonical browse ordering the
   * concept workspace depends on (oldest first, live arrivals banded apart).
   * "desc" exists for surfaces whose question is "what happened RECENTLY" --
   * the observability drill-in reads codeMetric windows newest-first
   * (memql#4192) -- and mints the same keyset cursor bound to its own
   * ordering. Additive: nothing changes for callers that do not pass it.
   */
  order?: "asc" | "desc";
  pageSize?: number;
}

export async function browseConceptPage(
  query: QueryClient,
  conceptId: string,
  opts: ConceptBrowseOptions = {},
): Promise<ConceptPage> {
  if (conceptId === "") {
    throw new Error("browseConceptPage: conceptId is required");
  }
  // Split off the browse-only knob and forward EVERYTHING else verbatim.
  // Hand-copying `cursor` + `signal` was correct for today's
  // QueryCallOptions and silently wrong the moment a field is added to it --
  // the new option would compile fine here and never reach the query.
  const { pageSize: requestedPageSize, order, ...callOpts } = opts;
  const pageSize =
    requestedPageSize !== undefined && requestedPageSize > 0
      ? requestedPageSize
      : DEFAULT_CONCEPT_BROWSE_PAGE_SIZE;

  // sort(paginate(concept==X, N), "createdAt", "asc") -- the canonical
  // keyset-eligible directive chain (leading createdAt sort plus a paginate
  // window). The engine appends `id ASC` as the tie-breaker under equal
  // createdAt.
  const direction = order === "desc" ? "desc" : "asc";
  const call = `sort(paginate(concept==${conceptId}, ${pageSize}), "createdAt", "${direction}")`;

  const result = await query.executeNamed("conceptBrowse", call, callOpts);

  return {
    rows: result.rawNodes(),
    nextCursor: result.meta()?.cursor ?? "",
  };
}

export async function getRowByConceptAndId(
  query: QueryClient,
  conceptId: string,
  rowId: string,
  opts: QueryCallOptions = {},
): Promise<Row | null> {
  if (conceptId === "") {
    throw new Error("getRowByConceptAndId: conceptId is required");
  }
  if (rowId === "") {
    throw new Error("getRowByConceptAndId: rowId is required");
  }
  const result = await query.executeNamed(
    "conceptRow",
    `concept==${conceptId} && id==${rowId}`,
    opts,
  );
  const nodes = result.rawNodes();
  return nodes.length > 0 ? (nodes[0] ?? null) : null;
}

/**
 * Count the rows of a concept the CALLER may see.
 *
 * The engine's `count` directive (memql#1730) computes this server-side through
 * the same pipeline a normal query uses -- deduped, latest-version,
 * post-filtered, and under the caller's own per-row authz -- and returns a
 * `{count: N}` envelope instead of materializing rows. A raw SQL COUNT(*) would
 * over-count under the time-series versioning model (many versions per id) and
 * skip the in-process folds, which is why this is a directive rather than a
 * cheaper query.
 *
 * WHY THIS EXISTS SEPARATELY FROM browseConceptPage. A surface wanting "how
 * many customers are there" used to fetch a page of rows and count what came
 * back, which answers a different question: it caps at the page size and has to
 * render "100+". Two calls -- this for the number, a bounded page for the rows
 * a surface actually shows -- is both cheaper and honest (memql#4263).
 *
 * The count is PER-CALLER by construction. A reader's count is the rows a
 * reader may see, and that is the correct number to show the person looking at
 * it; do not reach for a privileged count to make it look bigger.
 */
export async function countConcept(
  query: QueryClient,
  conceptId: string,
  opts: QueryCallOptions = {},
): Promise<number> {
  if (conceptId === "") {
    throw new Error("countConcept: conceptId is required");
  }
  const result = await query.executeNamed(
    "conceptCount",
    `count(concept==${conceptId})`,
    opts,
  );

  // The engine sets ResultMeta.count AND returns a {count: N} output row. Read
  // the meta first (it is the typed field), and fall back to the row for a
  // node that answered with only the envelope.
  const meta = result.meta();
  if (meta?.count !== undefined && meta.count !== null) {
    const n = typeof meta.count === "string" ? Number(meta.count) : meta.count;
    if (Number.isFinite(n) && n >= 0) return n;
  }
  const row = result.single();
  const raw = row?.["count"];
  const n = typeof raw === "string" ? Number(raw) : typeof raw === "number" ? raw : NaN;
  if (!Number.isFinite(n) || n < 0) {
    // -1 is the engine's "unknown count" sentinel (component/memql/result.go).
    // Reporting it as a number would put "-1 people" on a console; refusing is
    // what lets the caller render its own "could not read" state.
    throw new Error(`countConcept: ${conceptId} returned no usable count`);
  }
  return n;
}
