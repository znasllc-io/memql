// One construct's definition, and the way back to its source.
//
// The page is read-only, and that is a rule rather than a stage of
// development: editing happens in a `.memql` file, and a surface that could
// change a construct from here would be a second authoring path for something
// the language server already owns.
//
// OPEN SOURCE IS ONE ACT WITH THREE OUTCOMES, and the host picks, not the
// reader:
//
//   the file is in this workspace     -> open it, revealed at the signature
//   the file is NOT in this workspace -> read it from the cluster that loaded
//       it (memql#4248). The catalog reports a path relative to the CLUSTER's
//       tree, and the cluster is not obliged to be the checkout this editor
//       has open -- a remote cluster usually is not.
//   there is no file at all           -> the construct is PROMOTED. Its source
//       is on the page already, and there is no act.
//
// ON THE PAGE KIT (src/webview/ui). The document is assigned once per
// construct; the workspace lookup that decides the Open source act, and the
// Details disclosure, arrive as patches, so the page is drawn at once instead
// of blank until a file stat finishes.
//
// THE SIGNATURE RANGE IS NOT ON THE WIRE. `ListConstructs` reports a path and
// not a range, so the reveal is done here by searching the opened document for
// the construct's declaration -- the same keyword the engine slices source on.
// A search that finds nothing opens the file at the top rather than guessing,
// because landing on the wrong line is worse than landing on the first one.
//
// What this file is not allowed to decide -- the kind vocabulary, the
// grouping, the wire narrowing, the markup -- lives in
// state/constructCatalog.ts and webview/constructScreens.ts, under bare
// `node --test`.
//
// Refs: #4248 #3752 #3747

import { randomBytes } from "node:crypto";

import * as vscode from "vscode";

import { currentBodyThemeAttr, onAppearanceChange } from "./theme.js";

import type { CatalogConstruct } from "../state/constructCatalog.js";
import {
  CONSTRUCT_ACTS,
  CONSTRUCT_PAGE_STYLES,
  constructFailedParts,
  constructLoadingParts,
  constructPageParts,
  type ConstructSource,
} from "./constructScreens.js";
import { signatureLine } from "../constructs/signature.js";
import { workspaceCandidates } from "../handoff/resolve.js";
import { pageDocument } from "./ui/document.js";
import { LiveView, type RegionParts } from "./ui/liveView.js";
import { pageMessage } from "./ui/protocol.js";

/**
 * What the page needs from the host that it cannot reach itself.
 *
 * INJECTED rather than imported, because each needs the live connection or the
 * run path -- which live in extension.ts and which a webview module has no
 * business reading. The panel posts an intent; the host decides whether there
 * is a cluster to serve it.
 *
 * EACH TAKES THE PANEL'S CLUSTER, and may not read the connected one in its
 * place (memql#4253). This panel is a singleton that outlives the connection
 * its record was read over, so a construct opened on `staging` would otherwise
 * be served by `prod` after a switch, with nothing on the page saying so. The
 * host compares the two through `panelClusterRefusal`.
 */
export interface ConstructPanelDeps {
  /** Read the construct's file from the cluster that loaded it, read-only. */
  viewSourceFromCluster: (construct: CatalogConstruct, cluster: string) => Promise<void>;
  /** Open a concept's rows in the editor (the same page the Data view opens). */
  browseRows: (construct: CatalogConstruct, cluster: string) => Promise<void>;
  /** Open a concept's rows in MemQL OS (epic memql#5009). */
  openInOs: (construct: CatalogConstruct, cluster: string) => Promise<void>;
  /**
   * Run the construct through the ONE run path -- the same commands the
   * CodeLens and the Constructs view use, so the write confirmation, the
   * preflight and the Result tab are the ones that already exist.
   */
  run: (construct: CatalogConstruct, withArguments: boolean) => Promise<void>;
}

/**
 * Opens a file on disk and reveals the construct's declaration.
 *
 * EXPORTED so the MemQL OS handoff (memql#4251) lands on a construct the same
 * way a click on this page does. A second copy of "open, find the signature,
 * reveal" is a second answer to where the cursor ends up, which is the whole
 * visible behaviour of both.
 */
export async function openFileAtSignature(
  uri: vscode.Uri,
  kind: string,
  name: string,
): Promise<vscode.TextEditor> {
  const document = await vscode.workspace.openTextDocument(uri);
  const line = signatureLine(document.getText(), kind, name);
  const editor = await vscode.window.showTextDocument(document, {
    viewColumn: vscode.ViewColumn.One,
    preview: false,
  });
  if (line >= 0) {
    const at = new vscode.Position(line, 0);
    editor.selection = new vscode.Selection(at, at);
    editor.revealRange(new vscode.Range(at, at), vscode.TextEditorRevealType.InCenter);
  }
  return editor;
}

