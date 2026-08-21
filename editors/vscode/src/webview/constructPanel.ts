// One construct's definition, and the way back to its source.
//
// The page is read-only, and that is a rule rather than a stage of
// development: editing happens in a `.memql` file, and a surface that could
// change a construct from here would be a second authoring path for something
// the language server already owns.
//
// JUMP-TO-SOURCE HAS THREE OUTCOMES, and each is a different sentence:
//
//   the file is in this workspace  -> open it, revealed at the signature
//   the file is NOT in this workspace -> say so, naming the path, AND offer to
//       read it from the cluster (memql#4248). The catalog reports a path
//       relative to the CLUSTER's tree, and the cluster is not obliged to be
//       the checkout this editor has open -- a remote cluster usually is not.
//       This used to be the end of the road; it is not, because the cluster
//       that loaded the construct serves the file too.
//   there is no file at all -> the construct is PROMOTED. Its source is in the
//       page already, rendered from what the cluster holds, and labelled as
//       living in the database. That is the honest rendering, and it is where
//       a developer first meets the seeded-versus-trained distinction.
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

import { escapeHtml, viewKitStyles } from "@znasllc-io/memql-view-kit";

import { brandStrip, brandStyleBlock } from "./brandTokens.js";

import type { CatalogConstruct } from "../state/constructCatalog.js";
import { renderConstructPage } from "./constructScreens.js";
import {
  catalogAutomationTarget,
  catalogRunTarget,
  isAutomationRun,
  offersRun,
} from "../constructs/catalogTarget.js";
import { COMMAND_RUN, COMMAND_RUN_AUTOMATION, COMMAND_RUN_WITH } from "../constructs/runnable.js";
import { signatureLine } from "../constructs/signature.js";
import { workspaceCandidates } from "../handoff/resolve.js";

/**
 * What the page needs from the host that it cannot reach itself.
 *
 * Exactly one entry, and it is INJECTED rather than imported because the fetch
 * needs the live connection -- which lives in extension.ts and which a webview
 * module has no business reading. The panel posts an intent; the host decides
 * whether there is a cluster to serve it.
 */
export interface ConstructPanelDeps {
  viewSourceFromCluster: (construct: CatalogConstruct) => Promise<void>;
}

