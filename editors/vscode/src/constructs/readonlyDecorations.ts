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
// A LOCAL CLUSTER LOCKS NOTHING (memql#4244), so on one this file writes no
// patterns at all and every buffer stays open. What the CHECKOUT MATCH decides
// is the HINT: a local cluster is rebuilt from one directory, and a developer
// typing in a second clone of the same repository is editing a file that
// reaches nothing. That is a hover on an otherwise untouched file --
// `files.readonlyInclude` is not involved, because there is nothing to forbid --
// and the same fact is what the lens's "applies on the next Rebuild from
// checkout" wording keys on.
//
// IT WRITES INTO SOMEBODY'S REPOSITORY, so it is careful about what it owns.
// `files.readonlyInclude` is a shared object and an operator may have their own
// entries in it; this removes exactly the keys it wrote last time (recorded in
// workspaceState) and preserves every other key untouched. Clobbering the whole
// value would be a silent edit to a tracked file in their working tree -- and so
// is rewriting it to what it already said, which is why `writeSetting` does not
// write at all when nothing changed, and removes the key rather than leaving
// `{}` behind when nothing is marked.
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
  checkoutHint,
  constructsByPath,
  readonlyPatterns,
  readonlyVerdict,
  reasonBadge,
  reasonTooltip,
  showsCheckoutHint,
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
    // POSITIONAL, and the order is (badge, tooltip, color). It used to pass
    // `undefined` first and the badge into the tooltip slot, then overwrite the
    // tooltip on the next line -- so a read-only file has never carried a badge
    // and `reasonBadge` reached nothing but its own test (memql#4244).
    return new vscode.FileDecoration(
      reasonBadge(verdict.reason),
      reasonTooltip(verdict.reason, this.clusterName),
      new vscode.ThemeColor("disabledForeground"),
    );
  }

  /**
   * The hover for a file in a clone the cluster does not build from, or
   * undefined.
   *
   * WHICH FILES QUALIFY IS `showsCheckoutHint`'s ANSWER, not this method's --
   * the module header's rule about where decisions live. What is added here is
   * the one fact that module cannot know: whether there is a checkout PATH to
   * name. Without it the sentence would have an empty pair of brackets where
   * the directory goes, which is worse than saying nothing.
   *
   * NO DECORATION WHEN THIS IS THE CHECKOUT. A developer in the right folder is
   * in the ordinary case, and a badge on every construct file would be a
   * permanent mark that stops being read -- the same argument the status bar
   * makes for staying silent when nothing needs attention. That the edit
   * applies on the next rebuild is said where a developer is looking at a
   * changed construct: the `edited` lens.
   */
  private checkoutHintFor(filePath: string): vscode.FileDecoration | undefined {
    if (this.checkout === "") return undefined;
    const shows = showsCheckoutHint({
      path: filePath,
      catalog: this.catalog,
      clusterLocal: this.clusterLocal,
      workspaceIsClusterCheckout: this.workspaceIsClusterCheckout,
    });
    if (!shows) return undefined;
    // ONE LETTER, for the reason `reasonBadge` states at length: a longer badge
    // is refused by the editor and takes the hover down with it.
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
   *
   * NO WRITE WHEN NOTHING CHANGED, and this is not an optimisation. The target
   * is `.vscode/settings.json`, which in the checkout a local cluster is
   * rebuilt from is a TRACKED FILE -- so a write that changes nothing still
   * shows up as a modified file in the developer's `git status`, and the header
   * above promises this module does not silently edit one. A local cluster
   * marks nothing (memql#4244), which is the ordinary state and used to
   * materialise `"files.readonlyInclude": {}` on every catalog load.
   *
   * AND AN EMPTY RESULT REMOVES THE KEY rather than writing `{}`. `undefined`
   * is how the configuration API deletes a setting; writing an empty object
   * would leave behind a line that means exactly what its absence means, in
   * somebody's repository, forever.
   *
   * The memento is updated either way. It records which keys are OURS to
   * withdraw next time, and that set changes when the cluster does even in the
   * runs where the file on disk does not.
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

    if (!sameSetting(current, next)) {
      await config.update(
        "readonlyInclude",
        Object.keys(next).length === 0 ? undefined : next,
        vscode.ConfigurationTarget.Workspace,
      );
    }
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

/**
 * Whether two `files.readonlyInclude` values say the same thing.
 *
 * Shallow on purpose: the value is a flat map of pattern to boolean, and a
 * deep walk would be modelling a shape this setting does not have. What it is
 * for is the write that is not made -- see `writeSetting`.
 */
function sameSetting(a: Record<string, boolean>, b: Record<string, boolean>): boolean {
  const keys = Object.keys(a);
  if (keys.length !== Object.keys(b).length) return false;
  return keys.every((key) => Object.prototype.hasOwnProperty.call(b, key) && a[key] === b[key]);
}
