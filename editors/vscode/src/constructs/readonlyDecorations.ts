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
// AND ONE THING THAT IS NOT A LOCK (memql#4244). A local cluster rebuilds from
// ONE checkout, so a second clone of the same repository is editable -- it is
// the developer's file -- and yet nothing they write in it reaches that
// cluster. The decoration provider carries that as a HOVER on an otherwise
// untouched file; `files.readonlyInclude` is not involved, because there is
// nothing here to forbid.
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
// Refs: #4248 #3762 #3745

import * as path from "node:path";

import * as vscode from "vscode";

import { CLUSTER_DOCUMENT_SCHEME, safeDecode } from "./clusterDocument.js";
import {
  catalogKeyFor,
  checkoutHint,
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
  /** The directory the selected cluster is rebuilt from, or "" when unknown. */
  private checkout = "";
  /** Whether a workspace folder IS that directory. Computed once per update. */
  private workspaceIsClusterCheckout = false;

  constructor(private readonly memento: vscode.Memento) {}

  /**
   * Point at a cluster's catalog, or at nothing.
   *
   * `constructs === undefined` means NOT CONNECTED, which clears the marking
   * entirely -- see readonly.ts for why that direction is the safe one.
   */
  async update(
    constructs: readonly OriginatedConstruct[] | undefined,
    cluster: { name: string; local?: boolean; checkout?: string } | undefined,
  ): Promise<void> {
    this.catalog = constructs === undefined ? undefined : constructsByPath(constructs);
    this.clusterLocal = cluster?.local === true;
    this.clusterName = cluster?.name ?? "the selected cluster";
    this.checkout = cluster?.checkout ?? "";
    // HERE, not in provideFileDecoration: the answer is per-cluster, and the
    // decoration provider is asked once per file in view and again on every
    // repaint. The empty-checkout guard is load-bearing rather than tidy --
    // `path.resolve("")` is the process's working directory, so comparing
    // against "" could match a folder and unlock every file on the strength of
    // a receipt that records no checkout at all.
    this.workspaceIsClusterCheckout =
      this.checkout !== "" &&
      (vscode.workspace.workspaceFolders ?? []).some((f) => samePath(f.uri.fsPath, this.checkout));
    await this.writeSetting();
    // undefined = "all of them". Which files changed depends on the cluster as
    // well as the catalog, so enumerating would mean diffing two verdict sets
    // to save VS Code a cheap re-ask.
    this.changed.fire(undefined);
  }

  provideFileDecoration(uri: vscode.Uri): vscode.FileDecoration | undefined {
    // A CLUSTER DOCUMENT IS READ-ONLY BY CONSTRUCTION (memql#4248), not by the
    // catalog rule below: there is no file on this machine to write back to, so
    // the answer does not depend on a catalog and must not wait for one. Ahead
    // of the path logic because `relativePath` answers undefined for any uri
    // outside a workspace folder, which every cluster document is -- the badge
    // would simply never appear.
    if (uri.scheme === CLUSTER_DOCUMENT_SCHEME) {
      return {
        badge: "RO",
        // safeDecode, not decodeURIComponent: a cluster named with a literal
        // `%` makes the bare call throw URIError, and a throw inside a
        // FileDecorationProvider takes the badge with it silently.
        tooltip: `Served from ${safeDecode(uri.authority)} -- read-only. The file is not on this machine; this is the source the cluster loaded.`,
        propagate: false,
      };
    }
    const filePath = relativePath(uri);
    if (filePath === undefined) return undefined;
    const verdict = readonlyVerdict({
      path: filePath,
      catalog: this.catalog,
      clusterLocal: this.clusterLocal,
      workspaceIsClusterCheckout: this.workspaceIsClusterCheckout,
    });
    if (!verdict.readonly || verdict.reason === undefined) return this.checkoutHintFor(filePath);
    const decoration = new vscode.FileDecoration(
      undefined,
      reasonBadge(verdict.reason),
      new vscode.ThemeColor("disabledForeground"),
    );
    decoration.tooltip = reasonTooltip(verdict.reason, this.clusterName);
    return decoration;
  }

  /**
   * The hover for an editable file in the WRONG clone, or undefined.
   *
   * Four conditions on top of the editable verdict that got here, and each one
   * drops a case that would otherwise be told something false: the cluster must
   * be local (a remote one rebuilds from nothing this developer has), this
   * workspace must NOT be its checkout, the checkout must be known at all, and
   * the file must be one the cluster actually loaded -- a file the catalog has
   * never heard of is a new one, and a new file reaches the cluster by being
   * promoted rather than by sitting in the right directory.
   *
   * WHICH LEAVES IT NARROW TODAY, and worth saying so nobody reads its absence
   * as a defect: every catalog entry that carries a path is `core` or `bundle`
   * (promoted and staged constructs live in the database and report no file),
   * and in the wrong clone both of those are read-only -- so what a developer
   * there actually sees is `reasonTooltip`, which says the same thing about the
   * same folder. This is the branch for an editable file the cluster knows, and
   * it stays because the alternative is a surface that silently says nothing
   * the first time an origin does carry a path.
   */
  private checkoutHintFor(filePath: string): vscode.FileDecoration | undefined {
    if (!this.clusterLocal || this.workspaceIsClusterCheckout || this.checkout === "") {
      return undefined;
    }
    if (this.catalog?.has(catalogKeyFor(filePath)) !== true) return undefined;
    // ONE LETTER, which is what VS Code's explorer has room for. It is a marker
    // the hover explains rather than an abbreviation to decode -- and it is
    // deliberately not styled like the read-only badges, because nothing here
    // is read-only.
    const decoration = new vscode.FileDecoration("L", checkoutHint(this.clusterName, this.checkout));
    // A hint about ONE file. Propagating it up the tree would put the mark on
    // every ancestor folder of a checkout that is, itself, perfectly fine.
    decoration.propagate = false;
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
    const mine = readonlyPatterns({
      catalog: this.catalog,
      clusterLocal: this.clusterLocal,
      workspaceIsClusterCheckout: this.workspaceIsClusterCheckout,
    });
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

/**
 * Whether two paths name the same directory.
 *
 * `path.resolve` on both sides, so a trailing separator or a relative segment
 * in the receipt's `--dest` does not read as a different directory from the
 * folder VS Code reports. Case-insensitive on win32 only: the same two strings
 * differing in case are one directory there and two on Linux, and getting that
 * backwards either unlocks a checkout that is not the cluster's or fails to
 * unlock the one that is.
 *
 * Symlinks are NOT resolved. Doing so needs IO, and this runs inside an update
 * that must not block the editor; the honest failure -- a symlinked checkout
 * showing the hint -- is a sentence on a hover, which is recoverable, whereas
 * a blocking stat on every update is not.
 */
function samePath(a: string, b: string): boolean {
  const left = path.resolve(a);
  const right = path.resolve(b);
  return process.platform === "win32"
    ? left.toLowerCase() === right.toLowerCase()
    : left === right;
}