/**
 * Opens a file on disk and reveals the construct's declaration.
 *
 * EXPORTED so the portal handoff (memql#4251) lands on a construct the same way
 * a click on this page does. The two arrived at the same file by different
 * routes -- one from a webview message, one from a `vscode://` link -- and a
 * second copy of "open, find the signature, reveal" is a second answer to where
 * the cursor ends up, which is the whole visible behaviour of both.
 *
 * A signature the search does not find opens the file at the top rather than
 * guessing, for the reason this module's header gives: landing on the wrong
 * line is worse than landing on the first one.
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

export class ConstructPanel {
  private static open_: ConstructPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly disposables: vscode.Disposable[] = [];
  private construct: CatalogConstruct;
  private deps: ConstructPanelDeps;
  private fileUri: vscode.Uri | undefined;
  private error = "";
  private disposed = false;

  static open(
    context: vscode.ExtensionContext,
    construct: CatalogConstruct,
    deps: ConstructPanelDeps,
  ): ConstructPanel {
    const existing = ConstructPanel.open_;
    if (existing !== undefined && !existing.disposed) {
      existing.panel.reveal(vscode.ViewColumn.Beside);
      // Re-pointed along with the construct. The panel is a SINGLETON reused
      // across opens, so what it holds must be what THIS opener supplied,
      // rather than whatever the first one happened to hand over.
      existing.deps = deps;
      existing.pointAt(construct);
      return existing;
    }
    const panel = new ConstructPanel(context, construct, deps);
    ConstructPanel.open_ = panel;
    return panel;
  }

  private constructor(
    _context: vscode.ExtensionContext,
    construct: CatalogConstruct,
    deps: ConstructPanelDeps,
  ) {
    this.construct = construct;
    this.deps = deps;
    this.panel = vscode.window.createWebviewPanel(
      "memqlConstruct",
      `Construct: ${construct.name}`,
      vscode.ViewColumn.Beside,
      { enableScripts: true, retainContextWhenHidden: true },
    );
    this.disposables.push(
      this.panel.onDidDispose(() => {
        this.disposed = true;
        if (ConstructPanel.open_ === this) ConstructPanel.open_ = undefined;
        for (const d of this.disposables) d.dispose();
      }),
      this.panel.webview.onDidReceiveMessage((message: unknown) => {
        void this.onMessage(message);
      }),
    );
    void this.resolveFile();
  }

  private pointAt(construct: CatalogConstruct): void {
    this.construct = construct;
    this.fileUri = undefined;
    this.error = "";
    this.panel.title = `Construct: ${construct.name}`;
    void this.resolveFile();
  }

  /**
   * Whether this construct's file is reachable from here.
   *
   * The catalog's path is relative to the CLUSTER's tree. It resolves against
   * this workspace only when the two happen to be the same checkout, which is
   * the ordinary case for a local cluster and the unusual one for a remote --
   * so the answer is looked up rather than assumed, and the page says which it
   * got.
   */
  private async resolveFile(): Promise<void> {
    this.fileUri = undefined;
    const rel = this.construct.originPath;
    if (rel !== "") {
      // TWO LAYOUTS PER FOLDER (memql#4251). The catalog's path is relative to
      // the DSL TREE ROOT (`cognition/queries.memql`) and a repository checkout
      // keeps that tree under `dsl/`, so trying only the path as reported makes
      // an engine checkout -- the folder a local cluster's operator most likely
      // has open -- look like a machine that does not have the file. The
      // candidate list is the handoff's, deliberately: the page and the portal
      // link must not disagree about whether a file is in this workspace.
      outer: for (const folder of vscode.workspace.workspaceFolders ?? []) {
        for (const relative of workspaceCandidates(rel)) {
          const candidate = vscode.Uri.joinPath(folder.uri, relative);
          try {
            await vscode.workspace.fs.stat(candidate);
            this.fileUri = candidate;
            break outer;
          } catch {
            // Not in this folder under this layout; try the next.
          }
        }
      }
    }
    this.render();
  }

  private async onMessage(message: unknown): Promise<void> {
    if (message === null || typeof message !== "object") return;
    const { type } = message as { type?: unknown };
    if (type === "run" || type === "runWith") {
      await this.run(type === "runWith");
      return;
    }
    if (type === "viewSourceFromCluster") {
      await this.deps.viewSourceFromCluster(this.construct);
      return;
    }
    if (type !== "openFile") return;
    await this.openFile();
  }

  /**
   * Runs the construct through the ONE run path.
   *
   * The same commands the CodeLens uses, taking the same `RunTarget` -- so the
   * write confirmation, the supersession token, the preflight and the Result
   * view are all the ones that already existed. A second run path here would
   * be a second answer to "what ran, against which cluster", including a
   * second write-confirmation path, which is the one thing memql#3309 exists
   * to keep single.
   */
  private async run(withArguments: boolean): Promise<void> {
    // AN AUTOMATION TAKES THE OTHER COMMAND, and the branch is here rather than
    // inside the run path because the two take different TARGETS: an
    // AutomationTarget carries a trigger and no args, a RunTarget carries args
    // and no trigger. Sending one where the other is expected does not fail --
    // it opens a form with nothing in it (memql#3805).
    const automation = catalogAutomationTarget(this.construct);
    if (automation !== undefined) {
      // `withArguments` has no meaning here: an automation's inputs live in its
      // own form, and the page draws no "Run with arguments" for it.
      await vscode.commands.executeCommand(COMMAND_RUN_AUTOMATION, automation);
      return;
    }
    const target = catalogRunTarget(this.construct);
    if (target === undefined) {
      // Unreachable through the page, which renders no button in that case.
      // Present because the webview channel is untrusted and a message naming
      // an action the page never drew must not reach the run path.
      return;
    }
    await vscode.commands.executeCommand(withArguments ? COMMAND_RUN_WITH : COMMAND_RUN, target);
  }

  /**
   * Opens the file from disk, or says why it could not.
   *
   * The not-in-workspace sentence is UNCHANGED and still true -- what changed
   * is that it is no longer the end of the conversation: the page draws the
   * cluster-source button beside it (memql#4248), so the reader is told what
   * happened and offered the other route in the same breath.
   */
  private async openFile(): Promise<void> {
    const uri = this.fileUri;
    if (uri === undefined) {
      this.error =
        this.construct.originPath === ""
          ? "This construct has no file -- it was promoted, and its source is below."
          : `${this.construct.originPath} is not in this workspace. The catalog reports a path relative to the cluster's own tree, which is not always the checkout you have open.`;
      this.render();
      return;
    }
    await openFileAtSignature(uri, this.construct.kind, this.construct.name);
  }

  private render(): void {
    if (this.disposed) return;
    const nonce = nonceValue();
    const body = renderConstructPage({
      construct: this.construct,
      fileInWorkspace: this.fileUri !== undefined,
      offerRun: offersRun(this.construct),
      automationRun: isAutomationRun(this.construct),
      // There IS a file and it is not here -- the one situation the cluster can
      // answer. Whether a cluster is actually connected is the host's question,
      // asked when the button is pressed rather than guessed at render time.
      offerClusterSource: this.construct.originPath !== "" && this.fileUri === undefined,
      error: this.error,
    });
    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(this.construct.name)}</title>
