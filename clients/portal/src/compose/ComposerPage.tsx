import { useCallback, useMemo, useReducer, useState, type ReactNode } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { ComposeButton } from "./ComposeLayout";
import { ComposerSection } from "./ComposerSection";
import {
  composerReducer,
  draftArrangements,
  draftFromSavedView,
  emptyDraft,
  isSavable,
  type ComposerDraft,
} from "./composerState";
import { clusterSuggester, type ArrangementSuggester } from "./suggest";
import { composedViewPath, readConceptSelection } from "./urls";
import { useSavedView, useSaveView } from "./useSavedViews";

// The composer.
//
// ===========================================================================
// MULTI-CONCEPT, AND WHAT IT HONESTLY IS
// ===========================================================================
// A composed view may cover several concepts, and it does so as INDEPENDENT
// STACKED SECTIONS: one concept per section, each with its own row set, its
// own profile, its own candidate elements and its own arrangement. "Orders and
// customers on one page" works; "orders joined to their customers" does not,
// and is not attempted.
//
// That is a deliberate scope cut, not an oversight. A join needs the engine to
// correlate two concepts in one read -- a relationship traversal projected
// into a single row set -- and the query surface a client can reach
// (executeNamed over named DSL constructs) has no such construct: every query
// binds ONE concept in its signature. A client-side join would mean paging two
// concepts in full and correlating them in the browser, which is a different
// and much worse thing wearing the same word. So: several concepts, several
// sections, no correlation between them, and this paragraph rather than a
// feature that quietly returns the wrong rows.
//
// ===========================================================================
// THE DRAFT IS ONE VALUE
// ===========================================================================
// Everything the person does lands in one ComposerDraft through one pure
// reducer (composerState.ts), and the draft is what gets saved. The AI path
// dispatches the same action a hand edit does, with the same kind of payload,
// so there is no "suggested mode" for anything downstream to behave
// differently in.

export function ComposerPage(): ReactNode {
  const [params] = useSearchParams();
  const conceptIds = useMemo(() => readConceptSelection(params), [params]);
  const initial = useMemo(() => emptyDraft(conceptIds), [conceptIds]);

  if (conceptIds.length === 0) {
    return (
      <Empty>
        No concept selected. Pick one from the compose page to start a view.
      </Empty>
    );
  }
  // Keyed on the selection so changing it in the URL starts a fresh draft
  // rather than leaving sections from the previous one in state.
  return <Composer key={conceptIds.join("|")} initial={initial} mode="create" />;
}

// The edit route: load the saved view, then hand its draft to the same
// component. A separate component because the draft cannot exist until the row
// has arrived, and a reducer's initial state is read once.
export function ComposerEditPage(): ReactNode {
  const { viewId = "" } = useParams<{ viewId: string }>();
  const { view, loading, error, missing } = useSavedView(viewId);

  if (error) return <ErrorMessage>Failed to read the view: {error}</ErrorMessage>;
  if (loading) return <Loading what="the view" />;
  if (missing || view === null) {
    return <Empty>You have no saved view with that id.</Empty>;
  }
  return <Composer key={view.id} initial={draftFromSavedView(view)} mode="update" />;
}

function Composer({
  initial,
  mode,
}: {
  initial: ComposerDraft;
  mode: "create" | "update";
}): ReactNode {
  const navigate = useNavigate();
  const { dispatcher } = useCluster();
  const [draft, dispatch] = useReducer(composerReducer, initial);
  const { saving, error: saveError, save } = useSaveView();
  const [saveFailed, setSaveFailed] = useState("");

  // The suggester is UNDEFINED when there is no connection to ask over, and
  // that is the only thing the composer needs to know about the AI path. It is
  // built here rather than inside the section so all sections share one, and
  // so a test can drive the whole page with a suggester that throws.
  const suggester = useMemo<ArrangementSuggester | undefined>(
    () => (dispatcher === null ? undefined : clusterSuggester(dispatcher)),
    [dispatcher],
  );

  const onSave = useCallback(() => {
    setSaveFailed("");
    const viewId = draft.viewId === "" ? newShortId() : draft.viewId;
    void save(
      {
        viewId,
        name: draft.name.trim(),
        description: draft.description.trim(),
        conceptIds: draft.conceptIds,
        arrangements: draftArrangements(draft),
        origin: draft.origin,
      },
      mode,
    )
      .then((saved) => navigate(composedViewPath(saved)))
      .catch((err: unknown) => {
        setSaveFailed(err instanceof Error ? err.message : String(err));
      });
  }, [draft, mode, navigate, save]);

  return (
    <section className="flex min-h-full flex-col gap-8 pb-8">
      <header className="flex flex-wrap items-end justify-between gap-x-6 gap-y-3 border-b border-line pb-4">
        <div className="min-w-0 flex-1">
          <h1 className="text-xl font-semibold tracking-tight">
            {mode === "create" ? "Compose a view" : "Edit this view"}
          </h1>
          <p className="mt-1 max-w-2xl text-sm text-muted">
            Elements are matched against these concepts&rsquo; shape, so the arrangement
            below already works. Adjust it, or ask for a suggestion — neither is
            required, and a saved view is a row like any other.
          </p>
          <div className="mt-3 flex flex-wrap gap-3">
            <label className="flex min-w-64 flex-1 flex-col gap-1">
              <span className="text-xs font-semibold tracking-wide text-muted uppercase">
                Name
              </span>
              <input
                type="text"
                value={draft.name}
                onChange={(e) => dispatch({ kind: "named", name: e.target.value })}
                placeholder="What this view is for"
                className="rounded border border-line bg-surface px-2 py-1 text-sm text-fg"
              />
            </label>
            <label className="flex min-w-64 flex-1 flex-col gap-1">
              <span className="text-xs font-semibold tracking-wide text-muted uppercase">
                Description
              </span>
              <input
                type="text"
                value={draft.description}
                onChange={(e) =>
                  dispatch({ kind: "described", description: e.target.value })
                }
                placeholder="Optional"
                className="rounded border border-line bg-surface px-2 py-1 text-sm text-fg"
              />
            </label>
          </div>
        </div>
        <div className="flex shrink-0 flex-col items-end gap-2">
          <ComposeButton
            tone="accent"
            onClick={onSave}
            disabled={!isSavable(draft) || saving}
            title={
              isSavable(draft)
                ? "Save this view as a row you own"
                : "A view needs a name and at least one element"
            }
          >
            {saving ? "Saving…" : mode === "create" ? "Save view" : "Save changes"}
          </ComposeButton>
          <span className="text-xs text-subtle">
            Saved as a row in v1:portalviews:view, owned by you.
          </span>
        </div>
      </header>

      {saveFailed || saveError ? (
        <ErrorMessage>Could not save the view: {saveFailed || saveError}</ErrorMessage>
      ) : null}

      {draft.conceptIds.map((conceptId) => (
        <ComposerSection
          key={conceptId}
          conceptId={conceptId}
          draft={draft}
          dispatch={dispatch}
          suggester={suggester}
        />
      ))}
    </section>
  );
}
