// The Clusters tree.
//
// Rows come straight from ~/.memql/clusters.yaml, which the cockpit also
// writes, so the file is watched and the tree refreshes on external change.
// The tree renders state; it does not own it -- selection is persisted to the
// file and connection state lives in the ConnectionManager.

import * as vscode from "vscode";

import type { ClusterConfig } from "../clusters/model.js";
import { displayLabel, isOidcOnly, needsAuth } from "../clusters/model.js";
import { readClustersFile } from "../clusters/file.js";
import type { ConnectionManager, ConnectionState } from "../connection/manager.js";

export interface ClusterNode {
  cluster: ClusterConfig;
  selected: boolean;
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
    // Flat list: clusters have no children.
    if (element !== undefined) return [];
    const file = await readClustersFile(this.clustersPath);
    return file.clusters.map((cluster) => ({
      cluster,
      selected: cluster.name === file.selectedCluster,
    }));
  }

  getTreeItem(node: ClusterNode): vscode.TreeItem {
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
