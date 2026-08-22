// The instance page: what this deployment is, and the four things you can do
// to it.
//
// This is where the install machinery finally lives under a name that describes
// it. Three of the four actions are RE-PARENTED, NOT REWRITTEN -- installing on
// a machine with no cluster, repairing one, and uninstalling one are the flows
// the add-cluster wizard already drives, and this page opens them rather than
// growing a second copy. A second implementation of an uninstall is a second
// answer to "does this delete the operator's own k3d cluster".
//
// The fourth is new, and it is the one this page exists for: MOVING AN
// INSTALLED CLUSTER TO ANOTHER RELEASE TAG. That is a coherent deployment
// because a wizard-installed cluster is a detached checkout at a tag with
// ArgoCD reconciling the local overlay from it -- so it genuinely has a
// version, and changing the version is the whole verb.
//
// AND IT IS THE INSTALL GRAPH, RE-RUN. Not a second graph and not a subset:
// `stackCheckout` sees a tag it is not on and moves the checkout, `clusterUp`
// reconciles, and every other step verifies first and skips. The same property
// that makes re-running the graph a repair is what makes it a deployment. What
// this page adds on top is the FORECAST (state/upgradePlan.ts), because a run
// that reports fifteen steps and does work in two looks like a full reinstall
// to whoever is watching it.
//
// WHAT THIS FILE IS NOT ALLOWED TO DECIDE. Which actions an instance offers
// (deploy/instanceActions.ts), what the rows say (state/deploymentsCatalog.ts),
// what a tag list means (install/tags.ts), what a deployment will touch
// (state/upgradePlan.ts) and what the run screens render (webview/
// installScreens.ts) all live outside it, under bare `node --test`. This is the
// webview lifecycle, the postMessage boundary, and the run loop.
//
// Refs: #3739 #3733

import { randomBytes } from "node:crypto";
import * as fs from "node:fs/promises";
import * as path from "node:path";

import * as vscode from "vscode";

import { escapeHtml, viewKitStyles } from "@znasllc-io/memql-view-kit";

import { brandStrip, brandStyleBlock } from "./brandTokens.js";

import {
  DEPLOY_ACTIONS,
  confirmationMatches,
  confirmationPhrase,
  roleVisibility,
  rolloutRequiresConfirmation,
  type DeployActionId,
  type RoleVisibility,
} from "../deploy/actions.js";
import {
  readDeploymentStatus,
  runDeployAction,
  type DeployActionRequest,
  type DeployControlPort,
  type DeployOutcome,
  type StatusRead,
} from "../deploy/controller.js";
import { instanceActions, type InstanceActionId } from "../deploy/instanceActions.js";
import { upgradeVerdict, type UpgradeTarget, type UpgradeVerdict } from "../deploy/upgrade.js";
import { describeVersion } from "../version/describe.js";
import { pipelineState, type PipelineState } from "../deploy/pipelineState.js";
import { readCheckoutState } from "../install/checkoutState.js";
import type { ExecutionReport } from "../install/executor.js";
import { capabilityScriptPath, runCapabilityScript } from "../install/runner.js";
import {
  installSessionOptions,
  runInstall,
  runRebuild,
  type SessionHooks,
} from "../install/session.js";
import { listReleaseTags, tagProblem, type TagListing } from "../install/tags.js";
import type { ReleaseCache } from "../version/releaseCache.js";
import {
  DEFAULT_INPUTS,
  AddClusterState,
  type StepProgress,
} from "../state/addCluster.js";
import type { Instance, Run } from "../state/deployments.js";
import { buildCatalog, type CatalogInputs } from "../state/deploymentsCatalog.js";
import {
  recordedCheckout,
  recordedDomain,
  recordedProvider,
  recordedProviderKeyFile,
  readReceipt,
} from "../install/receipt.js";
import { rebuiltMessage } from "../state/imageLane.js";
import { rebuildPreflightItems, type RebuildPreflightInputs } from "../state/rebuildPreflight.js";
import { RunRecorder } from "../state/runRecorder.js";
import { defaultRunsDir } from "../state/runLog.js";
import { isSameVersion, upgradePlan, upgradeSummary, type PlannedStepView } from "../state/upgradePlan.js";
import { graphDocumentPath, loadGraphFile, type Graph } from "../install/graph.js";
import { DEFAULT_LOCAL_DOMAIN } from "../install/stackPin.js";
import { renderChooseTag, renderInstanceOverview, renderRemoteInstance } from "./deploymentScreens.js";
import {
  renderFailedScreen,
  renderRebuildScreen,
  renderRunningScreen,
  type RunMode,
} from "./installScreens.js";

/** The same DEFAULT ceiling the wizard gives a step; a step's own `timeoutSeconds` in the graph outranks it (memql#4076) -- see addClusterPanel.ts for the full note. */
const STEP_TIMEOUT_MS = 600_000;

/**
 * The rebuild's own ceiling: 45 minutes, matching rebuild.json's
 * `timeoutSeconds` (memql#4246).
 *
 * The step's declared value outranks this anyway -- that is memql#4076's whole
 * mechanism -- so the number here is what a run would fall back to, and a
 * default sized to kill a wedged step in ten minutes would kill a first build
 * that is going perfectly well. Nine node images from a cold Docker cache is
 * structurally more than ten minutes.
 */
const REBUILD_TIMEOUT_MS = 2_700_000;

/**
 * How long the Docker gate is given before the checklist says it is not
 * answering.
 *
 * Short on purpose. This is a read-only classification on a checklist, not a
 * step of the run: a Docker that takes half a minute to answer is a Docker the
 * operator wants told about, and the run itself blocks on the same gate with
 * the graph's own budget.
 */
const DOCKER_PROBE_TIMEOUT_MS = 15_000;

type Screen = "overview" | "chooseTag" | "rebuildPreflight" | "running" | "failedStep";

