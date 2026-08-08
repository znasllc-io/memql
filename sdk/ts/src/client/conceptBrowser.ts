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
// unmarked-list backstop (MEMORY_ENGINE_DEFAULT_LIST_CAP, 50 rows), so the
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
  const { pageSize: requestedPageSize, ...callOpts } = opts;
  const pageSize =
    requestedPageSize !== undefined && requestedPageSize > 0
      ? requestedPageSize
      : DEFAULT_CONCEPT_BROWSE_PAGE_SIZE;

  // sort(paginate(concept==X, N), "createdAt", "asc") -- the canonical
  // keyset-eligible directive chain (leading createdAt sort plus a paginate
  // window). The engine appends `id ASC` as the tie-breaker under equal
  // createdAt.
  const call = `sort(paginate(concept==${conceptId}, ${pageSize}), "createdAt", "asc")`;

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
