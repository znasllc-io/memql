import { useEffect, useMemo, useState, type Dispatch, type ReactNode } from "react";
import {
  elementCandidates,
  profileConcept,
  sanitizeArrangement,
  type ConceptProfile,
} from "@znasllc-io/memql-view-kit";

import { useCluster } from "../cluster/ClusterProvider";
import { useViewRows } from "../cluster/useViewRows";
import { Empty } from "../components/StatusMessage";
import { ErrorNotice, Skeleton } from "../ui";
import { ArrangementLayout } from "./ArrangementLayout";
import { ComposeButton, PopulationMeta, SectionHeader } from "./ComposeLayout";
import { SCENE_IDS } from "../scenes/registry";
import { WIDGET_IDS } from "../widgets/registry";
import { Inspector } from "./Inspector";
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

  // WHAT YOU SEE IS WHAT SHIPS, and it was not (found in the visual QA sweep,
  // task memql#4675). The preview rendered the DRAFT; a saved view renders the
  // draft put through sanitizeArrangement. So choosing `split` with no detail
  // pane drew a split in the composer and a stack everywhere else, and a
  // `focus` with no hero drew a lead column the saved view would not have.
  //
  // The preview is the sanitized value now. A person choosing a layout the
  // entries cannot honour sees it fall back immediately -- which is the honest
  // answer and one they can act on, where the old preview was a promise the
  // save quietly broke.
  //
  // The DRAFT is untouched: sanitize repairs the rendered value and never the
  // stored one, so the layout somebody chose is still what gets written and
  // becomes live the moment they add the element it needs.
  //
  // ABOVE the early returns, deliberately: a hook after a conditional return
  // changes the hook COUNT between renders, which React answers by discarding
  // the subtree -- the composer rendered nothing at all until this moved.
  const draftArrangement = draft.arrangements[conceptId];
  const preview = useMemo(
    () =>
      draftArrangement === undefined || profile === undefined
        ? undefined
        : sanitizeArrangement(draftArrangement, profile, {
            scenes: SCENE_IDS,
            widgets: WIDGET_IDS,
          }),
    [draftArrangement, profile],
  );

  const suggestion = useArrangementSuggestion(profile, suggester);
  // Which entry the inspector is on. Held HERE rather than in the draft: it is
  // a property of looking at a view, not of the view, and putting it in the
  // draft would make selecting an element mark the view dirty.
  const [selected, setSelected] = useState(0);

  if (data.registryError) {
    return <ErrorNotice sentence="Could not read the concept registry, so this section cannot be drawn." detail={data.registryError} />;
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
    if (data.registryLoading) return <Skeleton variant="rows" rows={4} />;
    return (
      <Empty>
        This cluster publishes no concept called{" "}
        <code className="font-mono break-all">{conceptId}</code>, so there is nothing to
        compose over.
      </Empty>
    );
  }

  const arrangement = draftArrangement;
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
            onLoadMore={data.loadMore}
            onRetry={data.retry}
          />
        }
      />

      {/* The suggestion, when there is one to report. Rendered ABOVE both panes
          rather than replacing either -- a proposal is an offer, and the thing
          it is an offer to replace has to stay visible while it is
          considered. */}
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

      {/* TWO PANES (spec E). The live view on the left is rendered by the
          SAME component that renders a saved view, with nothing added: no
          controls sit on it, because the thing a person is judging should not
          be covered in the buttons they are judging it with. Everything that
          edits is in the inspector. */}
      {arrangement === undefined || preview === undefined ? (
        <Skeleton variant="rows" rows={4} />
      ) : (
        <div className="flex min-w-0 flex-col gap-6 xl:flex-row xl:items-start">
          <div className="min-w-0 flex-1">
            <ArrangementLayout
              arrangement={preview}
              concept={concept}
              rows={data.rows}
              onSelectEntry={setSelected}
              selectedEntry={selected}
            />
          </div>
          <Inspector
            conceptId={conceptId}
            arrangement={arrangement}
            profile={profile}
            candidates={candidates}
            dispatch={dispatch}
            selected={selected}
            onSelect={setSelected}
            onSuggest={suggestion.ask}
            suggesting={suggestion.status === "asking"}
            onReset={() => dispatch({ kind: "reset", conceptId, profile })}
          />
        </div>
      )}
    </section>
  );
}
