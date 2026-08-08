// The Clusters tree.
//
// Rows come straight from ~/.memql/clusters.yaml, which the cockpit also
// writes, so the file is watched and the tree refreshes on external change.
// The tree renders state; it does not own it -- selection is persisted to the
// file and connection state lives in the ConnectionManager.

import * as vscode from "vscode";

import type { ClusterConfig } from "../clusters/model.js";
import { displayLabel, isOidcOnly, needsAuth } from "../clusters/model.js";
import { readClustersFileSafe } from "../clusters/file.js";
import type { ConnectionManager } from "../connection/manager.js";

export interface ClusterNode {
  cluster: ClusterConfig;
  selected: boolean;
  // error is set only on the single synthetic row getChildren returns when
  // clusters.yaml fails to read (e.g. malformed YAML, or a torn concurrent
  // write from the cockpit -- the file is shared and not lock-protected).
  // readClustersFile deliberately throws on that condition; getChildren has
  // no story for an unhandled rejection reaching VS Code's tree API, so it
  // is caught (via readClustersFileSafe) and rendered as a row instead,
  // rather than the panel silently going blank.
  error?: string;
}

export class ClustersTreeProvider implements vscode.TreeDataProvider<ClusterNode> {
  private readonly changed = new vscode.EventEmitter<ClusterNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  constructor(
    private readonly clustersPath: string,
    private readonly connections: ConnectionManager,
  ) {
    this.connections.onDidChangeState(() => this.changed.fire(undefined));
  }

  refresh(): void {
    this.changed.fire(undefined);
  }

  async getChildren(element?: ClusterNode): Promise<ClusterNode[]> {
    // Flat list: clusters (and the error row) have no children.
    if (element !== undefined) return [];
    const result = await readClustersFileSafe(this.clustersPath);
    if (!result.ok) {
      // Surface the failure as the sole row rather than letting the
      // rejection reach VS Code's tree API (which has no built-in way to
      // show it) or leaving the panel looking merely empty.
      return [{ cluster: { name: "", endpoint: "" }, selected: false, error: result.error }];
    }
    return result.file.clusters.map((cluster) => ({
      cluster,
      selected: cluster.name === result.file.selectedCluster,
    }));
  }

  getTreeItem(node: ClusterNode): vscode.TreeItem {
    if (node.error !== undefined) {
      const item = new vscode.TreeItem(
        "Failed to read clusters.yaml",
        vscode.TreeItemCollapsibleState.None,
      );
      item.contextValue = "memqlClustersError";
      item.description = node.error;
      item.tooltip = `ERROR: ${node.error}`;
      item.iconPath = new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
      return item;
    }

    const item = new vscode.TreeItem(
      displayLabel(node.cluster),
      vscode.TreeItemCollapsibleState.None,
    );
    item.contextValue = "memqlCluster";
    item.description = node.cluster.endpoint;
    item.command = {
      command: "memql.clusters.select",
      title: "Select Cluster",
      arguments: [node],
    };
    item.iconPath = this.iconFor(node);
    item.tooltip = this.tooltipFor(node);
    return item;
  }

  private iconFor(node: ClusterNode): vscode.ThemeIcon {
    const state = this.connections.state;
    const isActive =
      state.status !== "disconnected" && state.clusterName === node.cluster.name;

    if (isActive && state.status === "connected") {
      return new vscode.ThemeIcon(
        "circle-filled",
        new vscode.ThemeColor("charts.green"),
      );
    }
    if (isActive && state.status === "connecting") {
      return new vscode.ThemeIcon("loading~spin");
    }
    if (isActive && state.status === "error") {
      return new vscode.ThemeIcon(
        "error",
        new vscode.ThemeColor("charts.red"),
      );
    }
    if (needsAuth(node.cluster)) {
      return new vscode.ThemeIcon(
        "warning",
        new vscode.ThemeColor("charts.yellow"),
      );
    }
    return new vscode.ThemeIcon("circle-outline");
  }

  private tooltipFor(node: ClusterNode): string {
    const state = this.connections.state;
    if (state.status !== "disconnected" && state.clusterName === node.cluster.name) {
      if (state.status === "connected") return `Connected (node ${state.nodeId})`;
      if (state.status === "connecting") return "Connecting...";
      if (state.status === "error") return `ERROR: ${state.message}`;
    }
    if (isOidcOnly(node.cluster)) {
      return "Configured for OIDC. Authenticate in the memQL Cockpit, or add a PAT.";
    }
    if (needsAuth(node.cluster)) {
      return "Not configured. Set an endpoint and a PAT.";
    }
    return node.cluster.endpoint;
  }
}