export interface DeploymentPanelDeps {
  /** Everything buildCatalog needs, minus what this panel resolves itself. */
  catalog: Omit<CatalogInputs, "connection" | "readDeployments">;
  installRoot: string;
  receiptFile: string;
  runsDir?: string;
  /** Repaints the Deployments tree after a run changes what it says. */
  refreshTree: () => void;
  /**
   * Opens the add-cluster wizard on a named branch.
   *
   * Injected rather than imported, because it is the RE-PARENTING seam: this
   * page decides that installing, repairing and uninstalling belong to a
   * Deployments instance, and activation decides what runs them. A direct
   * import would make this file the second place that knows how the wizard is
   * constructed.
   */
  openInstallFlow: (action: "install" | "repair" | "uninstall") => void;
  /** Injected by tests; the real spawn-based runner when absent. */
  runScript?: SessionHooks["run"];
  /** Injected by tests; the real `git ls-remote` when absent. */
  listTags?: (cwd: string) => Promise<TagListing>;
  /**
   * The release listing, for the `latest` fact and the availability clause
   * (memql#3996).
   *
   * The SHARED cache instance, not one built here: it is single-flight, so a
   * page open beside the two trees still costs one `git ls-remote`. Optional
   * for the reason the trees make it optional -- a test gets a page that
   * renders versions and nothing else rather than one it cannot construct.
   *
   * Distinct from `listTags` above, deliberately. That one lists the tags of
   * THIS cluster's checkout, which is what the operator is choosing among; this
   * one answers "what has the project released", which is a question about the
   * project. They are usually the same set and are not the same question.
   */
  releases?: ReleaseCache;

  // --- the remote half (memql#3740) ---

  /**
   * The live connection and the deployment read, as thunks.
   *
   * Same shape the Deployments tree takes and for the same reason: both change
   * without this page being told, and a value captured when the panel opened
   * would leave the connected cluster's history permanently unreadable.
   */
  connection?: () => CatalogInputs["connection"];
  readDeployments?: () => CatalogInputs["readDeployments"];

  /**
   * The bridged deploy-control client for the CONNECTED cluster, or undefined
   * when nothing is connected.
   *
   * A thunk rather than a value, and rebuilt per call rather than cached, for
   * the reason clusterPanel.ts records: the ConnectionManager drops its
   * dispatcher the moment the socket dies, so a cached client would go on
   * writing into a dead stream.
   */
  deployPort?: () => DeployControlPort | undefined;
  /** The caller's cluster role, for deciding which actions to DRAW. */
  readRole?: () => Promise<RoleVisibility>;
  /**
   * Asks the operator to type a phrase back. Injected because a modal is the
   * extension host's, and because a test cannot click one.
   */
  confirm?: (prompt: string, phrase: string) => Promise<string | undefined>;
}

export class DeploymentPanel {
  private static open_: DeploymentPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly disposables: vscode.Disposable[] = [];
  /**
   * The RUN's state, borrowed whole from the wizard.
   *
   * Reused rather than reimplemented so Retry, Switch-to-guided and Cancel
   * behave here exactly as they do there -- guided is per-step and rides on the
   * step's own record, so a second state machine would be a second answer to
   * what those three buttons do to a half-finished install.
   */
  private readonly state = new AddClusterState();

  private screen: Screen = "overview";
  /** Which instance this page is about. Empty means the local one. */
  private instanceName = "";
  private instance: Instance | undefined;
  private pipeline: PipelineState | undefined;
  private outcome = "";
  private runs: readonly Run[] = [];
  private listing: TagListing = { tags: [], error: "" };
  private roleVisibility: RoleVisibility | undefined;
  private target = "";
  private tagError = "";
  private plan: PlannedStepView[] = [];
  /** The install graph, read once per visit to the tag screen. */
  private graph: Graph | undefined;
  /** The node types the next rebuild builds; "" is every app node (memql#4246). */
  private rebuildNodes = "";
  /**
   * The rebuild checklist's FACTS, undefined while they are being gathered.
   *
   * The facts, not the rendered items, and `nodes` is deliberately not among
   * them: it is the one input on that screen the operator can still change, so
   * the list is worded at RENDER time from these plus whatever is in the field
   * now. Storing the finished items would leave a checklist saying "all app
   * nodes" above a box reading `bff` -- a line that is wrong about the one
   * thing the screen asked for.
   */
  private rebuildFacts: Omit<RebuildPreflightInputs, "nodes"> | undefined;
  /**
   * Which run the progress screen is describing.
   *
   * A field rather than a constant since memql#4246: this page now drives two
   * kinds of run, and a screen headed "Deploying to the local cluster" over a
   * rebuild would name the one thing the operator did not ask for.
   */
  private runMode: RunMode = "deploy";
  /** The in-flight read `pointAt` started, for a caller that must act on it. */
  private loading: Promise<void> = Promise.resolve();
  private error = "";
  private disposed = false;
  /** Non-undefined exactly while a run is in flight; also the cancel handle. */
  private runAbort: AbortController | undefined;

  /**
   * Opens the page for an instance, or re-points the one already open.
   *
   * ONE PANEL for every instance rather than one per instance: the page is a
   * console for whichever deployment the operator is looking at, and a tab per
   * cluster would leave several of them showing states that stopped being true
   * the moment a run finished somewhere else.
   */
  static show(
    context: vscode.ExtensionContext,
    deps: DeploymentPanelDeps,
    instanceName = "",
  ): DeploymentPanel {
    const existing = DeploymentPanel.open_;
    if (existing !== undefined && !existing.disposed) {
      existing.panel.reveal(vscode.ViewColumn.Beside);
      existing.pointAt(instanceName);
      return existing;
    }
    const panel = new DeploymentPanel(context, deps);
    DeploymentPanel.open_ = panel;
    panel.pointAt(instanceName);
    return panel;
  }

  /**
   * Opens the page for the local instance and takes one of its actions
   * (memql#4246).
   *
   * WHY IT WAITS FOR THE LOAD `show` STARTED. Every other caller only needs the
   * page to paint, so `pointAt` fires the read and does not wait -- but `choose`
   * narrows the requested id against `instanceActions(instance)`, and an
   * instance that has not been read yet offers nothing at all. Acting before it
   * lands would open the page and silently do nothing, which is exactly the "a
   * click that does nothing teaches a developer the extension is broken"
   * failure the training surface guards against.
   *
   * The action is still NARROWED. This is a shortcut to a control the page
   * draws, not a second authority: a machine with no recorded checkout offers
   * no rebuild here either, and the command lands on the overview.
   */
  static async openAction(
    context: vscode.ExtensionContext,
    deps: DeploymentPanelDeps,
    id: InstanceActionId,
  ): Promise<void> {
    await DeploymentPanel.show(context, deps).takeAction(id);
  }

  private async takeAction(id: InstanceActionId): Promise<void> {
    await this.loading;
    if (this.disposed) return;
    await this.choose(id);
  }

