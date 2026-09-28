// The Constructs tree: every construct the cluster has loaded, by kind.
//
//   Constructs
//   |- Queries          118
//   |  |- cognition      44
//   |  |  \- spaceParticipants        [Run]
//   |  \- identity       12
//   |- Concepts          23
//   \- ...
//
// A DIFFERENT QUESTION FROM THE PACK BROWSER. That answers "show me this
// file"; this answers "what do you have" -- read from the live registries, so
// a promoted construct appears once it is promoted and a demoted one
// disappears. Nothing pushes those changes, so the view keeps its Refresh, and
// the training acts refresh it for themselves when they change the catalog.
//
// FOUR RULES THE SHAPE DEPENDS ON.
//
//  1. A VIEW-ONLY KIND RENDERS NO RUN AFFORDANCE -- not a disabled one. A
//     runnable construct's row carries `memqlRunnableConstruct`, and the
//     manifest's inline Run is keyed on exactly that value; every other row
//     carries `memqlConstruct` and gets nothing.
//  2. ROWS ONLY FOR A LIVE SESSION. Every other state of the connection leaves
//     the tree EMPTY, so the manifest's welcome for that state renders over it
//     with the one act that fixes it (state/clusterViewState.ts). While the
//     dial is in flight the view shows its own progress bar and no text.
//  3. A FAILED READ IS NOT AN EMPTY CATALOG. A connected cluster that could
//     not be read, or is too old to answer, gets a row saying so; a connected
//     cluster that genuinely has no constructs gets the view's message.
//  4. A READ BELONGS TO THE GENERATION THAT STARTED IT. Every refresh -- a
//     connection change, a Refresh click, a promote -- begins a new generation,
//     and a read that settles for an older one is dropped rather than painted.
//     Without this a catalog read started while dialing could land after the
//     session came up and leave "not answering" on screen for a live cluster.
//
// Renders state and owns none: the grouping, the counts, the kind vocabulary
// and the wire-to-run-path narrowing are all state/constructCatalog.ts, under
// bare `node --test`.
//
// Refs: #4425 #4423 #3752 #3747

import * as vscode from "vscode";

import { Latest, type LatestToken } from "../async/latest.js";
import type { ConnectionManager } from "../connection/manager.js";
import { clusterViewState } from "../state/clusterViewState.js";
import { briefMessage } from "../state/diagnostics.js";
import {
  ORIGIN_LABELS,
  kindWord,
  type CatalogConstruct,
  type CatalogState,
  type KindGroup,
  type NamespaceGroup,
} from "../state/constructCatalog.js";

export type ConstructNode =
  | { kind: "group"; group: KindGroup }
  | { kind: "namespace"; group: KindGroup; namespace: NamespaceGroup }
  | { kind: "construct"; construct: CatalogConstruct }
  /** A version mismatch or a failed read of a CONNECTED cluster -- never a blank view. */
  | { kind: "state"; state: CatalogState };

export interface ConstructsTreeDeps {
  /** The connection this view reads, and whose changes it follows. */
  connections: Pick<ConnectionManager, "state" | "onDidChangeState">;
  /** Reads the catalog over the live connection. Called only while connected. */
  load: () => Promise<CatalogState>;
  /**
   * The view has no live session to read from. Whatever rode the last read --
   * the read-only marking of `.memql` files -- is cleared here, because a
   * marking kept past its connection is a claim nothing backs any more.
   */
  unavailable?: () => void | Promise<void>;
  /**
   * The line VS Code draws above the rows (`TreeView.message`): here only the
   * sentence for a connected cluster with nothing loaded.
   */
  setMessage?: (message: string | undefined) => void;
  /** The view's id, for its own progress bar while dialing. */
  viewId?: string;
}

/** What the view says when a connected cluster has loaded nothing. */
export const NO_CONSTRUCTS_MESSAGE = "This cluster has no constructs loaded.";

export class ConstructsTreeProvider implements vscode.TreeDataProvider<ConstructNode> {
  private readonly changed = new vscode.EventEmitter<ConstructNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  private readonly generation = new Latest<"catalog">();
  /** The current generation's answer, once it has one. */
  private state: CatalogState | undefined;
  /** The current generation's read, shared by every caller that asks while it runs. */
  private inflight: { token: LatestToken<"catalog">; promise: Promise<CatalogState> } | undefined;
  /** Ends the progress bar drawn while dialing. */
  private endProgress: (() => void) | undefined;

