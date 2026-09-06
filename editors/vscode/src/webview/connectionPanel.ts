// One cluster's CONNECTION: what this editor dials, as whom, and what happened.
//
// This replaces the topology view, and not because it is nicer. The boundary
// rule this epic establishes is:
//
//   The extension owns what is on your machine and what you can reach.
//   The console owns what is inside a cluster.
//
// A pod grid, orphan verdicts and under-replica alarms are cluster state.
// MemQL OS's Fleet and Deployables apps already draw
// them, and two surfaces answering one question diverge on the day the second
// one ships.
//
// What NOTHING answered is the question an operator actually arrives with when
// a cluster will not come up: which endpoint, which issuer, whose credential,
// expiring when. Every field here is on this side of the boundary --
// clusters.yaml, VS Code SecretStorage, the live connection -- and none of it
// is anything the console knows or could show.
//
// WHAT THIS FILE IS NOT ALLOWED TO DECIDE. The wording, the verdict and the
// duration are `clusters/connectionView.ts`; where the console is, is
// `clusters/consoleUrl.ts`. Both run under bare `node --test`. This is the
// webview lifecycle, the postMessage boundary and the commands.
//
// Refs: #3742 #3733

import { randomBytes } from "node:crypto";

import * as vscode from "vscode";

import { escapeHtml, viewKitStyles } from "@znasllc-io/memql-view-kit";

import { brandHeader, brandStyleBlock } from "./brandTokens.js";
import { currentBodyThemeAttr, onAppearanceChange } from "./theme.js";
import { browseConceptPage, type Row } from "@znasllc-io/memql-sdk-core/client";

import { readClustersFileSafe } from "../clusters/file.js";
import type { ClusterConfig } from "../clusters/model.js";
import { connectionView, type ConnectionView } from "../clusters/connectionView.js";
import { SITE_CONCEPT, consoleTarget } from "../clusters/consoleUrl.js";
import type { ConnectionManager } from "../connection/manager.js";
import {
  defaultReceiptPath,
  readReceipt,
  recordedImageSource,
  recordedRebuild,
  recordedStackDir,
  type ImageSource,
} from "../install/receipt.js";

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
  /**
   * The local checkout the receipt records, when there is one (memql#4246).
   * Undefined for a remote cluster and for a local one whose receipt records
   * none -- the two facts and the "Open Checkout" button both key off this
   * being present, not off `cluster.local` alone.
   */
  private checkout: { path: string; ref: string; imageSource: ImageSource | "" } | undefined;
  private console = "";
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
      "MemQL connection",
      vscode.ViewColumn.Beside,
      { enableScripts: true, retainContextWhenHidden: true },
    );
    // REDRAWN ON A TIMER, because the token countdown is the one value on this
    // page that changes with nothing happening. A page that only updated on
    // reopen would show "expires in 11m" an hour after it did.
    this.ticker = setInterval(() => this.render(), TICK_MS);
    this.disposables.push(
      // The palette is a MemQL setting now, not the editor's theme, so an
      // OPEN panel repaints when either input moves (memql#4419).
      ...onAppearanceChange(() => this.render()),
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
    this.checkout = undefined;
    this.console = "";
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

    // THE RECORDED CHECKOUT, read off the install receipt rather than the
    // live connection (memql#4246) -- `local: true` is the ONLY condition, so
    // this page answers the same way whether or not a session is up, exactly
    // like the version fact above it. `ref` comes from the last RECORDED
    // REBUILD rather than from the install's own tag/commit: a checkout
    // directory says WHERE, and the rebuild's own git ref says what it was
    // last built FROM, which is the fact the "image source" row beside it is
    // about. Undefined -- not "" -- when the receipt records no rebuild, so
    // the checkout row still renders with a blank note rather than a false one.
    this.checkout = undefined;
    if (this.cluster.local === true) {
      const receipt = await readReceipt(defaultReceiptPath()).catch(() => null);
      if (this.disposed) return;
      const dir = recordedStackDir(receipt);
      this.checkout =
        dir === ""
          ? undefined
          : {
              path: dir,
              ref: recordedRebuild(receipt)?.ref ?? "",
              imageSource: recordedImageSource(receipt),
            };
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

  /** The console's own site row, when there is a connection to read it over. */
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
    this.console = consoleTarget(cluster, rows).url;
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
      case "openCheckout":
        // The same shared command the done screen and the tree row use
        // (memql#4246) -- one implementation of "where is the checkout",
        // not three.
        await vscode.commands.executeCommand("memql.deployments.openCheckout");
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
    if (this.console === "") {
      void vscode.window.showErrorMessage(
        "MemQL: no console address can be worked out for this cluster. Give it a domain, or connect to it so its site row can be read.",
      );
      return;
    }
    await vscode.env.openExternal(vscode.Uri.parse(this.console));
  }

  private render(): void {
    if (this.disposed) return;
    const nonce = nonceValue();
    this.panel.title = this.clusterName === "" ? "MemQL connection" : `Cluster: ${this.clusterName}`;
    this.view =
      this.cluster === undefined
        ? undefined
        : connectionView({
            cluster: this.cluster,
            state: this.deps.connections.state,
            ...(this.identity !== undefined ? { identity: this.identity } : {}),
            ...(this.expiresAt !== undefined ? { expiresAtEpochSeconds: this.expiresAt } : {}),
            ...(this.checkout !== undefined ? { checkout: this.checkout } : {}),
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
    // "Open Checkout" rides beside "Open Portal" and offers only when there is
    // a recorded checkout to open -- unlike Open Portal, which is always shown
    // and reports its own failure on click, this button has nothing useful to
    // do without one (memql#4246).
    const openCheckout =
      this.checkout === undefined
        ? ""
        : `<button class="secondary" type="button" data-act="openCheckout">Open Checkout</button>`;
    return `${brandHeader(
      `Cluster: ${view.title}`,
      `<button class="secondary" type="button" data-act="openPortal">Open Portal</button>${openCheckout}`,
    )}
<p class="lede">${escapeHtml(view.summary)}</p>
<h2>Connection</h2>
${factsHtml(view.connection)}
<h2>Identity</h2>
${factsHtml(view.identity)}
<p class="boundary">${escapeHtml(
      "This editor owns what is on your machine and what you can reach: install, repair, connect, sign in. " +
        "Managing what runs INSIDE the cluster -- people, modules, sites, deployments, observability -- is its console's job. Open Portal, above.",
    )}</p>
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
${brandStyleBlock()}
${viewKitStyles}

  body { max-width: 780px; }
  .fact { display: flex; gap: 8px; align-items: baseline; padding: 1px 0; }
  .fact-key { flex: none; min-width: 9em; color: var(--memql-muted); }
  .fact-value { min-width: 0; overflow-wrap: anywhere;
                font-family: var(--vscode-editor-font-family, ui-monospace, monospace);
                font-size: 0.95em; }
  .fact-note { color: var(--memql-muted); font-family: var(--vscode-font-family); }
  .error { color: var(--memql-danger); }
</style>
</head>
<body${currentBodyThemeAttr()}>
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
