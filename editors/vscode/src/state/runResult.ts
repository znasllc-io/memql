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

/**
 * TOOL_RESULT_BANNER is required on every tool result.
 *
 * A `tool` is a declaration bound to a Go-backed handler, so it cannot be
 * session-defined and Run necessarily invokes the DEPLOYED definition. The
 * button is the same one that runs a query straight out of the buffer, so
 * without this the developer has every reason to read a tool result as their
 * edits' output. Same button, different semantics -- the UI has to say so
 * rather than let the assumption stand.
 */
export const TOOL_RESULT_BANNER =
  "This ran the DEPLOYED tool, not this buffer. A tool is a declaration bound to a Go-backed handler, so it cannot be defined from an editor session -- edits to its declaration here have no effect on what just ran.";

/**
 * BUFFER_RESULT_BANNER is the counterpart for a construct that DID run from
 * the buffer, shown when this run injected it.
 *
 * Stating the good case as well as the bad one is what makes the bad one
 * legible: a banner that only ever appears on tools is one the developer
 * learns to skip.
 */
export const BUFFER_RESULT_BANNER =
  "This ran the construct as written in the editor, session-defined for this connection only. Nothing was saved or deployed.";

/**
 * REUSED_INJECTION_BANNER covers the third case: the buffer was already
 * injected on this stream and had not changed, so no new session-define was
 * needed and the run still executed the buffer.
 */
/**
 * CATALOG_RESULT_BANNER covers the fourth case, which arrived with the
 * Constructs view (memql#3753): the construct was run from the CATALOG, so
 * there was no local source to assemble, nothing was session-defined, and what
 * executed is the definition the cluster has loaded.
 *
 * It exists because the three banners above were each false for it. The one it
 * would otherwise have been given -- REUSED_INJECTION_BANNER -- says "as
 * previously session-defined from this buffer", and a catalog run has no
 * buffer, was never session-defined, and for a PROMOTED construct has no file
 * anywhere on the machine. A result view exists to say what executed; a
 * confidently wrong sentence there is worse than none.
 */
export const CATALOG_RESULT_BANNER =
  "This ran the definition this cluster has loaded. There is no local source for it in this workspace, so nothing was session-defined and nothing in your editor affected what ran.";

export const REUSED_INJECTION_BANNER =
  "This ran the construct as previously session-defined from this buffer on the current connection. The buffer had not changed since, so it was not re-injected.";

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
