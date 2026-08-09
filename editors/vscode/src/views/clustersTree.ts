// The Clusters tree.
//
// Rows come straight from ~/.memql/clusters.yaml, which the cockpit also
// writes, so the file is watched and the tree refreshes on external change.
// The tree renders state; it does not own it -- selection is persisted to the
// file and connection state lives in the ConnectionManager.

import * as vscode from "vscode";

import type { ClusterConfig } from "../clusters/model.js";
import { displayLabel } from "../clusters/model.js";
import { clusterRowStatus, type ClusterRowIcon } from "../clusters/status.js";
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
    // Both the icon and the tooltip come from ONE decision, taken in a module
    // that can be unit-tested (src/clusters/status.ts). This method is only the
    // mapping onto VS Code's icon vocabulary.
    const status = clusterRowStatus(node.cluster, this.connections.state);
    item.iconPath = themeIconFor(status.icon);
    item.tooltip = status.tooltip;
    return item;
  }
}

// The `credential` icon is deliberately NOT the red error dot. memql#3385:
// "an operator ... sees a red cluster icon with no indication that the
// CREDENTIAL is what expired, as distinct from the cluster going away." A key
// says which of the two it is at a glance, and yellow says it is fixable from
// here rather than being an outage.
function themeIconFor(icon: ClusterRowIcon): vscode.ThemeIcon {
  switch (icon) {
    case "connected":
      return new vscode.ThemeIcon("circle-filled", new vscode.ThemeColor("charts.green"));
    case "connecting":
      return new vscode.ThemeIcon("loading~spin");
    case "failed":
      return new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
    case "credential":
      return new vscode.ThemeIcon("key", new vscode.ThemeColor("charts.yellow"));
    case "unconfigured":
      return new vscode.ThemeIcon("warning", new vscode.ThemeColor("charts.yellow"));
    case "idle":
      return new vscode.ThemeIcon("circle-outline");
  }
}