  private pointAt(instanceName: string): void {
    if (instanceName !== this.instanceName) {
      // A different instance is a different page: nothing carried over from the
      // last one is true about this one.
      this.instanceName = instanceName;
      this.screen = "overview";
      this.pipeline = undefined;
      this.outcome = "";
      this.error = "";
    }
    // Kept so `takeAction` can wait for THIS read rather than starting a second
    // one beside it. Still fire-and-forget for every other caller.
    this.loading = this.load();
  }

  private constructor(
    _context: vscode.ExtensionContext,
    private readonly deps: DeploymentPanelDeps,
  ) {
    this.panel = vscode.window.createWebviewPanel(
      "memqlDeployment",
      "MemQL deployment",
      vscode.ViewColumn.Beside,
      { enableScripts: true, retainContextWhenHidden: true },
    );
    this.disposables.push(
      this.panel.onDidDispose(() => {
        this.disposed = true;
        // Abort FIRST. A run whose panel has gone has nowhere to report, and
        // the receipt is written after every step, so stopping at the next wave
        // boundary leaves the machine fully uninstallable.
        this.runAbort?.abort();
        if (DeploymentPanel.open_ === this) DeploymentPanel.open_ = undefined;
        for (const d of this.disposables) d.dispose();
      }),
      this.panel.webview.onDidReceiveMessage((message: unknown) => {
        void this.onMessage(message);
      }),
    );
    this.render();
  }

  /** Re-reads the instance and its runs. */
  private async load(): Promise<void> {
    const catalog = await buildCatalog({
      ...this.deps.catalog,
      ...(this.deps.connection !== undefined ? { connection: this.deps.connection() } : {}),
      ...(this.deps.readDeployments !== undefined
        ? { readDeployments: this.deps.readDeployments() }
        : {}),
    });
    if (this.disposed) return;
    this.instance =
      this.instanceName === ""
        ? catalog.instances.find((i) => i.kind === "local")
        : catalog.instances.find((i) => i.name === this.instanceName);
    this.runs = this.instance === undefined ? [] : (catalog.runs.get(this.instance.name) ?? []);
    if (catalog.error !== undefined) this.error = catalog.error;
    this.render();
    if (this.instance?.kind === "remote") await this.loadPipeline(this.instance.name);
  }

  /**
   * Which of the three states this cluster's deploy pipeline is in.
   *
   * Read on every load rather than once: a cluster that gains a deploy pack, or
   * an operator whose role changes, are both things this page would otherwise
   * go on being wrong about for as long as the tab stays open.
   */
  private async loadPipeline(name: string): Promise<void> {
    const port = this.deps.deployPort?.();
    const visibility = (await this.deps.readRole?.()) ?? roleVisibility(undefined);
    if (this.disposed) return;
    // Kept, because the upgrade button is drawn from the synchronous render
    // path and cannot await a role read. Undefined until the first load, which
    // upgradeVerdict reads as INDETERMINATE and therefore offers -- the same
    // call src/deploy/actions.ts makes, for the same reason.
    this.roleVisibility = visibility;
    if (port === undefined) {
      // NOT CONNECTED IS NOT "NO PIPELINE". The read never happened, so the
      // page says which of the two it is rather than reporting the cluster has
      // no deploy console on the strength of a socket this editor never opened.
      const read: StatusRead = {
        status: null,
        message:
          "This editor is not connected to this cluster, so its deployment status was not read. " +
          "Connect to it from the Clusters view.",
        reason: "unavailable",
      };
      this.pipeline = pipelineState(read, visibility);
      this.render();
      return;
    }
    const read = await readDeploymentStatus(port);
    if (this.disposed) return;
    this.pipeline = pipelineState(read, visibility);
    this.render();
  }

  // -------------------------------------------------------------------------
  // messages
  // -------------------------------------------------------------------------

  private async onMessage(message: unknown): Promise<void> {
    if (message === null || typeof message !== "object") return;
    const { type, value } = message as { type?: unknown; value?: unknown };
    if (typeof type !== "string") return;

    if (type === "upgrade") {
      // Its OWN message rather than a "choose" id, because the upgrade button
      // is not one of the instance actions: `choose` validates its id against
      // instanceActions(), and adding a fourth entry there would put the
      // move-to-newest button in the same list as install, repair and
      // uninstall -- which is exactly the confusion the epic is trying to end.
      await this.runUpgrade();
      return;
    }
    if (type === "choose" && typeof value === "string") {
      await this.choose(value as InstanceActionId);
      return;
    }
    if (type === "deploy" && typeof value === "string") {
      await this.runDeploy(value);
      return;
    }
    if (type === "pickTag" && typeof value === "string") {
      this.setTarget(value);
      this.render();
      return;
    }
    if (type === "input" && typeof value === "object" && value !== null) {
      const { field, text } = value as { field?: unknown; text?: unknown };
      // Recorded and NOT repainted, like every field on the wizard's forms: a
      // repaint replaces the whole document and would take the caret with it.
      if (field === "tag" && typeof text === "string") this.target = text.trim();
      if (field === "nodes" && typeof text === "string") this.rebuildNodes = text.trim();
      return;
    }
    if (type === "beginRebuild") {
      await this.startRebuild();
      return;
    }
    if (type === "beginDeploy") {
      this.setTarget(this.target);
      if (this.tagError !== "") {
        this.render();
        return;
      }
      await this.startDeploy();
      return;
    }
    if (type === "retry") {
      this.state.retry();
      this.render();
      // RETRY RE-RUNS WHAT FAILED, not whatever this page's older half runs. The
      // failure screen is shared by both kinds of run, so a retry that always
      // called startDeploy would answer a failed rebuild by moving the cluster
      // to a release tag -- silently, from a button labelled "Retry this step".
      await (this.runMode === "rebuild" ? this.startRebuild() : this.startDeploy());
      return;
    }
    if (type === "guided") {
      // The rebuild failure screen draws no guided control (installScreens.ts
      // says why), so this is a message the page never rendered -- dropped, the
      // same call `choose` makes for an action an instance does not offer,
      // rather than run as a second Retry.
      if (this.runMode === "rebuild") return;
      this.state.switchToGuided();
      this.render();
      await this.startDeploy();
      return;
    }
    if (type === "cancel") {
      this.runAbort?.abort();
      this.state.cancel();
      this.screen = "overview";
      await this.load();
      return;
    }
    if (type === "back") {
      this.screen = "overview";
      await this.load();
      return;
    }
  }