  constructor(private readonly deps: ConstructsTreeDeps) {
    // A connect or a drop changes the answer completely: the catalog is the
    // connected cluster's, and there is no catalog without one.
    this.deps.connections.onDidChangeState(() => this.onConnectionChange());
    this.onConnectionChange();
  }

  /** Start over: a new generation, and the view asks again. */
  refresh(): void {
    this.generation.invalidate();
    this.state = undefined;
    this.inflight = undefined;
    this.changed.fire(undefined);
  }

  private onConnectionChange(): void {
    this.endProgress?.();
    this.endProgress = undefined;
    const view = clusterViewState(this.deps.connections.state);
    if (view !== "connected") {
      this.deps.setMessage?.(undefined);
      void this.deps.unavailable?.();
    }
    if (view === "connecting" && this.deps.viewId !== undefined) {
      // The view's own progress bar, and no text: a dial is not a state to
      // explain, and it ends with a state change that ends this too.
      const done = new Promise<void>((resolve) => {
        this.endProgress = resolve;
      });
      void vscode.window.withProgress({ location: { viewId: this.deps.viewId } }, () => done);
    }
    this.refresh();
  }

  async getChildren(element?: ConstructNode): Promise<ConstructNode[]> {
    if (element === undefined) return this.rootChildren();
    if (element.kind === "group") {
      return element.group.namespaces.map((namespace) => ({
        kind: "namespace" as const,
        group: element.group,
        namespace,
      }));
    }
    if (element.kind === "namespace") {
      return element.namespace.constructs.map((construct) => ({
        kind: "construct" as const,
        construct,
      }));
    }
    return [];
  }

  private async rootChildren(): Promise<ConstructNode[]> {
    for (;;) {
      // EMPTY for every state but a live session, so the welcome renders --
      // and BEFORE the read, so a view with nothing to read issues no call.
      if (clusterViewState(this.deps.connections.state) !== "connected") return [];
      const token = this.generation.current;
      const state = await this.read(token);
      // Overtaken while it ran: answer for the generation that is current NOW,
      // rather than painting a read the view has already moved past.
      if (!this.generation.isCurrent(token)) continue;
      if (state.kind !== "loaded") {
        this.deps.setMessage?.(undefined);
        return [{ kind: "state", state }];
      }
      this.deps.setMessage?.(state.total === 0 ? NO_CONSTRUCTS_MESSAGE : undefined);
      return state.groups.map((group) => ({ kind: "group" as const, group }));
    }
  }

  private read(token: LatestToken<"catalog">): Promise<CatalogState> {
    if (this.state !== undefined) return Promise.resolve(this.state);
    // One read shared rather than duplicated: VS Code asks for the root more
    // than once while a view is being revealed, and the catalog is a
    // whole-registry read.
    if (this.inflight !== undefined && this.inflight.token === token) return this.inflight.promise;
    const promise = this.deps.load().then((state) => {
      if (this.generation.isCurrent(token)) this.state = state;
      return state;
    });
    this.inflight = { token, promise };
    return promise;
  }

  getTreeItem(node: ConstructNode): vscode.TreeItem {
    switch (node.kind) {
      case "state":
        return stateItem(node.state);
      case "group": {
        const item = new vscode.TreeItem(node.group.label, vscode.TreeItemCollapsibleState.Collapsed);
        item.description = `${node.group.count}`;
        item.contextValue = "memqlConstructKind";
        item.iconPath = new vscode.ThemeIcon(kindIcon(node.group.kind));
        return item;
      }
      case "namespace": {
        const item = new vscode.TreeItem(node.namespace.namespace, vscode.TreeItemCollapsibleState.Collapsed);
        item.description = `${node.namespace.constructs.length}`;
        item.contextValue = "memqlConstructNamespace";
        item.iconPath = new vscode.ThemeIcon("symbol-namespace");
        return item;
      }
      case "construct":
        return constructItem(node);
    }
  }
}

