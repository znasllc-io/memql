// Projecting a run's result into what the Result view renders.
//
// Rows go through view-kit and each concept's own @displayCard, exactly as the
// Concepts browser does -- there is NO result-specific row renderer, and there
// is no concept-specific code anywhere. A result set can span several concepts
// (a logic construct returning rows from two queries, say), so the rows are
// grouped by their `concept` intrinsic and each group is rendered against that
// concept's descriptor.
//
// A concept the extension has no descriptor for degrades to a synthetic one
// with no display card, which view-kit already handles: the row falls back to
// its id. That is what lets a concept declared five minutes ago render without
// a client change.
//
// Deliberately free of `vscode` imports; webview/runPanel.ts renders these.
// Tested under bare `node --test`.

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { ConceptLike } from "@znasllc-io/memql-view-kit";

/** One concept's rows within a result set. */
export interface ResultGroup {
  concept: ConceptLike;
  rows: Row[];
}

// The bucket for rows that carry no `concept` intrinsic at all. A logic
// construct can return arbitrary objects -- a computed summary, a count -- and
// those are still worth showing; they simply have no display card and no
// Concepts surface to link into.
export const UNTYPED_GROUP_ID = "";

/**
 * groupRowsByConcept buckets rows by their `concept` intrinsic, preserving the
 * order in which each concept first appeared.
 *
 * `concepts` is the descriptor map (from the Concepts surface's own list). A
 * missing entry is not an error: a concept can be registered on the cluster
 * and absent from a cached list, and the fallback descriptor renders fine.
 */
export function groupRowsByConcept(
  rows: readonly Row[],
  concepts: ReadonlyMap<string, ConceptLike>,
): ResultGroup[] {
  const order: string[] = [];
  const buckets = new Map<string, Row[]>();

  for (const row of rows) {
    const conceptId = typeof row.concept === "string" ? row.concept : UNTYPED_GROUP_ID;
    let bucket = buckets.get(conceptId);
    if (bucket === undefined) {
      bucket = [];
      buckets.set(conceptId, bucket);
      order.push(conceptId);
    }
    bucket.push(row);
  }

  return order.map((conceptId) => ({
    concept: concepts.get(conceptId) ?? fallbackConcept(conceptId),
    rows: buckets.get(conceptId) ?? [],
  }));
}

function fallbackConcept(conceptId: string): ConceptLike {
  if (conceptId === UNTYPED_GROUP_ID) {
    // `entity` is what view-kit puts in its empty-state text ("No rows for
    // X."), so it has to read as a phrase rather than as an identifier.
    return { id: UNTYPED_GROUP_ID, entity: "result" };
  }
  // No displayCard: view-kit falls back to the row id, which is correct and
  // is exactly what a concept declaring no card gets anyway.
  return { id: conceptId, entity: conceptId };
}

// PROVENANCE, IN THE RESULT'S META LINE. Every result says what ran -- the
// code in the editor, or the cluster's deployed definition -- because the same
// Run button means both, and a result read as the other would mislead. It used
// to be a paragraph on every run, which a reader learns to skip; it is now a
// few words beside the row count, where it is read with the result.

/**
 * A `tool` is bound to a handler in the engine and cannot be defined from an
 * editor session, so Run necessarily invokes the DEPLOYED tool. Said on every
 * tool result, because the same button runs a query straight out of the buffer.
 */
export const TOOL_RESULT_BANNER = "Deployed tool · edits here don't apply";

/**
 * A construct that ran from the editor: session-defined for this connection,
 * nothing saved or deployed. Said on the good case too, which is what makes the
 * other cases legible.
 */
export const BUFFER_RESULT_BANNER = "From this editor · not saved";

/**
 * The buffer was already defined on this connection and had not changed, so it
 * was not sent again -- and what ran is STILL the editor's code. The same words
 * as a fresh definition: whether it was re-sent is the run path's business, and
 * "not re-sent" must not read as "not your code".
 */
export const REUSED_INJECTION_BANNER = BUFFER_RESULT_BANNER;

/**
 * A construct run from the CATALOG (memql#3753): no local source, nothing
 * session-defined, and what executed is the definition the cluster has loaded.
 * Provenance in two words, not a disclaimer -- beside a legitimately empty
 * result, an explanation read as a failure (memql#4083).
 */
export const CATALOG_RESULT_BANNER = "Deployed version";

export function resultBannerFor(outcome: {
  ranDeployedDefinition: boolean;
  injected: boolean;
  /**
   * The construct's kind. REQUIRED, not optional, and that is deliberate: it
   * is needed since memql#3753 to tell the two DEPLOYED cases apart -- a tool
   * is deployed because a tool cannot be session-defined at all, anything else
   * because there was no local source for it -- and an optional field with a
   * default would have picked one of those two silently. It was optional for
   * about ten minutes, during which the sole call site (which passes the whole
   * outcome, whose kind lives at `target.kind`) would have flipped EVERY tool
   * result onto the catalog sentence with nothing failing. Required means the
   * compiler names the call site instead.
   */
  kind: string;
}): string {
  if (outcome.ranDeployedDefinition) {
    // A TOOL IS DEPLOYED FOR A DIFFERENT REASON THAN A CATALOG RUN IS, and the
    // two sentences say different things to a developer: one is about what a
    // tool IS, the other about where this construct's source lives. Collapsing
    // them would tell someone running a catalog query that they had run a tool.
    return outcome.kind === "tool" ? TOOL_RESULT_BANNER : CATALOG_RESULT_BANNER;
  }
  return outcome.injected ? BUFFER_RESULT_BANNER : REUSED_INJECTION_BANNER;
}
