import { useCallback, useMemo, useReducer, useState, type ReactNode } from "react";
import { useLocation, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { Empty } from "../components/StatusMessage";
import { ErrorNotice, Skeleton, TextInput } from "../ui";
import { ComposeButton } from "./ComposeLayout";
import { ComposerSection } from "./ComposerSection";
import type { ViewDraft } from "./describe";
import {
  composerReducer,
  draftArrangements,
  draftFromSavedView,
  draftFromSuggestion,
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
  // A DESCRIBED draft arrives through history state rather than the URL (epic
  // memql#4661): an arrangement is a structure, and a URL that carried one
  // would be a URL nobody could read, share or shorten. The concept ids stay
  // in the query string, so the address still says what the view is ABOUT and
  // a reload without the state falls back to composing those concepts by hand
  // -- which is a working composer, not an error.
  const described = useDescribedDraft();
  const initial = useMemo(
    () => (described === undefined ? emptyDraft(conceptIds) : draftFromSuggestion(described)),
    [conceptIds, described],
  );

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

  if (error) return <ErrorNotice sentence="Could not read this view." detail={error} />;
  if (loading) return <Skeleton variant="rows" rows={5} />;
  if (missing || view === null) {
    return <Empty>You have no saved view with that id.</Empty>;
  }
  return <Composer key={view.id} initial={draftFromSavedView(view)} mode="update" />;
}

// useDescribedDraft reads the draft "Describe it" put in history state.
//
// Typed defensively: history state is a value the BROWSER holds across a
// reload and a person can arrive with anything in it. A malformed one is
// treated as absent, which lands on the ordinary compose-by-hand path.
function useDescribedDraft(): ViewDraft | undefined {
  const { state } = useLocation();
  if (state === null || typeof state !== "object") return undefined;
  const draft = (state as Record<string, unknown>)["describedDraft"];
  if (draft === null || typeof draft !== "object") return undefined;
  const value = draft as Partial<ViewDraft>;
  if (!Array.isArray(value.arrangements) || value.arrangements.length === 0) return undefined;
  if (!Array.isArray(value.conceptIds) || value.conceptIds.length === 0) return undefined;
  return {
    name: typeof value.name === "string" ? value.name : "Suggested view",
    reasoning: typeof value.reasoning === "string" ? value.reasoning : "",
    conceptIds: value.conceptIds,
    arrangements: value.arrangements,
  };
}

function Composer({
  initial,
  mode,
}: {
  initial: ComposerDraft;
  mode: "create" | "update";
}): ReactNode {
  const navigate = useNavigate();
  const { clients } = useCluster();
  const [draft, dispatch] = useReducer(composerReducer, initial);
  const { saving, error: saveError, save } = useSaveView();
  const [saveFailed, setSaveFailed] = useState("");

  // The suggester is UNDEFINED when there is no connection to ask over, and
  // that is the only thing the composer needs to know about the AI path. It is
  // built here rather than inside the section so all sections share one, and
  // so a test can drive the whole page with a suggester that throws.
  const suggester = useMemo<ArrangementSuggester | undefined>(
    () => (clients === null ? undefined : clusterSuggester(clients.suggest)),
    [clients],
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
              <TextInput
                value={draft.name}
                onChange={(name) => dispatch({ kind: "named", name })}
                placeholder="What this view is for"
              />
            </label>
            <label className="flex min-w-64 flex-1 flex-col gap-1">
              <span className="text-xs font-semibold tracking-wide text-muted uppercase">
                Description
              </span>
              <TextInput
                value={draft.description}
                onChange={(description) => dispatch({ kind: "described", description })}
                placeholder="Optional"
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
                ? "Save this view"
                : "A view needs a name and at least one element"
            }
          >
            {saving ? "Saving…" : mode === "create" ? "Save view" : "Save changes"}
          </ComposeButton>
          <span className="text-xs text-subtle">
            Saved to your own views. Nobody else&rsquo;s list changes.
          </span>
        </div>
      </header>

      {saveFailed || saveError ? (
        <ErrorNotice sentence="The view was not saved." next="Your arrangement is still here; try saving again." detail={saveFailed || saveError} />
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
