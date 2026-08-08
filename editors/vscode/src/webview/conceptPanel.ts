// The concept browser tab: row list, keyset paging, and row detail.
//
// All rendering is delegated to view-kit, which emits an HTML string and knows
// nothing about VS Code. That is deliberate -- the same renderer serves the
// portal, so any VS Code-specific markup here would be markup the portal has
// to rebuild.
//
// The webview runs under a strict CSP with a per-load nonce: row data is
// untrusted, and view-kit escapes it, but a CSP means an escaping bug cannot
// become script execution. The postMessage boundary is untrusted too --
// webview content is HTML/JS this file's own render() emits, but nothing
// stops a malformed or malicious message from arriving on that channel, so
// the handler below validates its shape at runtime rather than trusting the
// compile-time `msg` type.
//
// Staleness + concurrency: loadPage() and selectRow() (row-detail fetch)
// each race an async round-trip against later events -- a Reload click, a
// faster second click on a different row, the connection switching
// clusters underneath the panel, a live CDC event on this concept (see
// subscribeToChanges() below), or (for loadPage specifically) a second
// "Load more" click before the first response lands. All are guarded by
// ConceptPanelState (see state/conceptPanelState.ts -- Latest guards for
// supersession, a separate in-flight marker for loadPage's concurrency
// case); this file's job is only to wire the connection lifecycle, the CDC
// subscription lifecycle, and the webview's HTML/postMessage boundary
// around it.

import * as vscode from "vscode";
import { randomBytes } from "node:crypto";

import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";
import { browseConceptPage, getRowByConceptAndId } from "@znasllc-io/memql-sdk-core/client";
import {
  renderDetail,
  renderRowList,
  renderToHtml,
  escapeHtml,
  viewKitStyles,
} from "@znasllc-io/memql-view-kit";

import type { ConnectionManager } from "../connection/manager.js";
import { ConceptPanelState } from "../state/conceptPanelState.js";
import { flattenForList } from "../state/rowProjection.js";

const PAGE_SIZE = 200;
const NOT_CONNECTED_MESSAGE = "Not connected. Select a cluster in the Clusters view.";

export class ConceptPanel {
  private static readonly open_ = new Map<string, ConceptPanel>();

  private readonly panel: vscode.WebviewPanel;
  private readonly state = new ConceptPanelState<Row>();
  private readonly disposeConnectionListener: () => void;
  private concept: Concept;
  // Unregister for the live-refresh CDC subscription (see
  // subscribeToChanges()). undefined whenever no subscription is live --
  // not yet established, torn down for a reconnect, or never started
  // because the connection has no SubscriptionManager.
  private unsubscribeChanges: (() => void) | undefined;

  static open(
    context: vscode.ExtensionContext,
    connections: ConnectionManager,
    concept: Concept,
  ): void {
    const existing = ConceptPanel.open_.get(concept.id);
    if (existing !== undefined) {
      // The Concepts tree can hand us a fresher descriptor for an
      // already-open tab (e.g. a refresh picked up a changed displayCard).
      // Adopt it and re-render BEFORE reveal(), or the tab would keep
      // rendering against the stale concept it was constructed with.
      existing.concept = concept;
      existing.render();
      existing.panel.reveal();
      return;
    }
    const panel = new ConceptPanel(context, connections, concept);
    ConceptPanel.open_.set(concept.id, panel);
  }

