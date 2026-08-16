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

import * as vscode from "vscode";

import { escapeHtml, viewKitStyles } from "@znasllc-io/memql-view-kit";

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
import type { ExecutionReport } from "../install/executor.js";
import { installSessionOptions, runInstall, type SessionHooks } from "../install/session.js";
import { listReleaseTags, tagProblem, type TagListing } from "../install/tags.js";
import type { ReleaseCache } from "../version/releaseCache.js";
import {
  DEFAULT_INPUTS,
  AddClusterState,
  type StepProgress,
} from "../state/addCluster.js";
import type { Instance, Run } from "../state/deployments.js";
import { buildCatalog, type CatalogInputs } from "../state/deploymentsCatalog.js";
import { recordedDomain, recordedProvider, recordedProviderKeyFile, readReceipt } from "../install/receipt.js";
import { RunRecorder } from "../state/runRecorder.js";
import { defaultRunsDir } from "../state/runLog.js";
import { isSameVersion, upgradePlan, upgradeSummary, type PlannedStepView } from "../state/upgradePlan.js";
import { graphDocumentPath, loadGraphFile, type Graph } from "../install/graph.js";
import { DEFAULT_LOCAL_DOMAIN } from "../install/stackPin.js";
import { renderChooseTag, renderInstanceOverview, renderRemoteInstance } from "./deploymentScreens.js";
import { renderFailedScreen, renderRunningScreen } from "./installScreens.js";

/** The same ceiling the wizard gives a step. */
const STEP_TIMEOUT_MS = 600_000;

