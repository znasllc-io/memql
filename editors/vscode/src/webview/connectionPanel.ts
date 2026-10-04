// One cluster's page: where it is, whether this editor is connected and as
// whom, and the next step.
//
// The boundary this page keeps: the extension owns what is on your machine
// and what you can reach; MemQL OS owns what is inside a cluster. Everything
// here is on this side of it, and Open MemQL OS is one click away for the rest.
//
// WHAT THIS FILE IS NOT ALLOWED TO DECIDE. The page's words, its facts and
// which acts are legal are `clusters/connectionView.ts` (over the state
// machine in `clusters/status.ts`); where MemQL OS is, is
// `clusters/consoleUrl.ts`. All three run under bare `node --test`. This is the
// webview lifecycle, the reads, and the mapping from an act to a command.
//
// A LIVE PAGE, NOT A REPAINTED ONE. The document is assigned once per cluster
// (LiveView, src/webview/ui/liveView.ts); a connect, a sign-in, a drop or a
// file change patches the regions that changed, so focus and scroll survive.
// The old page reassigned the whole document every 30 seconds for a token
// countdown this page no longer shows -- renewal is automatic, and whether the
// person is signed in is the state itself.

import { randomBytes } from "node:crypto";

import * as vscode from "vscode";

import { browseConceptPage, type Row } from "@znasllc-io/memql-sdk-core/client";

import type { ClusterFacts } from "../clusters/facts.js";
import { readClustersFileSafe } from "../clusters/file.js";
import { displayLabel, type ClusterConfig } from "../clusters/model.js";
import { clusterPage, type ClusterPageAct, type SignInFlight } from "../clusters/connectionView.js";
import { SITE_CONCEPT, consoleTarget } from "../clusters/consoleUrl.js";
import type { ConnectionManager } from "../connection/manager.js";
import type { ReleaseListing } from "../version/releaseCache.js";
import { currentBodyThemeAttr, onAppearanceChange } from "./theme.js";
import { pageDocument } from "./ui/document.js";
import { LiveView } from "./ui/liveView.js";
import { pageMessage } from "./ui/protocol.js";

export interface ConnectionPanelDeps {
  clustersPath: string;
  connections: ConnectionManager;
  /** What this machine knows about a cluster's sign-in (clusters/facts.ts). */
  factsFor: (cluster: ClusterConfig) => Promise<ClusterFacts>;
  /** The shared release listing, as the tree reads it (`ReleaseCache.peek`). */
  releases?: () => ReleaseListing | undefined;
  /** A sign-in to this cluster in flight, if any. */
  signInFlight: (clusterName: string) => SignInFlight | undefined;
  /** Stops the sign-in in flight for this cluster. */
  cancelSignIn: (clusterName: string) => void;
  /** Takes the "Use a code instead" offer for this cluster's sign-in. */
  useCode: (clusterName: string) => void;
  /** Reveals the MemQL Connection output, where the full reason a dial failed is. */
  showDetails: () => void;
}

export class ConnectionPanel {
  private static open_: ConnectionPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly live: LiveView;
  private readonly disposables: vscode.Disposable[] = [];
  private clusterName = "";
  private cluster: ClusterConfig | undefined;
  /** The label last shown, kept if the cluster leaves the list while the page is open. */
  private knownLabel = "";
  private registryError: string | undefined;
  private facts: ClusterFacts | undefined;
  private identity: { email: string; role: string } | "loading" | "unavailable" = "loading";
  /** The connection the identity was read over (`cluster|nodeId`), so a reconnect re-reads it. */
  private identityFor = "";
  private siteRowConsole = "";
  private siteRowFor = "";
  /** Supersedes a slower load that was overtaken by a newer one. */
  private loadGeneration = 0;
  private disposed = false;

  /**
   * Shows the page for a cluster, reusing the one panel. `preserveFocus` keeps
   * focus where it is -- a row click keeps the keyboard in the tree, and an
   * install's hand-off keeps its own done screen in front.
   */
  static open(
    context: vscode.ExtensionContext,
    deps: ConnectionPanelDeps,
    clusterName: string,
    preserveFocus = false,
  ): ConnectionPanel {
    const existing = ConnectionPanel.open_;
    if (existing !== undefined && !existing.disposed) {
      existing.panel.reveal(vscode.ViewColumn.Beside, preserveFocus);
      existing.pointAt(clusterName);
      return existing;
    }
    const panel = new ConnectionPanel(context, deps, preserveFocus);
    ConnectionPanel.open_ = panel;
    panel.pointAt(clusterName);
    return panel;
  }

