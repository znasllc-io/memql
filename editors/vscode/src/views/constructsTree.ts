// The Constructs tree: every construct the cluster has loaded, by kind.
//
//   Constructs
//   |- queries (118)      run
//   |  |- cognition (44)
//   |  |  \- spaceParticipants
//   |  \- identity (12)
//   |- concepts (23)      view only
//   \- ...
//
// A DIFFERENT QUESTION FROM THE PACK BROWSER. That answers "show me this
// file"; this answers "what do you have" -- read from the live registries, so
// a promoted construct appears the moment it is promoted and a demoted one
// disappears.
//
// THREE RULES THE SHAPE DEPENDS ON.
//
//  1. A VIEW-ONLY KIND RENDERS NO RUN AFFORDANCE -- not a disabled one. The
//     absence is the statement. A greyed-out Run beside `concept` invites the
//     question "why not", which is a question about the kind rather than about
//     this construct, and the tree is the wrong place to answer it.
//  2. AN UNREACHABLE CLUSTER IS NOT AN EMPTY CATALOG. An empty list reads as
//     "this cluster has no constructs", which is the one wrong answer
//     available. So does a version mismatch, which gets its own row naming the
//     message. WITH NO CLUSTER SELECTED, though, the view IS empty -- returning
//     `[]` is what lets the manifest's `viewsWelcome` render, and VS Code draws
//     welcome content only over a genuinely empty tree. The synthetic
//     "Not connected" row this file used to return is gone for exactly that
//     reason: it duplicated the welcome's sentence and suppressed the welcome
//     while doing it (memql#4425).
//  3. NOTHING HERE MUTATES. Editing happens in a `.memql` file; this view
//     reads.
//
// Renders state and owns none: the grouping, the counts, the kind vocabulary
// and the wire-to-run-path narrowing are all state/constructCatalog.ts, under
// bare `node --test`.
//
// Refs: #4425 #4423 #3752 #3747

import * as vscode from "vscode";
import { briefMessage } from "../state/diagnostics.js";

import type { ConnectionManager } from "../connection/manager.js";
import type { ConnectionContextSource } from "../state/connectionContext.js";
import {
  type CatalogConstruct,
  type CatalogState,
  type KindGroup,
  type NamespaceGroup,
} from "../state/constructCatalog.js";

export type ConstructNode =
  | { kind: "group"; group: KindGroup }
  | { kind: "namespace"; group: KindGroup; namespace: NamespaceGroup }
  | { kind: "construct"; construct: CatalogConstruct }
  /** Unreachable, a version mismatch, or a failed read -- never a blank view. */
  | { kind: "state"; state: CatalogState };

export interface ConstructsTreeDeps {
  connections: ConnectionManager;
  /**
   * The two context keys, read rather than re-derived (design D1).
   *
   * This view held the ConnectionManager already and could ask it directly.
   * It does not, because "is a cluster selected" now has ONE answer that the
   * manifest's `when` clauses are also keyed on, and a provider that computed
   * its own would be able to disagree with the workbench about whether the
   * welcome should be showing -- which presents as a blank panel, not as an
   * error.
   */
  connectionContext: ConnectionContextSource;
  /** Reads the catalog over the live connection. */
  load: () => Promise<CatalogState>;
}

export class ConstructsTreeProvider implements vscode.TreeDataProvider<ConstructNode> {
  private readonly changed = new vscode.EventEmitter<ConstructNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  private state: CatalogState = { kind: "loading" };
  private inflight: Promise<void> | undefined;

  constructor(private readonly deps: ConstructsTreeDeps) {
    // A connect or a drop changes the answer completely: the catalog is the
    // connected cluster's, and there is no catalog without one.
    this.deps.connections.onDidChangeState(() => this.refresh());
  }

  refresh(): void {
    this.state = { kind: "loading" };
    this.changed.fire(undefined);
  }

