import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { RowDetail } from "../components/RowDetail";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { useRowDetail } from "../cluster/useConceptRows";
import type { ViewDefinition } from "./registry";
import { viewPath } from "./urls";

// THE LAYOUT GRAMMAR every predefined view is built out of.
//
// One shape, five views (see registry.ts for why): a header that names the
// population, then bands -- a reading, a shape, a roll. This module owns the
// chrome around the bands; the bands' CONTENTS are always elements, rendered
// through <ViewElement>.
//
// TYPOGRAPHY AND HIERARCHY, stated once here so five views cannot each
// invent it:
//
//   eyebrow   the concept id, monospace, subtle. Not decoration -- it is the
//             one thing on the page that tells an operator which rows they
//             are looking at, and it is the address they would paste into a
//             query. Monospace because ids are read character by character.
//   title     text-xl semibold. The only element at that size on the page.
//   blurb     one line of muted prose, capped at a readable measure. It says
//             what the population IS, in the operator's words.
//   band      a small-caps label on a hairline rule that runs to the pane's
//             edge. A rule with a caption, not a card header: a band is a
//             HORIZON in one continuous page, and boxing each one would make
//             three readings of a single population look like three
//             unrelated widgets.
//
// The bands are not numbered. They are three simultaneous facts about one
// population, and "01 / 02 / 03" would assert a sequence that does not exist.
//
// COLOUR. The chrome here is greyscale plus the one accent. Colour appears in
// exactly two places on a view: series identity inside an element (view-kit's
// validated palette) and lifecycle severity on a status badge
// (src/styles/status.css). Anything else would spend the channel that carries
// "this deploy failed" on decoration.

// What every view body is handed.
//
// THE PRIMARY POPULATION COMES FROM ABOVE, and that is load-bearing rather
// than tidy: ViewPage owns the keyset walk so the frame's header can state
// how much of it is loaded, and so opening a row re-renders the body without
// restarting paging. Same reason ConceptPage owns the browser's walk. A view
// that needs a SECOND population (People's sessions, Agents' grants) fetches
// that one itself -- it is the view's own composition choice, and no shared
// chrome reports on it.
export interface ViewProps {
  view: ViewDefinition;
  concept: Concept;
  rows: readonly Row[];
  // "" when nothing is selected. A view passes it to the element that lists
  // rows so the selected one is marked, and to any action that operates on a
  // selection.
  selectedRowId: string;
  onSelect: (rowId: string) => void;
}

export function ViewFrame({
  view,
  conceptId,
  meta,
  actions,
  aside,
  children,
}: {
  view: ViewDefinition;
  // The id shown in the eyebrow. Usually the view's own concept; passed in so
  // a view whose header is about something else does not have to lie.
  conceptId: string;
  // Right-aligned, small: the honest state of the data behind the page (how
  // much of the population is loaded, and how to load more).
  meta?: ReactNode;
  // Right-aligned, prominent: what an operator can DO here. Absent when the
  // caller cannot do anything -- see each view for the role gate.
  actions?: ReactNode;
  aside?: ReactNode;
  children: ReactNode;
}): ReactNode {
  return (
    <section className="flex min-h-full flex-col gap-6 pb-8 xl:flex-row xl:items-start xl:gap-8">
      <div className="flex min-w-0 flex-1 flex-col gap-6">
        <header className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3 border-b border-line pb-4">
          <div className="min-w-0">
            <p className="font-mono text-xs break-all text-subtle">{conceptId}</p>
            <h1 className="mt-1 text-xl font-semibold tracking-tight">{view.title}</h1>
            <p className="mt-1 max-w-2xl text-sm text-muted">{view.blurb}</p>
          </div>
          <div className="flex shrink-0 flex-col items-end gap-2">
            {actions}
            {meta}
          </div>
        </header>

        {children}
      </div>

      {aside}
    </section>
  );
}

// Band is one horizon of the page.
//
// `title` omitted makes it the opening reading -- the numbers ARE the label,
// and captioning a stat strip "Summary" adds a word and no information.
export function Band({
  title,
  meta,
  panel = false,
  children,
}: {
  title?: string;
  meta?: ReactNode;
  // Wrap the contents in a surface. On for an enumerable body (a table needs
  // an edge and its own horizontal scroll); off for a reading or a rail,
  // which sit directly on the page.
  panel?: boolean;
  children: ReactNode;
}): ReactNode {
  return (
    <section className="min-w-0">
      {title === undefined ? null : (
        <div className="mb-3 flex items-baseline gap-3">
          <h2 className="shrink-0 text-xs font-semibold tracking-wide text-muted uppercase">
            {title}
          </h2>
          <span aria-hidden="true" className="h-px min-w-4 flex-1 bg-line" />
          {meta === undefined ? null : (
            <span className="shrink-0 text-xs text-subtle">{meta}</span>
          )}
        </div>
      )}
      {panel ? (
        <div className="overflow-x-auto rounded-lg border border-line bg-surface p-1">
          {children}
        </div>
      ) : (
        children
      )}
    </section>
  );
}