  /** Re-reads the open page, if any: the file changed, or a sign-in settled. */
  static refresh(): void {
    const open = ConnectionPanel.open_;
    if (open !== undefined && !open.disposed) void open.load();
  }

  /** Repaints the open page from what it already holds (a sign-in's phase moved). */
  static repaint(): void {
    const open = ConnectionPanel.open_;
    if (open !== undefined && !open.disposed) open.render();
  }

  /** The cluster the open page shows, if any. */
  static showing(): string | undefined {
    const open = ConnectionPanel.open_;
    return open !== undefined && !open.disposed ? open.clusterName : undefined;
  }

  private constructor(
    _context: vscode.ExtensionContext,
    private readonly deps: ConnectionPanelDeps,
    preserveFocus: boolean,
  ) {
    this.panel = vscode.window.createWebviewPanel(
      "memqlConnection",
      "Cluster",
      { viewColumn: vscode.ViewColumn.Beside, preserveFocus },
      { enableScripts: true },
    );
    const webview = this.panel.webview;
    this.live = new LiveView(
      { setHtml: (html) => (webview.html = html), postMessage: (msg) => webview.postMessage(msg) },
      (parts, screen) =>
        pageDocument({
          nonce: nonceValue(),
          title: this.panel.title,
          themeAttr: currentBodyThemeAttr(),
          screen,
          ...parts,
        }),
    );
    this.disposables.push(
      // The palette is a MemQL setting now, not the editor's theme, so an
      // OPEN panel repaints when either input moves (memql#4419). Every rule
      // may restyle, so it is a new document rather than a patch.
      ...onAppearanceChange(() => {
        this.live.invalidate();
        this.render();
      }),
      this.panel.onDidDispose(() => {
        this.disposed = true;
        if (ConnectionPanel.open_ === this) ConnectionPanel.open_ = undefined;
        for (const d of this.disposables) d.dispose();
      }),
      webview.onDidReceiveMessage((raw: unknown) => {
        if (this.live.handleMessage(raw)) return;
        void this.onMessage(raw);
      }),
    );
    // A connect, a sign-out or a dropped socket all change what this page says.
    this.disposables.push({
      dispose: this.deps.connections.onDidChangeState(() => void this.load()),
    });
  }

  private pointAt(clusterName: string): void {
    if (clusterName !== this.clusterName) {
      this.clusterName = clusterName;
      this.cluster = undefined;
      this.knownLabel = "";
      this.facts = undefined;
      this.identity = "loading";
      this.identityFor = "";
      this.siteRowConsole = "";
      this.siteRowFor = "";
    }
    this.render();
    void this.load();
  }

  private async load(): Promise<void> {
    const generation = ++this.loadGeneration;
    const result = await readClustersFileSafe(this.deps.clustersPath);
    if (this.disposed || generation !== this.loadGeneration) return;
    if (!result.ok) {
      this.registryError = result.error;
      this.cluster = undefined;
      this.render();
      return;
    }
    this.registryError = undefined;
    this.cluster = result.file.clusters.find((c) => c.name === this.clusterName);
    if (this.cluster !== undefined) this.knownLabel = displayLabel(this.cluster);
    if (this.cluster === undefined) {
      this.render();
      return;
    }
    const facts = await this.deps.factsFor(this.cluster).catch(() => undefined);
    if (this.disposed || generation !== this.loadGeneration) return;
    this.facts = facts;
    this.render();
    // Not generation-guarded: each is keyed on the live connection it read
    // over, and applies only while that connection is still the one up.
    await Promise.all([this.loadIdentity(), this.loadSiteRow()]);
  }

  /** The live connection to THIS cluster, or undefined. */
  private liveConnection(): { query: NonNullable<ConnectionManager["query"]>; key: string } | undefined {
    const state = this.deps.connections.state;
    const query = this.deps.connections.query;
    if (query === undefined || state.status !== "connected" || state.clusterName !== this.clusterName) return undefined;
    return { query, key: `${state.clusterName}|${state.nodeId}` };
  }

