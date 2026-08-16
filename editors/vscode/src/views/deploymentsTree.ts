// The Deployments tree: instances at the top, runs beneath, newest first.
//
//   DEPLOYMENTS
//   |- local     healthy - v0.17.0
//   |  |- upgrade   v0.16.1 -> v0.17.0   succeeded   2d ago
//   |  \- install                        succeeded   9d ago
//   \- staging   healthy - v0.9.2
//      |- rollout v0.9.2   succeeded     1d ago
//      \- rollout v0.9.1   rolled_back   3d ago
//
// This file renders state and owns none, the way views/clustersTree.ts does.
// Which instances exist, which runs belong to which, what each row says and
// which icon it carries are all decided in state/deploymentsCatalog.ts, where
// they run under bare `node --test`. What is left here is the TreeDataProvider
// lifecycle and the mapping onto VS Code's icon vocabulary.
//
// TWO RULES THE SHAPE OF THIS TREE DEPENDS ON.
//
//  1. AN INSTANCE WITH NO RUNS IS NOT AN EMPTY STATE. It renders with its
//     actions and no children, because "installed, never upgraded" is the
//     normal case and so is "a remote cluster this editor is not connected to".
//     Neither deserves a placeholder telling the operator something is missing.
//  2. A MACHINE WITH NO LOCAL CLUSTER STILL SHOWS THE `local` ROW, as
//     "not installed", carrying Create deployment as its only action. That row
//     is the entry point the Clusters "+" menu used to hide, and it is what
//     makes this view useful on a machine where nothing has been installed --
//     the operator who most needs somewhere to start.
//
// Refs: #3737 #3733

import * as vscode from "vscode";

import type { Instance, Run } from "../state/deployments.js";
import {
  buildCatalog,
  instanceContextValue,
  instanceRowStatus,
  runRowStatus,
  type Catalog,
  type CatalogInputs,
  type InstanceRowIcon,
  type RunRowIcon,
} from "../state/deploymentsCatalog.js";
import type { ReleaseCache } from "../version/releaseCache.js";

/**
 * A row.
 *
 * A discriminated union rather than two providers: the instance and its runs
 * are one tree, and VS Code hands getTreeItem whichever it asked for.
 */
export type DeploymentNode =
  | { kind: "instance"; instance: Instance }
  | { kind: "run"; run: Run; instance: string }
  // The single synthetic row shown when clusters.yaml will not read. Mirrors
  // views/clustersTree.ts: readClustersFile deliberately throws on a malformed
  // or torn file (the Cockpit writes it too, without a lock), and an unhandled
  // rejection reaching VS Code's tree API has nowhere to be displayed -- the
  // panel just goes blank, which reads as "no clusters" rather than as a fault.
  | { kind: "error"; message: string };

/** What the provider needs, minus the two values it resolves per refresh. */
export type DeploymentsTreeDeps = Omit<CatalogInputs, "connection" | "readDeployments"> & {
  /** Resolved per refresh, because both change without this view being told. */
  connection: () => CatalogInputs["connection"];
  readDeployments: () => CatalogInputs["readDeployments"];
  /** Injectable clock, so relative times are testable. */
  now?: () => number;
  /**
   * The release listing, for the availability clause on each instance row
   * (memql#3996).
   *
   * Optional so a caller that does not want it -- a test, a stripped build --
   * gets a tree that renders versions and nothing else, rather than one that
   * cannot be constructed. Same call views/clustersTree.ts makes.
   */
  releases?: ReleaseCache;
};

export class DeploymentsTreeProvider implements vscode.TreeDataProvider<DeploymentNode> {
  private readonly changed = new vscode.EventEmitter<DeploymentNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  // The catalog is built once per expansion pass and read again when VS Code
  // asks for an instance's children. Rebuilding per call would re-probe the
  // front door and re-read the cluster once per visible instance, and the two
  // passes could then disagree about the same cluster within one frame.
  private catalog: Catalog | undefined;

  constructor(private readonly deps: DeploymentsTreeDeps) {}

  refresh(): void {
    this.catalog = undefined;
    this.changed.fire(undefined);
  }

  async getChildren(element?: DeploymentNode): Promise<DeploymentNode[]> {
    if (element === undefined) {
      const catalog = await this.load();
      if (catalog.error !== undefined) return [{ kind: "error", message: catalog.error }];
      // Fire-and-forget, exactly as views/clustersTree.ts does it: THIS is what
      // triggers the first release fetch, so activation stays offline and the
      // work is caused by somebody actually looking at the tree. Not awaited,
      // because the rows must render now from what is already known.
      void this.learnReleases();
      return catalog.instances.map((instance) => ({ kind: "instance", instance }));
    }
    if (element.kind !== "instance") return [];
    const catalog = await this.load();
    return (catalog.runs.get(element.instance.name) ?? []).map((run) => ({
      kind: "run",
      run,
      instance: element.instance.name,
    }));
  }

  getTreeItem(node: DeploymentNode): vscode.TreeItem {
    if (node.kind === "error") return errorItem(node.message);
    return node.kind === "instance"
      ? this.instanceItem(node)
      : runItem(node.run, (this.deps.now ?? Date.now)());
  }