<style nonce="${nonce}">
${brandStyleBlock()}
${viewKitStyles}

  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0;
         padding: 16px 20px; max-width: 900px; }
  h1 { font-size: 1.2em; margin: 0 0 4px; }
  h2 { font-size: 1em; margin: 20px 0 6px; }
  .lede { color: var(--memql-muted); margin: 0 0 16px; }
  .facts { margin-bottom: 4px; }
  .fact { display: flex; gap: 8px; align-items: baseline; padding: 1px 0; }
  .fact-key { flex: none; min-width: 9em; color: var(--memql-muted); }
  .fact-value { min-width: 0; overflow-wrap: anywhere; }
  .args { display: block; }
  .arg { display: flex; gap: 8px; align-items: baseline; padding: 2px 0; }
  .arg-name { flex: none; min-width: 12em; }
  .arg-type { flex: none; min-width: 6em; color: var(--memql-muted); }
  .arg-flags { flex: none; min-width: 12em; color: var(--memql-muted); }
  .arg-description { min-width: 0; overflow-wrap: anywhere; }
  /* A required argument reads at full strength; an optional one is quieter,
     because the question a reader has is "what must I supply". */
  .arg[data-required="false"] .arg-name { opacity: 0.75; }
  .source { font-family: var(--vscode-editor-font-family, monospace);
            background: var(--memql-raised);
            border: 1px solid var(--memql-border);
            border-radius: 4px; padding: 8px 10px; margin: 6px 0 0;
            overflow-x: auto; white-space: pre; }
  .error { color: var(--memql-danger); margin-top: 3px; }
  .actions { display: flex; gap: 8px; margin: 16px 0; }
  button.primary, button.secondary {
    font: inherit; padding: 4px 12px; cursor: pointer; border-radius: 2px;
    border: 1px solid transparent; }
  button.primary { background: var(--vscode-button-background);
                   color: var(--vscode-button-foreground); }
  button.secondary { background: var(--vscode-button-secondaryBackground);
                     color: var(--vscode-button-secondaryForeground); }
</style>
</head>
<body>
${brandStrip("MemQL")}
${body}
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

/** A CSP nonce, from a CSPRNG: a predictable one is one an injection can carry. */
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
