// The concept page: a concept's rows beside the selected row, on the kit.
//
//   space                                  214 rows
//   +-----------------------+  +-------------------------------+
//   | Welcome space         |  | { id: "8f2...", name: ... }   |
//   | Design review    <-   |  |                               |
//   | ...                   |  |                               |
//   | Load more             |  |                               |
//   +-----------------------+  +-------------------------------+
//
// LOADING, EMPTY, FAILED AND DISCONNECTED LOOK DIFFERENT. The page drew "No
// rows for space." and "0 loaded" before its first read had answered, and on
// every reconnect; now the list is the shape of the content until the first
// page settles, and "no rows" is said only after a read that found none. With
// no live session there is one sentence and the act that fixes it -- not an
// error banner, a live-updates warning and an empty list saying the same
// thing three times.
//
// LIVE BY DEFAULT, SAID ONLY WHEN IT IS NOT. Rows follow the cluster's changes
// on their own, so Reload is offered only while live updates are off, and the
// head's meta says so.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { renderToHtml, renderValueView, type ConceptLike } from "@znasllc-io/memql-view-kit";

import type { ClusterViewState } from "../state/clusterViewState.js";
import { briefMessage } from "../state/diagnostics.js";
import { button, emptyState, head, notice, skeleton, type Act } from "./ui/kit.js";
import type { RegionParts } from "./ui/liveView.js";
import { ROW_LIST_STYLES, rowListHtml } from "./rowListView.js";

/** The messages the page posts. */
export const CONCEPT_ACTS = {
  selectRow: "selectRow",
  loadMore: "loadMore",
  reload: "reload",
  retryRow: "retryRow",
  selectCluster: "selectCluster",
  signIn: "signIn",
  reconnect: "reconnect",
  editCluster: "editCluster",
} as const;

export type ConceptDetail =
  | { state: "none" }
  | { state: "loading" }
  | { state: "found"; value: unknown }
  | { state: "missing" }
  | { state: "failed"; message: string };

export interface ConceptPageInput {
  concept: ConceptLike;
  /** What the connection lets this page show. */
  connection: ClusterViewState;
  /** The loaded rows, projected for the list. */
  rows: readonly Record<string, unknown>[];
  /** The current generation's first page has answered. */
  settled: boolean;
  /** A page read is running ("Load more" shows it). */
  loading: boolean;
  /** The last page read's failure, or "". */
  listError: string;
  /** Another page can be loaded. */
  more: boolean;
  selectedRowId?: string;
  detail: ConceptDetail;
  /** Why live updates are off, or "" while they are on. */
  liveOff: string;
}

/** The count, only once a read has answered: unavailable is not zero. */
function countText(input: ConceptPageInput): string {
  if (!input.settled || input.rows.length === 0) return "";
  const n = input.rows.length;
  return `${n}${input.more ? "+" : ""} row${n === 1 && !input.more ? "" : "s"}`;
}

/** One sentence for a page that has no live session to read from, and its act. */
function disconnectedHtml(connection: ClusterViewState): string {
  switch (connection) {
    case "signIn":
      return emptyState({
        line: "Sign in to see these rows.",
        acts: [{ act: CONCEPT_ACTS.signIn, label: "Sign in", tone: "secondary" }],
      });
    case "unreachable":
      return emptyState({
        line: "The cluster isn't answering.",
        acts: [{ act: CONCEPT_ACTS.reconnect, label: "Retry", tone: "secondary" }],
      });
    case "notConfigured":
      return emptyState({
        line: "This cluster has no address.",
        acts: [{ act: CONCEPT_ACTS.editCluster, label: "Edit cluster", tone: "secondary" }],
      });
    default:
      return emptyState({
        line: "Not connected.",
        acts: [{ act: CONCEPT_ACTS.selectCluster, label: "Connect to a cluster", tone: "secondary" }],
      });
  }
}

