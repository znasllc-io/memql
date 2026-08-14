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
//   the file is NOT in this workspace -> say so, naming the path. The catalog
//       reports a path relative to the CLUSTER's tree, and the cluster is not
//       obliged to be the checkout this editor has open -- a remote cluster
//       usually is not.
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
// Refs: #3752 #3747

import { randomBytes } from "node:crypto";

import * as vscode from "vscode";

import { escapeHtml, viewKitStyles } from "@znasllc-io/memql-view-kit";

import type { CatalogConstruct } from "../state/constructCatalog.js";
import { renderConstructPage } from "./constructScreens.js";
import { catalogRunTarget, offersRun, runUnavailableReason } from "../constructs/catalogTarget.js";
import { COMMAND_RUN, COMMAND_RUN_WITH } from "../constructs/runnable.js";
import { signatureLine } from "../constructs/signature.js";

export class ConstructPanel {
  private static open_: ConstructPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly disposables: vscode.Disposable[] = [];
  private construct: CatalogConstruct;
  private fileUri: vscode.Uri | undefined;
  private error = "";
  private disposed = false;

  static open(context: vscode.ExtensionContext, construct: CatalogConstruct): ConstructPanel {
    const existing = ConstructPanel.open_;
    if (existing !== undefined && !existing.disposed) {
      existing.panel.reveal(vscode.ViewColumn.Beside);
      existing.pointAt(construct);
      return existing;
    }
    const panel = new ConstructPanel(context, construct);
    ConstructPanel.open_ = panel;
    return panel;
  }

  private constructor(_context: vscode.ExtensionContext, construct: CatalogConstruct) {
    this.construct = construct;
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
      for (const folder of vscode.workspace.workspaceFolders ?? []) {
        const candidate = vscode.Uri.joinPath(folder.uri, rel);
        try {
          await vscode.workspace.fs.stat(candidate);
          this.fileUri = candidate;
          break;
        } catch {
          // Not in this folder; try the next.
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
    const target = catalogRunTarget(this.construct);
    if (target === undefined) {
      // Unreachable through the page, which renders no button in that case.
      // Present because the webview channel is untrusted and a message naming
      // an action the page never drew must not reach the run path.
      return;
    }
    await vscode.commands.executeCommand(withArguments ? COMMAND_RUN_WITH : COMMAND_RUN, target);
  }

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
    const document = await vscode.workspace.openTextDocument(uri);
    const line = signatureLine(document.getText(), this.construct.kind, this.construct.name);
    const editor = await vscode.window.showTextDocument(document, {
      viewColumn: vscode.ViewColumn.One,
      preview: false,
    });
    if (line >= 0) {
      const at = new vscode.Position(line, 0);
      editor.selection = new vscode.Selection(at, at);
      editor.revealRange(new vscode.Range(at, at), vscode.TextEditorRevealType.InCenter);
    }
  }

  private render(): void {
    if (this.disposed) return;
    const nonce = nonceValue();
    const body = renderConstructPage({
      construct: this.construct,
      fileInWorkspace: this.fileUri !== undefined,
      offerRun: offersRun(this.construct),
      runUnavailable: runUnavailableReason(this.construct),
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
         padding: 16px 20px; max-width: 900px; }
  h1 { font-size: 1.2em; margin: 0 0 4px; }
  h2 { font-size: 1em; margin: 20px 0 6px; }
  .lede { color: var(--vscode-descriptionForeground); margin: 0 0 16px; }
  .facts { margin-bottom: 4px; }
  .fact { display: flex; gap: 8px; align-items: baseline; padding: 1px 0; }
  .fact-key { flex: none; min-width: 9em; color: var(--vscode-descriptionForeground); }
  .fact-value { min-width: 0; overflow-wrap: anywhere; }
  .args { display: block; }
  .arg { display: flex; gap: 8px; align-items: baseline; padding: 2px 0; }
  .arg-name { flex: none; min-width: 12em; }
  .arg-type { flex: none; min-width: 6em; color: var(--vscode-descriptionForeground); }
  .arg-flags { flex: none; min-width: 12em; color: var(--vscode-descriptionForeground); }
  .arg-description { min-width: 0; overflow-wrap: anywhere; }
  /* A required argument reads at full strength; an optional one is quieter,
     because the question a reader has is "what must I supply". */
  .arg[data-required="false"] .arg-name { opacity: 0.75; }
  .source { font-family: var(--vscode-editor-font-family, monospace);
            background: var(--vscode-textCodeBlock-background, transparent);
            border: 1px solid var(--vscode-panel-border);
            border-radius: 4px; padding: 8px 10px; margin: 6px 0 0;
            overflow-x: auto; white-space: pre; }
  .error { color: var(--vscode-inputValidation-errorForeground,
                   var(--vscode-editorError-foreground)); margin-top: 3px; }
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
