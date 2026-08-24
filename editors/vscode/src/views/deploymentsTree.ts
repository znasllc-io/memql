// The Deployments tree: the SELECTED cluster's deployment runs, newest first.
//
//   DEPLOYMENTS   local · healthy · v0.17.0
//   |- upgrade   v0.16.1 -> v0.17.0   succeeded   2d ago
//   \- install                        succeeded   9d ago
//
// This file renders state and owns none, the way views/clustersTree.ts does.
// Which runs belong to the selection, what each row says and which icon it
// carries are all decided in state/deploymentsCatalog.ts, where they run under
// bare `node --test`. What is left here is the TreeDataProvider lifecycle and
// the mapping onto VS Code's icon vocabulary.
//
// THREE RULES THE SHAPE OF THIS TREE DEPENDS ON.
//
//  1. NO INSTANCE WRAPPER ROW, of any kind. The view is about one cluster --
//     the one this editor has in hand -- so a row naming it would be a heading
//     an operator has to expand before reaching the only list on the view. The
//     instance's own facts move to `TreeView.description`, which is the API for
//     a heading, and its ACTIONS move to the view title menu; the instance page
//     keeps its buttons unchanged. Nothing lost a route (memql#4426).
//  2. WITH NO CLUSTER SELECTED THIS VIEW IS EMPTY, deliberately and completely.
//     `getChildren` returns `[]`, and the manifest's `viewsWelcome` entry --
//     keyed on `!memql.clusterSelected` -- renders in the space. VS Code draws
//     welcome content ONLY over a genuinely empty tree, so a single synthetic
//     row here, however well meant, silently deletes the welcome (memql#4425).
//  3. AN INSTANCE WITH NO RUNS IS NOT AN EMPTY STATE. A selected cluster with
//     nothing recorded renders no rows and keeps its description: "installed,
//     never upgraded" is the normal case, and so is a remote cluster whose
//     history has not loaded. That is NOT the same nothing as rule 2, and the
//     description is what tells them apart on screen -- present means a cluster
//     is selected.
//
// WHERE THE INSTALL ENTRY POINT WENT (memql#4426, design D4). This file used to
// carry a rule requiring the `local` row to render even on a machine with no
// local cluster, as "not installed", so that an operator with nothing installed
// had somewhere to start. That row is gone and the entry point is not: the
// Clusters welcome offers it, this view's own disconnected welcome offers it,
// and Create Deployment is in the view title menu. Refs #3737 and #3733 argued
// for the row; this epic supersedes that argument rather than leaving it
// standing beside the reversed decision.
//
// Refs: #4426 #4425 #4423 (supersedes #3737 #3733)

import * as vscode from "vscode";

import type { Instance, Run } from "../state/deployments.js";
import type { ConnectionContextSource } from "../state/connectionContext.js";
import {
  buildCatalog,
  runsForSelected,
  selectedInstanceContext,
  selectedViewDescription,
  runRowStatus,
  type Catalog,
  type CatalogInputs,
  type RunRowIcon,
} from "../state/deploymentsCatalog.js";
import type { ReleaseCache } from "../version/releaseCache.js";

/**
 * A row.
 *
 * Still a union with one non-error member rather than a bare `Run`, because
 * `getTreeItem` is handed whichever node it asked for and the error row has to
 * be one of them. The instance member is gone with the wrapper row it drew.
 */
export type DeploymentNode =
  | { kind: "run"; run: Run; instance: string }
  // The single synthetic row shown when clusters.yaml will not read. Mirrors
  // views/clustersTree.ts: readClustersFile deliberately throws on a malformed
  // or torn file (the Cockpit writes it too, without a lock), and an unhandled
  // rejection reaching VS Code's tree API has nowhere to be displayed -- the
  // panel just goes blank, which reads as "no clusters" rather than as a fault.
  //
  // IT IS DRAWN ONLY WHILE A CLUSTER IS SELECTED. With nothing selected the
  // view is empty by rule 2 and the welcome is the correct thing in the space;
  // an unreadable registry is not the operator's next problem there.
  | { kind: "error"; message: string };

/** What the provider needs, minus the two values it resolves per refresh. */
export type DeploymentsTreeDeps = Omit<CatalogInputs, "connection" | "readDeployments"> & {
  /** Resolved per refresh, because both change without this view being told. */
  connection: () => CatalogInputs["connection"];
  readDeployments: () => CatalogInputs["readDeployments"];
  /**
   * The two context keys, read rather than re-derived (design D1).
   *
   * Distinct from `connection` above even though the manager backs both:
   * `connection` says WHICH cluster and is the catalog's input, this says
   * WHETHER there is one and decides whether any of it is drawn. Injected so a
   * test can drive the empty case without a ConnectionManager -- which is the
   * case worth driving, because it is the one that makes the welcome appear.
   */
  connectionContext: ConnectionContextSource;
  /** Injectable clock, so relative times are testable. */
  now?: () => number;
  /**
   * The release listing, for the availability clause in the view description
   * (memql#3996).
   *
   * Optional so a caller that does not want it -- a test, a stripped build --
   * gets a tree that renders versions and nothing else, rather than one that
   * cannot be constructed. Same call views/clustersTree.ts makes.
   */
  releases?: ReleaseCache;
  /**
   * Where the instance line is written (memql#4426).
   *
   * A callback rather than a `TreeView` handle, so this file stays a
   * TreeDataProvider and the view object stays owned by activation -- the
   * provider is constructed before `createTreeView` can exist, and holding the
   * view would make the order load-bearing. Optional for the same reason
   * `releases` is: a test drives `getChildren` without a workbench.
   */
  setDescription?: (description: string) => void;
  /**
   * Where `memql.deploymentsInstance` is written (memql#4426).
   *
   * The view title menu's instance actions are scoped by it, so it is written
   * on EVERY pass -- including as "" -- for the reason the description is:
   * a stale value left over from the last selection would offer Uninstall over
   * a remote cluster, and the doctrine this extension holds to is that a button
   * whose only outcome is a refusal must not be drawn.
   *
   * A callback for the same reason `setDescription` is one: `setContext` is a
   * `vscode` API and this provider is constructed before activation has one to
   * hand it.
   */
  setInstanceContext?: (value: string) => void;
};