  private async choose(id: InstanceActionId): Promise<void> {
    const instance = this.instance;
    if (instance === undefined) return;
    // The catalog decides what is offered; this only routes what it offered.
    // A message naming an action this instance does not have is a webview
    // saying something the page never rendered, and it is ignored.
    if (!instanceActions(instance).some((a) => a.id === id)) return;

    if (id === "repair") {
      this.deps.openInstallFlow("repair");
      return;
    }
    if (id === "uninstall") {
      this.deps.openInstallFlow("uninstall");
      return;
    }
    if (id === "rebuildFromCheckout") {
      // NOT RE-PARENTED INTO THE WIZARD, unlike repair and uninstall. Those are
      // flows the wizard already drives end to end; a rebuild asks one optional
      // question and needs facts about THIS instance -- its checkout, its image
      // source -- which the wizard has no screen for and no reason to learn.
      await this.openRebuild();
      return;
    }
    if (id !== "createDeployment") return;

    if (instance.presence === "absent") {
      // Nothing is here, so there is nothing to move: this is the install, and
      // the install collects answers no receipt can supply.
      this.deps.openInstallFlow("install");
      return;
    }

    this.screen = "chooseTag";
    this.target = "";
    this.tagError = "";
    this.plan = [];
    this.graph = undefined;
    this.render();
    // The graph is what the forecast is derived FROM, so it is read before the
    // tag list rather than lazily on the first keystroke -- a preview that
    // appeared one character late would be answering about the previous choice.
    try {
      this.graph = await loadGraphFile(graphDocumentPath("install", this.deps.installRoot));
    } catch (err) {
      // A graph that will not load is a fault in the extension's own
      // installation, not in the operator's choice. It costs the PREVIEW and
      // nothing else: the run loads the graph itself and will fail loudly.
      this.error = `The install graph could not be read, so there is no preview: ${
        err instanceof Error ? err.message : String(err)
      }`;
    }
    if (this.disposed) return;
    await this.loadTags();
  }

  private async loadTags(): Promise<void> {
    const cwd = this.deps.installRoot;
    const list = this.deps.listTags ?? ((dir: string) => listReleaseTags({ cwd: dir }));
    this.listing = await list(cwd);
    // Warmed in the same async phase, so the page paints once with both
    // answers rather than twice. `render` below reads it back through peek().
    await this.deps.releases?.get();
    if (this.disposed) return;
    this.render();
  }

  /**
   * Records the chosen tag and re-derives the forecast.
   *
   * The forecast is recomputed on every change rather than once at Start: what
   * the run will touch is the question the operator is answering, and a preview
   * that lagged the field would be answering it about the previous choice.
   */
  private setTarget(value: string): void {
    this.target = value.trim();
    if (this.target === "") {
      this.tagError = "";
      this.plan = [];
      return;
    }
    this.tagError = tagProblem(this.target) ?? "";
    if (this.tagError !== "") {
      this.plan = [];
      return;
    }
    const graph = this.graph;
    this.plan =
      graph === undefined
        ? []
        : upgradePlan({ graph, from: this.instance?.version ?? "", to: this.target });
  }

  // -------------------------------------------------------------------------
  // rebuild from checkout (memql#4246)
  // -------------------------------------------------------------------------

  /**
   * Opens the rebuild screen and gathers the facts its checklist states.
   *
   * PAINTS FIRST, THEN GATHERS. Docker's probe spawns a script and the git
   * reads spawn three more, so a screen that waited for all of them would sit
   * blank after a click. It renders with the field and no checklist -- the same
   * shape the wizard's collect screen takes -- and repaints when the answers
   * land.
   *
   * The staleness guard is the same one `computePreflight` uses: the facts are
   * only adopted if this page is STILL on the rebuild screen when they arrive.
   * An operator who clicked Back is not shown a checklist about a run they
   * abandoned.
   */
  private async openRebuild(): Promise<void> {
    const instance = this.instance;
    if (instance === undefined) return;
    this.screen = "rebuildPreflight";
    this.error = "";
    this.rebuildFacts = undefined;
    this.render();

    const dir = instance.checkout ?? "";
    const [dockerReachable, checkoutIsMemql, state] = await Promise.all([
      this.dockerReachable(),
      isMemqlCheckout(dir),
      readCheckoutState(dir),
    ]);
    const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    if (this.disposed || this.screen !== "rebuildPreflight") return;
    this.rebuildFacts = {
      dockerReachable,
      checkoutDir: dir,
      checkoutIsMemql,
      ...(state === undefined ? {} : { state }),
      // Off the INSTANCE, which derives it from the same receipt every other
      // local fact comes from -- rather than a second read that could disagree
      // with the row the operator is looking at.
      imageSource: instance.imageSource ?? "",
      releasedTag: recordedCheckout(receipt).tag,
    };
    this.render();
  }

  /**
   * Whether Docker answers, asked with the install graph's own gate.
   *
   * `install.dockerAccess` is READ-ONLY by design -- its header says so at
   * length -- and it is the same classification the install graph blocks on, so
   * the checklist and the run cannot disagree about the same machine. Anything
   * other than a clean exit is "not reachable": the script reports a missing
   * daemon, a stopped one and one refusing this user all as exit 4, and each of
   * those is a rebuild that will fail at its first command.
   */
  private async dockerReachable(): Promise<boolean> {
    const run = this.deps.runScript ?? runCapabilityScript;
    try {
      const outcome = await run({
        scriptPath: capabilityScriptPath("install.dockerAccess", this.deps.installRoot),
        params: {},
        capability: "install.dockerAccess",
        timeoutMs: DOCKER_PROBE_TIMEOUT_MS,
      });
      return outcome.exitCode === 0;
    } catch {
      // The probe is a courtesy on a checklist. A probe that could not be
      // spawned says "not reachable", which is the fail-closed direction and
      // costs an operator one sentence they can check for themselves.
      return false;
    }
  }

