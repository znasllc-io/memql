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
// The persistent notice shown whenever there is no connection to carry CDC
// events. Deliberately distinct from NOT_CONNECTED_MESSAGE, which is the
// transient data-fetch error a later successful query clears: this one answers
// "are live updates on?", and it must survive that success exactly as the
// subscribe-failure notice does.
const LIVE_UPDATES_OFFLINE_MESSAGE = "live updates unavailable: not connected";

function titleFor(concept: Concept): string {
  return `Concept: ${concept.entity}`;
}

export class ConceptPanel {
  private static readonly open_ = new Map<string, ConceptPanel>();

  private readonly panel: vscode.WebviewPanel;
  private readonly state = new ConceptPanelState<Row>();
  private readonly disposeConnectionListener: () => void;
  // The map key this panel registered itself under. Captured at construction
  // rather than read off `this.concept` at teardown: adopt() swaps in a
  // fresher descriptor, and a panel that removed itself under a different key
  // would leave a dead entry in open_ forever, making that concept impossible
  // to reopen for the rest of the session.
  private readonly conceptId: string;
  private concept: Concept;
  // Unregister for the live-refresh CDC subscription (see
  // subscribeToChanges()). undefined whenever no subscription is live --
  // not yet established, torn down for a reconnect, or never started
  // because the connection has no SubscriptionManager.
  private unsubscribeChanges: (() => void) | undefined;

  // This panel's OWN disposables, disposed together when the tab closes.
  //
  // Registering them straight onto context.subscriptions (as B1 did) leaks:
  // that array is disposed exactly once, at extension deactivation, so every
  // listener from every concept tab the operator ever opened accumulated there
  // for the life of the window, each still holding a reference to a
  // long-disposed panel. Unbounded across a session, bounded only by the
  // concept count.
  private readonly disposables: vscode.Disposable[] = [];
  // The single entry this panel puts on context.subscriptions, so a tab still
  // open at deactivation is torn down properly. Spliced back out when the tab
  // closes on its own -- that removal is what keeps the extension-lifetime
  // array from growing one entry per tab ever opened.
  private readonly contextSubscriptions: { dispose(): unknown }[];
  private readonly contextEntry: vscode.Disposable;
  private disposed = false;

  static open(
    context: vscode.ExtensionContext,
    connections: ConnectionManager,
    concept: Concept,
  ): void {
    const existing = ConceptPanel.open_.get(concept.id);
    if (existing !== undefined) {
      existing.adopt(concept);
      existing.panel.reveal();
      return;
    }
    const panel = new ConceptPanel(context, connections, concept);
    ConceptPanel.open_.set(concept.id, panel);
  }

  // adopt takes a fresher descriptor for the concept this panel is already
  // showing -- the Concepts tree hands one over when a refresh picks up a
  // changed displayCard or entity name.
  //
  // The TAB LABEL is updated alongside the content. Re-rendering against the
  // new descriptor while leaving the title from the old one showed a renamed
  // entity's new name inside the panel and its old name on the tab, and with
  // several concepts open the tab label is what an operator actually
  // navigates by. Both happen BEFORE the caller's reveal(), so nothing stale
  // is ever brought to the front.
  private adopt(concept: Concept): void {
    this.concept = concept;
    this.panel.title = titleFor(concept);
    this.render();
  }

