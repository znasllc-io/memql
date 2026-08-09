import { useCallback, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";

import { useRowDetail } from "../cluster/useConceptRows";
import { RowDetail } from "../components/RowDetail";
import { RowList } from "../components/RowList";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { liveBandIsEmpty } from "../concepts/liveBand";
import { CURSOR_LOOP_ERROR } from "../concepts/rowWalk";
import { conceptPath, conceptRowPath } from "../concepts/urls";
import { useConceptPane } from "./conceptContext";

// A concept's rows: the paged window, the live band above it, and the
// selected row's detail beside it.
//
// Every row is a LINK. `/concepts/<id>/rows/<rowId>` is the address of that
// row, so an operator can paste one into a ticket and the person who opens it
// lands on the same row -- which is the point of routing the selection rather
// than holding it in state.
//
// Nothing here knows the name of any concept. The list projects through
// view-kit's display-card resolution and the detail through its recursive
// walk; both are enforced concept-agnostic by portal_render_path_test.go in
// the repo root.

export function ConceptRowsPane(): ReactNode {
  const { concept, rows } = useConceptPane();
  const { rowId = "" } = useParams<{ rowId: string }>();
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const search = params.toString();

  const { walk, live, liveDegraded, loadMore, retry, reload } = rows;

  const select = useCallback(
    (id: string) => navigate(conceptRowPath(concept.id, id, search)),
    [navigate, concept.id, search],
  );

  // What the list area shows before there is a list.
  //
  //   "loading"  nothing fetched yet. "idle" counts: it is the machine having
  //              asked for a page that the fetch effect has not issued yet,
  //              and painting view-kit's "No rows for X" for that one frame
  //              is a flash of a false statement.
  //   "hidden"   the first page FAILED. The footer already says so; rendering
  //              "No rows for X" above it would be a second, contradictory
  //              answer to the same question.
  //   "list"     everything else, including the genuinely-empty concept --
  //              view-kit's empty state is the right answer there, and the
  //              footer says "This concept has no rows" underneath it.
  const listArea =
    walk.rows.length > 0
      ? "list"
      : walk.status === "failed"
        ? "hidden"
        : walk.status === "loading" || walk.status === "idle"
          ? "loading"
          : "list";

  return (
    <div className="flex min-h-0 flex-col gap-4 pb-6 xl:flex-row xl:items-start">
      <div className="min-w-0 flex-1">
        {liveDegraded ? (
          <p className="mb-3 rounded border border-warn bg-warn-subtle px-3 py-2 text-xs text-fg">
            Live updates are off for this pane: {liveDegraded}. Rows already loaded are
            still accurate; new ones will not appear until you reload.
          </p>
        ) : null}

        <LiveBand
          band={live}
          concept={concept}
          onSelect={select}
          onReload={reload}
          selectedRowId={rowId}
        />

        {listArea === "hidden" ? null : (
          <div className="overflow-hidden rounded-lg border border-line bg-surface">
            {listArea === "loading" ? (
              <div className="p-3">
                <Loading what="rows" />
              </div>
            ) : (
              <RowList
                rows={[...walk.rows]}
                concept={concept}
                {...(rowId ? { selectedRowId: rowId } : {})}
                onSelect={select}
              />
            )}
          </div>
        )}

        <WalkFooter
          walk={rows.walk}
          onLoadMore={loadMore}
          onRetry={retry}
          onReload={reload}
        />
      </div>

      {rowId ? (
        <aside className="min-w-0 shrink-0 xl:sticky xl:top-0 xl:w-[28rem]">
          <div className="flex items-baseline justify-between gap-2 pb-2">
            <h2 className="text-sm font-semibold">Row detail</h2>
            <Link
              to={conceptPath(concept.id, search)}
              className="text-xs text-muted hover:text-fg hover:underline"
            >
              Close
            </Link>
          </div>
          <RowDetailPane conceptId={concept.id} rowId={rowId} />
        </aside>
      ) : null}
    </div>
  );
}

// WalkFooter is the honest state of the walk. Every branch says something
// different, because "loading", "there is more", "that is everything" and
// "this broke part-way" are four different facts and collapsing any two makes
// a truncated list read as a complete one.
function WalkFooter({
  walk,
  onLoadMore,
  onRetry,
  onReload,
}: {
  walk: ReturnType<typeof useConceptPane>["rows"]["walk"];
  onLoadMore: () => void;
  onRetry: () => void;
  onReload: () => void;
}): ReactNode {
  const count = walk.rows.length;

  if (walk.status === "failed") {
    const looped = walk.error === CURSOR_LOOP_ERROR;
    return (
      <div className="mt-3 flex flex-col gap-2">
        <ErrorMessage>
          {count > 0
            ? `Paging stopped after ${count} rows: ${walk.error}`
            : `Could not read rows: ${walk.error}`}
        </ErrorMessage>
        <div className="flex gap-2">
          {/* A cursor loop is NOT resumable -- the cursor that caused it is
              the one a retry would re-send. Reload is the only honest exit,
              so it is the only button offered. */}
          {looped ? null : (
            <FooterButton onClick={onRetry}>Retry from where it stopped</FooterButton>
          )}
          <FooterButton onClick={onReload}>Reload from the first page</FooterButton>
        </div>
      </div>
    );
  }

  if (walk.status === "loading" && count > 0) {
    return <p className="mt-3 text-xs text-muted">Loading more rows… ({count} so far)</p>;
  }

  if (walk.status === "ready") {
    return (
      <div className="mt-3 flex items-center gap-3">
        <FooterButton onClick={onLoadMore}>Load more</FooterButton>
        <span className="text-xs text-subtle">{count} rows loaded, more available</span>
      </div>
    );
  }

  if (walk.status === "exhausted") {
    return (
      <p className="mt-3 text-xs text-subtle">
        {count === 0
          ? "This concept has no rows."
          : `All ${count} rows loaded — that is the whole concept.`}
      </p>
    );
  }

  return null;
}

function FooterButton({
  onClick,
  children,
}: {
  onClick: () => void;
  children: ReactNode;
}): ReactNode {
  return (
    <button
      type="button"
      onClick={onClick}
      className="rounded border border-line bg-surface px-3 py-1.5 text-xs font-medium text-fg hover:bg-raised"
    >
      {children}
    </button>
  );
}

// LiveBand renders the CDC arrivals. See src/concepts/liveBand.ts for WHY
// they are a band rather than being spliced into the paged list -- in short,
// the keyset cursor orders by createdAt ascending, so a row created now is a
// row the walk has not reached yet, and inserting it guarantees a duplicate
// when paging catches up.
function LiveBand({
  band,
  concept,
  onSelect,
  onReload,
  selectedRowId,
}: {
  band: ReturnType<typeof useConceptPane>["rows"]["live"];
  concept: ReturnType<typeof useConceptPane>["concept"];
  onSelect: (rowId: string) => void;
  onReload: () => void;
  selectedRowId: string;
}): ReactNode {
  if (liveBandIsEmpty(band)) return null;

  return (
    <div className="mb-3 overflow-hidden rounded-lg border border-accent bg-accent-subtle/40">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-accent/40 px-3 py-1.5">
        <span className="text-xs font-medium text-fg">
          New since you opened this
          {band.created.length > 0 ? ` — ${band.created.length}` : ""}
          {band.changedIds.length > 0
            ? `, ${band.changedIds.length} existing ${
                band.changedIds.length === 1 ? "row" : "rows"
              } changed`
            : ""}
        </span>
        <button
          type="button"
          onClick={onReload}
          className="rounded border border-line bg-surface px-2 py-0.5 text-xs text-fg hover:bg-raised"
        >
          Reload the list
        </button>
      </div>
      {band.created.length > 0 ? (
        <RowList
          rows={[...band.created]}
          concept={concept}
          {...(selectedRowId ? { selectedRowId } : {})}
          onSelect={onSelect}
        />
      ) : null}
    </div>
  );
}

function RowDetailPane({
  conceptId,
  rowId,
}: {
  conceptId: string;
  rowId: string;
}): ReactNode {
  const { row, loading, error, missing } = useRowDetail(conceptId, rowId);

  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3">
      <p className="mb-2 font-mono text-xs break-all text-subtle">{rowId}</p>
      {error ? (
        <ErrorMessage>Failed to read the row: {error}</ErrorMessage>
      ) : loading ? (
        <Loading what="the row" />
      ) : missing ? (
        <Empty>
          The cluster has no row with that id under this concept. It may have been
          deleted, or the link may name a row from another cluster.
        </Empty>
      ) : row ? (
        <RowDetail row={row} />
      ) : null}
    </div>
  );
}
