// The Clusters tree.
//
// Rows come straight from ~/.memql/clusters.yaml, which the cockpit also
// writes, so the file is watched and the tree refreshes on external change.
// The tree renders state; it does not own it -- selection is persisted to the
// file and connection state lives in the ConnectionManager.

import * as vscode from "vscode";

import type { ClusterConfig } from "../clusters/model.js";
import { displayLabel } from "../clusters/model.js";
import { clusterRowStatus, clusterRowText, type ClusterRowIcon } from "../clusters/status.js";
import { readClustersFileSafe } from "../clusters/file.js";
import type { ConnectionManager } from "../connection/manager.js";
import type { ClusterVersionRefresher } from "../version/learners.js";
import type { ReleaseCache } from "../version/releaseCache.js";

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
    // The version machinery, optional so a caller that does not want it -- a
    // test, a stripped build -- gets a tree that renders endpoints and nothing
    // else rather than one that cannot be constructed.
    private readonly releases?: ReleaseCache,
    private readonly versions?: ClusterVersionRefresher,
  ) {
    this.connections.onDidChangeState(() => this.changed.fire(undefined));
  }

  refresh(): void {
    this.changed.fire(undefined);
  }

  /**
   * Re-fetch the release listing, ignoring its TTL, and repaint.
   *
   * The explicit escape hatch behind `memql.clusters.refreshReleases`: the
   * listing is cached for ten minutes, and an operator who has just cut a
   * release should not have to wait out a timer to see it.
   */
  async refreshReleases(): Promise<void> {
    await this.releases?.refresh();
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
    // Fire-and-forget: THIS is what triggers the first release fetch and the
    // first version refresh, so activation stays offline and the work is
    // caused by somebody actually looking at the tree. Not awaited, because
    // the rows must render now from what is already known -- `peek()` in
    // getTreeItem -- rather than after a subprocess and two round trips.
    void this.learn(result.file.clusters);
    return result.file.clusters.map((cluster) => ({
      cluster,
      selected: cluster.name === result.file.selectedCluster,
    }));
  }

  /**
   * Learn what exists and what each cluster is running, then repaint IF
   * ANYTHING CHANGED.
   *
   * The conditional repaint is what makes this terminate. A repaint calls
   * getChildren, which calls this again; firing unconditionally would be an
   * infinite loop. On the second pass the listing is inside its TTL and every
   * learner finds the value already recorded, so nothing changed and nothing
   * fires.
   */
  private async learn(clusters: ClusterConfig[]): Promise<void> {
    let changed = false;

    if (this.releases !== undefined) {
      const before = this.releases.peek();
      const after = await this.releases.get();
      // Compared on the two fields that describe the ANSWER rather than on
      // object identity: get() hands back a fresh snapshot every call.
      changed = before?.fetchedAt !== after.fetchedAt || before?.error !== after.error;
    }

    if (this.versions !== undefined) {
      // One refresh per row, concurrently. The release cache is single-flight
      // so N rows still cost one `git ls-remote`, and each cluster's own
      // supersession guard is separate so they cannot cancel each other.
      const decisions = await Promise.all(clusters.map((c) => this.versions!.refresh(c)));
      if (decisions.some((d) => d.write)) changed = true;
    }

    if (changed) this.changed.fire(undefined);
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
    // Two context values, because the two kinds of row offer different actions:
    // only a LOCAL cluster can be uninstalled from this machine, and a `when`
    // clause is the only way to scope a menu entry to one of them.
    //
    // `=== true`, never a truthiness test, matching src/clusters/presence.ts and
    // the field's own documentation: absent means NOT local, and every cluster
    // registered before the flag existed carries no flag. Reading those as local
    // would offer to uninstall an operator's staging cluster.
    item.contextValue = node.cluster.local === true ? "memqlLocalCluster" : "memqlCluster";
    item.command = {
      command: "memql.clusters.select",
      title: "Select Cluster",
      arguments: [node],
    };
    // The icon, the description and the tooltip all come from decisions taken
    // in a module that can be unit-tested (src/clusters/status.ts). This method
    // is only the mapping onto VS Code's icon vocabulary.
    //
    // `peek()`, never `get()`: this is a synchronous render path, and fetching
    // here would put a subprocess behind every repaint. The fetch is triggered
    // by getChildren instead, which repaints when it learns something.
    const status = clusterRowStatus(node.cluster, this.connections.state);
    const text = clusterRowText(node.cluster, this.connections.state, this.releases?.peek());
    item.iconPath = themeIconFor(status.icon);
    item.description = text.description;
    item.tooltip = text.tooltip;
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