  /**
   * Who this editor is on the cluster: read over the live session, because a
   * token this editor holds and a session the cluster accepted are two
   * different facts, and the second is the one shown.
   */
  private async loadIdentity(): Promise<void> {
    const live = this.liveConnection();
    if (live === undefined || live.key === this.identityFor) return;
    this.identityFor = live.key;
    this.identity = "loading";
    this.render();
    const access = await live.query.getMyAccess().catch(() => null);
    if (this.disposed || this.identityFor !== live.key) return;
    this.identity =
      access === null || (access.primaryEmail ?? "") === ""
        ? "unavailable"
        : { email: access.primaryEmail ?? "", role: String(access.role ?? "") };
    this.render();
  }

  /** MemQL OS as the cluster itself names it, when there is a connection to ask over. */
  private async loadSiteRow(): Promise<void> {
    const live = this.liveConnection();
    const cluster = this.cluster;
    if (live === undefined || cluster === undefined || live.key === this.siteRowFor) return;
    this.siteRowFor = live.key;
    const page = await browseConceptPage(live.query, SITE_CONCEPT, { pageSize: 50 }).catch(() => null);
    if (this.disposed || this.siteRowFor !== live.key) return;
    const rows: Row[] = page?.rows ?? [];
    const target = consoleTarget(cluster, rows);
    this.siteRowConsole = target.fromSiteRow ? target.url : "";
    this.render();
  }

  /** The site row's answer while connected, the composed one otherwise. */
  private consoleUrl(): string {
    if (this.liveConnection() !== undefined && this.siteRowConsole !== "") return this.siteRowConsole;
    return this.facts?.consoleUrl ?? "";
  }

  private render(): void {
    if (this.disposed) return;
    const page = clusterPage({
      clusterName: this.clusterName,
      cluster: this.cluster,
      knownLabel: this.knownLabel,
      ...(this.registryError !== undefined ? { registryError: this.registryError } : {}),
      facts: this.facts,
      connection: this.deps.connections.state,
      identity: this.identity,
      consoleUrl: this.consoleUrl(),
      listing: this.deps.releases?.(),
      signingIn: this.deps.signInFlight(this.clusterName),
    });
    this.panel.title = page.title;
    this.live.render(page.screen, { head: page.head, body: page.body, actions: page.actions });
  }

  private async onMessage(raw: unknown): Promise<void> {
    const msg = pageMessage(raw);
    if (msg === undefined) return;
    const type = msg.type as ClusterPageAct;
    if (type === "close") {
      this.panel.dispose();
      return;
    }
    if (type === "openFile") {
      await vscode.commands.executeCommand("vscode.open", vscode.Uri.file(this.deps.clustersPath));
      return;
    }
    const cluster = this.cluster;
    if (cluster === undefined) return;
    const node = { cluster, selected: false, ...(this.facts !== undefined ? { facts: this.facts } : {}) };
    switch (type) {
      case "edit":
        await vscode.commands.executeCommand("memql.clusters.edit", node);
        return;
      case "remove":
        await vscode.commands.executeCommand("memql.clusters.remove", node);
        return;
      case "signIn":
        await vscode.commands.executeCommand("memql.clusters.signIn", node);
        return;
      case "signInWithCode":
        await vscode.commands.executeCommand("memql.clusters.signInWithCode", node);
        return;
      case "takeOwnership":
        await vscode.commands.executeCommand("memql.clusters.takeOwnership", node);
        return;
      case "signOut":
        await vscode.commands.executeCommand("memql.clusters.signOut", node);
        return;
      case "disconnect":
        // THIS page's cluster: the command acts only when the live connection
        // names it, rather than on whichever cluster happens to be connected.
        await vscode.commands.executeCommand("memql.clusters.disconnect", node);
        return;
      case "cancel":
        if (this.deps.signInFlight(this.clusterName) !== undefined) {
          this.deps.cancelSignIn(this.clusterName);
          return;
        }
        await vscode.commands.executeCommand("memql.clusters.disconnect", node);
        return;
      case "useCode":
        this.deps.useCode(this.clusterName);
        return;
      case "connect":
        await vscode.commands.executeCommand("memql.clusters.select", node);
        return;
      case "repair":
        await vscode.commands.executeCommand("memql.clusters.repair");
        return;
      case "showDetails":
        this.deps.showDetails();
        return;
      case "openConsole": {
        const url = this.consoleUrl();
        if (url !== "") await vscode.env.openExternal(vscode.Uri.parse(url));
        return;
      }
      default:
        return;
    }
  }
}

/** A CSP nonce, from a CSPRNG: a predictable one is one an injection can carry. */
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