type Screen = "overview" | "chooseTag" | "running" | "failedStep";

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
  /** The deploy-console environment this instance maps to. */
  deployEnv?: (instanceName: string) => string;
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
    void this.load();
  }

  private constructor(
    _context: vscode.ExtensionContext,
    private readonly deps: DeploymentPanelDeps,
  ) {
    this.panel = vscode.window.createWebviewPanel(
      "memqlDeployment",
      "memQL deployment",
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
    const read = await readDeploymentStatus(port, this.deps.deployEnv?.(name) ?? name);
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
      await this.startDeploy();
      return;
    }
    if (type === "guided") {
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
    this.screen = "running";
    this.render();

    const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    const providerKeyFile = recordedProviderKeyFile(receipt);
    if (providerKeyFile === "") {
      // REFUSE RATHER THAN START, the same call the repair path makes: without
      // a key path the run cannot pass the providerKey gate, and the failure it
      // would produce is exit 2 -- whose guidance says "a fault in memQL rather
      // than in your machine", which would be a lie here.
      this.error =
        "memQL has no record of an AI provider key for this machine, so it cannot re-run the install graph. " +
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
    const env = this.deps.deployEnv?.(this.instance.name) ?? this.instance.name;

    const request = await this.deployRequest(spec.id, env);
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
    const env = this.deps.deployEnv?.(target.instanceName) ?? target.instanceName;

    const cut = await runDeployAction(port, {
      id: "cutVersion",
      env,
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
   * THE PHRASE IS THE TARGET, never the word "yes": re-typing the version being
   * promoted, or the deployment being rolled back to, forces the operator to
   * look at what they selected. Undefined means "do not run" -- a cancelled
   * prompt and a mismatched phrase are the same answer.
   */
  private async deployRequest(
    id: DeployActionId,
    env: string,
  ): Promise<DeployActionRequest | undefined> {
    const ask = this.deps.confirm ?? (async () => undefined);
    const current = this.runs[0];
    switch (id) {
      case "cutVersion":
        return { id, env, bump: "patch", version: "" };
      case "deploy": {
        const target = current?.id ?? "";
        if (target === "") {
          this.outcome = "ERROR: there is no deployment record to ship.";
          this.render();
          return undefined;
        }
        return { id, deploymentId: target };
      }
      case "promote": {
        const version = current?.toVersion ?? "";
        if (!(await this.confirmed(ask, id, version))) return undefined;
        return { id, version };
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
        if (rolloutRequiresConfirmation(subAction) && !(await this.confirmed(ask, id, env))) {
          return undefined;
        }
        return { id, env, rollout: "", subAction };
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
    mode: "deploy";
    running: boolean;
  } {
    return { steps, mode: "deploy", running: this.runAbort !== undefined };
  }

  private render(): void {
    if (this.disposed) return;
    const nonce = nonceValue();
    this.panel.title =
      this.instance === undefined ? "memQL deployment" : `Deployment: ${this.instance.name}`;
    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(this.instance?.name ?? "memQL deployment")}</title>
<style nonce="${nonce}">
  :root {
    --vk-fg: var(--vscode-foreground);
    --vk-muted-fg: var(--vscode-descriptionForeground);
    --vk-border: var(--vscode-panel-border);
    --vk-hover-bg: var(--vscode-list-hoverBackground);
    --vk-selected-bg: var(--vscode-list-activeSelectionBackground);
    --vk-selected-fg: var(--vscode-list-activeSelectionForeground);
    --vk-mono-font: var(--vscode-editor-font-family, monospace);
    --vk-subtle-bg: var(--vscode-textCodeBlock-background, transparent);
  }

${viewKitStyles}

  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0;
         padding: 16px 20px; max-width: 780px; }
  h1 { font-size: 1.2em; margin: 0 0 4px; }
  h2 { font-size: 1em; margin: 20px 0 6px; }
  .lede { color: var(--vscode-descriptionForeground); margin: 0 0 16px; }
  .notice { color: var(--vscode-descriptionForeground); margin: 0 0 12px; }
  .facts { margin-bottom: 12px; }
  .fact { display: flex; gap: 8px; align-items: baseline; padding: 1px 0; }
  .fact-key { flex: none; min-width: 8em; color: var(--vscode-descriptionForeground); }
  .runs, .plan { list-style: none; margin: 0; padding: 0; }
  .run, .plan-step { display: flex; gap: 8px; align-items: baseline; padding: 2px 0; }
  .run-kind, .plan-id { flex: none; min-width: 10em; }
  .run-detail, .plan-detail { color: var(--vscode-descriptionForeground); }
  .plan-mark { flex: none; width: 2em; color: var(--vscode-descriptionForeground); }
  /* "Node types", never "Steps": a remote run's items are per-tier specs, not
     script executions, and the label is what stops one being read as the other. */
  .items-label { color: var(--vscode-descriptionForeground); margin: 2px 0 0 1em; }
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
                 color: var(--vscode-input-foreground);
                 background: var(--vscode-input-background);
                 border: 1px solid var(--vscode-input-border, var(--vscode-panel-border)); }
  .field[data-invalid="true"] input { border-color: var(--vscode-editorError-foreground); }
  .hint { color: var(--vscode-descriptionForeground); margin-top: 3px; }
  .said { margin: 0 0 8px; }
  .remedy { font-family: var(--vscode-editor-font-family, monospace);
            background: var(--vscode-textCodeBlock-background, transparent);
            border: 1px solid var(--vscode-panel-border);
            border-radius: 4px; padding: 8px 10px; margin: 6px 0 0;
            overflow-x: auto; white-space: pre; }
  .error { color: var(--vscode-inputValidation-errorForeground,
                   var(--vscode-editorError-foreground)); margin-top: 3px; }
  .actions { display: flex; gap: 8px; margin-top: 16px; }
  button.primary, button.secondary {
    font: inherit; padding: 4px 12px; cursor: pointer; border-radius: 2px;
    border: 1px solid transparent; }
  button.primary { background: var(--vscode-button-background);
                   color: var(--vscode-button-foreground); }
  button.secondary { background: var(--vscode-button-secondaryBackground);
                     color: var(--vscode-button-secondaryForeground); }
  button.destructive { color: var(--vscode-editorWarning-foreground); }
  button[disabled] { opacity: 0.5; cursor: default; }
</style>
</head>
<body>
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
 * Two engine outcomes as one line.
 *
 * BOTH survive, audit ids and all. A remote upgrade is two audited calls, and a
 * line reporting only the second would leave the cut version untraceable -- the
 * audit id is the deliverable of runDeployAction, not a decoration on it.
 */
function joinOutcomes(first: DeployOutcome, second: string): string {
  return `${first.line} ${second}`;
}
