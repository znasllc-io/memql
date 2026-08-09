import { useEffect, useMemo, type Dispatch, type ReactNode } from "react";
import {
  elementCandidates,
  profileConcept,
  type ConceptProfile,
} from "@znasllc-io/memql-view-kit";

import { useCluster } from "../cluster/ClusterProvider";
import { useViewRows } from "../cluster/useViewRows";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { ArrangementBands } from "./ArrangementBands";
import { ComposeButton, PopulationMeta, SectionHeader } from "./ComposeLayout";
import type { ComposerAction, ComposerDraft } from "./composerState";
import { useArrangementSuggestion, type ArrangementSuggester } from "./suggest";

// One concept's section of the composer.
//
// THE ORDER OF EVENTS IS THE ARGUMENT OF THIS ISSUE, so it is worth stating
// plainly. This component:
//
//   1. reads the concept's rows through exactly the machinery the concept
//      browser and the predefined views use (useViewRows -- same registry,
//      same keyset walk), so there is no second answer to "what are this
//      concept's rows";
//   2. profiles them and asks view-kit which elements fit -- deterministic,
//      explainable, and complete before anything else happens;
//   3. seeds the draft with the deterministic arrangement, so the section is
//      ALREADY A WORKING VIEW at first paint;
//   4. offers a model, as a button.
//
// Nothing in steps 1-3 can fail because of a missing provider, and step 4
// cannot take away what steps 1-3 produced. A cluster with no AI configured
// reaches step 4, gets a refusal, and shows the sentence saying so under a
// view that works.
//
// EACH SECTION OWNS ITS OWN WALK. A composed view may stack several concepts,
// and a hook cannot be called in a loop of varying length -- so the section is
// the component, one per concept, and the page holds only the draft. That is
// also the honest data model: the sections are independent row sets over
// independent concepts, and no join relates them (see the multi-concept note
// in ComposerPage).

export interface ComposerSectionProps {
  conceptId: string;
  draft: ComposerDraft;
  dispatch: Dispatch<ComposerAction>;
  // Absent when there is no connection to ask over. The section still works;
  // the suggest button says why it cannot.
  suggester: ArrangementSuggester | undefined;
}

