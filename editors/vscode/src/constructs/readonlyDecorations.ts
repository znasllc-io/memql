// The read-only marking, wired into the editor.
//
// An ADAPTER, in the sense the no-vscode-import rule means it: every decision
// lives in constructs/readonly.ts, and this file converts a Uri to a
// workspace-relative path, a verdict to a FileDecoration, and a path list to a
// settings value. It decides nothing.
//
// TWO HALVES, BECAUSE ONE IS NOT ENOUGH:
//
//   `files.readonlyInclude`  -- what actually greys the buffer. VS Code reads
//       it for files it opens LATER, which is why it is a SETTING rather than
//       a per-file answer computed on open: the latter would arrive after the
//       editor had already decided.
//   a FileDecorationProvider  -- the badge and the hover. Without it a file is
//       read-only for a reason the operator cannot see, which reads as the
//       editor being broken rather than as a rule being applied.
//
// IT WRITES INTO SOMEBODY'S REPOSITORY, so it is careful about what it owns.
// `files.readonlyInclude` is a shared object and an operator may have their own
// entries in it; this removes exactly the keys it wrote last time (recorded in
// workspaceState) and preserves every other key untouched. Clobbering the whole
// value would be a silent edit to a tracked file in their working tree.
//
// AND IT IS A COURTESY, NOT THE CONTROL. The setting is overridable and this
// module does nothing to stop that -- what refuses a bad promotion is
// `PromoteAuthoredConstruct`, on the engine. The editor explains; the engine
// enforces.
//
// Refs: #3762 #3745

import * as vscode from "vscode";

import {
  constructsByPath,
  readonlyPatterns,
  readonlyVerdict,
  reasonBadge,
  reasonTooltip,
  type OriginatedConstruct,
} from "./readonly.js";

/** Where the keys written last time are remembered, so they can be withdrawn. */
const OWNED_KEYS = "memql.readonly.ownedKeys";

export class ReadonlyMarker implements vscode.FileDecorationProvider {
  private readonly changed = new vscode.EventEmitter<vscode.Uri[] | undefined>();
  readonly onDidChangeFileDecorations = this.changed.event;

  private catalog: ReadonlyMap<string, OriginatedConstruct[]> | undefined;
  private clusterLocal = false;
  private clusterName = "the selected cluster";

  constructor(private readonly memento: vscode.Memento) {}

  /**
   * Point at a cluster's catalog, or at nothing.
   *
   * `constructs === undefined` means NOT CONNECTED, which clears the marking
   * entirely -- see readonly.ts for why that direction is the safe one.
   */
  async update(
    constructs: readonly OriginatedConstruct[] | undefined,
    cluster: { name: string; local?: boolean } | undefined,
  ): Promise<void> {
    this.catalog = constructs === undefined ? undefined : constructsByPath(constructs);
    this.clusterLocal = cluster?.local === true;
    this.clusterName = cluster?.name ?? "the selected cluster";
    await this.writeSetting();
    // undefined = "all of them". Which files changed depends on the cluster as
    // well as the catalog, so enumerating would mean diffing two verdict sets
    // to save VS Code a cheap re-ask.
    this.changed.fire(undefined);
  }

  provideFileDecoration(uri: vscode.Uri): vscode.FileDecoration | undefined {
    const path = relativePath(uri);
    if (path === undefined) return undefined;
    const verdict = readonlyVerdict({
      path,
      catalog: this.catalog,
      clusterLocal: this.clusterLocal,
    });
    if (!verdict.readonly || verdict.reason === undefined) return undefined;
    const decoration = new vscode.FileDecoration(
      undefined,
      reasonBadge(verdict.reason),
      new vscode.ThemeColor("disabledForeground"),
    );
    decoration.tooltip = reasonTooltip(verdict.reason, this.clusterName);
    return decoration;
  }

  dispose(): void {
    this.changed.dispose();
  }

  /**
   * Rewrite `files.readonlyInclude`, touching only the keys this extension owns.
   *
   * No-op with no workspace folder: the setting target is the workspace, and
   * there is nothing to classify against in a bare window anyway.
   */
  private async writeSetting(): Promise<void> {
    if ((vscode.workspace.workspaceFolders ?? []).length === 0) return;

    const owned = this.memento.get<string[]>(OWNED_KEYS, []);
    const config = vscode.workspace.getConfiguration("files");
    const current = config.get<Record<string, boolean>>("readonlyInclude") ?? {};

    const next: Record<string, boolean> = {};
    for (const [key, value] of Object.entries(current)) {
      // Everything that is not ours survives verbatim -- including a key an
      // operator added by hand that happens to name a construct file.
      if (!owned.includes(key)) next[key] = value;
    }
    const mine = readonlyPatterns({ catalog: this.catalog, clusterLocal: this.clusterLocal });
    for (const key of mine) next[key] = true;

    await config.update("readonlyInclude", next, vscode.ConfigurationTarget.Workspace);
    await this.memento.update(OWNED_KEYS, mine);
  }
}

/**
 * A workspace-relative POSIX path, or undefined when the file is outside every
 * workspace folder.
 *
 * Outside means unclassifiable, not editable-by-default -- but the two coincide
 * here, because the catalog's paths are relative to a tree and a file outside
 * every folder cannot match one.
 */
function relativePath(uri: vscode.Uri): string | undefined {
  const folder = vscode.workspace.getWorkspaceFolder(uri);
  if (folder === undefined) return undefined;
  return vscode.workspace.asRelativePath(uri, false).replace(/\\/g, "/");
}