  /**
   * Rebuilds this cluster's images from its checkout.
   *
   * THE SAME RUN MACHINERY AS EVERY OTHER RUN ON THIS PAGE: `runRebuild` goes
   * through `executeGraph`, so the progress rows, the receipt entry and the
   * failure screen are the ones `startDeploy` already gets. What differs is the
   * graph document, three params, and the wording.
   *
   * NO RECEIPT-DERIVED ANSWERS TO COLLECT. A deployment needs the provider key,
   * the domain and the owner because it re-runs the install graph; a rebuild
   * runs one step that takes a directory, an Application name and a node list.
   * So there is no `providerKeyFile` refusal here -- there is nothing it could
   * be missing for.
   */
  private async startRebuild(): Promise<void> {
    if (this.runAbort !== undefined) return;
    const instance = this.instance;
    const checkout = instance?.checkout ?? "";
    if (instance === undefined || checkout === "") {
      // The action is not offered without a checkout, so this is a message the
      // page never rendered -- refused rather than run against a guessed path.
      this.error =
        "MemQL has no record of a checkout for this cluster, so there is nothing to build from. " +
        "Repair the install to clone one.";
      this.screen = "overview";
      this.render();
      return;
    }

    this.error = "";
    this.runMode = "rebuild";
    this.screen = "running";
    this.render();

    const recorder = await RunRecorder.begin({
      dir: this.deps.runsDir ?? defaultRunsDir(),
      instance: instance.name,
      kind: "rebuild",
      entropy: randomBytes(4).toString("hex"),
    });
    // The tree reads the run log, so it can show this run before its first step
    // reports -- a rebuild is minutes long, and that is a long time for a click
    // to have left no trace.
    this.deps.refreshTree();

    const controller = new AbortController();
    this.runAbort = controller;

    let report: ExecutionReport | undefined;
    let failure: string | undefined;
    try {
      report = await runRebuild(
        {
          root: this.deps.installRoot,
          receiptFile: this.deps.receiptFile,
          skip: new Set<string>(),
          // Not read by `rebuildPlan`, and required by the type: a rebuild
          // touches no AI provider, so naming one would be an assertion about
          // this machine that this run has no business making.
          provider: "",
          stepParams: {},
          stackDir: checkout,
          nodes: this.rebuildNodes,
          timeoutMs: REBUILD_TIMEOUT_MS,
        },
        {
          onEvent: (event) => {
            this.state.apply(event);
            void recorder.apply(event);
            this.render();
          },
          signal: controller.signal,
          ...(this.deps.runScript !== undefined ? { run: this.deps.runScript } : {}),
        },
      );
    } catch (err) {
      // A THROW IS NOT A FAILED STEP -- the same distinction startDeploy draws.
      failure = err instanceof Error ? err.message : String(err);
    } finally {
      this.runAbort = undefined;
    }

    const cancelled = controller.signal.aborted;
    const ok = !cancelled && failure === undefined && report?.ok === true;
    await recorder.finish(cancelled ? "cancelled" : ok ? "succeeded" : "failed");
    this.deps.refreshTree();

    if (this.disposed) return;
    if (failure !== undefined) {
      this.error = failure;
      this.state.finish({ ok: false });
    } else {
      this.state.finish({ ok: report?.ok === true });
    }

    if (ok) {
      // THE CONSTRUCT CATALOG IS NOW STALE, and nothing else would notice. The
      // cluster loaded a new DSL tree seconds ago, so every construct's
      // training state was decided against the tree that is no longer there --
      // which is the state the `edited` lens reads to offer this very button.
      void vscode.commands.executeCommand("memql.constructs.refresh");
      void vscode.window.showInformationMessage(
        rebuiltMessage(
          instance.name,
          report?.outcomes.find((o) => o.id === "rebuildFromCheckout")?.envelope?.result,
        ),
      );
    }

    this.screen = this.state.failures.length > 0 ? "failedStep" : "overview";
    if (this.screen === "overview") await this.load();
    else this.render();
  }

  // -------------------------------------------------------------------------
  // the run
  // -------------------------------------------------------------------------

  /**
   * Moves the cluster to the chosen tag.
   *
   * EVERY ANSWER BUT THE TAG COMES OFF THE RECEIPT, which is exactly what a
   * repair does and for the same reason: this machine already answered these
   * questions, and asking again invites a different answer that would seed a
   * second identity or point the hosts block at a second domain. The tag is the
   * one input that is deliberately new -- it is the whole verb.
   */
  private async startDeploy(): Promise<void> {
    if (this.runAbort !== undefined) return;
    const target = this.target;
    if (target === "" || this.tagError !== "") return;

    this.error = "";
    this.runMode = "deploy";
    this.screen = "running";
    this.render();

    const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    const providerKeyFile = recordedProviderKeyFile(receipt);
    if (providerKeyFile === "") {
      // REFUSE RATHER THAN START, the same call the repair path makes: without
      // a key path the run cannot pass the providerKey gate, and the failure it
      // would produce is exit 2 -- whose guidance says "a fault in MemQL rather
      // than in your machine", which would be a lie here.
      this.error =
        "MemQL has no record of an AI provider key for this machine, so it cannot re-run the install graph. " +
        "Repair or reinstall from the Clusters page, where the key can be collected and verified.";
      this.screen = "overview";
      this.render();
      return;
    }

    const from = this.instance?.version ?? "";
    const recorder = await RunRecorder.begin({
      dir: this.deps.runsDir ?? defaultRunsDir(),
      instance: this.instance?.name ?? "local",
      kind: "upgrade",
      ...(from !== "" ? { fromVersion: from } : {}),
      toVersion: target,
      entropy: randomBytes(4).toString("hex"),
    });
    // The tree reads the run log, so it can show this run before its first step
    // reports.
    this.deps.refreshTree();

    const controller = new AbortController();
    this.runAbort = controller;

    let report: ExecutionReport | undefined;
    let failure: string | undefined;
    try {
      report = await runInstall(
        installSessionOptions({
          root: this.deps.installRoot,
          receiptFile: this.deps.receiptFile,
          provider: recordedProvider(receipt) || DEFAULT_INPUTS.provider,
          domain: recordedDomain(receipt) || DEFAULT_LOCAL_DOMAIN,
          ownerEmail: DEFAULT_INPUTS.ownerEmail,
          ownerFirstName: DEFAULT_INPUTS.ownerFirstName,
          ownerLastName: DEFAULT_INPUTS.ownerLastName,
          providerKeyFile,
          // THE ONE VALUE THAT IS NOT THE RECORDED ONE.
          tag: target,
          timeoutMs: STEP_TIMEOUT_MS,
        }),
        {
          onEvent: (event) => {
            this.state.apply(event);
            void recorder.apply(event);
            this.render();
          },
          signal: controller.signal,
          ...(this.deps.runScript !== undefined ? { run: this.deps.runScript } : {}),
        },
      );
    } catch (err) {
      // A THROW IS NOT A FAILED STEP. Everything a step can do wrong arrives as
      // an event and is already on screen; reaching here means the run could
      // not be attempted at all.
      failure = err instanceof Error ? err.message : String(err);
    } finally {
      this.runAbort = undefined;
    }

    const cancelled = controller.signal.aborted;
    await recorder.finish(
      cancelled ? "cancelled" : failure !== undefined || report?.ok !== true ? "failed" : "succeeded",
    );
    this.deps.refreshTree();

    if (this.disposed) return;
    if (failure !== undefined) {
      this.error = failure;
      this.state.finish({ ok: false });
    } else {
      this.state.finish({ ok: report?.ok === true });
    }
    this.screen = this.state.failures.length > 0 ? "failedStep" : "overview";
    if (this.screen === "overview") await this.load();
    else this.render();
  }

