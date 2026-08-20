// One cluster's CONNECTION: what this editor dials, as whom, and what happened.
//
// This replaces the topology view, and not because it is nicer. The boundary
// rule this epic establishes is:
//
//   The extension owns what is on your machine and what you can reach.
//   The portal owns what is inside a cluster.
//
// A pod grid, orphan verdicts and under-replica alarms are cluster state.
// `clients/portal/src/cluster/` and `clients/portal/src/deploy/` already draw
// them, and two surfaces answering one question diverge on the day the second
// one ships.
//
// What NOTHING answered is the question an operator actually arrives with when
// a cluster will not come up: which endpoint, which issuer, whose credential,
// expiring when. Every field here is on this side of the boundary --
// clusters.yaml, VS Code SecretStorage, the live connection -- and none of it
// is anything the portal knows or could show.
//
// WHAT THIS FILE IS NOT ALLOWED TO DECIDE. The wording, the verdict and the
// duration are `clusters/connectionView.ts`; where the portal is, is
// `clusters/portalUrl.ts`. Both run under bare `node --test`. This is the
// webview lifecycle, the postMessage boundary and the commands.
//
// Refs: #3742 #3733

import { randomBytes } from "node:crypto";

import * as vscode from "vscode";

import { escapeHtml, viewKitStyles } from "@znasllc-io/memql-view-kit";
import { browseConceptPage, type Row } from "@znasllc-io/memql-sdk-core/client";

import { readClustersFileSafe } from "../clusters/file.js";
import type { ClusterConfig } from "../clusters/model.js";
import { connectionView, type ConnectionView } from "../clusters/connectionView.js";
import { SITE_CONCEPT, portalTarget } from "../clusters/portalUrl.js";
import type { ConnectionManager } from "../connection/manager.js";

/** How often the token countdown is redrawn while the page is open. */
const TICK_MS = 30_000;

export interface ConnectionPanelDeps {
  clustersPath: string;
  connections: ConnectionManager;
  /** The access token's absolute expiry, in epoch seconds. */
  readExpiry: (clusterName: string) => Promise<number | undefined>;
}

export class ConnectionPanel {
  private static open_: ConnectionPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly disposables: vscode.Disposable[] = [];
  private cluster: ClusterConfig | undefined;
  private view: ConnectionView | undefined;
  private identity: { email: string; role: string } | undefined;
  private expiresAt: number | undefined;
  private portal = "";
  private error = "";
  private disposed = false;
  private ticker: NodeJS.Timeout | undefined;

  static open(
    context: vscode.ExtensionContext,
    deps: ConnectionPanelDeps,
    clusterName: string,
  ): ConnectionPanel {
    const existing = ConnectionPanel.open_;
    if (existing !== undefined && !existing.disposed) {
      existing.panel.reveal(vscode.ViewColumn.Beside);
      existing.pointAt(clusterName);
      return existing;
    }
    const panel = new ConnectionPanel(context, deps);
    ConnectionPanel.open_ = panel;
    panel.pointAt(clusterName);
    return panel;
  }

  private constructor(
    _context: vscode.ExtensionContext,
    private readonly deps: ConnectionPanelDeps,
  ) {
    this.panel = vscode.window.createWebviewPanel(
      "memqlConnection",
      "memQL connection",
      vscode.ViewColumn.Beside,
      { enableScripts: true, retainContextWhenHidden: true },
    );
    // REDRAWN ON A TIMER, because the token countdown is the one value on this
    // page that changes with nothing happening. A page that only updated on
    // reopen would show "expires in 11m" an hour after it did.
    this.ticker = setInterval(() => this.render(), TICK_MS);
    this.disposables.push(
      this.panel.onDidDispose(() => {
        this.disposed = true;
        if (this.ticker !== undefined) clearInterval(this.ticker);
        this.ticker = undefined;
        if (ConnectionPanel.open_ === this) ConnectionPanel.open_ = undefined;
        for (const d of this.disposables) d.dispose();
      }),
      this.panel.webview.onDidReceiveMessage((message: unknown) => {
        void this.onMessage(message);
      }),
    );
    // A connect, a sign-out or a dropped socket all change what this page says.
    this.disposables.push({
      dispose: this.deps.connections.onDidChangeState(() => void this.load()),
    });
    this.render();
  }

