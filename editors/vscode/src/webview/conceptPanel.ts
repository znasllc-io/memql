// The concept browser tab: row list, keyset paging, and row detail.
//
// All rendering is delegated to view-kit, which emits an HTML string and knows
// nothing about VS Code. That is deliberate -- the same renderer serves the
// portal, so any VS Code-specific markup here would be markup the portal has
// to rebuild.
//
// The webview runs under a strict CSP with a per-load nonce: row data is
// untrusted, and view-kit escapes it, but a CSP means an escaping bug cannot
// become script execution.
//
// Staleness: loadPage() and selectRow() (row-detail fetch) each race an
// async round-trip against later events -- a Reload click, a faster second
// click, or the connection switching clusters underneath the panel. All
// three are guarded by ConceptPanelState's generation counters (see
// conceptPanelState.ts); this file's job is only to wire the connection
// lifecycle and the webview's HTML/postMessage boundary around it.

import * as vscode from "vscode";
import { randomBytes } from "node:crypto";

import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";
import { browseConceptPage, getRowByConceptAndId } from "@znasllc-io/memql-sdk-core/client";
import { renderDetail, renderRowList, renderToHtml, escapeHtml } from "@znasllc-io/memql-view-kit";

import type { ConnectionManager } from "../connection/manager.js";
import { ConceptPanelState } from "./conceptPanelState.js";

const PAGE_SIZE = 200;
const NOT_CONNECTED_MESSAGE = "Not connected. Select a cluster in the Clusters view.";

// flattenForList projects a wire node into the flat shape a display card names
// its fields on. The wire keeps payload nested; the display card names payload
// fields directly, so the list needs the merge. Detail rendering deliberately
// does NOT flatten -- it shows the nesting.
function flattenForList(node: Row): Row {
  const out: Row = {};
  for (const [k, v] of Object.entries(node)) {
    if (k === "payload" && v !== null && typeof v === "object" && !Array.isArray(v)) {
      for (const [pk, pv] of Object.entries(v as Record<string, unknown>)) {
        out[pk] = pv;
      }
      continue;
    }
    out[k] = v;
  }
  return out;
}

export class ConceptPanel {
  private static readonly open_ = new Map<string, ConceptPanel>();

  private readonly panel: vscode.WebviewPanel;
  private readonly state = new ConceptPanelState<Row>();
  private readonly disposeConnectionListener: () => void;

  static open(
    context: vscode.ExtensionContext,
    connections: ConnectionManager,
    concept: Concept,
  ): void {
    const existing = ConceptPanel.open_.get(concept.id);
    if (existing !== undefined) {
      existing.panel.reveal();
      return;
    }
    const panel = new ConceptPanel(context, connections, concept);
    ConceptPanel.open_.set(concept.id, panel);
  }

  private constructor(
    context: vscode.ExtensionContext,
    private readonly connections: ConnectionManager,
    private readonly concept: Concept,
  ) {
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
    this.disposeConnectionListener = this.connections.onDidChangeState(() => {
      this.state.reset();
      this.render();
      void this.loadPage();
    });

    this.panel.onDidDispose(
      () => {
        ConceptPanel.open_.delete(concept.id);
        this.disposeConnectionListener();
      },
      null,
      context.subscriptions,
    );

    this.panel.webview.onDidReceiveMessage(
      (msg: { type: string; rowId?: string }) => {
        if (msg.type === "selectRow" && msg.rowId !== undefined) {
          void this.selectRow(msg.rowId);
        } else if (msg.type === "loadMore") {
          void this.loadPage();
        } else if (msg.type === "reload") {
          this.state.reset();
          this.render();
          void this.loadPage();
        }
      },
      null,
      context.subscriptions,
    );

    this.render();
    void this.loadPage();
  }

  private async loadPage(): Promise<void> {
    const query = this.connections.query;
    if (query === undefined) {
      this.state.setConnectionError(NOT_CONNECTED_MESSAGE);
      this.render();
      return;
    }
    // Snapshot the cursor synchronously: it must reflect the page already
    // loaded at call time, not whatever loadPage()'s own await lets it drift
    // to (loadPage never runs two fetches concurrently against the same
    // cursor, but reading it inside the closure at call time -- rather than
    // whenever the closure happens to run -- keeps that invariant explicit).
    const cursor = this.state.nextCursor;
    const changed = await this.state.loadPage(() =>
      browseConceptPage(query, this.concept.id, {
        pageSize: PAGE_SIZE,
        ...(cursor === "" ? {} : { cursor }),
      }),
    );
    // A false return means this settle lost the race (Reload or a cluster
    // switch ran first) -- ConceptPanelState already discarded it without
    // writing state, so render() must not run either, or a superseded page
    // would flash onto a panel that has already moved on.
    if (changed) this.render();
  }

  private async selectRow(rowId: string): Promise<void> {
    const token = this.state.beginSelection(rowId);
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
    // fetch was in flight, resolveSelection() already dropped it -- render()
    // must not paint this row's detail over the newer selection's.
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
    const detailHtml =
      this.state.detail === null
        ? '<div class="vk-empty">Select a row.</div>'
        : renderToHtml(renderDetail(this.state.detail));

    const errorHtml =
      this.state.error === ""
        ? ""
        : `<div class="error">ERROR: ${escapeHtml(this.state.error)}</div>`;

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
  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0; padding: 0; }
  .toolbar { padding: 8px 12px; border-bottom: 1px solid var(--vscode-panel-border);
             display: flex; gap: 12px; align-items: center; }
  .layout { display: grid; grid-template-columns: minmax(240px, 40%) 1fr; height: calc(100vh - 42px); }
  .pane { overflow: auto; padding: 8px 12px; }
  .pane + .pane { border-left: 1px solid var(--vscode-panel-border); }
  .vk-rows { list-style: none; margin: 0; padding: 0; }
  .vk-row { padding: 4px 6px; cursor: pointer; border-radius: 3px;
            display: flex; gap: 8px; align-items: baseline; }
  .vk-row:hover { background: var(--vscode-list-hoverBackground); }
  .vk-row[data-selected="true"] { background: var(--vscode-list-activeSelectionBackground);
                                  color: var(--vscode-list-activeSelectionForeground); }
  .vk-row-secondary, .vk-row-tertiary { opacity: 0.7; font-size: 0.9em; }
  .vk-row-status { margin-left: auto; font-size: 0.8em; opacity: 0.8;
                   border: 1px solid var(--vscode-panel-border); border-radius: 8px; padding: 0 6px; }
  .vk-field { display: flex; gap: 8px; padding: 1px 0; }
  .vk-key { opacity: 0.7; min-width: 8em; }
  .vk-nested { padding-left: 12px; border-left: 1px solid var(--vscode-panel-border); }
  .vk-null, .vk-empty-value, .vk-cycle { opacity: 0.5; font-style: italic; }
  .vk-empty { opacity: 0.6; padding: 8px 0; }
  .error { color: var(--vscode-errorForeground); padding: 8px 12px; }
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