// PopulationMeta is the honest state of the keyset walk behind a view.
//
// Every reading on the page -- the counts, the proportions -- describes THE
// ROWS LOADED, not the whole concept. Saying so is not a caveat, it is the
// difference between a dashboard and a lie: an operator who reads "3 owners"
// off a page that has loaded one of nine pages has been misinformed. So the
// count is always present and always says "loaded".
export function PopulationMeta({
  count,
  status,
  error,
  onLoadMore,
  onRetry,
}: {
  count: number;
  status: "idle" | "loading" | "ready" | "exhausted" | "failed";
  error: string;
  onLoadMore: () => void;
  onRetry: () => void;
}): ReactNode {
  if (status === "failed") {
    return (
      <div className="flex items-center gap-2">
        {/* Said in words at full contrast, not in a hue. This sits in the
            header at 12px, where --portal-danger is about 4.0:1 on the page
            ground in light mode -- under the floor, and the sentence is
            unambiguous without any colour at all. */}
        <span className="text-xs text-fg">
          {count > 0 ? `Paging stopped after ${count}` : "Could not read rows"}: {error}
        </span>
        <MetaButton onClick={onRetry}>Try again</MetaButton>
      </div>
    );
  }
  if (status === "loading" || status === "idle") {
    return <span className="text-xs text-subtle">Loading… ({count} so far)</span>;
  }
  if (status === "ready") {
    return (
      <div className="flex items-center gap-2">
        <span className="text-xs text-subtle">{count} loaded, more available</span>
        <MetaButton onClick={onLoadMore}>Load more</MetaButton>
      </div>
    );
  }
  return (
    <span className="text-xs text-subtle">
      {count === 0 ? "Nothing here yet" : `All ${count} loaded`}
    </span>
  );
}

export function MetaButton({
  onClick,
  disabled = false,
  tone = "quiet",
  children,
}: {
  onClick: () => void;
  disabled?: boolean;
  // "quiet" for navigation-ish actions, "danger" for the one that cannot be
  // undone. There is deliberately no "primary": on these screens the most
  // prominent thing should be the data, not a button.
  tone?: "quiet" | "danger";
  children: ReactNode;
}): ReactNode {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className={
        "rounded border px-2.5 py-1 text-xs font-medium disabled:cursor-not-allowed disabled:opacity-40 " +
        (tone === "danger"
          ? "border-danger bg-danger-subtle text-fg hover:bg-danger hover:text-accent-fg"
          : "border-line bg-surface text-fg hover:bg-raised")
      }
    >
      {children}
    </button>
  );
}

// RowAside is the selected row, opened beside the view.
//
// Deliberately the SAME component the concept browser uses: a row's full
// nested shape is the one thing a designed view has no business restyling,
// and an operator who drills into a row from a view should see exactly what
// they would see drilling in from the browser.
export function RowAside({
  view,
  conceptId,
  rowId,
}: {
  view: ViewDefinition;
  conceptId: string;
  rowId: string;
}): ReactNode {
  const { row, loading, error, missing } = useRowDetail(conceptId, rowId);

  return (
    <aside className="min-w-0 shrink-0 xl:sticky xl:top-0 xl:w-[26rem]">
      <div className="flex items-baseline justify-between gap-2 pb-2">
        <h2 className="text-sm font-semibold">Row detail</h2>
        <Link
          to={viewPath(view.id)}
          className="text-xs text-muted hover:text-fg hover:underline"
        >
          Close
        </Link>
      </div>
      <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3">
        <p className="mb-2 font-mono text-xs break-all text-subtle">{rowId}</p>
        {error ? (
          <ErrorMessage>Failed to read the row: {error}</ErrorMessage>
        ) : loading ? (
          <Loading what="the row" />
        ) : missing ? (
          <Empty>
            This cluster has no row with that id. It may have been deleted, or the
            link may name a row from another cluster.
          </Empty>
        ) : row ? (
          <RowDetail row={row} />
        ) : null}
      </div>
    </aside>
  );
}