  async getChildren(element?: ConstructNode): Promise<ConstructNode[]> {
    if (element === undefined) {
      // EMPTY, so the welcome can render. Before the load, so a view nobody has
      // selected a cluster for issues no ListConstructs call.
      if (!this.deps.connectionContext().clusterSelected) return [];
      await this.ensureLoaded();
      if (this.state.kind !== "loaded") return [{ kind: "state", state: this.state }];
      // A connected cluster with genuinely no constructs is not a state to
      // explain -- it is a fact, and an empty tree says it. Every OTHER empty
      // tree is one of the rows above.
      return this.state.groups.map((group) => ({ kind: "group" as const, group }));
    }
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

  getTreeItem(node: ConstructNode): vscode.TreeItem {
    switch (node.kind) {
      case "state":
        return stateItem(node.state);
      case "group": {
        const item = new vscode.TreeItem(
          node.group.label,
          vscode.TreeItemCollapsibleState.Collapsed,
        );
        item.description = `${node.group.count}`;
        item.contextValue = "memqlConstructKind";
        item.iconPath = new vscode.ThemeIcon(node.group.runnable ? "play-circle" : "symbol-misc");
        item.tooltip = node.group.runnable
          ? `${node.group.count} ${node.group.label}, which can be run`
          : `${node.group.count} ${node.group.label}, view only`;
        return item;
      }
      case "namespace": {
        const item = new vscode.TreeItem(
          node.namespace.namespace,
          vscode.TreeItemCollapsibleState.Collapsed,
        );
        item.description = `${node.namespace.constructs.length}`;
        item.contextValue = "memqlConstructNamespace";
        item.iconPath = new vscode.ThemeIcon("symbol-namespace");
        return item;
      }
      case "construct":
        return constructItem(node);
    }
  }

  private async ensureLoaded(): Promise<void> {
    if (this.state.kind !== "loading") return;
    // One in-flight read shared rather than duplicated: VS Code asks for the
    // root more than once while a view is being revealed, and the catalog is a
    // whole-registry read.
    if (this.inflight !== undefined) return this.inflight;
    this.inflight = this.deps
      .load()
      .then((state) => {
        this.state = state;
      })
      .finally(() => {
        this.inflight = undefined;
      });
    return this.inflight;
  }
}

function constructItem(node: Extract<ConstructNode, { kind: "construct" }>): vscode.TreeItem {
  const { construct } = node;
  const item = new vscode.TreeItem(construct.name, vscode.TreeItemCollapsibleState.None);
  item.description = construct.description;
  item.tooltip = tooltipFor(construct);
  // TWO CONTEXT VALUES, because the run affordance is contributed by a `when`
  // clause and a view-only construct must not carry one at all -- not even
  // disabled. The absence is the statement.
  item.contextValue = construct.runnableKind === undefined
    ? "memqlConstruct"
    : "memqlRunnableConstruct";
  item.iconPath = originIcon(construct.origin);
  item.command = {
    command: "memql.constructs.open",
    title: "Open Construct",
    arguments: [node],
  };
  return item;
}

function tooltipFor(construct: CatalogConstruct): vscode.MarkdownString {
  const lines = [
    `**${construct.name}**`,
    "",
    `kind: \`${construct.kind}\``,
    `origin: \`${construct.origin}\``,
  ];
  if (construct.boundConcept !== "") lines.push(`bound concept: \`${construct.boundConcept}\``);
  if (construct.originPath !== "") lines.push(`file: \`${construct.originPath}\``);
  // A promoted construct has no file, and saying where it DOES live is the
  // point -- it is where a developer first meets the seeded-vs-trained split.
  if (construct.origin === "promoted") lines.push("_lives in the cluster's database_");
  if (construct.description !== "") lines.push("", construct.description);
  const md = new vscode.MarkdownString(lines.join("\n"));
  md.supportHtml = false;
  return md;
}

/**
 * The origin, as an icon.
 *
 * All four are distinguishable at a glance, which is an acceptance item: a
 * `promoted` construct is the one that exists only in the cluster, and a
 * developer needs to see that before they go looking for its file.
 *
 * `staged` shares `promoted`'s database icon and takes a different COLOUR, and
 * that pairing is the point: the two are the same fact about where the
 * construct lives, and differ only in who can call it. A fourth silhouette
 * would say they were unrelated.
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

function stateItem(state: CatalogState): vscode.TreeItem {
  switch (state.kind) {
    case "loading": {
      const item = new vscode.TreeItem("Reading the catalog...", vscode.TreeItemCollapsibleState.None);
      item.iconPath = new vscode.ThemeIcon("loading~spin");
      return item;
    }
    case "unreachable": {
      // REACHED ONLY WITH A CLUSTER SELECTED -- getChildren returns `[]` before
      // the load otherwise -- so this row describes a cluster that was chosen
      // and has not answered, never "you have chosen nothing". The wording says
      // which of the two it is; the welcome says the other.
      const item = new vscode.TreeItem("Cluster not answering", vscode.TreeItemCollapsibleState.None);
      item.description = "no live session, so its constructs could not be listed";
      item.tooltip =
        "A cluster is selected but this editor holds no session to it. " +
        "Open the Clusters view to reconnect or sign in.";
      item.contextValue = "memqlConstructsUnreachable";
      item.iconPath = new vscode.ThemeIcon("debug-disconnect", new vscode.ThemeColor("charts.yellow"));
      item.command = { command: "memql.clusters.select", title: "Select Cluster" };
      return item;
    }
    case "versionMismatch": {
      const item = new vscode.TreeItem("ListConstructs is not available", vscode.TreeItemCollapsibleState.None);
      item.description = state.message;
      item.tooltip = state.message;
      item.contextValue = "memqlConstructsUnavailable";
      item.iconPath = new vscode.ThemeIcon("warning", new vscode.ThemeColor("charts.yellow"));
      return item;
    }
    case "failed": {
      const item = new vscode.TreeItem("Failed to read the catalog", vscode.TreeItemCollapsibleState.None);
      // A BRIEF verdict, not the raw error (memql#4194, audit 17): the full
      // text is recorded to the MemQL Connection channel where the fetch
      // failed, and a sidebar row is no place for a stack fragment.
      item.description = briefMessage(state.message, 60);
      item.tooltip = `${briefMessage(state.message)}\nFull error: the MemQL Connection output channel.`;
      item.contextValue = "memqlConstructsError";
      item.iconPath = new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
      return item;
    }
    case "loaded": {
      // Unreachable: a loaded state never becomes a row. Present so a future
      // variant cannot fall through to something arbitrary.
      const item = new vscode.TreeItem("", vscode.TreeItemCollapsibleState.None);
      return item;
    }
  }
}
