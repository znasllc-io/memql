// The cluster page: one cluster's state and version, its history, and the acts
// legal from where it stands.
//
// WHAT THIS FILE IS. The webview lifecycle, the postMessage boundary, the reads
// that feed the page and the dispatch of what the page asks for. WHAT IT DOES
// NOT DECIDE: which acts exist (deploy/instanceActions.ts), what a state or a
// run is called (state/deploymentsCatalog.ts), how each screen is drawn
// (webview/deploymentScreens.ts), and how a long run is carried out
// (deploy/localRun.ts) -- all of those run under bare `node --test`.
//
// ONE DOCUMENT PER SCREEN, PATCHED AFTER. The page is built on the kit's
// LiveView: moving to a new screen assigns a document, and everything that
// changes while it shows -- a re-read of the machine, a run's bar and log --
// arrives as a message the page applies in place. The old page reassigned the
// whole document once a second during a run, which threw the reader to the
// top and took focus off Cancel.
//
// THE PAGE KEEPS ITSELF CURRENT. It re-reads when the connection changes, when
// the files under ~/.memql change, when it becomes visible, and on a timer
// while it is visible (briskly while a run is going). It used to re-read only
// when opened, and offered acts that had stopped being legal.
//
// A RUN IS NOT THE PAGE'S. Runs live in the machine's one run slot
// (deploy/localRun.ts), so closing this tab leaves a run going, and opening the
// page -- or a run's row -- while one is going shows it live, with Cancel when
// Cancel can still act, and nothing that would start a second.
//
// Refs: #3739 #3733 #4427 #4246

import { randomBytes } from "node:crypto";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import * as vscode from "vscode";

import { currentBodyThemeAttr, onAppearanceChange } from "./theme.js";
import { pageDocument } from "./ui/document.js";
import { LiveView } from "./ui/liveView.js";
import { pageMessage, type PageToHost } from "./ui/protocol.js";

import { confirmationMatches, roleVisibility, type RoleVisibility } from "../deploy/actions.js";
import {
  readDeploymentStatus,
  runDeployAction,
  type DeployActionRequest,
  type DeployControlPort,
  type DeployOutcome,
} from "../deploy/controller.js";
import {
  barOffers,
  localOverviewBar,
  offersLocal,
  remoteOverviewBar,
  runDetailBar,
  type LocalActId,
  type PageAct,
  type PageBar,
} from "../deploy/instanceActions.js";
import { localRuns, type LocalRun, type LocalRunDeps, type LocalRuns } from "../deploy/localRun.js";
import { pipelineState, type PipelineState } from "../deploy/pipelineState.js";
import { upgradeVerdict, type UpgradeVerdict } from "../deploy/upgrade.js";
import { readCheckoutState } from "../install/checkoutState.js";
import { graphDocumentPath, loadGraphFile, rebuildGraphPath, updateRebuildGraphPath, installGraphPath, type Graph } from "../install/graph.js";
import { capabilityScriptPath, runCapabilityScript } from "../install/runner.js";
import { readReceipt, recordedCheckout, recordedStackBranch } from "../install/receipt.js";
import type { SessionHooks } from "../install/session.js";
import { tagProblem } from "../install/tags.js";
import { readUpdateState, type UpdateState } from "../install/updateState.js";
import type { Instance, Run } from "../state/deployments.js";
import {
  buildCatalog,
  instanceConnectionWord,
  parseItemDetail,
  pollIntervalMs,
  type CatalogInputs,
  type ConnectionWord,
} from "../state/deploymentsCatalog.js";
import { rebuildCheck, type RebuildCheck, type RebuildPreflightInputs } from "../state/rebuildPreflight.js";
import { defaultRunsDir } from "../state/runLog.js";
import { updateCheck } from "../state/updatePreflight.js";
import { isSameVersion, upgradePlan, upgradeSummary, type PlannedStepView } from "../state/upgradePlan.js";
import { describeVersion } from "../version/describe.js";
import type { ReleaseCache, ReleaseListing } from "../version/releaseCache.js";
import {
  DEPLOYMENT_STYLES,
  OTHER_VERSION,
  chooseVersionScreen,
  loadingScreen,
  localOverviewScreen,
  memqlOsAddress,
  missingRunScreen,
  pullRebuildScreen,
  rebuildScreen,
  remoteOverviewScreen,
  runDetailScreen,
  runScreen,
  stepLogFileOf,
  unavailableScreen,
  type ActOutcome,
  type PageNotice,
} from "./deploymentScreens.js";

/**
 * How long the Docker gate is given before the check says it is not running.
 * Short: this is a read-only check before a run, and the run itself blocks on
 * the same gate with the graph's own budget.
 */
const DOCKER_PROBE_TIMEOUT_MS = 15_000;

/** How often a visible run screen moves its bar between executor events. */
const RUN_TICK_MS = 1_000;

/** Where a runbook the page links to is read. */
const DOCS_BASE = "https://github.com/znasllc-io/memql/blob/main/";

type Screen =
  | { kind: "overview" }
  | { kind: "chooseVersion" }
  | { kind: "rebuild" }
  | { kind: "pullRebuild" }
  | { kind: "run" }
  | { kind: "runDetail"; runId: string };