export function ComposerSection({
  conceptId,
  draft,
  dispatch,
  suggester,
}: ComposerSectionProps): ReactNode {
  const { status } = useCluster();
  const data = useViewRows(conceptId);
  const concept = data.concept;

  // The profile is the input to everything below. Recomputed when the rows
  // change (a "Load more" widens the sample and can make an element fit that
  // did not), and undefined only while the concept itself is unknown.
  const profile = useMemo<ConceptProfile | undefined>(
    () => (concept === undefined ? undefined : profileConcept(concept, data.rows)),
    [concept, data.rows],
  );

  // Seed once, as soon as there is something to seed from. Idempotent in the
  // reducer, so a re-profile after paging does not undo an edit.
  useEffect(() => {
    if (profile === undefined) return;
    dispatch({ kind: "seeded", conceptId, profile });
  }, [profile, conceptId, dispatch]);

  const suggestion = useArrangementSuggestion(profile, suggester);

  if (data.registryError) {
    return <ErrorMessage>Failed to list concepts: {data.registryError}</ErrorMessage>;
  }
  if (concept === undefined || profile === undefined) {
    // "Not connected" and "the registry does not carry that" are different
    // facts and want different words. Checking the connection FIRST matters:
    // before a dial settles the registry is legitimately empty, and reporting
    // that as "this cluster publishes no such concept" would accuse the
    // cluster of something the browser has not yet asked it.
    if (status !== "connected") {
      return <Empty>Not connected to a cluster. See the connection state in the header.</Empty>;
    }
    if (data.registryLoading) return <Loading what="the concept registry" />;
    return (
      <Empty>
        This cluster publishes no concept called{" "}
        <code className="font-mono break-all">{conceptId}</code>, so there is nothing to
        compose over.
      </Empty>
    );
  }

  const arrangement = draft.arrangements[conceptId];
  const candidates = elementCandidates(profile);

  return (
    <section className="flex min-w-0 flex-col gap-5">
      <SectionHeader
        conceptId={concept.id}
        entity={concept.entity}
        meta={
          <PopulationMeta
            count={data.rows.length}
            status={data.walk.status}
            error={data.walk.error}
            onLoadMore={data.loadMore}
            onRetry={data.retry}
          />
        }
        actions={
          <div className="flex items-center gap-2">
            <ComposeButton
              onClick={suggestion.ask}
              disabled={suggestion.status === "asking"}
              title="Ask a model to propose an arrangement. The one below already works."
            >
              {suggestion.status === "asking" ? "Asking…" : "Suggest an arrangement"}
            </ComposeButton>
            <ComposeButton
              onClick={() => dispatch({ kind: "reset", conceptId, profile })}
              title="Discard edits and go back to the arrangement matched from these rows."
            >
              Start over
            </ComposeButton>
          </div>
        }
      />

      {/* The suggestion, when there is one to report. Rendered ABOVE the view
          rather than replacing it -- a proposal is an offer, and the thing it
          is an offer to replace has to stay visible while it is considered. */}
      {suggestion.status === "unavailable" ? (
        <p className="rounded border border-line bg-raised px-3 py-2 text-sm text-muted">
          No suggestion available: {suggestion.unavailable}. The arrangement below was
          matched from these rows and needs no model.
        </p>
      ) : null}

      {suggestion.status === "ready" && suggestion.arrangement !== undefined ? (
        <div className="rounded border border-accent bg-surface px-3 py-2">
          <p className="text-sm text-fg">
            {suggestion.reasoning === ""
              ? "A model proposed a different arrangement."
              : suggestion.reasoning}
          </p>
          {suggestion.problems.length > 0 ? (
            <ul className="mt-2 list-disc pl-5 text-xs text-subtle">
              {suggestion.problems.map((problem, i) => (
                <li key={`${problem.fault}:${i}`}>{problem.detail}</li>
              ))}
            </ul>
          ) : null}
          <div className="mt-2 flex items-center gap-2">
            <ComposeButton
              tone="accent"
              onClick={() => {
                dispatch({
                  kind: "applied",
                  conceptId,
                  arrangement: suggestion.arrangement!,
                });
                suggestion.dismiss();
              }}
            >
              Use it
            </ComposeButton>
            <ComposeButton onClick={suggestion.dismiss}>Keep mine</ComposeButton>
          </div>
        </div>
      ) : null}

      {arrangement === undefined ? (
        <Loading what="the rows" />
      ) : (
        <ArrangementBands
          arrangement={arrangement}
          concept={concept}
          rows={data.rows}
          controls={(index) => (
            <span className="flex items-center gap-1">
              <ComposeButton
                onClick={() => dispatch({ kind: "elementMoved", conceptId, at: index, by: -1 })}
                disabled={index === 0}
                title="Move up"
              >
                ↑
              </ComposeButton>
              <ComposeButton
                onClick={() => dispatch({ kind: "elementMoved", conceptId, at: index, by: 1 })}
                disabled={index === arrangement.elements.length - 1}
                title="Move down"
              >
                ↓
              </ComposeButton>
              <ComposeButton
                tone="danger"
                onClick={() => dispatch({ kind: "elementRemoved", conceptId, at: index })}
                title="Remove from this view"
              >
                Remove
              </ComposeButton>
            </span>
          )}
        />
      )}

      {/* WHAT FITS, AND WHY NOT. Every element in the library is listed --
          including the ones that CANNOT render these rows -- because the
          question a person actually has is "why can't I pick the calendar",
          and the answer is view-kit's own sentence rather than an absence.
          Always visible rather than folded into a disclosure: in a composer
          the list of what fits is the primary tool, not a footnote. */}
      <div className="rounded border border-line bg-surface px-3 py-2">
        <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">
          What fits {concept.entity} ({candidates.filter((c) => c.usable).length} of{" "}
          {candidates.length})
        </h3>
        <ul className="mt-3 flex flex-col gap-3">
          {candidates.map((candidate) => {
            const inView =
              arrangement !== undefined &&
              arrangement.elements.some((e) => e.element === candidate.element.id);
            return (
              <li key={candidate.element.id} className="flex items-start gap-3">
                <span className="w-24 shrink-0">
                  {candidate.usable && !inView ? (
                    <ComposeButton
                      onClick={() =>
                        dispatch({
                          kind: "elementAdded",
                          conceptId,
                          element: candidate.element.id,
                        })
                      }
                    >
                      Add
                    </ComposeButton>
                  ) : (
                    <span className="text-xs text-subtle">
                      {inView ? "in this view" : "does not fit"}
                    </span>
                  )}
                </span>
                <span className="min-w-0">
                  <span className="text-sm font-medium text-fg">
                    {candidate.element.title}
                  </span>{" "}
                  <span className="text-xs text-subtle">({candidate.band} band)</span>
                  <p className="text-xs text-muted">{candidate.explanation}</p>
                </span>
              </li>
            );
          })}
        </ul>
      </div>
    </section>
  );
}