  private constructor(
    context: vscode.ExtensionContext,
    private readonly connections: ConnectionManager,
    concept: Concept,
  ) {
    this.concept = concept;
    this.panel = vscode.window.createWebviewPanel(
      "memqlConcept",
      `Concept: ${concept.entity}`,
      vscode.ViewColumn.Active,
      { enableScripts: true, retainContextWhenHidden: true },
    );

    // The reconnect staleness guard: a cluster switch (or a reconnect to the
    // SAME cluster) must not let a response already in flight against the
    // OLD connection land on this panel. reset() bumps both of
    // ConceptPanelState's generation counters, so loadPage()/selectRow()
    // calls started before this fire discard their result when they settle.
    // We then re-render the now-empty state and kick off a fresh load --
    // ConceptsTreeProvider follows the same invalidate-on-every-state-change
    // policy for the same reason (concepts.memql is per-cluster too).
    //
    // The CDC subscription is tied to the OLD connection's socket, so it
    // must be torn down on every state change too, not just "connected" ->
    // something else -- otherwise a cluster switch would leave a
    // subscription registered against a socket this panel no longer reads
    // from (a leak) while ALSO leaving the panel silently unsubscribed from
    // the new one. Re-establish it only once the new state is "connected";
    // "connecting" / "error" / "disconnected" leave it unsubscribed until
    // the next successful connect.
    //
    // subscribeToChanges() runs BEFORE reset()/render(): it only writes the
    // liveUpdatesDegradedMessage field (see state/conceptPanelState.ts), which
    // reset() does not touch, so a single render() below picks up both the
    // fresh (empty) row state AND whatever the subscribe attempt just did to
    // the live-updates notice, in one paint -- no separate render() inside
    // subscribeToChanges() itself.
    this.disposeConnectionListener = this.connections.onDidChangeState((connState) => {
      this.unsubscribeChanges?.();
      this.unsubscribeChanges = undefined;
      if (connState.status === "connected") {
        this.subscribeToChanges();
      }
      this.state.reset();
      this.render();
      void this.loadPage();
    });

    this.panel.onDidDispose(
      () => {
        this.unsubscribeChanges?.();
        this.unsubscribeChanges = undefined;
        ConceptPanel.open_.delete(concept.id);
        this.disposeConnectionListener();
      },
      null,
      context.subscriptions,
    );

    this.panel.webview.onDidReceiveMessage(
      // The webview posts plain JSON; treat it as untrusted input rather
      // than trusting the compile-time annotation. `msg.type` on a
      // null/non-object message would throw, and `rowId !== undefined`
      // would admit any non-undefined value (a number, an object) straight
      // into getRowByConceptAndId -- narrow both before use.
      (msg: unknown) => {
        if (msg === null || typeof msg !== "object") return;
        const { type, rowId } = msg as { type?: unknown; rowId?: unknown };
        if (type === "selectRow" && typeof rowId === "string") {
          void this.selectRow(rowId);
        } else if (type === "loadMore") {
          void this.loadPage();
        } else if (type === "reload") {
          this.state.reset();
          this.render();
          void this.loadPage();
        }
      },
      null,
      context.subscriptions,
    );

    this.subscribeToChanges();
    this.render();
    void this.loadPage();
  }

  // Live refresh. A CDC subscription on this concept means a row written by
  // anything -- another operator, an automation, or a mutation run from the
  // editor in a later increment -- appears without a manual reload. That is
  // the loop this panel exists to close.
  //
  // The whole page set is reloaded rather than patched: a CDC event carries
  // the change, not the row's position in this query's sort order, so
  // splicing it in would put rows in the wrong place. Reloading is correct
  // and a concept browser is not hot enough for the cost to matter.
  //
  // The reload goes through ConceptPanelState.reset(), the same
  // invalidation path Reload and a connection-state change already use --
  // it bumps both generation counters, so any page or detail fetch already
  // in flight when the event arrives is discarded instead of landing on top
  // of the reload triggered here.
  //
  // Does not call render() itself -- both call sites (the constructor, the
  // reconnect listener) already render immediately afterward, so a render()
  // here would just be a wasted extra paint on every panel open where the
  // subscribe attempt fails.
  private subscribeToChanges(): void {
    const subs = this.connections.subscriptions;
    if (subs === undefined) return;
    try {
      this.unsubscribeChanges = subs.subscribeGraph(
        () => {
          this.state.reset();
          this.render();
          void this.loadPage();
        },
        { concept: this.concept.id, actions: ["created", "updated", "deleted"] },
      );
      // A prior attempt's degraded notice (if any) no longer applies --
      // this subscription is live.
      this.state.clearLiveUpdatesDegraded();
    } catch (err) {
      // A subscription failure degrades to manual reload; it must never
      // take the panel down with it. This is a PERSISTENT notice, deliberately
      // not routed through setConnectionError/state.error: that field is
      // cleared by every successful loadPage()/resolveSelection(), and an
      // ordinary query succeeding on the same connection the subscribe just
      // failed on is the common case, not the exception -- routing through
      // it would flash this message away within moments of showing it,
      // leaving the panel looking fully live when it silently is not (the
      // exact failure mode this notice exists to prevent). It clears only
      // when a later subscribe attempt succeeds (see clearLiveUpdatesDegraded()
      // above).
      this.state.setLiveUpdatesDegraded(
        `live updates unavailable: ${err instanceof Error ? err.message : String(err)}`,
      );
    }
  }