/** What the page is showing. */
type Shown =
  | { kind: "loading"; name: string }
  | { kind: "failed"; name: string; message: string }
  | { kind: "construct"; construct: CatalogConstruct };

export class ConstructPanel {
  private static open_: ConstructPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly live: LiveView;
  private readonly disposables: vscode.Disposable[] = [];
  private shown: Shown;
  private deps: ConstructPanelDeps;
  /**
   * The cluster this panel's record was read from, or "" when the opener could
   * not say. Re-pointed with the construct, never read off the connection.
   */
  private cluster: string;
  private fileUri: vscode.Uri | undefined;
  private source: ConstructSource = "checking";
  private detailsOpen = false;
  private error = "";
  /** Re-reads the construct after a failed pending open. */
  private retry: (() => void) | undefined;
  /** Bumped per construct, so a slow file lookup cannot land on the next one. */
  private lookup = 0;
  private disposed = false;

  /**
   * `cluster` is the cluster the construct was resolved FROM -- "" when the
   * opener genuinely cannot say. It is a parameter rather than a lookup because
   * only the opener knows: every call site already holds it, and reading it
   * back off the live connection is precisely the bug this closes.
   */
  static open(
    context: vscode.ExtensionContext,
    construct: CatalogConstruct,
    deps: ConstructPanelDeps,
    cluster: string,
  ): ConstructPanel {
    const panel = ConstructPanel.reveal(context, { kind: "construct", construct }, deps, cluster);
    panel.pointAt(construct);
    return panel;
  }

  /**
   * Opens the page AT ONCE, in its loading shape, while `load` reads the
   * construct -- so a click on a cluster document's Details lens is answered
   * by a page, not by nothing until a whole-registry read returns.
   *
   * `load` resolves the construct, or rejects with the sentence the page shows;
   * the page then offers Try again, which runs `load` again.
   */
  static openLoading(
    context: vscode.ExtensionContext,
    name: string,
    deps: ConstructPanelDeps,
    cluster: string,
    load: () => Promise<CatalogConstruct>,
  ): ConstructPanel {
    const panel = ConstructPanel.reveal(context, { kind: "loading", name }, deps, cluster);
    const attempt = (): void => {
      panel.shown = { kind: "loading", name };
      panel.retry = undefined;
      panel.render();
      load().then(
        (construct) => {
          if (panel.disposed || panel.shown.kind !== "loading" || panel.shown.name !== name) return;
          panel.pointAt(construct);
        },
        (err: unknown) => {
          if (panel.disposed || panel.shown.kind !== "loading" || panel.shown.name !== name) return;
          panel.shown = { kind: "failed", name, message: err instanceof Error ? err.message : String(err) };
          panel.retry = attempt;
          panel.render();
        },
      );
    };
    attempt();
    return panel;
  }

  private static reveal(
    context: vscode.ExtensionContext,
    shown: Shown,
    deps: ConstructPanelDeps,
    cluster: string,
  ): ConstructPanel {
    const existing = ConstructPanel.open_;
    if (existing !== undefined && !existing.disposed) {
      existing.panel.reveal(vscode.ViewColumn.Beside);
      // Re-pointed along with the construct. The panel is a SINGLETON reused
      // across opens, so what it holds must be what THIS opener supplied. The
      // cluster goes with them: a reused panel showing a new construct is
      // showing a new cluster's record as often as not.
      existing.deps = deps;
      existing.cluster = cluster;
      existing.shown = shown;
      return existing;
    }
    const panel = new ConstructPanel(context, shown, deps, cluster);
    ConstructPanel.open_ = panel;
    return panel;
  }

  private constructor(
    _context: vscode.ExtensionContext,
    shown: Shown,
    deps: ConstructPanelDeps,
    cluster: string,
  ) {
    this.shown = shown;
    this.deps = deps;
    this.cluster = cluster;
    this.panel = vscode.window.createWebviewPanel(
      "memqlConstruct",
      titleOf(shown),
      vscode.ViewColumn.Beside,
      { enableScripts: true, retainContextWhenHidden: true },
    );
    this.live = new LiveView(
      {
        setHtml: (html) => {
          this.panel.webview.html = html;
        },
        postMessage: (message) => this.panel.webview.postMessage(message),
      },
      (parts, screen) =>
        pageDocument({
          nonce: nonceValue(),
          title: titleOf(this.shown),
          themeAttr: currentBodyThemeAttr(),
          screen,
          styles: CONSTRUCT_PAGE_STYLES,
          ...parts,
        }),
    );
    this.disposables.push(
      // The palette is a MemQL setting, not the editor's theme, so an OPEN panel
      // restyles when either input moves (memql#4419): a new document, because
      // a theme change touches every rule.
      ...onAppearanceChange(() => {
        this.live.invalidate();
        this.render();
      }),
      this.panel.onDidDispose(() => {
        this.disposed = true;
        if (ConstructPanel.open_ === this) ConstructPanel.open_ = undefined;
        for (const d of this.disposables) d.dispose();
      }),
      this.panel.webview.onDidReceiveMessage((message: unknown) => {
        if (this.live.handleMessage(message)) return;
        void this.onMessage(message);
      }),
    );
    this.render();
  }