function listHtml(input: ConceptPageInput): string {
  if (!input.settled) return skeleton({ shape: "list", rows: 6, label: `Loading ${input.concept.entity} rows` });
  if (input.rows.length === 0) {
    if (input.listError !== "") {
      return notice({
        tone: "error",
        line: "Couldn't load rows.",
        next: briefMessage(input.listError, 120),
        acts: [{ act: CONCEPT_ACTS.reload, label: "Try again" }],
      });
    }
    return emptyState({ line: "No rows yet." });
  }
  const list =
    rowListHtml({
      rows: input.rows,
      concept: input.concept,
      selectedRowId: input.selectedRowId,
      act: CONCEPT_ACTS.selectRow,
      label: `Rows of ${input.concept.entity}`,
    }) ?? "";
  // Load more sits at the END of the list, where the reader runs out of rows.
  let tail = "";
  if (input.listError !== "") {
    tail = notice({ tone: "error", line: "Couldn't load more rows.", acts: [{ act: CONCEPT_ACTS.loadMore, label: "Try again" }] });
  } else if (input.more) {
    const act: Act = { act: CONCEPT_ACTS.loadMore, label: "Load more", busy: input.loading };
    tail = `<div class="concept-more">${button(act)}</div>`;
  }
  return list + tail;
}

function detailHtml(input: ConceptPageInput): string {
  switch (input.detail.state) {
    case "none":
      // Said only once there are rows to select.
      return input.settled && input.rows.length > 0
        ? `<p class="mq-empty-line concept-hint">Select a row to see all of it.</p>`
        : "";
    case "loading":
      return skeleton({ shape: "facts", rows: 5, label: "Loading row" });
    case "missing":
      return emptyState({ line: "This row no longer exists." });
    case "failed":
      return notice({
        tone: "error",
        line: "Couldn't load this row.",
        next: briefMessage(input.detail.message, 120),
        acts: [{ act: CONCEPT_ACTS.retryRow, label: "Try again" }],
      });
    case "found":
      return renderToHtml(renderValueView(input.detail.value));
  }
}

/** The page, whole. It has no action bar: browsing rows changes nothing. */
export function conceptPageParts(input: ConceptPageInput): RegionParts {
  const connected = input.connection === "connected";
  const live = input.liveOff === "";
  const meta = [countText(input), connected && !live ? "Not live" : ""].filter((part) => part !== "").join(" · ");
  const asideActs: Act[] =
    connected && !live && input.settled
      ? [{ act: CONCEPT_ACTS.reload, label: "Reload", title: `Live updates are off: ${briefMessage(input.liveOff, 100)}` }]
      : [];
  const headHtml = head({ title: input.concept.entity, meta, asideActs });

  if (input.connection === "connecting") {
    return { head: headHtml, body: skeleton({ shape: "list", rows: 6, label: "Connecting" }), actions: "" };
  }
  if (!connected) return { head: headHtml, body: disconnectedHtml(input.connection), actions: "" };

  // One column until there is something to look at in a second one.
  if (input.settled && input.rows.length === 0) {
    return { head: headHtml, body: listHtml(input), actions: "" };
  }
  const body =
    `<div class="concept-panes">` +
    `<section class="concept-pane" aria-label="Rows">${listHtml(input)}</section>` +
    `<section class="concept-pane concept-detail" aria-label="Selected row">${detailHtml(input)}</section>` +
    `</div>`;
  return { head: headHtml, body, actions: "" };
}

/** Panel-local layout: two panes that each scroll, stacked when narrow. */
export const CONCEPT_PAGE_STYLES = `${ROW_LIST_STYLES}
  .concept-panes { display: grid; grid-template-columns: minmax(260px, 42%) minmax(0, 1fr);
                   column-gap: 24px; align-items: start; }
  .concept-pane { min-width: 0; max-height: calc(100vh - 120px); overflow: auto; }
  .concept-more { padding: 6px 0 2px; }
  .concept-hint { margin: 4px 0; }
  @media (max-width: 640px) {
    .concept-panes { grid-template-columns: minmax(0, 1fr); row-gap: 20px; }
    .concept-pane { max-height: none; overflow: visible; }
  }
`;