  private async loadPage(): Promise<void> {
    const query = this.connections.query;
    if (query === undefined) {
      this.state.setConnectionError(NOT_CONNECTED_MESSAGE);
      this.render();
      return;
    }
    // Snapshot the cursor synchronously: it must reflect the page already
    // loaded at call time, not whatever loadPage()'s own await lets it
    // drift to. ConceptPanelState.loadPage() itself refuses to run a
    // second fetch concurrently against the same generation (a second
    // "Load more" click before this one resolves is dropped, not
    // double-fetched), so this snapshot is never read by two overlapping
    // fetches.
    const cursor = this.state.nextCursor;
    const changed = await this.state.loadPage(() =>
      browseConceptPage(query, this.concept.id, {
        pageSize: PAGE_SIZE,
        ...(cursor === "" ? {} : { cursor }),
      }),
    );
    // A false return means this settle lost the race (a concurrent
    // in-flight load, Reload, or a cluster switch) -- ConceptPanelState
    // already discarded it without writing state, so render() must not run
    // either, or a superseded/duplicate page would flash onto a panel that
    // has already moved on.
    if (changed) this.render();
  }

  private async selectRow(rowId: string): Promise<void> {
    const token = this.state.beginSelection(rowId);
    // Render immediately: beginSelection() already wrote the new
    // selectedRowId, so the clicked row highlights right away instead of
    // waiting on the detail round-trip (which may be slow, may fail, or may
    // end up superseded and never render again at all).
    this.render();
    const query = this.connections.query;
    if (query === undefined) {
      this.state.setConnectionError(NOT_CONNECTED_MESSAGE);
      this.render();
      return;
    }
    const changed = await this.state.resolveSelection(token, () =>
      getRowByConceptAndId(query, this.concept.id, rowId),
    );
    // Same discard-must-not-render rule as loadPage(): if the user clicked a
    // different row (or Reload, or the connection changed) while this
    // fetch was in flight, resolveSelection() already dropped it -- this
    // second render() must not paint this row's detail over the newer
    // selection's.
    if (changed) this.render();
  }

  private render(): void {
    const nonce = nonceValue();
    const listHtml = renderToHtml(
      renderRowList(
        this.state.nodes.map(flattenForList),
        this.concept,
        this.state.selectedRowId,
      ),
    );
    // "Select a row." (nothing chosen yet) and "Row not found." (a row was
    // chosen but getRowByConceptAndId came back null -- deleted between the
    // list load and the click, or a bug) are different situations; folding
    // them into the same "detail === null" placeholder made a genuine miss
    // indistinguishable from never having clicked anything.
    const detailHtml =
      this.state.selectedRowId === undefined
        ? '<div class="placeholder">Select a row.</div>'
        : this.state.detail === null
          ? this.state.error === ""
            ? '<div class="placeholder">Row not found.</div>'
            : '<div class="placeholder">Failed to load row.</div>'
          : renderToHtml(renderDetail(this.state.detail));

    const errorHtml =
      this.state.error === ""
        ? ""
        : `<div class="error">ERROR: ${escapeHtml(this.state.error)}</div>`;

    // Persistent, independent of errorHtml: a successful loadPage() clears
    // state.error but must NOT clear this -- see the doc comment on
    // ConceptPanelState.liveUpdatesDegradedMessage. It stays on screen until
    // a later reconnect's subscribe attempt actually succeeds.
    const liveUpdatesHtml =
      this.state.liveUpdatesError === ""
        ? ""
        : `<div class="warning">WARNING: ${escapeHtml(this.state.liveUpdatesError)}</div>`;

    const moreHtml =
      this.state.nextCursor === ""
        ? ""
        : '<button id="more" type="button">Load more</button>';

    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(this.concept.entity)}</title>
<style nonce="${nonce}">
  /* Theme view-kit by mapping ITS tokens onto VS Code's. This block is the
     entire coupling between the two: view-kit never names a --vscode-*
     variable, so the same renderer + stylesheet drop into the portal with a
     different six-line mapping and nothing else. */
  :root {
    --vk-fg: var(--vscode-foreground);
    --vk-muted-fg: var(--vscode-descriptionForeground);
    --vk-border: var(--vscode-panel-border);
    --vk-hover-bg: var(--vscode-list-hoverBackground);
    --vk-selected-bg: var(--vscode-list-activeSelectionBackground);
    --vk-selected-fg: var(--vscode-list-activeSelectionForeground);
  }