export class DeploymentsTreeProvider implements vscode.TreeDataProvider<DeploymentNode> {
  private readonly changed = new vscode.EventEmitter<DeploymentNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;

  // The catalog is built once per expansion pass. Rebuilding per call would
  // re-probe the front door and re-read the cluster once per visible row.
  private catalog: Catalog | undefined;

  constructor(private readonly deps: DeploymentsTreeDeps) {}

  refresh(): void {
    this.catalog = undefined;
    this.changed.fire(undefined);
  }

  async getChildren(element?: DeploymentNode): Promise<DeploymentNode[]> {
    // A FLAT LIST: nothing has children. Kept as an explicit early return
    // rather than falling out of the code below, because a run row that
    // accidentally reported children would render an expand arrow that opens
    // onto nothing.
    if (element !== undefined) return [];

    // RULE 2, AND IT COMES BEFORE THE READ. Returning early also means a
    // machine with no cluster selected does no presence probe, no receipt read
    // and no `git ls-remote` on the strength of a view it is not going to draw.
    if (!this.deps.connectionContext().clusterSelected) {
      this.deps.setDescription?.("");
      this.deps.setInstanceContext?.("");
      return [];
    }

    const catalog = await this.load();
    const selected = runsForSelected(catalog, this.deps.connection());
    // Written on every pass, including when it is "": the selection changes
    // without this view being told, and a description left over from the
    // previous cluster is a heading that names the wrong machine.
    this.deps.setDescription?.(selectedViewDescription(selected.instance, this.deps.releases?.peek()));
    this.deps.setInstanceContext?.(selectedInstanceContext(selected.instance));
    if (catalog.error !== undefined) return [{ kind: "error", message: catalog.error }];
    // Fire-and-forget, exactly as views/clustersTree.ts does it: THIS is what
    // triggers the first release fetch, so activation stays offline and the
    // work is caused by somebody actually looking at the tree. Not awaited,
    // because the rows must render now from what is already known.
    void this.learnReleases();
    return selected.runs.map((run) => ({
      kind: "run" as const,
      run,
      instance: selected.instance?.name ?? run.instance,
    }));
  }

  getTreeItem(node: DeploymentNode): vscode.TreeItem {
    return node.kind === "error"
      ? errorItem(node.message)
      : runItem(node, (this.deps.now ?? Date.now)());
  }

  /**
   * The instance the view is currently describing, for the commands in its
   * title menu (memql#4426).
   *
   * The menu entries act on the SELECTION rather than on a row -- there is no
   * instance row left to right-click -- so something has to answer "which
   * instance". This does, from the catalog the view last built, so the action
   * an operator takes is the one the description in front of them names.
   *
   * Undefined before the first read and whenever nothing is selected. Callers
   * treat that as "no target", never as the local instance: silently defaulting
   * is how an Uninstall aimed at nothing would find something.
   */
  selectedInstance(): Instance | undefined {
    if (this.catalog === undefined) return undefined;
    return runsForSelected(this.catalog, this.deps.connection()).instance;
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
    // catalog input. So do `releases`, `connectionContext` and `setDescription`
    // -- they decide how the view READS, and the catalog is what it reads
    // ABOUT.
    const {
      connection,
      readDeployments,
      now: _now,
      releases: _releases,
      connectionContext: _connectionContext,
      setDescription: _setDescription,
      setInstanceContext: _setInstanceContext,
      ...rest
    } = this.deps;
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
}

function runItem(node: Extract<DeploymentNode, { kind: "run" }>, nowMs: number): vscode.TreeItem {
  const status = runRowStatus(node.run, nowMs);
  const item = new vscode.TreeItem(status.label, vscode.TreeItemCollapsibleState.None);
  item.description = status.description;
  item.tooltip = status.tooltip;
  item.iconPath = runIcon(status.icon);
  item.contextValue = "memqlDeploymentRun";
  // SELECTING A DEPLOYMENT OPENS IT (memql#4427). These rows carried no command
  // at all, so clicking one did nothing -- the single most direct way to teach
  // an operator that a view is decorative. The run and the instance it belongs
  // to both travel, because the detail page's action buttons are the
  // INSTANCE's role-gated set, contextualised by what this run did.
  item.command = {
    command: "memql.deployments.openRun",
    title: "Open Deployment",
    arguments: [node],
  };
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