export interface DeploymentPanelDeps {
  /** Everything buildCatalog needs, minus what this panel resolves itself. */
  catalog: Omit<CatalogInputs, "connection" | "readDeployments">;
  installRoot: string;
  receiptFile: string;
  runsDir?: string;
  /** Repaints the Deployments and Clusters trees after something changed. */
  refreshTree: () => void;
  /** The SHARED release listing (single-flight), for the update and the version list. */
  releases?: ReleaseCache;
  /** The live connection, as a thunk: it changes without this page being told. */
  connection?: () => CatalogInputs["connection"];
  readDeployments?: () => CatalogInputs["readDeployments"];
  /** The deploy-control client for the CONNECTED cluster, rebuilt per call. */
  deployPort?: () => DeployControlPort | undefined;
  /** The caller's cluster role, for deciding which acts to draw. */
  readRole?: () => Promise<RoleVisibility>;
  /** Asks for a phrase to be typed back; undefined is Cancel. */
  confirm?: (c: { title: string; prompt: string; phrase: string }) => Promise<string | undefined>;
  /** Opens the add-cluster wizard on the two flows only it has: adopt and reconnect. */
  openInstallFlow: (action: "adopt" | "reconnect") => void;
  /** Connects to a registered cluster by name (the Clusters view's select). */
  connectTo?: (name: string) => Promise<void> | void;
  /** Mirrors a run's log line to the MemQL output channel, and shows it. */
  logLine?: (line: string) => void;
  showOutput?: () => void;
  /** Fires on anything the page should re-read for: a connection change, a file under ~/.memql. */
  onDidChange?: (listener: () => void) => { dispose(): void };
  /** The machine's run slot; tests inject their own. */
  runs?: LocalRuns;
  /** Called when a run the page started settles (toasts, construct refresh). */
  onRunSettled?: (run: LocalRun, pageVisible: boolean) => void;
  /** Injected by tests. */
  runScript?: SessionHooks["run"];
  graphs?: LocalRunDeps["graphs"];
  home?: string;
  now?: () => number;
}

export class DeploymentPanel {
  private static open_: DeploymentPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly live: LiveView;
  private readonly disposables: { dispose(): void }[] = [];
  private screen: Screen = { kind: "overview" };
  /** Which cluster this page is about; "" is the local one. */
  private instanceName = "";
  private instance: Instance | undefined;
  private runs: readonly Run[] = [];
  /** The first read has landed; before it the page is a skeleton. */
  private loaded = false;
  /** The cluster list would not read. */
  private registryError = "";
  private connection: ConnectionWord = "none";
  private pipeline: PipelineState | undefined;
  private visibility: RoleVisibility | undefined;
  private outcome: ActOutcome | undefined;
  private notice: PageNotice | undefined;
  private detailsOpen = false;
  private logsOpen = false;
  // change version
  private choice = "";
  private typed = "";
  private typedError = "";
  private installGraph: Graph | undefined;
  // rebuild and pull
  private nodes = "";
  private merge = false;
  private check: RebuildCheck | undefined;
  private update: UpdateState | undefined;
  // a run from the history
  private readonly logs = new Map<string, string>();
  private readonly openLogs = new Set<string>();
  private stepLabels: ReadonlyMap<string, string> = new Map();
  // the run being shown, and its subscription
  private watched: LocalRun | undefined;
  private unwatch: (() => void) | undefined;
  private tick: ReturnType<typeof setInterval> | undefined;
  private poll: ReturnType<typeof setTimeout> | undefined;
  private reading: Promise<void> = Promise.resolve();
  private disposed = false;