  private pointAt(construct: CatalogConstruct): void {
    this.shown = { kind: "construct", construct };
    this.fileUri = undefined;
    this.error = "";
    this.retry = undefined;
    // A construct with no file has its source on the page; one with a file
    // gets its Open source act when the workspace lookup answers.
    this.source = construct.originPath === "" ? "none" : "checking";
    this.panel.title = construct.name;
    this.render();
    if (construct.originPath !== "") void this.resolveFile(construct);
  }

  /**
   * Whether this construct's file is reachable from here.
   *
   * The catalog's path is relative to the CLUSTER's tree. It resolves against
   * this workspace only when the two happen to be the same checkout, which is
   * the ordinary case for a local cluster and the unusual one for a remote --
   * so the answer is looked up rather than assumed.
   */
  private async resolveFile(construct: CatalogConstruct): Promise<void> {
    const lookup = ++this.lookup;
    let found: vscode.Uri | undefined;
    // TWO LAYOUTS PER FOLDER (memql#4251). The catalog's path is relative to
    // the DSL TREE ROOT (`cognition/queries.memql`) and a repository checkout
    // keeps that tree under `dsl/`. The candidate list is the handoff's,
    // deliberately: the page and a MemQL OS link must not disagree about
    // whether a file is in this workspace.
    outer: for (const folder of vscode.workspace.workspaceFolders ?? []) {
      for (const relative of workspaceCandidates(construct.originPath)) {
        const candidate = vscode.Uri.joinPath(folder.uri, relative);
        try {
          await vscode.workspace.fs.stat(candidate);
          found = candidate;
          break outer;
        } catch {
          // Not in this folder under this layout; try the next.
        }
      }
    }
    if (this.disposed || lookup !== this.lookup) return;
    this.fileUri = found;
    this.source = found === undefined ? "cluster" : "workspace";
    this.render();
  }

  private async onMessage(raw: unknown): Promise<void> {
    const message = pageMessage(raw);
    if (message === undefined) return;
    if (message.type === CONSTRUCT_ACTS.retry) {
      this.retry?.();
      return;
    }
    if (message.type === CONSTRUCT_ACTS.details) {
      this.detailsOpen = message.open === true;
      return;
    }
    if (this.shown.kind !== "construct") return;
    const construct = this.shown.construct;
    switch (message.type) {
      case CONSTRUCT_ACTS.run:
      case CONSTRUCT_ACTS.runWith:
        // A view-only construct draws no Run, and the webview channel is
        // untrusted: a message naming an act the page never drew stops here.
        if (construct.runnableKind === undefined) return;
        await this.deps.run(construct, message.type === CONSTRUCT_ACTS.runWith);
        return;
      case CONSTRUCT_ACTS.openSource:
        await this.openSource(construct);
        return;
      case CONSTRUCT_ACTS.browseRows:
        if (construct.kind === "concept") await this.deps.browseRows(construct, this.cluster);
        return;
      case CONSTRUCT_ACTS.openInOs:
        if (construct.kind === "concept") await this.deps.openInOs(construct, this.cluster);
        return;
    }
  }

  /** The workspace file when it is here, the cluster's copy when it is not. */
  private async openSource(construct: CatalogConstruct): Promise<void> {
    if (this.fileUri !== undefined) {
      try {
        await openFileAtSignature(this.fileUri, construct.kind, construct.name);
        this.error = "";
      } catch {
        // Gone since the lookup (deleted, renamed, a folder closed). Said on
        // the page, and the lookup runs again so the act follows the file.
        this.error = "Couldn't open the file. It may have moved.";
        void this.resolveFile(construct);
      }
      this.render();
      return;
    }
    if (construct.originPath !== "") await this.deps.viewSourceFromCluster(construct, this.cluster);
  }

  private render(): void {
    if (this.disposed) return;
    const [screen, parts] = this.parts();
    this.live.render(screen, parts);
  }

  private parts(): [string, RegionParts] {
    switch (this.shown.kind) {
      case "loading":
        return ["loading", constructLoadingParts()];
      case "failed":
        return ["failed", constructFailedParts(this.shown.name, this.shown.message)];
      case "construct": {
        const { construct } = this.shown;
        return [
          `construct:${construct.kind}:${construct.name}`,
          constructPageParts({
            construct,
            cluster: this.cluster,
            source: this.source,
            detailsOpen: this.detailsOpen,
            error: this.error,
          }),
        ];
      }
    }
  }
}

function titleOf(shown: Shown): string {
  return shown.kind === "construct" ? shown.construct.name : shown.name;
}

/** A CSP nonce, from a CSPRNG: a predictable one is one an injection can carry. */
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