  /**
   * Run one deploy-control action against the connected cluster.
   *
   * THE GATE IS NEVER THIS METHOD. The id is narrowed against the catalog
   * because the postMessage channel is untrusted and an unrecognised id must be
   * dropped rather than reaching `actionById` and throwing -- but whether the
   * caller MAY run it is the engine's decision, taken again on the far side of
   * the same gate the unary path runs. What comes back on a refusal names the
   * role required, and it is rendered verbatim.
   */
  private async runDeploy(rawId: string): Promise<void> {
    const spec = DEPLOY_ACTIONS.find((a) => a.id === rawId);
    if (spec === undefined || this.instance === undefined) return;
    const port = this.deps.deployPort?.();
    if (port === undefined) {
      this.outcome = "ERROR: not connected to this cluster.";
      this.render();
      return;
    }
    const request = await this.deployRequest(spec.id);
    if (request === undefined) return;

    const outcome = await runDeployAction(port, request);
    if (this.disposed) return;
    // The engine's own line, including the audit id and -- on a refusal -- the
    // role that would have worked. Surfaced rather than reworded: a paraphrase
    // is one more thing that can be wrong, and the operator may need to match
    // it against a log line.
    this.outcome = outcome.line;
    await this.load();
  }

  // -------------------------------------------------------------------------
  // the upgrade button (memql#3997)
  // -------------------------------------------------------------------------

  /**
   * Whether this page offers the move to the newest release, and what happens
   * when it is taken.
   *
   * Recomputed per render rather than stored: the recorded version changes
   * under this page (a run finishes, a learner writes) and the release listing
   * arrives after the first paint, so a cached verdict would be answering about
   * a cluster the page has since re-read.
   */
  private upgrade(): UpgradeVerdict {
    const instance = this.instance;
    if (instance === undefined) return { kind: "none", reason: "no instance loaded" };
    return upgradeVerdict({
      instance,
      // peek(), never get(): this is called from the synchronous render path.
      version: describeVersion({
        recorded: instance.version,
        listing: this.deps.releases?.peek(),
      }),
      ...(this.roleVisibility === undefined ? {} : { visibility: this.roleVisibility }),
    });
  }

  /**
   * The button, end to end: refuse, or confirm once and run.
   *
   * ONE CONFIRMATION covers the whole move, including the remote path's two
   * RPCs. Prompting twice would turn a single decision into a sequence the
   * operator can be halfway through, and there is no coherent state to be
   * halfway into: a cut version with nothing shipped is a pending record they
   * did not ask for.
   */
  private async runUpgrade(): Promise<void> {
    const verdict = this.upgrade();
    if (verdict.kind === "none") return;

    if (verdict.kind === "refused") {
      // REFUSAL, NOT A WARNING. The move can leave a cluster running with an
      // empty graph and no error anywhere (version/barriers.ts says why), and a
      // warning is something an operator clicks past.
      //
      // `error`, not `outcome`: this is "a failure this page produced" -- the
      // type's own words -- rather than a line the engine wrote, and it is the
      // field BOTH the local and the remote page render. Nothing was sent, so
      // there is no audit id and no engine line to surface.
      this.error = verdict.message;
      this.render();
      return;
    }

    const ask = this.deps.confirm ?? (async () => undefined);
    const typed = await ask(
      `${verdict.confirmation} Type ${verdict.phrase} to confirm.`,
      verdict.phrase,
    );
    if (typed === undefined) return;
    if (!confirmationMatches(verdict.phrase, typed)) {
      this.error = `That did not match ${verdict.phrase}, so nothing was run.`;
      this.render();
      return;
    }
    this.error = "";

    if (verdict.target.flow === "upgradeToTag") {
      // The SAME run path "Create deployment" reaches, with the target already
      // decided. Not a second implementation that could disagree with it about
      // what a move does.
      this.target = verdict.target.to;
      this.tagError = "";
      await this.startDeploy();
      return;
    }
    await this.runRemoteUpgrade(verdict.target);
  }

  /**
   * THE ONE PLACE THE REMOTE UPGRADE TOUCHES THE DEPLOY-CONTROL SURFACE.
   *
   * Epic memql#3943 is collapsing that surface to a single target and removing
   * the environment parameter. Everything env-shaped about a remote upgrade is
   * inside this function -- one `env` value, resolved once and passed to
   * `cutVersion`, which is the only call on this path that still takes one
   * (`deploy` already takes a deploymentId and nothing else). So the collapse
   * is a line here rather than a scavenger hunt, and nothing about this button
   * introduces a NEW env-shaped parameter.
   *
   * TWO RPCs, ONE DECISION. `cutVersion` writes a pending deployment record at
   * the target version; `deploy` ships it. They are separate calls because the
   * engine has no combined one, not because the operator made two choices --
   * which is why the confirmation happened before either.
   */
  private async runRemoteUpgrade(target: UpgradeTarget): Promise<void> {
    const port = this.deps.deployPort?.();
    if (port === undefined) {
      this.outcome = "ERROR: not connected to this cluster.";
      this.render();
      return;
    }
    const cut = await runDeployAction(port, {
      id: "cutVersion",
      bump: "patch",
      // The version is NAMED rather than left to the engine's suggestion. The
      // operator confirmed a specific release; cutting whatever comes next
      // would ship a version nobody agreed to.
      version: target.to,
    });
    if (this.disposed) return;
    if (cut.kind === "error") {
      // Verbatim, audit id and all -- including, on a refusal, the role that
      // would have worked.
      this.outcome = cut.line;
      await this.load();
      return;
    }

    // THE RECORD THE CUT JUST CREATED, BY ID. cutVersion returns it on
    // ActionResult.details (component/deploycontrol/cutversion.go stamps
    // `deploymentId`), so the ship names that row rather than "whatever is
    // newest now".
    //
    // This read the catalog back and took runs[0] until memql#3997 carried
    // `details` through deploy/controller.ts. That was correct almost always
    // and wrong for whichever operator lost a race: two cuts against one
    // cluster inside the reload window and the second ship names the first
    // one's record. There is no window now, because there is no interval --
    // the id came back with the cut.
    const record = cut.details.deploymentId ?? "";
    if (record === "") {
      // An engine too old to stamp the id. REFUSE rather than fall back to the
      // catalog: the fallback is the race this replaced, and a pending record
      // nobody shipped is visible, inert and easy to ship by hand -- while the
      // wrong record shipped is neither.
      this.outcome = joinOutcomes(
        cut,
        `ERROR: ${target.to} was cut, but the cluster returned no deployment id, so nothing was shipped. ` +
          `The pending record is on the Deployments list and can be shipped from there.`,
      );
      await this.load();
      return;
    }

    const ship = await runDeployAction(port, { id: "deploy", deploymentId: record });
    if (this.disposed) return;
    // BOTH audit ids reach the operator. Two calls happened, the engine audited
    // each, and reporting only the second would leave the cut untraceable.
    this.outcome = joinOutcomes(cut, ship.line);
    await this.load();
  }

