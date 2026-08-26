import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { RowDetail } from "../components/RowDetail";
import { Empty } from "../components/StatusMessage";
import { useRowDetail } from "../cluster/useConceptRows";
import {
  Band as UiBand,
  Button,
  ErrorNotice,
  PageHeader,
  Panel,
  PopulationMeta as UiPopulationMeta,
  Skeleton,
} from "../ui";
import type { ViewDefinition } from "./registry";
import { viewPath } from "./urls";

// THE LAYOUT GRAMMAR every predefined view is built out of.
//
// One shape, five views (see registry.ts for why): a header that names the
// population, then bands -- a reading, a shape, a roll. Since memql#4178 the
// PRIMITIVES live in src/ui (PageHeader, Band, Button, PopulationMeta) --
// one vocabulary for the whole portal -- and this module keeps only what is
// view-specific: the ViewProps contract, the frame that knows a view's
// eyebrow is its concept id, and the row aside. The five view BODIES import
// Band/MetaButton from here unchanged, which keeps the guarded views
// directory decoupled from ui's file layout.
//
// TYPOGRAPHY AND HIERARCHY live in src/ui/README.md now, stated once for
// every surface. The view-specific statement stands: the eyebrow is the
// concept id -- the one thing on the page that tells an operator which rows
// they are looking at, and the address they would paste into a query.
//
// COLOUR. The chrome here is greyscale plus the one accent. Colour appears
// in exactly two places on a view: series identity inside an element
// (view-kit's validated palette) and lifecycle severity on a status badge
// (src/styles/status.css). Anything else would spend the channel that
// carries "this deploy failed" on decoration.

// What every view body is handed.
//
// THE PRIMARY POPULATION COMES FROM ABOVE, and that is load-bearing rather
// than tidy: ViewPage owns the keyset walk so the frame's header can state
// how much of it is loaded, and so opening a row re-renders the body without
// restarting paging. Same reason ConceptPage owns the browser's walk. A view
// that needs a SECOND population (Users' sessions, Agents' grants) fetches
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
  // Right-aligned, prominent: what an operator can DO here -- rendered as a
  // ROW by PageHeader. Absent when the caller cannot do anything.
  actions?: ReactNode;
  aside?: ReactNode;
  children: ReactNode;
}): ReactNode {
  return (
    <section className="flex min-h-full flex-col gap-6 pb-8 xl:flex-row xl:items-start xl:gap-8">
      <div className="flex min-w-0 flex-1 flex-col gap-6">
        <PageHeader
          eyebrow={conceptId}
          title={view.title}
          blurb={view.blurb}
          {...(actions === undefined ? {} : { actions })}
          {...(meta === undefined ? {} : { meta })}
        />

        {children}
      </div>

      {aside}
    </section>
  );
}

// Band and PopulationMeta ARE the ui primitives; re-exported so the guarded
// view bodies keep their import path.
export const Band = UiBand;
export const PopulationMeta = UiPopulationMeta;

// MetaButton is ui/Button at the meta size, preserved as a named idiom
// because "the small header action" is a real role the five views share.
// There is deliberately no "primary" here: on these screens the most
// prominent thing should be the data, not a button.
export function MetaButton({
  onClick,
  disabled = false,
  tone = "quiet",
  children,
}: {
  onClick: () => void;
  disabled?: boolean;
  tone?: "quiet" | "danger";
  children: ReactNode;
}): ReactNode {
  return (
    <Button size="xs" tone={tone} onClick={onClick} disabled={disabled}>
      {children}
    </Button>
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
      <Panel>
        <p className="mb-2 font-mono text-xs break-all text-subtle">{rowId}</p>
        {error ? (
          <ErrorNotice sentence="Could not read this row." detail={error} />
        ) : loading ? (
          <Skeleton variant="kv" rows={4} />
        ) : missing ? (
          <Empty>
            This cluster has no row with that id. It may have been deleted, or the
            link may name a row from another cluster.
          </Empty>
        ) : row ? (
          <RowDetail row={row} />
        ) : null}
      </Panel>
    </aside>
  );
}
