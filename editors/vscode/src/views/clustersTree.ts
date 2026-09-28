// The Clusters tree.
//
// Rows come straight from ~/.memql/clusters.yaml, which the cockpit also
// writes, so the file is watched and the tree refreshes on external change.
// The tree renders state; it does not own it -- selection is persisted to the
// file and connection state lives in the ConnectionManager.
//
// WHAT A ROW SAYS AND OFFERS is decided in src/clusters/status.ts, over the
// same state machine the cluster page uses: the state in words, the version
// only when it tells something, the cluster in use marked, and a contextValue
// carrying the flags the menus are gated on -- so the one inline act is the
// state's next step (Sign in, or Open MemQL OS) and the context menu offers
// only what is legal now.

import * as vscode from "vscode";

import { fileOnlyFacts, type ClusterFacts } from "../clusters/facts.js";
import type { ClusterConfig } from "../clusters/model.js";
import { displayLabel } from "../clusters/model.js";
import {
  clusterContextValue,
  clusterRowText,
  clusterStatus,
  type ClusterState,
} from "../clusters/status.js";
import { readClustersFileSafe } from "../clusters/file.js";
import type { ConnectionManager } from "../connection/manager.js";
import type { ClusterVersionRefresher } from "../version/learners.js";
import type { ReleaseCache } from "../version/releaseCache.js";

export interface ClusterNode {
  cluster: ClusterConfig;
  selected: boolean;
  /**
   * What this machine knows about the cluster's sign-in (clusters/facts.ts),
   * read before the row renders so it never flashes "Sign in" for a cluster
   * a stored session would connect. Absent on a node built by a caller that
   * did not read them (a toast's argument, the palette).
   */
  facts?: ClusterFacts;
  // error is set only on the single synthetic row getChildren returns when
  // clusters.yaml fails to read (e.g. malformed YAML, or a torn concurrent
  // write from the cockpit -- the file is shared and not lock-protected).
  // readClustersFile deliberately throws on that condition; getChildren has
  // no story for an unhandled rejection reaching VS Code's tree API, so it
  // is caught (via readClustersFileSafe) and rendered as a row instead,
  // rather than the panel silently going blank.
  error?: string;
}

export interface ClustersTreeDeps {
  /** The facts for one cluster; never rejects. */
  factsFor?: (cluster: ClusterConfig) => Promise<ClusterFacts>;
  /** Told after every read of the file, with what it listed (the welcome's presence key). */
  onRead?: (clusters: readonly ClusterConfig[]) => void;
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
    private readonly deps: ClustersTreeDeps = {},
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
    this.deps.onRead?.(result.file.clusters);
    // Fire-and-forget: THIS is what triggers the first release fetch and the
    // first version refresh, so activation stays offline and the work is
    // caused by somebody actually looking at the tree. Not awaited, because
    // the rows must render now from what is already known -- `peek()` in
    // getTreeItem -- rather than after a subprocess and two round trips.
    void this.learn(result.file.clusters);
    // The facts ARE awaited: they are local reads (SecretStorage and the
    // receipt), and rendering before them would show "Sign in" on a row whose
    // stored session a click would simply use.
    const factsFor = this.deps.factsFor ?? ((cluster: ClusterConfig) => Promise.resolve(fileOnlyFacts(cluster)));
    const facts = await Promise.all(
      result.file.clusters.map((cluster) => factsFor(cluster).catch(() => undefined)),
    );
    return result.file.clusters.map((cluster, i) => ({
      cluster,
      selected: cluster.name === result.file.selectedCluster,
      ...(facts[i] !== undefined ? { facts: facts[i] } : {}),
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
      // THE CLICK IS THE REMEDY: it opens the file at the parser's complaint.
      // The full parser message is the tooltip; the watcher repaints after
      // the fix.
      const item = new vscode.TreeItem("Can't read your cluster list", vscode.TreeItemCollapsibleState.None);
      item.contextValue = "memqlClustersError";
      item.description = "Open the file to fix it";
      item.tooltip = node.error;
      item.iconPath = new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
      item.command = {
        command: "vscode.open",
        title: "Open File",
        arguments: [vscode.Uri.file(this.clustersPath)],
      };
      return item;
    }

    const cluster = node.cluster;
    const facts = node.facts ?? fileOnlyFacts(cluster);
    const state = this.connections.state;
    const status = clusterStatus({ cluster, connection: state, facts });
    // The cluster in use: the selected one, or the one the live connection
    // names (the two agree unless the file was edited underneath).
    const inUse =
      node.selected || (state.status !== "disconnected" && state.clusterName === cluster.name);
    const label = displayLabel(cluster);
    // MARKED, so a reader can tell which cluster every other view is about --
    // including after a reload, before anything has connected.
    const item = new vscode.TreeItem(
      inUse ? { label, highlights: [[0, label.length]] } : label,
      vscode.TreeItemCollapsibleState.None,
    );
    item.contextValue = clusterContextValue(cluster, status, facts, inUse);
    item.command = {
      command: "memql.clusters.select",
      title: "Select Cluster",
      arguments: [node],
    };
    // `peek()`, never `get()`: this is a synchronous render path, and fetching
    // here would put a subprocess behind every repaint. The fetch is triggered
    // by getChildren instead, which repaints when it learns something.
    const text = clusterRowText(cluster, status, inUse, this.releases?.peek());
    item.iconPath = themeIconFor(status.state, inUse);
    item.description = text.description;
    item.tooltip = text.tooltip;
    item.accessibilityInformation = { label: text.accessibilityLabel };
    return item;
  }
}

// The `signIn` icon is deliberately NOT the red error dot. memql#3385:
// "an operator ... sees a red cluster icon with no indication that the
// CREDENTIAL is what expired, as distinct from the cluster going away." A key
// says which of the two it is at a glance, and yellow says it is fixable from
// here rather than being an outage. An idle cluster in use is a filled dot, so
// the working cluster is marked even before it connects.
function themeIconFor(state: ClusterState, inUse: boolean): vscode.ThemeIcon {
  switch (state) {
    case "connected":
      return new vscode.ThemeIcon("circle-filled", new vscode.ThemeColor("charts.green"));
    case "connecting":
      return new vscode.ThemeIcon("loading~spin");
    case "unreachable":
      return new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
    case "signIn":
      return new vscode.ThemeIcon("key", new vscode.ThemeColor("charts.yellow"));
    case "notConfigured":
      return new vscode.ThemeIcon("warning", new vscode.ThemeColor("charts.yellow"));
    case "idle":
      return new vscode.ThemeIcon(inUse ? "circle-filled" : "circle-outline");
  }
}