  private constructor(
    context: vscode.ExtensionContext,
    private readonly connections: ConnectionManager,
    concept: Concept,
  ) {
    this.concept = concept;
    this.conceptId = concept.id;
    this.contextSubscriptions = context.subscriptions;
    this.panel = vscode.window.createWebviewPanel(
      "memqlConcept",
      titleFor(concept),
      vscode.ViewColumn.Active,
      // No retainContextWhenHidden. It keeps the hidden tab's whole webview
      // process alive to preserve DOM state, and there is no DOM state here
      // worth preserving: every render() replaces webview.html wholesale, so a
      // revealed tab is repainted from ConceptPanelState (which lives in the
      // extension host and survives regardless) rather than resumed. All it
      // would buy is scroll position -- which the wholesale re-render already
      // discards on any reload -- at the cost of a retained process per
      // background concept tab.
      { enableScripts: true },
    );

    // One entry on context.subscriptions per panel, so a tab still open when
    // the extension deactivates is torn down rather than left holding a
    // connection listener. Everything else this panel registers goes into its
    // own bag (this.disposables) and is disposed with the tab.
    this.contextEntry = new vscode.Disposable(() => this.dispose(false));
    context.subscriptions.push(this.contextEntry);

    // The reconnect staleness guard: a cluster switch (or a reconnect to the
    // SAME cluster) must not let a response already in flight against the
    // OLD connection land on this panel. reset() invalidates both of
    // ConceptPanelState's Latest guards, so loadPage()/selectRow()
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
    // the new one. Re-establish it only once the new state is "connected".
    //
    // "connecting" / "error" / "disconnected" therefore leave this panel with
    // NO subscription until the next successful connect, and the else branch
    // is what says so. Without it there was a window -- every reconnect passes
    // through "connecting", and a dropped socket parks in "error" -- in which
    // live updates were off and nothing on screen mentioned it: precisely the
    // "looks live, silently isn't" failure mode the notice exists to prevent.
    // subscribeToChanges() clears it again the moment a subscription is
    // actually up.
    //
    // subscribeToChanges() (and this else branch) run BEFORE reset()/render():
    // both only write the liveUpdatesDegradedMessage field (see
    // state/conceptPanelState.ts), which reset() does not touch, so a single
    // render() below picks up both the fresh (empty) row state AND whatever
    // just happened to the live-updates notice, in one paint -- no separate
    // render() inside subscribeToChanges() itself.
    this.disposeConnectionListener = this.connections.onDidChangeState((connState) => {
      this.unsubscribeChanges?.();
      this.unsubscribeChanges = undefined;
      if (connState.status === "connected") {
        this.subscribeToChanges();
      } else {
        this.state.setLiveUpdatesDegraded(LIVE_UPDATES_OFFLINE_MESSAGE);
      }
      this.state.reset();
      this.render();
      void this.loadPage();
    });

    this.disposables.push(
      this.panel.onDidDispose(() => this.dispose(true)),
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
      ),
    );

    this.subscribeToChanges();
    this.render();
    void this.loadPage();
  }

  // dispose tears the panel down exactly once, from either direction: the user
  // closed the tab (fromPanel === true) or the extension is deactivating and
  // context.subscriptions is being drained (fromPanel === false).
  //
  // fromPanel is what decides whether to splice this panel's entry out of
  // context.subscriptions. Removing it is the point of the whole arrangement
  // when the tab closes on its own -- but during deactivation VS Code is
  // ITERATING that same array to dispose it, and mutating it mid-iteration
  // would skip whatever entry slid into the vacated index. So the removal
  // happens only on the path that is not already inside that walk.
  private dispose(fromPanel: boolean): void {
    if (this.disposed) return;
    this.disposed = true;

    if (fromPanel) {
      const at = this.contextSubscriptions.indexOf(this.contextEntry);
      if (at >= 0) this.contextSubscriptions.splice(at, 1);
    }

    this.unsubscribeChanges?.();
    this.unsubscribeChanges = undefined;
    this.disposeConnectionListener();
    for (const d of this.disposables.splice(0)) d.dispose();
    ConceptPanel.open_.delete(this.conceptId);
    // A no-op when the panel is what triggered this; the call matters on the
    // deactivation path, where the tab is still open and nothing else closes
    // it. The `disposed` flag above absorbs the onDidDispose that comes back.
    this.panel.dispose();
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
  // it invalidates both Latest guards, so any page or detail fetch already
  // in flight when the event arrives is discarded instead of landing on top
  // of the reload triggered here.
  //
  // Does not call render() itself -- both call sites (the constructor, the
  // reconnect listener) already render immediately afterward, so a render()
  // here would just be a wasted extra paint on every panel open where the
  // subscribe attempt fails.
  private subscribeToChanges(): void {
    const subs = this.connections.subscriptions;
    if (subs === undefined) {
      // No SubscriptionManager means no live updates, and the panel must say
      // so rather than look live. This is the constructor's case when a
      // concept tab is opened while disconnected -- the reconnect listener
      // has its own else branch for the same condition, and both must set the
      // notice or the panel is silently static.
      this.state.setLiveUpdatesDegraded(LIVE_UPDATES_OFFLINE_MESSAGE);
      return;
    }
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