  /**
   * Opens the page for a cluster, or re-points the one already open.
   *
   * ONE PANEL for every cluster: a tab per cluster would leave several showing
   * states that stopped being true the moment a run finished elsewhere.
   */
  static show(context: vscode.ExtensionContext, deps: DeploymentPanelDeps, instanceName = ""): DeploymentPanel {
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
   * Opens the page on one run from the history (memql#4427).
   *
   * A RUN THAT IS STILL GOING OPENS LIVE. Clicking the spinning row is the
   * natural way to check on a run, and it used to swap the progress screen for
   * a static snapshot with the whole cluster's acts on it.
   */
  static showRun(context: vscode.ExtensionContext, deps: DeploymentPanelDeps, instanceName: string, runId: string): DeploymentPanel {
    const panel = DeploymentPanel.show(context, deps, instanceName);
    panel.screen = panel.slot().current?.recordId === runId && panel.slot().inFlight ? { kind: "run" } : { kind: "runDetail", runId };
    panel.notice = undefined;
    void panel.openRunDetail();
    panel.render();
    return panel;
  }

  /**
   * Opens the local cluster's page on one of its acts: the palette and the
   * training lens reach Change version, Rebuild and Pull and rebuild this way.
   *
   * NARROWED, NOT TRUSTED: the page waits for its read and takes the act only
   * when the cluster offers it -- a machine with no recorded checkout has no
   * rebuild -- and returns false so the caller can say why.
   */
  static async openAction(
    context: vscode.ExtensionContext,
    deps: DeploymentPanelDeps,
    id: Extract<LocalActId, "changeVersion" | "rebuildFromCheckout" | "updateAndRebuild">,
  ): Promise<boolean> {
    const panel = DeploymentPanel.show(context, deps);
    await panel.reading;
    const instance = panel.instance;
    if (panel.disposed || instance === undefined || !offersLocal(instance, id)) return false;
    if (panel.slot().inFlight) {
      panel.screen = { kind: "run" };
      panel.render();
      return true;
    }
    if (id === "changeVersion") panel.openChangeVersion();
    else if (id === "rebuildFromCheckout") await panel.openRebuild();
    else await panel.openPullRebuild();
    return true;
  }

  private constructor(
    _context: vscode.ExtensionContext,
    private readonly deps: DeploymentPanelDeps,
  ) {
    this.panel = vscode.window.createWebviewPanel("memqlDeployment", "Cluster", vscode.ViewColumn.Beside, {
      enableScripts: true,
      retainContextWhenHidden: true,
    });
    this.live = new LiveView(
      {
        setHtml: (html) => {
          this.panel.webview.html = html;
        },
        postMessage: (msg) => this.panel.webview.postMessage(msg),
      },
      (parts, screen) =>
        pageDocument({
          nonce: randomBytes(16).toString("base64"),
          title: this.panel.title,
          themeAttr: currentBodyThemeAttr(),
          screen,
          styles: DEPLOYMENT_STYLES,
          ...(screen.startsWith("overview") || screen.startsWith("run:") ? {} : { escapeAct: "back" }),
          ...parts,
        }),
    );
    this.disposables.push(
      ...onAppearanceChange(() => {
        this.live.invalidate();
        this.render();
      }),
      this.panel.onDidDispose(() => this.dispose()),
      this.panel.webview.onDidReceiveMessage((raw: unknown) => {
        void this.onMessage(raw);
      }),
    );
    // Not every host carries the view-state event (the test stub does not):
    // a page that cannot tell when it is shown still re-reads on its timer.
    const viewState = (this.panel as { onDidChangeViewState?: vscode.WebviewPanel["onDidChangeViewState"] }).onDidChangeViewState;
    if (typeof viewState === "function") {
      this.disposables.push(
        viewState.call(this.panel, () => {
          if (this.visible()) this.reload();
        }),
      );
    }
    const changes = this.deps.onDidChange?.(() => this.reload());
    if (changes !== undefined) this.disposables.push(changes);
    this.disposables.push({ dispose: this.slot().onDidStart(() => this.render()) });
    this.render();
  }

  private slot(): LocalRuns {
    return this.deps.runs ?? localRuns;
  }

  private visible(): boolean {
    return (this.panel as { visible?: boolean }).visible !== false;
  }

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    // THE RUN IS NOT STOPPED. It belongs to the machine's run slot, and a tab
    // closed during a 45-minute rebuild must not throw the rebuild away.
    this.unwatch?.();
    if (this.tick !== undefined) clearInterval(this.tick);
    if (this.poll !== undefined) clearTimeout(this.poll);
    if (DeploymentPanel.open_ === this) DeploymentPanel.open_ = undefined;
    for (const d of this.disposables) d.dispose();
  }

  private pointAt(instanceName: string): void {
    // "" and the local instance's own name are the same page, and treating
    // them as two reset a page mid-run when the palette and the tree opened it.
    const same =
      instanceName === this.instanceName ||
      (this.instance?.kind === "local" && (instanceName === "" || instanceName === this.instance.name));
    if (!same) {
      this.instanceName = instanceName;
      this.instance = undefined;
      this.loaded = false;
      this.pipeline = undefined;
      this.outcome = undefined;
      this.notice = undefined;
      this.screen = { kind: "overview" };
    }
    // A run going on this machine is what the page shows, whatever it was on.
    if (this.slot().inFlight && this.screen.kind !== "runDetail") this.screen = { kind: "run" };
    this.reload();
  }

  // -------------------------------------------------------------------------
  // reading
  // -------------------------------------------------------------------------

  /** Re-read now, and schedule the next read while the page is visible. */
  private reload(): void {
    if (this.disposed) return;
    this.reading = this.load();
  }

  private async load(): Promise<void> {
    const connection = this.deps.connection?.();
    const readDeployments = this.deps.readDeployments?.();
    const catalog = await buildCatalog({
      ...this.deps.catalog,
      ...(connection !== undefined ? { connection } : {}),
      ...(readDeployments !== undefined ? { readDeployments } : {}),
    });
    if (this.disposed) return;
    this.registryError = catalog.error ?? "";
    this.instance =
      this.instanceName === ""
        ? catalog.instances.find((i) => i.kind === "local")
        : catalog.instances.find((i) => i.name === this.instanceName);
    this.runs = this.instance === undefined ? [] : (catalog.runs.get(this.instance.name) ?? []);
    this.connection = this.instance === undefined ? "none" : instanceConnectionWord(this.instance, connection);
    this.loaded = true;
    this.render();
    if (this.instance?.kind === "remote") await this.loadPipeline();
    void this.warmReleases();
    this.schedule();
  }

  /** The next read: briskly while something is running, slowly otherwise, never while hidden. */
  private schedule(): void {
    if (this.poll !== undefined) clearTimeout(this.poll);
    this.poll = undefined;
    if (this.disposed || !this.visible()) return;
    this.poll = setTimeout(() => this.reload(), pollIntervalMs(this.runs));
    // A timer must never keep the extension host alive on its own.
    (this.poll as { unref?: () => void }).unref?.();
  }

  /** The release listing, fetched once per TTL and shared with the trees. */
  private async warmReleases(): Promise<void> {
    const releases = this.deps.releases;
    if (releases === undefined) return;
    const before = releases.peek()?.fetchedAt;
    const after = await releases.get();
    if (!this.disposed && after.fetchedAt !== before) this.render();
  }

  /**
   * Which of three states the remote cluster's deploy pipeline is in, and the
   * caller's role. Only for a connected cluster: a page not connected says so
   * on its bar, and never "no pipeline" on the strength of a socket it lacks.
   */
  private async loadPipeline(): Promise<void> {
    if (this.connection !== "connected") {
      this.pipeline = undefined;
      return;
    }
    const port = this.deps.deployPort?.();
    const visibility = (await this.deps.readRole?.()) ?? roleVisibility(undefined);
    if (this.disposed) return;
    this.visibility = visibility;
    if (port === undefined) {
      this.pipeline = undefined;
      this.render();
      return;
    }
    const read = await readDeploymentStatus(port);
    if (this.disposed) return;
    this.pipeline = pipelineState(read, visibility);
    this.render();
  }

  private upgrade(): UpgradeVerdict {
    const instance = this.instance;
    if (instance === undefined) return { kind: "none", reason: "no instance loaded" };
    return upgradeVerdict({
      instance,
      version: describeVersion({ recorded: instance.version, listing: this.deps.releases?.peek() }),
      ...(this.visibility === undefined ? {} : { visibility: this.visibility }),
    });
  }

  // -------------------------------------------------------------------------
  // messages
  // -------------------------------------------------------------------------

  private async onMessage(raw: unknown): Promise<void> {
    if (this.live.handleMessage(raw)) return;
    const msg = pageMessage(raw);
    if (msg === undefined) return;
    const rawValue = (msg as { value?: unknown }).value;
    const value = typeof rawValue === "string" ? rawValue : undefined;

    switch (msg.type) {
      case "input":
        this.onInput(String((msg as { field?: unknown }).field ?? ""), value ?? "");
        return;
      case "toggleDetails":
        this.detailsOpen = (msg as { open?: unknown }).open === true;
        return;
      case "toggleLogs":
        this.logsOpen = (msg as { open?: unknown }).open === true;
        return;
      case "toggleStepLog": {
        const file = stepLogFileOf(String((msg as { disclosure?: unknown }).disclosure ?? ""));
        if (file === "") return;
        if ((msg as { open?: unknown }).open === true) this.openLogs.add(file);
        else this.openLogs.delete(file);
        return;
      }
      case "openRun":
        if (value !== undefined && this.runs.some((run) => run.id === value)) {
          this.screen = this.slot().current?.recordId === value && this.slot().inFlight ? { kind: "run" } : { kind: "runDetail", runId: value };
          this.notice = undefined;
          await this.openRunDetail();
          this.render();
        }
        return;
      case "back":
        this.back();
        return;
      case "openOs": {
        const os = memqlOsAddress(this.instance?.domain);
        if (os !== undefined) void vscode.env.openExternal(vscode.Uri.parse(os.url));
        return;
      }
      case "openGuide": {
        const verdict = this.upgrade();
        if (verdict.kind === "refused" && value !== undefined && value === verdict.docHref) {
          void vscode.env.openExternal(vscode.Uri.parse(`${DOCS_BASE}${verdict.docHref}`));
        }
        return;
      }
      case "openCheckout":
        void vscode.commands.executeCommand("memql.deployments.openCheckout");
        return;
      case "cancel":
        this.watched?.cancel();
        return;
      case "retry":
        if (this.watched !== undefined && !this.watched.inFlight) {
          this.logsOpen = false;
          void this.watched.retry();
          this.render();
        }
        return;
      case "copyLog":
        await this.copyLog();
        return;
      case "openOutput":
        this.deps.showOutput?.();
        return;
      case "runRemedy": {
        const remedy = this.watched?.failure?.remedy ?? "";
        if (remedy !== "") openRemedyTerminal(remedy);
        return;
      }
      case "checkAgain":
        if (this.screen.kind === "rebuild") await this.openRebuild();
        else if (this.screen.kind === "pullRebuild") await this.openPullRebuild();
        return;
      case "beginChange":
        await this.beginChange(value);
        return;
      case "beginRebuild":
        this.beginRebuild();
        return;
      case "beginPullRebuild":
        this.beginPullRebuild();
        return;
      case "repair":
        // The fix a rebuild check offers when the folder is not a MemQL
        // checkout; everywhere else Repair is a bar act, narrowed there.
        if ((this.screen.kind === "rebuild" || this.screen.kind === "pullRebuild") && this.check?.notices.some((n) => n.fix === "repair")) {
          void vscode.commands.executeCommand("memql.clusters.repair");
          return;
        }
        await this.onBarAct(msg, value);
        return;
      case "signIn":
      case "connect":
        // CONNECTION ACTS, legal wherever the page offers them -- the bar, an
        // expired session's notice, a not-connected notice. They change nothing
        // about the cluster, so they are not narrowed against the bar.
        this.notice = undefined;
        if (this.instance !== undefined) await this.takeAct({ id: msg.type as "signIn" | "connect", label: "" });
        return;
      default:
        await this.onBarAct(msg, value);
    }
  }

  /** A field changed. Recorded; repainted only where the screen depends on it. */
  private onInput(fieldName: string, value: string): void {
    switch (fieldName) {
      case "version":
        this.choice = value;
        this.render();
        return;
      case "typed":
        this.typed = value.trim();
        this.typedError = this.typed === "" ? "" : (tagProblem(this.typed) ?? "");
        this.render();
        return;
      case "nodes":
        this.nodes = value.trim();
        return;
      case "merge":
        this.merge = value === "true";
        this.render();
        return;
    }
  }

  /** The act's bar, for whatever screen offered it. */
  private currentBar(): PageBar | undefined {
    const instance = this.instance;
    if (instance === undefined) return undefined;
    if (this.screen.kind === "runDetail") {
      const runId = this.screen.runId;
      const run = this.runs.find((r) => r.id === runId);
      return run === undefined ? undefined : this.detailBar(instance, run);
    }
    return this.overviewBar(instance);
  }

  private overviewBar(instance: Instance): PageBar {
    return instance.kind === "local"
      ? localOverviewBar({ instance, connection: this.connection, upgrade: this.upgrade() })
      : remoteOverviewBar({
          instance,
          connection: this.connection,
          upgrade: this.upgrade(),
          pipeline: this.pipeline,
          ...(this.visibility === undefined ? {} : { visibility: this.visibility }),
          runs: this.runs,
        });
  }

  private detailBar(instance: Instance, run: Run): PageBar {
    return runDetailBar({
      instance,
      run,
      connection: this.connection,
      ...(this.pipeline === undefined ? {} : { pipeline: this.pipeline }),
      ...(this.visibility === undefined ? {} : { visibility: this.visibility }),
      runInFlight: this.slot().inFlight,
    });
  }

  /**
   * An act from a bar. The webview is untrusted, so the act is narrowed
   * against the bar the page was DRAWN from -- id AND target: a rollback
   * aimed at a deployment other than the one on the button is dropped.
   */
  private async onBarAct(msg: PageToHost, value: string | undefined): Promise<void> {
    const bar = this.currentBar();
    if (bar === undefined) return;
    if (msg.type === "more") {
      const picked = await vscode.window.showQuickPick(
        bar.more.map((act) => ({ label: act.label, act })),
        { placeHolder: "More actions" },
      );
      if (picked !== undefined) await this.takeAct((picked as { act: PageAct }).act);
      return;
    }
    const act = barOffers(bar, msg.type, value);
    if (act !== undefined) await this.takeAct(act);
  }

  private async takeAct(act: PageAct): Promise<void> {
    const instance = this.instance;
    if (instance === undefined) return;
    this.notice = undefined;
    switch (act.id) {
      case "install":
        void vscode.commands.executeCommand("memql.deployments.createDeployment");
        return;
      case "repair":
        void vscode.commands.executeCommand("memql.clusters.repair");
        return;
      case "uninstall":
        void vscode.commands.executeCommand("memql.clusters.uninstall");
        return;
      case "adopt":
      case "reconnect":
        this.deps.openInstallFlow(act.id);
        return;
      case "signIn":
        // THE sign-in act, with the cluster named: no picker in front of it.
        void vscode.commands.executeCommand("memql.clusters.signIn", instance.name);
        return;
      case "connect":
        // Through the Clusters view's own select, handed the registered entry
        // by activation; without that seam, its picker.
        if (this.deps.connectTo !== undefined) await this.deps.connectTo(instance.name);
        else await vscode.commands.executeCommand("memql.clusters.select");
        this.reload();
        return;
      case "changeVersion":
        this.openChangeVersion(act.value);
        return;
      case "rebuildFromCheckout":
        await this.openRebuild();
        return;
      case "updateAndRebuild":
        await this.openPullRebuild();
        return;
      case "update":
        await this.runUpdate();
        return;
      case "showRun":
        this.screen = { kind: "run" };
        this.render();
        return;
      case "deploy":
      case "cutVersion":
      case "rollback":
      case "rolloutPromote":
      case "rolloutAbort":
        await this.runRemote(act);
        return;
      default:
        return;
    }
  }

  private back(): void {
    if (this.screen.kind === "run") {
      const run = this.watched;
      if (run !== undefined && run.inFlight) return;
      if (run !== undefined) this.slot().dismiss(run);
    }
    this.screen = { kind: "overview" };
    this.notice = undefined;
    this.reload();
    this.render();
  }

  // -------------------------------------------------------------------------
  // change version
  // -------------------------------------------------------------------------

  private openChangeVersion(preset?: string): void {
    this.screen = { kind: "chooseVersion" };
    this.choice = "";
    this.typed = "";
    this.typedError = "";
    this.render();
    void this.prepareChangeVersion(preset);
  }

  private async prepareChangeVersion(preset: string | undefined): Promise<void> {
    if (this.installGraph === undefined) {
      try {
        this.installGraph = await loadGraphFile(graphDocumentPath("install", this.deps.installRoot));
      } catch {
        // The forecast is a courtesy; the run loads the graph itself and fails
        // loudly if it cannot.
      }
    }
    const listing = await this.deps.releases?.get();
    if (this.disposed || this.screen.kind !== "chooseVersion") return;
    if (preset !== undefined && preset !== "") {
      if (listing?.tags.includes(preset) === true) this.choice = preset;
      else {
        this.choice = OTHER_VERSION;
        this.typed = preset;
      }
    }
    this.render();
  }

  /** The listing the version picker shows: undefined while loading. */
  private listing(): ReleaseListing | undefined {
    const releases = this.deps.releases;
    if (releases === undefined) return { tags: [], fetchedAt: 0 };
    return releases.peek();
  }

  /** The version the change would move to, when a valid one is chosen. */
  private changeTarget(): string {
    const tags = this.listing()?.tags ?? [];
    if (tags.length > 0 && this.choice !== OTHER_VERSION) return this.choice;
    return this.typed !== "" && tagProblem(this.typed) === undefined ? this.typed : "";
  }

  private async beginChange(value: string | undefined): Promise<void> {
    const target = this.changeTarget();
    if (this.screen.kind !== "chooseVersion" || target === "" || (value !== undefined && value !== target)) {
      if (this.typed !== "") {
        this.typedError = tagProblem(this.typed) ?? "";
        this.render();
      }
      return;
    }
    this.startRun({ kind: "changeVersion", from: this.instance?.version ?? "", to: target });
  }

  // -------------------------------------------------------------------------
  // the update (local run or remote cut-and-ship)
  // -------------------------------------------------------------------------

  /**
   * Update to the newest release: confirm once, typed, then move.
   *
   * ONE CONFIRMATION covers the remote path's two calls: prompting twice would
   * turn one decision into a sequence the operator can be halfway through.
   */
  private async runUpdate(): Promise<void> {
    const verdict = this.upgrade();
    if (verdict.kind !== "offer") return;
    const ok = await this.confirmTyped(verdict.title, verdict.confirmation, verdict.phrase);
    if (!ok) return;
    if (verdict.target.flow === "upgradeToTag") {
      this.startRun({ kind: "update", from: verdict.target.from, to: verdict.target.to });
      return;
    }
    const port = this.deps.deployPort?.();
    if (port === undefined) return this.notConnected();
    const cut = await runDeployAction(port, { id: "cutVersion", bump: "patch", version: verdict.target.to });
    if (this.disposed) return;
    if (cut.kind === "error") return this.reportOutcome(cut);
    // THE RECORD THE CUT JUST CREATED, BY ID -- never "whatever is newest
    // now", which is a race with another operator's cut.
    const record = cut.details.deploymentId ?? "";
    if (record === "") {
      this.deps.logLine?.(cut.line);
      this.outcome = { tone: "error", line: `${verdict.target.to} is ready but wasn't deployed.`, auditId: cut.auditEventId, signIn: false };
      this.reload();
      return;
    }
    const ship = await runDeployAction(port, { id: "deploy", deploymentId: record });
    if (this.disposed) return;
    this.deps.logLine?.(cut.line);
    this.reportOutcome(ship, ship.kind === "success" ? `Updating to ${verdict.target.to}.` : undefined, [cut.auditEventId]);
  }

  // -------------------------------------------------------------------------
  // remote deploy-control acts
  // -------------------------------------------------------------------------

  private async runRemote(act: PageAct): Promise<void> {
    const instance = this.instance;
    const port = this.deps.deployPort?.();
    if (instance === undefined) return;
    if (port === undefined) return this.notConnected();
    let request: DeployActionRequest;
    let success: string | undefined;
    switch (act.id) {
      case "deploy": {
        const version = this.runs.find((run) => run.id === act.value)?.toVersion ?? "";
        request = { id: "deploy", deploymentId: act.value ?? "" };
        success = version === "" ? undefined : `Deploying ${version}.`;
        break;
      }
      case "cutVersion":
        request = { id: "cutVersion", bump: "patch", version: "" };
        break;
      case "rollback": {
        const target = this.runs.find((run) => run.id === act.value);
        if (target === undefined) return;
        const to = target.toVersion ?? "";
        const phrase = to !== "" ? to : target.id;
        const now = (instance.versionLabel ?? "") === "" ? "" : ` (now ${instance.versionLabel})`;
        if (!(await this.confirmTyped(`Roll back ${instance.name}`, `Roll back ${instance.name} to ${phrase}${now}?`, phrase))) return;
        request = { id: "rollback", toDeploymentId: target.id };
        success = `Rolling back to ${phrase}.`;
        break;
      }
      case "rolloutPromote":
        request = { id: "rolloutAction", rollout: act.value ?? "", subAction: "promote" };
        break;
      case "rolloutAbort": {
        const rollout = act.value ?? "";
        // ABORT IS TYPED; PROMOTE IS NOT. Confirming both would train the
        // operator to type through the prompt (deploy/actions.ts).
        if (!(await this.confirmTyped(`Abort ${rollout}`, `Abort the ${rollout} rollout?`, rollout))) return;
        request = { id: "rolloutAction", rollout, subAction: "abort" };
        break;
      }
      default:
        return;
    }
    const outcome = await runDeployAction(port, request);
    if (this.disposed) return;
    this.reportOutcome(outcome, outcome.kind === "success" ? success : undefined);
  }

  /** An outcome on the page as one sentence; the log line and audit ids go to Output and Details. */
  private reportOutcome(outcome: DeployOutcome, success?: string, earlierAudits: readonly string[] = []): void {
    this.deps.logLine?.(outcome.line);
    const audits = [...earlierAudits, outcome.auditEventId].filter((id) => id !== "");
    this.outcome = {
      tone: outcome.kind === "success" ? "info" : "error",
      line: success ?? outcome.message,
      auditId: audits.join(", "),
      signIn: outcome.needsSignIn,
    };
    this.reload();
  }

  private notConnected(): void {
    this.notice = { tone: "error", line: "Not connected to this cluster.", acts: [{ act: "connect", label: "Connect" }] };
    this.render();
  }

  /**
   * A typed confirmation: the phrase is the TARGET (a version, a rollout), so
   * confirming means reading what is about to change. Cancel does nothing; a
   * mismatch does nothing and says so.
   */
  private async confirmTyped(title: string, question: string, phrase: string): Promise<boolean> {
    const ask = this.deps.confirm;
    if (ask === undefined || phrase === "") return false;
    const typed = await ask({ title, prompt: `${question} Type ${phrase} to confirm.`, phrase });
    if (typed === undefined) return false;
    if (!confirmationMatches(phrase, typed)) {
      this.notice = { tone: "error", line: "That didn't match, so nothing changed." };
      this.render();
      return false;
    }
    return true;
  }

  // -------------------------------------------------------------------------
  // rebuild and pull
  // -------------------------------------------------------------------------

  /**
   * The facts a build from the checkout depends on. PAINTS FIRST, THEN
   * GATHERS -- the probes spawn scripts -- and adopts the answer only if the
   * page is still on the screen that asked.
   */
  private async gather(withUpdate: boolean): Promise<RebuildPreflightInputs & { update?: UpdateState }> {
    const instance = this.instance!;
    const dir = instance.checkout ?? "";
    const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    const [dockerReachable, checkoutIsMemql, state, update] = await Promise.all([
      this.dockerReachable(),
      isMemqlCheckout(dir),
      readCheckoutState(dir),
      withUpdate ? readUpdateState(dir, { fallbackBranch: recordedStackBranch(receipt) }) : Promise.resolve(undefined),
    ]);
    return {
      dockerReachable,
      checkoutDir: dir,
      checkoutIsMemql,
      ...(state === undefined ? {} : { state }),
      ...(update === undefined ? {} : { update }),
      imageSource: instance.imageSource ?? "",
      releasedTag: recordedCheckout(receipt).tag,
      ...(instance.extensionCommit !== undefined ? { extensionCommit: instance.extensionCommit } : {}),
      ...(instance.extensionDirty === true ? { extensionDirty: true } : {}),
    };
  }

  private async openRebuild(): Promise<void> {
    if (this.instance === undefined || !offersLocal(this.instance, "rebuildFromCheckout")) return;
    this.screen = { kind: "rebuild" };
    this.check = undefined;
    this.render();
    const inputs = await this.gather(false);
    if (this.disposed || this.screen.kind !== "rebuild") return;
    this.check = rebuildCheck(inputs);
    this.render();
  }

  private async openPullRebuild(): Promise<void> {
    if (this.instance === undefined || !offersLocal(this.instance, "updateAndRebuild")) return;
    this.screen = { kind: "pullRebuild" };
    this.check = undefined;
    this.update = undefined;
    this.merge = false;
    this.render();
    const inputs = await this.gather(true);
    if (this.disposed || this.screen.kind !== "pullRebuild") return;
    this.update = inputs.update;
    this.check = updateCheck(inputs);
    this.render();
  }

  private beginRebuild(): void {
    const instance = this.instance;
    if (this.screen.kind !== "rebuild" || instance === undefined || this.check === undefined || this.check.blocked) return;
    this.startRun({ kind: "rebuild", checkout: instance.checkout ?? "", nodes: this.nodes });
  }

  private beginPullRebuild(): void {
    const instance = this.instance;
    if (this.screen.kind !== "pullRebuild" || instance === undefined || this.check === undefined || this.check.blocked) return;
    this.startRun({
      kind: "pullRebuild",
      checkout: instance.checkout ?? "",
      nodes: this.nodes,
      branch: this.update?.branch ?? "",
      strategy: this.merge ? "merge" : "fastForward",
    });
  }

  /**
   * Whether Docker answers, asked with the install graph's own read-only gate,
   * so the check and the run cannot disagree about the same machine.
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
      return false;
    }
  }

  // -------------------------------------------------------------------------
  // runs
  // -------------------------------------------------------------------------

  /** Start a run in the machine's slot -- or, when one is going, show that one. */
  private startRun(
    request:
      | { kind: "update" | "changeVersion"; from: string; to: string }
      | { kind: "rebuild"; checkout: string; nodes: string }
      | { kind: "pullRebuild"; checkout: string; nodes: string; branch: string; strategy: "merge" | "fastForward" },
  ): void {
    const instance = this.instance;
    if (instance === undefined) return;
    this.logsOpen = false;
    this.notice = undefined;
    this.slot().start(
      { ...request, instance: instance.name },
      {
        installRoot: this.deps.installRoot,
        receiptFile: this.deps.receiptFile,
        runsDir: this.deps.runsDir ?? defaultRunsDir(),
        ...(this.deps.runScript !== undefined ? { runScript: this.deps.runScript } : {}),
        ...(this.deps.graphs !== undefined ? { graphs: this.deps.graphs } : {}),
        ...(this.deps.now !== undefined ? { now: this.deps.now } : {}),
        ...(this.deps.logLine !== undefined ? { log: this.deps.logLine } : {}),
        onRecord: () => this.deps.refreshTree(),
        onSettled: (run) => {
          this.deps.onRunSettled?.(run, !this.disposed && this.visible());
          this.reload();
        },
      },
    );
    this.screen = { kind: "run" };
    this.render();
  }

  /** Follow a run: its changes repaint the page, its lines stream into the log. */
  private watch(run: LocalRun | undefined): void {
    if (run === this.watched) return;
    this.unwatch?.();
    this.unwatch = undefined;
    this.watched = run;
    if (run === undefined) return;
    this.live.log(run.log, { reset: true });
    this.unwatch = run.onEvent((event) => {
      if (this.disposed) return;
      if (event.type === "log") {
        this.live.log(event.lines, { reset: event.lines.length === 0 });
        return;
      }
      if (run.status === "failed") this.logsOpen = true;
      this.render();
    });
  }

  /** While a run screen is up and the run is going, keep its bar moving. */
  private syncTicker(showingRun: boolean): void {
    const going = showingRun && this.watched !== undefined && this.watched.inFlight;
    if (going && this.tick === undefined) {
      this.tick = setInterval(() => {
        if (this.watched !== undefined) this.live.progress(this.watched.progress());
      }, RUN_TICK_MS);
      (this.tick as { unref?: () => void }).unref?.();
    } else if (!going && this.tick !== undefined) {
      clearInterval(this.tick);
      this.tick = undefined;
    }
  }

  private async copyLog(): Promise<void> {
    const text = this.live
      .logLines()
      .map((line) => (line.label === undefined ? line.text : `${line.label}  ${line.text}`))
      .join("\n");
    try {
      await vscode.env.clipboard.writeText(text);
    } catch {
      // A host with no clipboard: the Output channel still has every line.
    }
  }

  /** Read what a run's detail shows beyond the record: step names and saved logs. */
  private async openRunDetail(): Promise<void> {
    if (this.stepLabels.size === 0) this.stepLabels = await readStepLabels(this.deps.installRoot);
    const screen = this.screen;
    if (screen.kind !== "runDetail") return;
    const run = this.runs.find((r) => r.id === screen.runId);
    if (run === undefined) return;
    const dir = this.deps.runsDir ?? defaultRunsDir();
    for (const item of run.items) {
      if (item.status !== "failed") continue;
      const file = parseItemDetail(item.detail).logFile;
      if (file === "" || this.logs.has(file) || file.includes("/") || file.includes("\\")) continue;
      try {
        this.logs.set(file, await fs.readFile(path.join(dir, file), "utf8"));
      } catch {
        this.logs.set(file, "");
      }
    }
    if (!this.disposed) this.render();
  }

  // -------------------------------------------------------------------------
  // rendering
  // -------------------------------------------------------------------------

  private render(): void {
    if (this.disposed) return;
    const { key, parts, run } = this.compose();
    this.watch(run);
    this.panel.title = this.instance?.presence === "absent" ? "Local cluster" : (this.instance?.name ?? "Cluster");
    this.live.render(key, parts);
    if (run !== undefined && key.startsWith("run:")) this.live.progress(run.progress());
    this.syncTicker(key.startsWith("run:"));
  }

  /** The screen to show now, as a LiveView key and its three regions. */
  private compose(): { key: string; parts: { head: string; body: string; actions: string }; run?: LocalRun } {
    const instance = this.instance;
    if (this.registryError !== "") {
      return {
        key: "unavailable",
        parts: unavailableScreen({
          title: "Cluster",
          line: "Can't read your cluster list.",
          next: this.registryError,
          bar: { state: "Unavailable", acts: [{ act: "back", label: "Try again", tone: "primary" }] },
        }),
      };
    }
    if (!this.loaded) return { key: "loading", parts: loadingScreen() };
    if (instance === undefined) {
      return {
        key: "unavailable",
        parts: unavailableScreen({
          title: this.instanceName === "" ? "Local cluster" : this.instanceName,
          line: "This cluster is no longer in your list.",
          bar: { state: "Not in your list", acts: [] },
        }),
      };
    }
    const nowMs = (this.deps.now ?? Date.now)();
    const home = this.deps.home ?? os.homedir();

    // A RUN GOING ON THIS MACHINE IS THE PAGE for the local cluster, unless the
    // operator deliberately opened another run from the history.
    const current = this.slot().current;
    const local = instance.kind === "local";
    if (local && current !== undefined && (this.screen.kind === "run" || (current.inFlight && this.screen.kind === "overview"))) {
      return {
        key: `run:${current.startedAt}`,
        run: current,
        parts: runScreen({
          words: current.words,
          status: current.status,
          progress: current.progress(nowMs),
          failure: current.failure,
          cancellable: current.cancellable,
          logsOpen: this.logsOpen || current.status === "failed",
        }),
      };
    }

    switch (this.screen.kind) {
      case "chooseVersion": {
        const listing = this.listing();
        const target = this.changeTarget();
        const plan: PlannedStepView[] =
          target === "" || this.installGraph === undefined
            ? []
            : upgradePlan({ graph: this.installGraph, from: instance.version ?? "", to: target });
        return {
          key: "chooseVersion",
          parts: chooseVersionScreen({
            instance,
            listing,
            choice: this.choice,
            typed: this.typed,
            typedError: this.typedError,
            target,
            plan,
            summary: plan.length === 0 ? "" : upgradeSummary(plan),
            sameVersion: isSameVersion(instance.version ?? "", target),
          }),
        };
      }
      case "rebuild":
        return { key: "rebuild", parts: rebuildScreen({ instance, check: this.check, nodes: this.nodes, home }) };
      case "pullRebuild":
        return {
          key: "pullRebuild",
          parts: pullRebuildScreen({
            instance,
            check: this.check,
            nodes: this.nodes,
            home,
            merge: this.merge,
            offerMerge: this.update !== undefined && (this.update.ahead === undefined || this.update.ahead > 0),
          }),
        };
      case "runDetail": {
        const runId = this.screen.runId;
        const run = this.runs.find((r) => r.id === runId);
        if (run === undefined) return { key: `runDetail:${runId}`, parts: missingRunScreen(instance) };
        return {
          key: `runDetail:${runId}`,
          parts: runDetailScreen({
            instance,
            run,
            bar: this.detailBar(instance, run),
            nowMs,
            labels: this.stepLabels,
            logs: this.logs,
            openLogs: this.openLogs,
            detailsOpen: this.detailsOpen,
          }),
        };
      }
      default: {
        const bar = this.overviewBar(instance);
        return {
          key: `overview:${instance.name}`,
          parts: local
            ? localOverviewScreen({
                instance,
                bar,
                runs: this.runs,
                nowMs,
                upgrade: this.upgrade(),
                releases: this.deps.releases?.peek(),
                ...(this.notice === undefined ? {} : { notice: this.notice }),
                detailsOpen: this.detailsOpen,
                home,
              })
            : remoteOverviewScreen({
                instance,
                bar,
                connection: this.connection,
                runs: this.runs,
                nowMs,
                pipeline: this.pipeline,
                upgrade: this.upgrade(),
                ...(this.outcome === undefined ? {} : { outcome: this.outcome }),
                ...(this.notice === undefined ? {} : { notice: this.notice }),
                detailsOpen: this.detailsOpen,
              }),
        };
      }
    }
  }
}

/**
 * A remedy command, typed into a terminal and NOT run: the operator reads it
 * and presses Enter, which is how a step that needs a password gets run at all.
 */
function openRemedyTerminal(command: string): void {
  const terminal = vscode.window.createTerminal({ name: "MemQL fix" });
  terminal.show();
  terminal.sendText(command, false);
}

/**
 * Every step id's short label, from the graph documents the runs came from.
 * A document that will not read costs its labels and nothing else: the steps
 * are then shown by id.
 */
async function readStepLabels(root: string): Promise<ReadonlyMap<string, string>> {
  const files = [
    graphDocumentPath("install", root),
    installGraphPath(root, true),
    graphDocumentPath("uninstall", root),
    rebuildGraphPath(root),
    updateRebuildGraphPath(root),
  ];
  const labels = new Map<string, string>();
  for (const file of files) {
    try {
      const graph = await loadGraphFile(file);
      for (const step of graph.steps) if (step.label !== "") labels.set(step.id, step.label);
    } catch {
      // See above.
    }
  }
  return labels;
}

/**
 * Whether a directory is a MemQL checkout, by the two files `k3d.dev` itself
 * gates on (memql#4246) -- the same two, so a check here never passes a folder
 * the run would then refuse.
 */
async function isMemqlCheckout(dir: string): Promise<boolean> {
  if (dir === "") return false;
  const required = [path.join(dir, "Dockerfile"), path.join(dir, "deploy", "k8s", "overlays", "local", "kustomization.yaml")];
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
