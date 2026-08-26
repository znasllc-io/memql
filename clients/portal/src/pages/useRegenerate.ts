import { useCallback, useState } from "react";
import {
  LAYOUT_DESCRIPTIONS,
  SECTION_LAYOUTS,
  arrangementLayout,
  arrangementRequest,
  profileConcept,
  readArrangement,
  sanitizeArrangement,
  type Arrangement,
  type ConceptLike,
  type RowLike,
} from "@znasllc-io/memql-view-kit";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { SCENE_IDS } from "../nexus/scene/registry";
import { WIDGET_IDS } from "../widgets/registry";
import { ARRANGEMENT_SUGGEST_DOMAIN } from "../compose/suggest";
import { serializeArrangement } from "../compose/savedViews";
import { fetchPageOverride } from "./overrides";
import { requiredForSection, type PageManifest } from "./manifest";

// Regenerating a page (epic memql#4661, task memql#4669).
//
// ===========================================================================
// THE ONLY PLACE AI RUNS
// ===========================================================================
// Spec D3: the model runs on this explicit action and nowhere else. Not at
// render, not on a subscription, not on a reconnect. Everything in the render
// path is a read of what was already stored, which is what makes a page cost
// nothing to look at and makes the console work on a cluster with no provider.
//
// ===========================================================================
// THE REPAIRED RESULT IS WHAT IS WRITTEN, NEVER THE RAW REPLY
// ===========================================================================
// readArrangement parses the reply, drops elements that do not exist or do not
// fit, repairs a layout that cannot be honoured, and re-inserts the page's
// REQUIRED entries. What lands in the row is therefore a value a person could
// have built by hand -- which is the property that makes it safe to store
// forever and safe to render without asking anything again.
//
// ===========================================================================
// A FAILED REGENERATION WRITES NOTHING
// ===========================================================================
// No provider, a refusal, a transport error, a reply that parses to nothing:
// every one of them leaves the page exactly as it was and says so. A partial
// write -- section one regenerated, section two not -- would be a page in a
// state nobody chose, so the write happens once, after every section has an
// answer.

export interface RegenerateState {
  readonly status: "idle" | "working" | "failed";
  // Why it did not happen, in words a person can act on. Empty unless failed.
  readonly error: string;
  // Ask for a regeneration. `hint` is what the person typed, and may be empty
  // -- regenerate is a button before it is a conversation.
  readonly run: (hint: string) => void;
  readonly dismiss: () => void;
}

export function useRegenerate(
  manifest: PageManifest,
  pageId: string,
  // The concept + rows each section currently holds, reported upward by
  // ArrangedSection. Needed because the request is built from a PROFILE, and a
  // profile needs the rows the page actually loaded.
  sections: Readonly<Record<string, { concept: ConceptLike; rows: readonly RowLike[] }>>,
  current: Readonly<Record<string, Arrangement>>,
  onWritten: () => void,
): RegenerateState {
  // `query` is the typed client: the generated mutation methods hang off it
  // beside the reads (useSavedViews takes the same route), and `clients`
  // carries the connection-scoped suggest. One Connection behind both.
  const { query, clients } = useCluster();
  const [status, setStatus] = useState<RegenerateState["status"]>("idle");
  const [error, setError] = useState("");

  const dismiss = useCallback(() => {
    setStatus("idle");
    setError("");
  }, []);

  const run = useCallback(
    (hint: string) => {
      if (query === null || clients === null) {
        setStatus("failed");
        setError(
          "Not connected to a cluster, so there is nothing to ask. The page below is " +
            "unchanged.",
        );
        return;
      }
      setStatus("working");
      setError("");

      void (async () => {
        try {
          const written: Arrangement[] = [];
          const conceptIds: string[] = [];

          for (const section of manifest.sections) {
            const loaded = sections[section.conceptId];
            // A section whose rows have not arrived is SKIPPED rather than
            // regenerated from nothing: a profile built on zero rows would
            // fit almost nothing, and the model would faithfully propose a
            // page for a concept it was told is empty.
            if (loaded === undefined) continue;

            const profile = profileConcept(loaded.concept, loaded.rows);
            const existing =
              current[section.conceptId] ?? section.arrangement;
            const request = {
              ...arrangementRequest(profile),
              // The layout vocabulary and the current layout, which the
              // reshaped prompt asks for and the baseline alone does not
              // carry.
              layouts: SECTION_LAYOUTS.map((layout) => ({
                layout,
                description: LAYOUT_DESCRIPTIONS[layout],
              })),
              currentLayout: arrangementLayout(existing),
              baseline: existing,
              hint,
            };

            const reply = await clients.suggest(
              ARRANGEMENT_SUGGEST_DOMAIN,
              request as unknown as Record<string, unknown>,
              {},
            );
            const proposal = readArrangement(reply.result, profile, {
              required: requiredForSection(manifest, section.conceptId),
              scenes: SCENE_IDS,
              widgets: WIDGET_IDS,
            });
            // Repaired ONCE MORE against the page's guardrail before it is
            // written. readArrangement already applied it; running sanitize on
            // the way out means the stored value is the repaired one whatever
            // path produced it, so a future caller of this function cannot
            // skip the guardrail by not knowing about it.
            written.push(
              sanitizeArrangement(proposal.arrangement, profile, {
                required: requiredForSection(manifest, section.conceptId),
                scenes: SCENE_IDS,
                widgets: WIDGET_IDS,
              }),
            );
            conceptIds.push(section.conceptId);
          }

          if (written.length === 0) {
            throw new Error(
              "None of this page's sections have loaded their rows yet, so there is " +
                "nothing to arrange.",
            );
          }

          // ONE WRITE, after every section has an answer. The row id is the
          // existing override's when there is one -- a write is an append onto
          // it, which is what makes the history a history rather than a pile
          // of unrelated rows.
          const existing = await fetchPageOverride(query, pageId);
          await query.writePageOverride({
            viewId: existing?.id ?? newShortId(),
            targetPageId: pageId,
            arrangements: written.map(serializeArrangement),
            conceptIds,
            origin: "suggested",
          });

          setStatus("idle");
          onWritten();
        } catch (err: unknown) {
          // EVERY failure lands here and every one of them means the same
          // thing to the person: the page is unchanged. The message says which
          // it was, because "no provider configured" and "the call timed out"
          // want different responses from an operator.
          setStatus("failed");
          setError(err instanceof Error ? err.message : String(err));
        }
      })();
    },
    [query, clients, manifest, pageId, sections, current, onWritten],
  );

  return { status, error, run, dismiss };
}