  /**
   * The parameters an action needs, and the confirmation the destructive ones
   * demand.
   *
   * THE PHRASE IS THE TARGET, never the word "yes": re-typing the deployment
   * being rolled back to forces the operator to look at what they selected.
   * Undefined means "do not run" -- a cancelled prompt and a mismatched phrase
   * are the same answer.
   */
  private async deployRequest(
    id: DeployActionId,
  ): Promise<DeployActionRequest | undefined> {
    const ask = this.deps.confirm ?? (async () => undefined);
    switch (id) {
      case "cutVersion":
        return { id, bump: "patch", version: "" };
      case "deploy": {
        // THE RECORD THIS PAGE RENDERED, BY ID (memql#4017). `load()` resolves
        // it (state/deploymentHistory.pendingDeploymentId) and the overview
        // prints it above this button, so the ship names the row the operator
        // was looking at.
        //
        // This took `runs[0]` -- the newest record in the catalog the page last
        // read, re-derived at the click -- and it was wrong twice over. It was
        // not the PENDING record, and `deploy` transitions whatever it is given
        // pending -> in_progress without checking what it is transitioning from
        // (component/deploycontrol/deploy.go), so a cluster whose newest record
        // had landed re-shipped a succeeded deployment. And it was resolved at
        // the CLICK, so two cuts against one cluster inside the reload window
        // and the ship named the other operator's record -- the same shape
        // #4015 removed from the upgrade path by carrying the id the cut
        // returned. There is no cut to return one here, so the id is fixed when
        // the page is built instead.
        const target = this.instance?.pendingDeploymentId ?? "";
        if (target === "") {
          // REFUSE rather than fall back to another record, for the reason the
          // remote upgrade path gives about a missing id: a pending record
          // nobody shipped is visible, inert and easy to ship by hand, while
          // the wrong record shipped is none of those things.
          this.outcome =
            "ERROR: nothing is cut, so there is no pending deployment record to ship.";
          this.render();
          return undefined;
        }
        return { id, deploymentId: target };
      }
      case "rollback": {
        // The newest run that actually LANDED, which is what a rollback target
        // has to be -- rolling back to a record that failed would redeploy a
        // digest that never worked.
        const landed = this.runs.find((r) => r.status === "succeeded");
        const target = landed?.id ?? "";
        if (!(await this.confirmed(ask, id, target))) return undefined;
        return { id, toDeploymentId: target };
      }
      case "rolloutAction": {
        const subAction = "promote";
        if (rolloutRequiresConfirmation(subAction) && !(await this.confirmed(ask, id, this.instance?.name ?? ""))) {
          return undefined;
        }
        return { id, rollout: "", subAction };
      }
      default:
        return undefined;
    }
  }

  private async confirmed(
    ask: (prompt: string, phrase: string) => Promise<string | undefined>,
    id: DeployActionId,
    target: string,
  ): Promise<boolean> {
    const phrase = confirmationPhrase(id, target);
    if (phrase === "") {
      // An action with nothing identifiable to re-type must not proceed
      // unchallenged -- see confirmationPhrase.
      this.outcome = `ERROR: nothing to confirm against, so ${id} was not run.`;
      this.render();
      return false;
    }
    const typed = await ask(`Type ${phrase} to confirm.`, phrase);
    if (typed === undefined) return false;
    if (!confirmationMatches(phrase, typed)) {
      this.outcome = `ERROR: that did not match ${phrase}, so ${id} was not run.`;
      this.render();
      return false;
    }
    return true;
  }

  // -------------------------------------------------------------------------
  // rendering
  // -------------------------------------------------------------------------

  private bodyHtml(): string {
    const instance = this.instance;
    if (instance === undefined) {
      return `<h1>Deployments</h1>
<p class="lede">Reading this machine...</p>`;
    }
    switch (this.screen) {
      case "chooseTag":
        return renderChooseTag({
          instance,
          listing: this.listing,
          target: this.target,
          tagError: this.tagError,
          plan: this.plan,
          summary: this.plan.length === 0 ? "" : upgradeSummary(this.plan),
          sameVersion: isSameVersion(instance.version ?? "", this.target),
        });
      case "rebuildPreflight":
        return renderRebuildScreen({
          checkoutDir: instance.checkout ?? "",
          nodes: this.rebuildNodes,
          ...(this.rebuildFacts === undefined
            ? {}
            : {
                preflight: rebuildPreflightItems({
                  ...this.rebuildFacts,
                  nodes: this.rebuildNodes,
                }),
              }),
        });
      case "running":
        return renderRunningScreen(this.runScreenInput(this.state.steps));
      case "failedStep":
        return renderFailedScreen({
          ...this.runScreenInput(this.state.steps),
          failures: this.state.failures,
        });
      case "overview":
        if (instance.kind === "remote") {
          return renderRemoteInstance({
            instance,
            runs: this.runs,
            // Undefined only in the instant between the page opening and the
            // status read returning. Rendering it as "not configured" would be
            // a claim about the cluster made before anything asked it.
            pipeline: this.pipeline ?? {
              kind: "present",
              title: "Deploy",
              detail: "Reading this cluster's deployment status...",
              actions: [],
            },
            nowMs: Date.now(),
            outcome: this.outcome,
            error: this.error,
            // peek(), never get(): bodyHtml is synchronous. loadTags warms it.
            releases: this.deps.releases?.peek(),
            upgrade: this.upgrade(),
          });
        }
        return renderInstanceOverview({
          instance,
          runs: this.runs,
          actions: instanceActions(instance),
          nowMs: Date.now(),
          error: this.error,
          releases: this.deps.releases?.peek(),
          upgrade: this.upgrade(),
        });
    }
  }