  /**
   * Fetch the release listing, then repaint IF IT CHANGED.
   *
   * The conditional repaint is what makes this terminate: a repaint calls
   * getChildren, which calls this again, so firing unconditionally would be an
   * infinite loop. On the second pass the listing is inside its TTL, nothing
   * changed, and nothing fires.
   *
   * The cache is single-flight and shared with the Clusters tree, so a user
   * with both views open still pays for one `git ls-remote` -- which is the
   * reason to import the shared instance rather than construct one here.
   */
  private async learnReleases(): Promise<void> {
    const releases = this.deps.releases;
    if (releases === undefined) return;
    const before = releases.peek();
    // Compared on the two fields that describe the ANSWER rather than on object
    // identity: get() hands back a fresh snapshot every call.
    const after = await releases.get();
    if (before?.fetchedAt !== after.fetchedAt || before?.error !== after.error) {
      this.changed.fire(undefined);
    }
  }

  private async load(): Promise<Catalog> {
    if (this.catalog !== undefined) return this.catalog;
    // The two thunks are RESOLVED here and dropped from what is passed on, so
    // the catalog receives values rather than the functions that produce them.
    // `now` goes with them: it is this view's clock for relative times, not a
    // catalog input. So is `releases` -- it decides how a row READS, and the
    // catalog is what a row reads ABOUT.
    const { connection, readDeployments, now: _now, releases: _releases, ...rest } = this.deps;
    const resolvedConnection = connection();
    const resolvedReadDeployments = readDeployments();
    this.catalog = await buildCatalog({
      ...rest,
      ...(resolvedConnection !== undefined ? { connection: resolvedConnection } : {}),
      ...(resolvedReadDeployments !== undefined
        ? { readDeployments: resolvedReadDeployments }
        : {}),
    });
    return this.catalog;
  }

  private instanceItem(node: Extract<DeploymentNode, { kind: "instance" }>): vscode.TreeItem {
    const { instance } = node;
    const hasRuns = (this.catalog?.runs.get(instance.name) ?? []).length > 0;
    const item = new vscode.TreeItem(
      instance.name,
      // Collapsed rather than Expanded: an instance with many runs would
      // otherwise push every other instance off the view on first open.
      hasRuns
        ? vscode.TreeItemCollapsibleState.Collapsed
        : vscode.TreeItemCollapsibleState.None,
    );
    // `peek()`, never `get()`: this is a synchronous render path, and fetching
    // here would put a subprocess behind every repaint. The fetch is triggered
    // by getChildren instead, which repaints when it learns something.
    const status = instanceRowStatus(instance, this.deps.releases?.peek());
    item.description = status.description;
    item.tooltip = status.tooltip;
    item.iconPath = instanceIcon(status.icon);
    // Three values, because the three kinds of row offer different actions and
    // a `when` clause is the only way to scope a menu entry to one of them: an
    // absent local instance can only be created, an installed one can also be
    // repaired and uninstalled, and a remote one does neither.
    item.contextValue = instanceContextValue(instance);
    // The row carries the instance it names, and the page renders the local or
    // the remote half accordingly (#3739, #3740).
    item.command = {
      command: "memql.deployments.open",
      title: "Open Instance",
      arguments: [node],
    };
    return item;
  }
}

function runItem(run: Run, nowMs: number): vscode.TreeItem {
  const status = runRowStatus(run, nowMs);
  const item = new vscode.TreeItem(status.label, vscode.TreeItemCollapsibleState.None);
  item.description = status.description;
  item.tooltip = status.tooltip;
  item.iconPath = runIcon(status.icon);
  item.contextValue = "memqlDeploymentRun";
  return item;
}

function errorItem(message: string): vscode.TreeItem {
  const item = new vscode.TreeItem(
    "Failed to read clusters.yaml",
    vscode.TreeItemCollapsibleState.None,
  );
  item.contextValue = "memqlDeploymentsError";
  item.description = message;
  item.tooltip = `ERROR: ${message}`;
  item.iconPath = new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
  return item;
}

function instanceIcon(icon: InstanceRowIcon): vscode.ThemeIcon {
  switch (icon) {
    case "healthy":
      return new vscode.ThemeIcon("circle-filled", new vscode.ThemeColor("charts.green"));
    case "unreachable":
      // Yellow, not red: something IS installed and the next action is a
      // repair, which is a different story from an outage with nothing to fix.
      return new vscode.ThemeIcon("warning", new vscode.ThemeColor("charts.yellow"));
    case "absent":
      return new vscode.ThemeIcon("circle-outline");
  }
}

function runIcon(icon: RunRowIcon): vscode.ThemeIcon {
  switch (icon) {
    case "running":
      return new vscode.ThemeIcon("loading~spin");
    case "succeeded":
      return new vscode.ThemeIcon("check", new vscode.ThemeColor("charts.green"));
    case "failed":
      return new vscode.ThemeIcon("error", new vscode.ThemeColor("charts.red"));
    case "cancelled":
      return new vscode.ThemeIcon("circle-slash");
    case "interrupted":
      // `debug-disconnect` rather than a warning triangle: nothing went wrong
      // with the work, the editor stopped watching it. Yellow because it is
      // still the row worth acting on -- the run never reached a verdict, so
      // whether the machine changed is genuinely unknown (memql#3886).
      return new vscode.ThemeIcon("debug-disconnect", new vscode.ThemeColor("charts.yellow"));
    case "replaced":
      return new vscode.ThemeIcon("history");
  }
}