  private pointAt(clusterName: string): void {
    this.clusterName = clusterName;
    this.identity = undefined;
    this.expiresAt = undefined;
    this.portal = "";
    this.error = "";
    void this.load();
  }

  private clusterName = "";

  private async load(): Promise<void> {
    const result = await readClustersFileSafe(this.deps.clustersPath);
    if (this.disposed) return;
    if (!result.ok) {
      // The same synthetic-row treatment the tree gives an unreadable registry:
      // a rejection here has nowhere to be shown, and a blank page reads as
      // "this cluster has nothing to say about itself".
      this.error = result.error;
      this.cluster = undefined;
      this.render();
      return;
    }
    this.error = "";
    this.cluster = result.file.clusters.find((c) => c.name === this.clusterName);
    if (this.cluster === undefined) {
      this.render();
      return;
    }

    this.expiresAt = await this.deps.readExpiry(this.clusterName).catch(() => undefined);
    if (this.disposed) return;
    this.render();

    await Promise.all([this.loadIdentity(), this.loadPortal()]);
  }

  /**
   * Who this editor is on that cluster.
   *
   * Only for the CONNECTED cluster, and only over the live session: the page
   * reports what the connection produced rather than what the credential
   * claims, because a token this editor holds and a session the cluster
   * accepted are two different facts and the second is the one being diagnosed.
   */
  private async loadIdentity(): Promise<void> {
    const state = this.deps.connections.state;
    const query = this.deps.connections.query;
    if (query === undefined || state.status !== "connected" || state.clusterName !== this.clusterName) {
      this.identity = undefined;
      return;
    }
    const access = await query.getMyAccess().catch(() => null);
    if (this.disposed) return;
    this.identity =
      access === null
        ? undefined
        : { email: access.primaryEmail ?? "", role: access.clusterRole ?? "" };
    this.render();
  }

  /** The portal's own site row, when there is a connection to read it over. */
  private async loadPortal(): Promise<void> {
    const cluster = this.cluster;
    if (cluster === undefined) return;
    const state = this.deps.connections.state;
    const query = this.deps.connections.query;
    let rows: Row[] = [];
    if (query !== undefined && state.status === "connected" && state.clusterName === cluster.name) {
      const page = await browseConceptPage(query, SITE_CONCEPT, { pageSize: 50 }).catch(() => null);
      rows = page?.rows ?? [];
    }
    if (this.disposed) return;
    this.portal = portalTarget(cluster, rows).url;
    this.render();
  }

  private async onMessage(message: unknown): Promise<void> {
    if (message === null || typeof message !== "object") return;
    const { type } = message as { type?: unknown };
    if (typeof type !== "string" || this.cluster === undefined) return;
    const node = { cluster: this.cluster, selected: false };

    switch (type) {
      case "openPortal":
        await this.openPortal();
        return;
      case "signOut":
        await vscode.commands.executeCommand("memql.clusters.signOut", node);
        return;
      case "signIn":
        await vscode.commands.executeCommand("memql.clusters.signIn", node);
        return;
      case "disconnect":
        await vscode.commands.executeCommand("memql.clusters.disconnect", node);
        return;
      case "edit":
        await vscode.commands.executeCommand("memql.clusters.edit", node);
        return;
      case "remove":
        await vscode.commands.executeCommand("memql.clusters.remove", node);
        return;
      default:
        return;
    }
  }

  private async openPortal(): Promise<void> {
    if (this.portal === "") {
      void vscode.window.showErrorMessage(
        "memQL: no portal address can be worked out for this cluster. Give it a domain, or connect to it so its site row can be read.",
      );
      return;
    }
    await vscode.env.openExternal(vscode.Uri.parse(this.portal));
  }

  private render(): void {
    if (this.disposed) return;
    const nonce = nonceValue();
    this.panel.title = this.clusterName === "" ? "memQL connection" : `Cluster: ${this.clusterName}`;
    this.view =
      this.cluster === undefined
        ? undefined
        : connectionView({
            cluster: this.cluster,
            state: this.deps.connections.state,
            ...(this.identity !== undefined ? { identity: this.identity } : {}),
            ...(this.expiresAt !== undefined ? { expiresAtEpochSeconds: this.expiresAt } : {}),
            nowMs: Date.now(),
          });
    this.panel.webview.html = this.html(nonce);
  }