  private runScreenInput(steps: StepProgress[]): {
    steps: StepProgress[];
    mode: RunMode;
    running: boolean;
  } {
    return { steps, mode: this.runMode, running: this.runAbort !== undefined };
  }

  private render(): void {
    if (this.disposed) return;
    const nonce = nonceValue();
    this.panel.title =
      this.instance === undefined ? "MemQL deployment" : `Deployment: ${this.instance.name}`;
    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(this.instance?.name ?? "MemQL deployment")}</title>
<style nonce="${nonce}">
${brandStyleBlock()}
${viewKitStyles}

  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0;
         padding: 16px 20px; max-width: 780px; }
  h1 { font-size: 1.2em; margin: 0 0 4px; }
  h2 { font-size: 1em; margin: 20px 0 6px; }
  .lede { color: var(--memql-muted); margin: 0 0 16px; }
  .notice { color: var(--memql-muted); margin: 0 0 12px; }
  .facts { margin-bottom: 12px; }
  .fact { display: flex; gap: 8px; align-items: baseline; padding: 1px 0; }
  .fact-key { flex: none; min-width: 8em; color: var(--memql-muted); }
  .runs, .plan { list-style: none; margin: 0; padding: 0; }
  .run, .plan-step { display: flex; gap: 8px; align-items: baseline; padding: 2px 0; }
  .run-kind, .plan-id { flex: none; min-width: 10em; }
  .run-detail, .plan-detail { color: var(--memql-muted); }
  .plan-mark { flex: none; width: 2em; color: var(--memql-muted); }
  /* "Node types", never "Steps": a remote run's items are per-tier specs, not
     script executions, and the label is what stops one being read as the other. */
  .items-label { color: var(--memql-muted); margin: 2px 0 0 1em; }
  .run-block { margin-bottom: 10px; }
  .run-block .runs { margin-left: 1em; }
  /* The steps that will actually change something read at full strength; the
     ones expected to skip are quiet, because the question the forecast answers
     is "what is this going to touch". */
  .plan-step[data-effect="runs"] .plan-id { font-weight: 600; }
  .plan-step[data-effect="skip"] { opacity: 0.65; }
  .field { margin-bottom: 12px; }
  .field label { display: block; margin-bottom: 3px; }
  .field input, .field select { width: 100%; box-sizing: border-box; padding: 4px 6px; font: inherit;
                 color: var(--memql-fg);
                 background: var(--memql-surface);
                 border: 1px solid var(--memql-border-strong); border-radius: 3px; }
  .field[data-invalid="true"] input { border-color: var(--vscode-editorError-foreground); }
  .hint { color: var(--memql-muted); margin-top: 3px; }
  .said { margin: 0 0 8px; }
  .remedy { font-family: var(--vscode-editor-font-family, monospace);
            background: var(--memql-raised);
            border: 1px solid var(--memql-border);
            border-radius: 4px; padding: 8px 10px; margin: 6px 0 0;
            overflow-x: auto; white-space: pre; }
  .error { color: var(--memql-danger); margin-top: 3px; }
  .actions { display: flex; gap: 8px; margin-top: 16px; }
  button.primary, button.secondary {
    font: inherit; padding: 4px 12px; cursor: pointer; border-radius: 2px;
    border: 1px solid transparent; }
  button.primary { background: var(--vscode-button-background);
                   color: var(--vscode-button-foreground); }
  button.secondary { background: var(--vscode-button-secondaryBackground);
                     color: var(--vscode-button-secondaryForeground); }
  button.destructive { color: var(--memql-data-string); }
  button[disabled] { opacity: 0.5; cursor: default; }
</style>
</head>
<body>
${brandStrip("MemQL")}
${this.bodyHtml()}
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  document.addEventListener('click', (e) => {
    const choose = e.target.closest('[data-choose]');
    if (choose) { vscode.postMessage({ type: 'choose', value: choose.dataset.choose }); return; }
    const deploy = e.target.closest('[data-deploy]');
    if (deploy) { vscode.postMessage({ type: 'deploy', value: deploy.dataset.deploy }); return; }
    const act = e.target.closest('[data-act]');
    if (act && act.tagName !== 'SELECT') vscode.postMessage({ type: act.dataset.act });
  });
  // Recording only -- the host does NOT repaint on a keystroke, because a
  // repaint replaces the whole document and would take the caret with it.
  // The select is the exception: choosing a tag IS the decision, and the
  // forecast beneath it has to follow. (No backticks in here: this script is
  // itself inside a template literal.)
  document.addEventListener('input', (e) => {
    const field = e.target.closest('[data-field]');
    if (field) vscode.postMessage({
      type: 'input', value: { field: field.dataset.field, text: field.value } });
  });
  document.addEventListener('change', (e) => {
    const pick = e.target.closest('select[data-act="pickTag"]');
    if (pick) vscode.postMessage({ type: 'pickTag', value: pick.value });
  });
</script>
</body>
</html>`;
  }
}

/**
 * A CSP nonce, from a CSPRNG.
 *
 * The same call the other panels make, and for the same reason: a nonce a page
 * can predict is a nonce an injected script can carry, which defeats its whole
 * purpose.
 */
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}

/**
 * Whether a directory is a MemQL checkout, by the two files `k3d.dev` itself
 * gates on (memql#4246).
 *
 * THE SAME TWO, deliberately: the script refuses a repo-root with no
 * `Dockerfile` or no `deploy/k8s/overlays/local/kustomization.yaml` with exit 4,
 * so a checklist testing anything else would pass a directory the run then
 * refuses -- which is the one thing a preflight must not do.
 */
async function isMemqlCheckout(dir: string): Promise<boolean> {
  if (dir === "") return false;
  const required = [
    path.join(dir, "Dockerfile"),
    path.join(dir, "deploy", "k8s", "overlays", "local", "kustomization.yaml"),
  ];
  const found = await Promise.all(
    required.map((file) =>
      fs
        .access(file)
        .then(() => true)
        .catch(() => false),
    ),
  );
  return found.every((ok) => ok);
}

/**
 * Two engine outcomes as one line.
 *
 * BOTH survive, audit ids and all. A remote upgrade is two audited calls, and a
 * line reporting only the second would leave the cut version untraceable -- the
 * audit id is the deliverable of runDeployAction, not a decoration on it.
 */
function joinOutcomes(first: DeployOutcome, second: string): string {
  return `${first.line} ${second}`;
}
