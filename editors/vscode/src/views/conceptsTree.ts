// The Concepts tree: every registered concept on the connected cluster,
// grouped by domain.
//
// The list comes from ConceptsListMsg via the SDK's listConcepts, which the
// engine answers from its own registry -- so a concept added to the DSL shows
// up here with no client change. That is the whole point of a generic browser.

import * as vscode from "vscode";

import type { Concept } from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionManager } from "../connection/manager.js";

export type ConceptTreeNode =
  | { kind: "domain"; domain: string }
  | { kind: "concept"; concept: Concept }
  // error is the single synthetic row getChildren returns when
  // listConcepts() rejects -- the connection can drop between the state
  // check and the call. getChildren has no story for an unhandled
  // rejection reaching VS Code's tree API, so it is caught and rendered as
  // a row instead, matching the precedent in ClustersTreeProvider.
  | { kind: "error"; message: string };

export class ConceptsTreeProvider implements vscode.TreeDataProvider<ConceptTreeNode> {
  private readonly changed = new vscode.EventEmitter<ConceptTreeNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  // Cached so expanding a domain does not re-issue the list. Cleared whenever
  // the connection changes, since concepts are per-cluster.
  private cache: Concept[] | undefined;
  private error: string | undefined;

  constructor(private readonly connections: ConnectionManager) {
    this.connections.onDidChangeState(() => {
      this.cache = undefined;
      this.error = undefined;
      this.changed.fire(undefined);
    });
  }

  refresh(): void {
    this.cache = undefined;
    this.error = undefined;
    this.changed.fire(undefined);
  }

  private async load(): Promise<Concept[]> {
    if (this.cache !== undefined) return this.cache;
    const query = this.connections.query;
    if (query === undefined) return [];
    try {
      const concepts = await query.listConcepts();
      this.cache = concepts;
      this.error = undefined;
      return concepts;
    } catch (err) {
      this.error = err instanceof Error ? err.message : String(err);
      return [];
    }
  }

  async getChildren(element?: ConceptTreeNode): Promise<ConceptTreeNode[]> {
    const concepts = await this.load();

    if (element === undefined) {
      if (this.error !== undefined) {
        return [{ kind: "error", message: this.error }];
      }
      const domains = [...new Set(concepts.map((c) => c.domain))].sort();
      return domains.map((domain) => ({ kind: "domain", domain }));
    }

    if (element.kind === "domain") {
      return concepts
        .filter((c) => c.domain === element.domain)
        .sort((a, b) => a.entity.localeCompare(b.entity))
        .map((concept) => ({ kind: "concept", concept }));
    }

    return [];
  }

  getTreeItem(node: ConceptTreeNode): vscode.TreeItem {
    if (node.kind === "error") {
      const item = new vscode.TreeItem(
        "Failed to load concepts",
        vscode.TreeItemCollapsibleState.None,
      );
      item.contextValue = "memqlConceptsError";
      item.description = node.message;
      item.tooltip = `ERROR: ${node.message}`;
      item.iconPath = new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
      return item;
    }

    if (node.kind === "domain") {
      const item = new vscode.TreeItem(
        node.domain,
        vscode.TreeItemCollapsibleState.Collapsed,
      );
      item.contextValue = "memqlConceptDomain";
      item.iconPath = new vscode.ThemeIcon("folder");
      return item;
    }

    const item = new vscode.TreeItem(
      node.concept.entity,
      vscode.TreeItemCollapsibleState.None,
    );
    item.contextValue = "memqlConcept";
    item.description = node.concept.id;
    item.tooltip = node.concept.description !== "" ? node.concept.description : node.concept.id;
    item.iconPath = new vscode.ThemeIcon("symbol-class");
    item.command = {
      command: "memql.concepts.open",
      title: "Open Concept",
      arguments: [node.concept],
    };
    return item;
  }
}