function constructItem(node: Extract<ConstructNode, { kind: "construct" }>): vscode.TreeItem {
  const { construct } = node;
  const item = new vscode.TreeItem(construct.name, vscode.TreeItemCollapsibleState.None);
  // SAID ONLY WHEN IT IS NEWS. Most rows are built in or from the bundle, and
  // a word on every row would be a column of the same word; the icon carries
  // those. A promoted or staged construct has no file, which is the thing a
  // reader needs to see before they go looking for one.
  if (construct.origin === "promoted" || construct.origin === "staged") {
    item.description = ORIGIN_LABELS[construct.origin];
  }
  item.tooltip = tooltipFor(construct);
  // TWO CONTEXT VALUES, because the inline Run is contributed by a `when`
  // clause and a view-only construct must not carry one at all.
  item.contextValue = construct.runnableKind === undefined ? "memqlConstruct" : "memqlRunnableConstruct";
  item.iconPath = originIcon(construct.origin);
  item.command = {
    command: "memql.constructs.open",
    title: "Open Construct",
    arguments: [node],
  };
  return item;
}

/** One line of facts, the description, then where it lives. */
function tooltipFor(construct: CatalogConstruct): vscode.MarkdownString {
  const facts = [kindWord(construct.kind), ORIGIN_LABELS[construct.origin]].filter((part) => part !== "");
  const lines = [`**${escapeMarkdown(construct.name)}** · ${facts.join(" · ")}`];
  if (construct.description !== "") lines.push("", escapeMarkdown(construct.description));
  if (construct.originPath !== "") lines.push("", `\`${construct.originPath}\``);
  const md = new vscode.MarkdownString(lines.join("\n"));
  md.supportHtml = false;
  return md;
}

function escapeMarkdown(text: string): string {
  return text.replace(/([\\`*_{}[\]()#+!|<>])/g, "\\$1");
}

/**
 * The origin, as an icon.
 *
 * All four are distinguishable at a glance: a `promoted` construct is the one
 * that exists only in the cluster, and a developer needs to see that before
 * they go looking for its file. `staged` shares `promoted`'s database icon and
 * takes a different colour -- the same place, a different audience.
 */
function originIcon(origin: CatalogConstruct["origin"]): vscode.ThemeIcon {
  switch (origin) {
    case "core":
      return new vscode.ThemeIcon("library");
    case "bundle":
      return new vscode.ThemeIcon("package");
    case "promoted":
      return new vscode.ThemeIcon("database", new vscode.ThemeColor("charts.purple"));
    case "staged":
      return new vscode.ThemeIcon("database", new vscode.ThemeColor("charts.orange"));
  }
}

/** One codicon per kind, so a group reads by shape before its label. */
export function kindIcon(kind: string): string {
  switch (kind) {
    case "query":
      return "search";
    case "mutation":
      return "edit";
    case "logic":
      return "symbol-function";
    case "tool":
      return "tools";
    case "automation":
      return "zap";
    case "concept":
      return "symbol-class";
    case "shape":
      return "symbol-structure";
    case "spec":
    case "trait":
      return "symbol-boolean";
    case "prompt":
      return "comment";
    case "provider":
      return "plug";
    case "builtin":
      return "symbol-method";
    case "policy":
      return "law";
    case "seed":
      return "symbol-constant";
    default:
      return "symbol-misc";
  }
}

function stateItem(state: CatalogState): vscode.TreeItem {
  switch (state.kind) {
    case "versionMismatch": {
      const item = new vscode.TreeItem(state.message, vscode.TreeItemCollapsibleState.None);
      item.contextValue = "memqlConstructsUnavailable";
      item.iconPath = new vscode.ThemeIcon("warning", new vscode.ThemeColor("charts.yellow"));
      return item;
    }
    case "failed": {
      // A BRIEF reason, not the raw error: the full text is recorded to the
      // MemQL Connection output where the read failed. The row's click is the
      // fix -- a read that failed is usually a read the next attempt makes.
      const item = new vscode.TreeItem("Couldn't load constructs", vscode.TreeItemCollapsibleState.None);
      item.description = briefMessage(state.message, 60);
      item.tooltip = `${briefMessage(state.message)}\nSelect to try again.`;
      item.contextValue = "memqlConstructsError";
      item.iconPath = new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
      item.command = { command: "memql.constructs.refresh", title: "Try Again" };
      return item;
    }
    case "loading":
    case "unreachable":
    case "loaded": {
      // Unreachable as rows: a loading read is the view's pending promise,
      // "unreachable" is the welcome's, and a loaded catalog is its groups.
      // Present so a future variant cannot fall through to something arbitrary.
      return new vscode.TreeItem("", vscode.TreeItemCollapsibleState.None);
    }
  }
}