  /* view-kit's own stylesheet, shipped with the markup contract it styles.
     Hand-authoring rules for vk- classes here is what this replaces: those
     rules and view-kit's class names drifted the moment either side moved,
     and the portal would have had to re-derive every one of them. */
${viewKitStyles}

  /* Page chrome below -- the panel's, not view-kit's. view-kit renders rows;
     it has no view of this layout and must not style it. */
  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0; padding: 0; }
  .toolbar { padding: 8px 12px; border-bottom: 1px solid var(--vscode-panel-border);
             display: flex; gap: 12px; align-items: center; }
  .layout { display: grid; grid-template-columns: minmax(240px, 40%) 1fr; height: calc(100vh - 42px); }
  .pane { overflow: auto; padding: 8px 12px; }
  .pane + .pane { border-left: 1px solid var(--vscode-panel-border); }
  /* This panel's OWN empty-state text. Deliberately not view-kit's .vk-empty:
     that class means "this row set is empty" and belongs to the renderer that
     emits it. Borrowing it for "Select a row." would put a second author on a
     view-kit class -- the exact drift the shared stylesheet exists to stop. */
  .placeholder { color: var(--vscode-descriptionForeground); opacity: 0.6; padding: 8px 0; }
  .error { color: var(--vscode-errorForeground); padding: 8px 12px; }
  .warning { color: var(--vscode-editorWarning-foreground); padding: 8px 12px; }
  button { background: var(--vscode-button-background); color: var(--vscode-button-foreground);
           border: none; padding: 4px 10px; cursor: pointer; border-radius: 2px; }
</style>
</head>
<body>
<div class="toolbar">
  <strong>${escapeHtml(this.concept.id)}</strong>
  <span>${this.state.nodes.length} loaded</span>
  <button id="reload" type="button">Reload</button>
  ${moreHtml}
</div>
${liveUpdatesHtml}
${errorHtml}
<div class="layout">
  <div class="pane" id="rows">${listHtml}</div>
  <div class="pane" id="detail">${detailHtml}</div>
</div>
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  // One delegated listener: view-kit emits data attributes, never inline
  // handlers, which is what lets the CSP forbid them outright.
  document.getElementById('rows').addEventListener('click', (e) => {
    const row = e.target.closest('[data-row-id]');
    if (row) vscode.postMessage({ type: 'selectRow', rowId: row.dataset.rowId });
  });
  document.getElementById('reload').addEventListener('click', () =>
    vscode.postMessage({ type: 'reload' }));
  const more = document.getElementById('more');
  if (more) more.addEventListener('click', () => vscode.postMessage({ type: 'loadMore' }));
</script>
</body>
</html>`;
  }
}

// A CSP nonce is a security control, so it comes from a CSPRNG. Math.random()
// is not one -- its output is predictable from prior draws, which defeats the
// nonce's purpose. node:crypto is built in, so this costs no dependency.
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