  private bodyHtml(): string {
    if (this.error !== "") {
      return `<h1>Failed to read clusters.yaml</h1>
<p class="error">${escapeHtml(this.error)}</p>`;
    }
    const view = this.view;
    if (view === undefined) {
      return `<h1>${escapeHtml(this.clusterName)}</h1>
<p class="lede">This cluster is no longer in your list.</p>`;
    }
    return `<div class="head">
  <h1>Cluster: ${escapeHtml(view.title)}</h1>
  <button class="secondary" type="button" data-act="openPortal">Open Portal</button>
</div>
<p class="lede">${escapeHtml(view.summary)}</p>
<h2>Connection</h2>
${factsHtml(view.connection)}
<h2>Identity</h2>
${factsHtml(view.identity)}
<div class="actions">
  <button class="secondary" type="button" data-act="signIn">Sign in</button>
  <button class="secondary" type="button" data-act="signOut">Sign out</button>
  <button class="secondary" type="button" data-act="disconnect">Disconnect</button>
  <button class="secondary" type="button" data-act="edit">Edit</button>
  <button class="secondary destructive" type="button" data-act="remove">Remove from list</button>
</div>`;
  }

  private html(nonce: string): string {
    return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(this.clusterName)}</title>
<style nonce="${nonce}">
  :root {
    --vk-fg: var(--vscode-foreground);
    --vk-muted-fg: var(--vscode-descriptionForeground);
    --vk-border: var(--vscode-panel-border);
    --vk-hover-bg: var(--vscode-list-hoverBackground);
    --vk-selected-bg: var(--vscode-list-activeSelectionBackground);
    --vk-selected-fg: var(--vscode-list-activeSelectionForeground);
    --vk-mono-font: var(--vscode-editor-font-family, monospace);
    --vk-subtle-bg: var(--vscode-textCodeBlock-background, transparent);
  }

${viewKitStyles}

  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0;
         padding: 16px 20px; max-width: 780px; }
  .head { display: flex; gap: 12px; align-items: baseline; justify-content: space-between; }
  h1 { font-size: 1.2em; margin: 0 0 4px; }
  h2 { font-size: 1em; margin: 20px 0 6px; }
  .lede { color: var(--vscode-descriptionForeground); margin: 0 0 16px; }
  .fact { display: flex; gap: 8px; align-items: baseline; padding: 1px 0; }
  .fact-key { flex: none; min-width: 9em; color: var(--vscode-descriptionForeground); }
  .fact-value { min-width: 0; overflow-wrap: anywhere; }
  .fact-note { color: var(--vscode-descriptionForeground); }
  .error { color: var(--vscode-inputValidation-errorForeground,
                   var(--vscode-editorError-foreground)); }
  .actions { display: flex; gap: 8px; margin-top: 20px; flex-wrap: wrap; }
  button.secondary {
    font: inherit; padding: 4px 12px; cursor: pointer; border-radius: 2px;
    border: 1px solid transparent;
    background: var(--vscode-button-secondaryBackground);
    color: var(--vscode-button-secondaryForeground); }
  button.destructive { color: var(--vscode-editorWarning-foreground); }
</style>
</head>
<body>
${this.bodyHtml()}
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  document.addEventListener('click', (e) => {
    const act = e.target.closest('[data-act]');
    if (act) vscode.postMessage({ type: act.dataset.act });
  });
</script>
</body>
</html>`;
  }
}

function factsHtml(facts: readonly { key: string; value: string; note: string }[]): string {
  return facts
    .map(
      (fact) => `<div class="fact">
  <span class="fact-key">${escapeHtml(fact.key)}</span>
  <span class="fact-value">${escapeHtml(fact.value)}</span>
  ${fact.note === "" ? "" : `<span class="fact-note">${escapeHtml(fact.note)}</span>`}
</div>`,
    )
    .join("");
}

/** A CSP nonce, from a CSPRNG: a predictable one is one an injection can carry. */
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
