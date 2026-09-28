// The Add a cluster panel: install, repair, connect and uninstall, on one page.
//
// WHAT IT IS FOR. The Clusters view's "+", "Create Deployment", "Repair Local
// Cluster" and "Uninstall Local Cluster" all open this one panel, because all
// four are decisions about what is on THIS computer, and the answer to "what
// can I do here" depends on what detection finds. One panel, one run at a
// time: two runs against one k3d cluster is not a state anything downstream
// is prepared for.
//
// HOW IT DRAWS. The page is the kit (src/webview/ui/kit.ts), through LiveView:
// a document is assigned once per screen, and everything that changes while a
// screen is showing -- region patches, the progress bar, log lines -- arrives
// as a message the page applies in place. Scroll, focus, a half-typed field and
// an open log survive. The screens themselves are pure functions of a view
// model in addClusterScreens.ts; this file builds the view models from the
// state machines (state/addCluster.ts, state/uninstallRun.ts) and turns page
// messages into transitions.
//
// WHAT DECIDES WHAT. What the landing offers is `landingView` (state/
// addCluster.ts) over what detection found; a command that names an act
// (Repair, Uninstall) is checked against the same function, so a menu cannot
// open a form for a cluster that is not there. Detection finishes before any
// choice is drawn -- the landing is a skeleton until then, never a guess --
// and runs again every time the page comes back to the landing, so a verdict
// is never older than the last thing that changed it.
//
// THE CHANNEL FROM THE PAGE IS UNTRUSTED. Every message is matched against a
// name written out in this file, every field against a list, every step id
// against the run's own records; a value is never indexed into a table or
// cast into a command. The one irreversible act -- an uninstall -- is reached
// only from the preview it was consented to.
//
// Refs: #3475 #3472 #3470 #3471 #3469 #3463

import { randomBytes } from "node:crypto";
import * as os from "node:os";
import * as vscode from "vscode";

import { addCluster, readClustersFileSafe, upsertCluster } from "../clusters/file.js";
import { composeConsoleUrl } from "../clusters/consoleUrl.js";
import type { ClusterConfig } from "../clusters/model.js";
import {
  LOCAL_CLUSTER_NAME,
  type AddClusterAction,
  type ClusterPresence,
  type PresenceVerdict,
} from "../clusters/presence.js";
import { completeLocalUninstall } from "../clusters/registry.js";
import { planLocalReconnect } from "../clusters/reconnect.js";
import { discoverIssuer } from "../connection/discovery.js";
import { composeEndpointFromDomain } from "../connection/endpoint.js";
import { errorText } from "../auth/errors.js";
import { ClaimError, claimUrlFrom, openClaimLink } from "../install/claim.js";
import { ownerAccountExistsFrom } from "../install/enrolment.js";
import type { ExecEvent, ExecutionReport } from "../install/executor.js";
import { graphDocumentPath, installGraphPath, loadGraphFile, type Graph, type GraphKind } from "../install/graph.js";
import { completeInstallHandoff } from "../install/handoff.js";
import { platformRefuseEvents, refuseUnsupportedPlatform } from "../install/platform.js";
import {
  checkoutPinFor,
  deleteReceipt,
  readReceipt,
  recordedCheckout,
  recordedDomain,
  recordedImageSource,
  recordedOwner,
  recordedStackTag,
  type Receipt,
} from "../install/receipt.js";
import { recoveryKeyStateFrom, revealedRecoveryKeyFrom } from "../install/recoveryKey.js";
import { removalRows, sharedToolRows } from "../install/removalPreview.js";
import type { RunScript } from "../install/runner.js";
import { redactForDisplay } from "../install/secrets.js";
import {
  installSessionOptions,
  previewUninstall,
  runInstall,
  runUninstall,
  type SessionHooks,
  type SessionOptions,
  type UninstallPreview,
} from "../install/session.js";
import { DEFAULT_STACK_REPO, isMainBranchChoice, localInstallRemoteProblem } from "../install/stackPin.js";
import { elevationEnv, startSudoAgent, sudoAccepts, sudoRunsWithoutAsking, type SudoAgent } from "../install/sudoAgent.js";
import { listReleaseTags } from "../install/tags.js";
import {
  AddClusterState,
  landingOffers,
  landingView,
  type ConnectField,
  type ConnectProbeTargets,
  type ConnectProbeVerdict,
  type LandingAct,
  type LandingFacts,
  type LandingView,
  type StepProgress,
  derivationLine,
} from "../state/addCluster.js";
import { recordDiagnostic, type DiagnosticSink } from "../state/diagnostics.js";
import { LOCAL_INSTANCE_NAME } from "../state/deployments.js";
import {
  DEFAULT_STEP_TIMEOUT_SECONDS,
  failureGuidance,
  isUnsupportedPlatformRefuse,
} from "../state/installProgress.js";
import { preflightChecks, type PreflightCheck } from "../state/preflight.js";
import { defaultRunsDir } from "../state/runLog.js";
import { failedLabel, historicalWeights } from "../state/runProgress.js";
// EVERY LOCAL RUN WRITES A RECORD (memql#3739). The Deployments tree reads the
// run log as one history, so the wizard's install, repair and uninstall have to
// appear in it beside the deployments the instance page drives.
import { RunRecorder } from "../state/runRecorder.js";
import { RunLogLines } from "../state/stepRecords.js";
import { UninstallRunState } from "../state/uninstallRun.js";
import {
  ADD_CLUSTER_STYLES,
  CONNECT_FIELD_IDS,
  INPUT_FIELDS,
  TAB_TITLES,
  addedScreen,
  collectScreen,
  connectScreen,
  doneScreen,
  landingScreen,
  phraseMatches,
  doneProgressUpdate,
  runProgressUpdate,
  runScreen,
  uninstalledProgressUpdate,
  uninstallPreviewScreen,
  uninstalledScreen,
  type AddedInput,
  type DoneInput,
  type FailureView,
  type PanelFlow,
  type RunInput,
  type RunPhase,
  type UninstalledInput,
} from "./addClusterScreens.js";
import { currentBodyThemeAttr, onAppearanceChange } from "./theme.js";
import { pageDocument } from "./ui/document.js";
import { LiveView, type LogLine, type RegionParts } from "./ui/liveView.js";
import { pageMessage } from "./ui/protocol.js";

/**
 * The phrase a person TYPES, and the value the script receives.
 *
 * TWO SPELLINGS, MAPPED IN ONE PLACE (memql#5118, D9). A person types a
 * sentence; `remove-artifact.sh` takes a flag value, and a flag value with
 * spaces in it is a quoting problem on every shell the installer runs on. The
 * typed form lives with the screen (addClusterScreens.ts); this is the wire
 * form, and the script's own test pins both halves apart.
 */
const DELETE_DATA_CONFIRM_VALUE = "delete-memql-data";

/** The landing's acts the page may post. A real guard, not a cast. */
const LANDING_ACTS: readonly LandingAct[] = [
  "install",
  "connect",
  "signIn",
  "openOs",
  "repair",
  "uninstall",
  "reconnect",
  "adopt",
];

/**
 * The connect form's fields, by the names the PAGE uses for them.
 *
 * Its own names, so the connect form's `domain` and the install form's
 * `domain` cannot be mistaken for one another on the wire.
 */
const CONNECT_FIELDS_BY_ID: ReadonlyMap<string, ConnectField> = new Map(
  (Object.entries(CONNECT_FIELD_IDS) as [ConnectField, string][]).map(([field, id]) => [id, field]),
);

/**
 * How long any ONE step may take before it is killed, as the default a step's
 * own `timeoutSeconds` in the graph replaces (memql#3474, memql#4076).
 *
 * `runner.ts` reads absent as NO timeout, so omitting this removes the ceiling
 * rather than choosing one. Ten minutes stays tight so a hang in an ordinary
 * step surfaces in minutes; the cluster step and the image build price their
 * own slowness in the graph.
 */
const STEP_TIMEOUT_MS = DEFAULT_STEP_TIMEOUT_SECONDS * 1000;

/** How long the landing's platform check may take before it is read as "no refusal". */
const PLATFORM_CHECK_TIMEOUT_MS = 30_000;

/** How often the bar is recomputed while a run is in flight (the clock ticks on the page). */
const PROGRESS_TICK_MS = 1_000;

/** How long log lines are gathered before they cross to the page in one message. */
const LOG_FLUSH_MS = 120;

/**
 * The real reachability probe for the connect form: does this cluster answer,
 * and does its identity service publish a keyset (memql#4432).
 *
 * A 200 with a JSON body carrying `keys` means the identity service is up, its
 * TLS chain verified and the front door routed it. It does NOT prove the
 * person can sign in, and the form does not claim it does. TLS is verified,
 * not bypassed; the localhost family never reaches here (the form refuses it).
 * The timeout is the point: an unroutable host hangs rather than refusing.
 */
const CONNECT_PROBE_TIMEOUT_MS = 10_000;

export async function probeClusterOverHttps(targets: ConnectProbeTargets): Promise<ConnectProbeVerdict> {
  if (targets.jwksUrl === "") {
    return { ok: false, reason: "no sign-in address could be worked out from that domain" };
  }
  const timer = new AbortController();
  const bell = setTimeout(() => timer.abort(), CONNECT_PROBE_TIMEOUT_MS);
  try {
    const res = await fetch(targets.jwksUrl, { signal: timer.signal, redirect: "follow" });
    if (!res.ok) {
      return { ok: false, reason: `its sign-in service answered HTTP ${String(res.status)}` };
    }
    const body = (await res.json()) as { keys?: unknown };
    if (!Array.isArray(body.keys)) {
      return { ok: false, reason: "something other than MemQL answered at that address" };
    }
    // THE CLUSTER'S OWN ISSUER (memql#4624), recorded on the entry so an
    // identity service at a non-conventional host works. A failure here is not
    // a failed probe: an older cluster publishes no document and must still be
    // registrable.
    const issuerBase = targets.jwksUrl.replace(/\/\.well-known\/jwks\.json$/, "");
    const discovered = await discoverIssuer(issuerBase, (url, init) =>
      fetch(url, { method: init.method, headers: init.headers, redirect: "follow" }),
    ).catch(() => ({ ok: false }) as const);
    if (discovered.ok) return { ok: true, issuer: discovered.issuer };
    return { ok: true };
  } catch (err) {
    if (err instanceof Error && err.name === "AbortError") {
      return { ok: false, reason: `no answer within ${String(CONNECT_PROBE_TIMEOUT_MS / 1000)} seconds` };
    }
    // The shared renderer (memql#4619): a wrong hostname, a firewall and an
    // untrusted certificate all arrive as "fetch failed" otherwise.
    return { ok: false, reason: errorText(err) };
  } finally {
    clearTimeout(bell);
  }
}

/**
 * What a modal has to say before something irreversible happens: the bold
 * headline, the paragraph under it, and the label on the button that goes
 * through with it.
 */
export interface DestructiveConfirmation {
  message: string;
  detail: string;
  proceed: string;
}

export interface AddClusterDeps {
  /** ~/.memql/clusters.yaml, resolved once at activation. */
  clustersPath: string;
  /**
   * The MemQL Install output channel (memql#4194). Every line a run writes is
   * recorded here, redacted, as the run emits it -- the page's log is the same
   * stream, and this is where it outlives the page.
   */
  diagnostics: DiagnosticSink;
  /** Brings the MemQL Install output forward ("Open in Output"). */
  showDiagnostics?: () => void;
  /** Repaints the Clusters view once an entry lands. */
  refreshTree: () => void;
  /** Injected by tests; the real https probe when absent (memql#4432). */
  probeCluster?: (targets: ConnectProbeTargets) => Promise<ConnectProbeVerdict>;
  /** Where the graph documents and capability scripts are (memql#3487). */
  installRoot: string;
  /** ~/.memql/install-receipt.json, resolved once at activation: one answer for every reader. */
  receiptFile: string;
  /** Injected by tests; the real spawn-based runner when absent (memql#3514). */
  runScript?: RunScript;
  /** Injected by tests; `sudoRunsWithoutAsking` when absent -- a safety rail as much as a seam (memql#3586). */
  sudoIsFree?: () => Promise<boolean>;
  /** Injected by tests; `sudoAccepts` when absent. */
  sudoAccepts?: (askpassPath: string) => Promise<boolean>;
  /** Drops a cluster's registry entry, stored credential and live connection, as "Remove from list" does. */
  removeRegistryEntry: (name: string) => Promise<unknown>;
  /** Where the run log lives; ~/.memql/runs when absent. */
  runsDir?: string;
  /** A modal confirmation; `window.showWarningMessage` when absent (memql#4615). */
  confirmDestructive?: (prompt: DestructiveConfirmation) => Promise<boolean>;
  /**
   * The k3d clusters on this computer, by name; none when absent.
   *
   * WHAT MAKES A CLUSTER WITH NO INSTALL RECORD UNINSTALLABLE. A cluster built
   * with `make up`, or adopted into the list, has no receipt; presence reads
   * it as installed from the list entry alone and never asks k3d. Before the
   * uninstall preview refuses for want of a record, this asks whether the
   * cluster is really there -- and when it is, the preview plans exactly that
   * one removal, kept unless its data is deliberately deleted.
   */
  listLocalClusters?: () => Promise<string[]>;
  /** Whether the editor holds a live session with the named cluster; never, when absent. */
  isSignedIn?: (clusterName: string) => boolean;
}

/** What detection found, held for the landing and for checking a command's act. */
interface Detected {
  facts: LandingFacts;
  clusterName?: string;
  /** Detection itself failed; the landing says so and offers only what is safe. */
  failed?: boolean;
}

export class AddClusterPanel {
  private static open_: AddClusterPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly live: LiveView;
  private readonly state = new AddClusterState();
  private readonly uninstall = new UninstallRunState();
  private readonly disposables: vscode.Disposable[] = [];
  private disposed = false;
  private flow: PanelFlow = "add";

  // ---- detection ----
  /** Undefined while detection runs: the landing draws a skeleton, never a guess. */
  private detected: Detected | undefined;
  private detecting: Promise<void> | undefined;
  /** Messages that arrived while detection was running, replayed once it settles. */
  private queued: unknown[] = [];
  /** An act a command named, applied once detection says whether it applies. */
  private requested: AddClusterAction | undefined;
  /** detect.sh's platform answer, asked once: a computer does not change platform under a panel. */
  private platform: "supported" | "unsupported" | undefined;

  // ---- the install / repair form ----
  private versionChoices: readonly string[] = [];
  private checks: PreflightCheck[] | undefined;
  private moreOpen = false;
  private passwordProblem: string | undefined;

  // ---- the connect form ----
  private connectMoreOpen = false;
  private saving = false;
  private added: (AddedInput & { cluster: ClusterConfig }) | undefined;

  // ---- a run (install, repair or uninstall: one at a time) ----
  /** The cancel handle; set exactly while the executor is running. */
  private runAbort: AbortController | undefined;
  /** From Install or Retry until the run settles, including the password prompt before it. */
  private runInFlight = false;
  /** Why the run could not be attempted at all. Not a step failure. */
  private runError = "";
  private runStartedAt: number | undefined;
  private runEndedAt: number | undefined;
  private readonly runLog = new RunLogLines();
  private pendingLines: LogLine[] = [];
  private flushTimer: NodeJS.Timeout | undefined;
  private ticker: NodeJS.Timeout | undefined;
  /** Answers sudo for every step of the run in flight. See sudoAgent.ts. */
  private sudoAgent: SudoAgent | undefined;

  // ---- done ----
  private doneKind: "installed" | "repaired" | "added" = "installed";
  private handoffDomain = "";
  /** A hand-off is writing the list entry and selecting it; a second one would race it. */
  private handingOff = false;

  // ---- uninstall ----
  private uninstallPreview: UninstallPreview | undefined;
  /** Nothing to uninstall was found; `removeFromList` when a list entry still points at it. */
  private uninstallNothing: { removeFromList: boolean } | undefined;
  private uninstallLoading = false;
  /** Shared removals switched ON. Empty is the default and the safe answer (memql#3566). */
  private readonly removeShared = new Set<string>();
  /** The delete-data consent: the switch, and the typed phrase (memql#5118, D9). */
  private deleteData = false;
  private deleteDataPhrase = "";
  private uninstallAbort: AbortController | undefined;
  private uninstalling = false;
  /** The local cluster's list entry, read BEFORE the removal so cleanup targets what was consented to. */
  private localClusterName: string | undefined;
  /** The preview was planned for a cluster with no install record. */
  private uninstallUnreceipted = false;

  /**
   * Opens the page, or reveals the one already open.
   *
   * `initialAction` opens the page ON a branch -- Repair, Uninstall, Install --
   * for the affordances that name it themselves; it is applied once detection
   * says the branch applies to this computer, and the landing is shown when it
   * does not.
   */
  static show(
    context: vscode.ExtensionContext,
    presence: ClusterPresence,
    deps: AddClusterDeps,
    initialAction?: AddClusterAction,
  ): AddClusterPanel {
    const existing = AddClusterPanel.open_;
    if (existing !== undefined && !existing.disposed) {
      existing.panel.reveal(vscode.ViewColumn.Beside);
      existing.openOn(initialAction);
      return existing;
    }
    const panel = new AddClusterPanel(context, presence, deps);
    AddClusterPanel.open_ = panel;
    panel.openOn(initialAction);
    return panel;
  }

  private constructor(
    context: vscode.ExtensionContext,
    private readonly presence: ClusterPresence,
    private readonly deps: AddClusterDeps,
  ) {
    this.panel = vscode.window.createWebviewPanel(
      "memqlAddCluster",
      TAB_TITLES.add,
      // Beside, not Active: the person asked for this from a view in the side
      // bar and is very likely reading something else.
      vscode.ViewColumn.Beside,
      { enableScripts: true },
    );
    const webview = this.panel.webview;
    this.live = new LiveView(
      { setHtml: (html) => (webview.html = html), postMessage: (msg) => webview.postMessage(msg) },
      (parts, screen) =>
        pageDocument({
          nonce: nonceValue(),
          title: this.panel.title,
          themeAttr: currentBodyThemeAttr(),
          screen,
          styles: ADD_CLUSTER_STYLES,
          ...escapeActFor(screen),
          ...parts,
        }),
    );

    this.disposables.push(
      // A theme or appearance change restyles every rule, so it is a new document.
      ...onAppearanceChange(() => {
        this.live.invalidate();
        this.render();
      }),
      webview.onDidReceiveMessage((raw: unknown) => this.receive(raw)),
    );
    this.panel.onDidDispose(() => this.dispose(), null, this.disposables);
    context.subscriptions.push(new vscode.Disposable(() => this.dispose()));

    void this.redetect();
  }

  /**
   * Puts the page on a named branch, as if the person had chosen it on the
   * landing.
   *
   * A PAGE MID-RUN IS REVEALED, NEVER RE-ROUTED: switching the screen out from
   * under a run would leave no view of work still happening.
   */
  private openOn(action: AddClusterAction | undefined): void {
    if (this.runBusy()) return;
    if (action === undefined) {
      // A reveal onto the landing looks again: something may have changed
      // since the page last looked (memql#5118 audit: stale verdict).
      if (this.state.screen === "landing") void this.redetect();
      return;
    }
    // THE ONE-TIME RECOVERY KEY IS NOT NAVIGATED AWAY FROM by a menu: the
    // page is revealed, and leaving stays the person's own act.
    if (this.state.recoveryKeyWouldBeLost) return;
    this.requested = action;
    this.setFlow(flowOf(action));
    // Judged against a FRESH look: the page may have been open for an hour.
    if (this.state.screen !== "landing") this.state.back();
    void this.redetect();
  }

  /** Whether an install, repair or uninstall is in flight (the password prompt included). */
  private runBusy(): boolean {
    return this.runInFlight || this.uninstalling;
  }

  private setFlow(flow: PanelFlow): void {
    this.flow = flow;
    const title = TAB_TITLES[flow];
    if (this.panel.title !== title) this.panel.title = title;
  }

  // ---------------------------------------------------------------------------
  // detection
  // ---------------------------------------------------------------------------

  /**
   * Looks at this computer again, drawing the landing's skeleton meanwhile.
   *
   * THE MEMO IS DROPPED FIRST. Presence caches its answer for thirty seconds,
   * which is right for a menu and wrong for a page someone has just come back
   * to after an install, a `make up`, or a removal.
   */
  private redetect(): Promise<void> {
    if (this.detecting !== undefined) return this.detecting;
    this.detected = undefined;
    this.presence.invalidate();
    this.render();
    const run = this.detect()
      .catch(() => {
        // Detection answers rather than rejects; this is the belt to that.
        this.detected = {
          facts: { verdict: "absent", registered: false, hasReceipt: false, platform: "unknown", signedIn: false },
          failed: true,
        };
      })
      .finally(() => {
        this.detecting = undefined;
        if (this.disposed) return;
        this.applyRequested();
        this.render();
        this.drainQueue();
      });
    this.detecting = run;
    return run;
  }

  private async detect(): Promise<void> {
    const result = await this.presence.get();
    const verdict: PresenceVerdict = result.verdict;
    // The platform is asked only where Install would be offered: every other
    // verdict means something is already here.
    if (verdict === "absent" && this.platform === undefined) {
      const refused = await refuseUnsupportedPlatform(
        { ...this.platformSession(), timeoutMs: PLATFORM_CHECK_TIMEOUT_MS },
        this.hooks({}),
      ).catch(() => undefined);
      this.platform = refused === undefined ? "supported" : "unsupported";
      if (refused !== undefined) {
        this.deps.diagnostics.appendLine(
          `this computer is not supported for a local cluster: ${refused.outcomes[0]?.reason ?? ""}`,
        );
      }
    }
    if (this.disposed) return;
    const clusterName = result.clusterName;
    this.detected = {
      facts: {
        verdict,
        registered: clusterName !== undefined,
        hasReceipt: result.evidence.receipt,
        platform: verdict === "absent" ? (this.platform ?? "unknown") : "unknown",
        signedIn: clusterName !== undefined && (this.deps.isSignedIn?.(clusterName) ?? false),
      },
      ...(clusterName === undefined ? {} : { clusterName }),
    };
  }

  /** Applies the act a command named, now that detection says whether it is here to take. */
  private applyRequested(): void {
    const action = this.requested;
    this.requested = undefined;
    if (action === undefined || this.detected === undefined || this.runBusy()) return;
    const act: LandingAct = action === "installGuided" ? "install" : action;
    if (!LANDING_ACTS.includes(act) || !this.offers(act)) {
      // Not something this computer has: the landing, which says what it does.
      this.state.back();
      this.setFlow("add");
      return;
    }
    this.choose(act);
  }

  /** Whether the landing, as detection found this computer, offers `act`. */
  private offers(act: LandingAct): boolean {
    const detected = this.detected;
    if (detected === undefined) return false;
    if (detected.failed === true) return act === "connect";
    return landingOffers(detected.facts, act);
  }

  private drainQueue(): void {
    const waiting = this.queued.splice(0);
    for (const msg of waiting) this.onMessage(msg);
  }

  private currentLanding(): LandingView | undefined {
    const detected = this.detected;
    if (detected === undefined || this.detecting !== undefined) return undefined;
    if (detected.failed === true) {
      return {
        state: "Couldn't check this computer",
        tone: "warn",
        choices: [{ act: "connect", label: "Connect to a cluster", note: "Add a cluster that runs somewhere else." }],
      };
    }
    return landingView(detected.facts);
  }

  // ---------------------------------------------------------------------------
  // messages
  // ---------------------------------------------------------------------------

  private receive(raw: unknown): void {
    if (this.live.handleMessage(raw)) return;
    // WHILE DETECTION RUNS, messages wait for it. The landing draws nothing
    // to click then, but a command's act and a test's scripted clicks can
    // arrive before the verdict does, and a choice judged against no verdict
    // is the guess this page no longer makes.
    if (this.detecting !== undefined) {
      this.queued.push(raw);
      return;
    }
    this.onMessage(raw);
  }

  private onMessage(raw: unknown): void {
    const parsed = pageMessage(raw);
    if (parsed === undefined || this.disposed) return;
    const msg = parsed as { type: string } & Record<string, unknown>;
    const { type } = msg;
    const value = typeof msg.value === "string" ? msg.value : undefined;

    switch (type) {
      case "choose": {
        const act = LANDING_ACTS.find((known) => known === value);
        // EVERY CHOICE IS JUDGED AGAINST THE VERDICT, not merely recognised.
        // Reconnect and adopt WRITE a registry entry with no form in front of
        // them, and the page channel is untrusted.
        if (act === undefined || !this.offers(act)) return;
        this.choose(act);
        return;
      }
      case "input":
        this.onInput(msg);
        return;
      case "toggleMore": {
        const open = msg.open === true;
        if (this.state.screen === "connect") this.connectMoreOpen = open;
        else this.moreOpen = open;
        // The summary beside the toggle is for a closed disclosure only.
        this.render();
        return;
      }
      case "toggleLogs":
        if (this.state.screen === "uninstallPreview") this.uninstall.setLogsOpen(msg.open === true);
        else this.state.setLogsOpen(msg.open === true);
        return;
      case "back":
        void this.leaveScreen();
        return;
      case "begin":
        void this.begin();
        return;
      case "cancel":
        this.cancelRun();
        return;
      case "retry":
        this.retry();
        return;
      case "resume":
        this.resume();
        return;
      case "leave":
        if (this.state.screen === "uninstallPreview") this.uninstallBack();
        else void this.leaveScreen();
        return;
      case "remedy":
        if (value !== undefined) this.openRemedyTerminal(value);
        return;
      case "copyLog":
        void this.copyLog();
        return;
      case "openOutput":
        this.deps.showDiagnostics?.();
        return;
      case "connect":
        void this.saveConnect();
        return;
      case "connectCancel":
        this.state.discardConnect();
        this.connectMoreOpen = false;
        this.toLanding();
        return;
      case "connectEscape":
        // ESCAPE LEAVES ONLY A FORM WITH NOTHING IN IT. It used to discard
        // every field, so a stray key in a half-filled form cost the draft.
        if (this.state.screen === "connect" && this.added === undefined && this.state.connectIsPristine) {
          this.state.discardConnect();
          this.toLanding();
        }
        return;
      case "signIn":
        void this.signIn();
        return;
      case "openOs":
        void this.openOs();
        return;
      case "enrolPasskey":
        void this.enrolPasskey();
        return;
      case "claimCluster":
        void this.claimCluster();
        return;
      case "copyRecoveryKey":
        void this.copyRecoveryKey();
        return;
      case "revealRecoveryKey":
        this.state.revealRecoveryKey();
        this.render();
        return;
      case "retryHandoff":
        if (this.state.handoff !== undefined && !this.state.handoff.ok && this.handoffDomain !== "") {
          void this.handOffAfterInstall(this.handoffDomain);
        }
        return;
      case "shared":
        this.onSharedSwitch(msg);
        return;
      case "deleteData":
        this.onDeleteDataSwitch(msg.checked === true);
        return;
      case "uninstallStart":
        void this.startUninstall();
        return;
      case "uninstallBack":
        this.uninstallBack();
        return;
      case "removeFromList":
        void this.removeFromList();
        return;
      default:
        return;
    }
  }

  /** A keystroke or a choice in a field: recorded, then the regions that changed are patched. */
  private onInput(msg: Record<string, unknown>): void {
    const field = typeof msg.field === "string" ? msg.field : "";
    const text = typeof msg.value === "string" ? msg.value : "";
    const install = INPUT_FIELDS.find((known) => known === field);
    if (install !== undefined) {
      this.state.setInput(install, text);
      // A new version names a different graph document (the from-source lane
      // has one more step), so the checks are for a different run.
      if (install === "version" && this.state.screen === "collect") void this.computeChecks("install");
      this.render();
      return;
    }
    const connect = CONNECT_FIELDS_BY_ID.get(field);
    if (connect !== undefined) {
      this.state.setConnectInput(connect, text);
      this.render();
      return;
    }
    if (field === "deletePhrase" && this.deleteData) {
      this.deleteDataPhrase = text;
      this.render();
    }
  }

  /**
   * Moves to what a landing choice opens.
   *
   * One route for a click and a command alike, so a branch opened from a
   * menu is the same state as one chosen here.
   */
  private choose(act: LandingAct): void {
    switch (act) {
      case "install":
      case "repair": {
        this.setFlow(act);
        this.state.chooseAction(act);
        this.passwordProblem = undefined;
        this.moreOpen = false;
        if (act === "install") void this.loadVersionChoices();
        // A repair opens with the recorded answers already in the boxes.
        if (act === "repair") void this.prefillFromReceipt();
        void this.computeChecks(act);
        this.render();
        return;
      }
      case "connect":
        this.setFlow("add");
        this.added = undefined;
        this.state.chooseAction("connect");
        void this.loadRegistry();
        this.render();
        return;
      case "uninstall":
        this.setFlow("uninstall");
        this.state.chooseAction("uninstall");
        void this.loadUninstallPreview();
        return;
      case "reconnect":
      case "adopt":
        if (this.handingOff) return;
        this.setFlow("add");
        void this.reconnectLocal();
        return;
      case "signIn":
        void this.signInToLocal();
        return;
      case "openOs":
        void this.openOsForLocal();
        return;
    }
  }

  /** Back to the landing, looking at the computer again. */
  private toLanding(): void {
    this.setFlow("add");
    void this.redetect();
  }

  // ---------------------------------------------------------------------------
  // install and repair
  // ---------------------------------------------------------------------------

  /**
   * Validates, then starts.
   *
   * A REMOTE WINDOW CANNOT INSTALL A LOCAL CLUSTER (memql#4623): Install is
   * absent from the form there, and this is the wall behind it.
   */
  private async begin(): Promise<void> {
    if (this.state.screen !== "collect" || this.runBusy()) return;
    if (localInstallRemoteProblem(vscode.env.remoteName) !== undefined) {
      this.render();
      return;
    }
    if (this.checks !== undefined && this.checks.some((check) => check.tone === "error")) return;
    this.passwordProblem = undefined;
    if (this.state.beginRun()) {
      this.startFreshRun();
      void this.startRun();
    }
    this.render();
  }

  /** A new run's log, clock and bar. A Retry is a new run too: the last attempt's output is not this one's. */
  private startFreshRun(): void {
    this.runLog.reset();
    this.pendingLines = [];
    this.live.log([], { reset: true });
    this.runStartedAt = Date.now();
    this.runEndedAt = undefined;
    this.runError = "";
    // The bar starts from this run, not from where the last one ended: a
    // reloaded page is sent the latest progress, and it must be this one's.
    this.pushProgress();
  }

  /** The hooks every session call gets: the caller's, plus the injected runner. */
  private hooks(own: SessionHooks): SessionHooks {
    return { ...own, ...(this.deps.runScript ? { run: this.deps.runScript } : {}) };
  }

  /**
   * Drives `session.ts` and folds every event into the state machine.
   *
   * ONE RUN AT A TIME, guarded by `runInFlight`: Retry and Resume reach here
   * too, and a second graph against one k3d cluster is not a state anything
   * downstream is prepared for. Repair is the same call: every step verifies
   * first and skips when satisfied.
   */
  private async startRun(): Promise<void> {
    if (this.runInFlight) return;
    const action = this.state.action;
    if (action !== "install" && action !== "installGuided" && action !== "repair") return;
    this.runInFlight = true;
    this.startTicker();
    try {
      await this.runOnce(action === "repair" ? "repair" : "install");
    } finally {
      this.runInFlight = false;
      this.stopTicker();
      this.runEndedAt = Date.now();
      this.flushLog();
      if (!this.disposed) {
        this.render();
        this.pushProgress();
      }
    }
  }

  private async runOnce(action: "install" | "repair"): Promise<void> {
    const inputs = this.state.inputs;
    // Read ONCE, up here: the recorded pin is needed below, and a second read
    // could see a different file (memql#3605).
    const priorReceipt = await readReceipt(this.deps.receiptFile);

    // Repair refuses here, before the password, on an unsupported platform
    // (memql#4294); an install was refused at the landing.
    if (action === "repair") {
      const refused = await refuseUnsupportedPlatform(this.platformSession(), this.hooks({}));
      if (refused !== undefined) {
        for (const event of platformRefuseEvents(refused)) this.foldInstallEvent(event);
        return;
      }
    }

    // ONE PASSWORD FOR THE WHOLE RUN (memql#3568), and DISMISSING IT CANCELS
    // (memql#5118 audit: a dismissed prompt used to start the run anyway, and
    // an uninstall then deleted the cluster before its privileged steps
    // failed). Back to the form, nothing run.
    const password = await this.collectSudoPassword("install");
    if (this.disposed) return;
    if (password === "cancelled" || password === "refused" || this.state.stopping) {
      await this.releaseSudoAgent();
      if (password === "refused") this.passwordProblem = "Your password wasn't accepted. Nothing was changed.";
      this.state.returnToCollect();
      this.setFlow(action);
      return;
    }

    const controller = new AbortController();
    this.runAbort = controller;
    const pin = checkoutPinFor(action, priorReceipt, inputs.version);
    const runKind = action === "repair" ? "repair" : "install";
    this.state.setStepWeights(await historicalWeights(this.deps.runsDir ?? defaultRunsDir(), { kinds: [runKind] }));
    const recorder = await RunRecorder.begin({
      dir: this.deps.runsDir ?? defaultRunsDir(),
      instance: LOCAL_INSTANCE_NAME,
      kind: runKind,
      // A repair returns the cluster to the checkout its receipt names: that
      // is both where it came from and where it is going (memql#3901).
      ...(action === "repair" && recordedCheckout(priorReceipt).label !== ""
        ? { fromVersion: recordedCheckout(priorReceipt).label, toVersion: recordedCheckout(priorReceipt).label }
        : {}),
    });
    this.deps.refreshTree();

    let report: ExecutionReport | undefined;
    let failure: string | undefined;
    try {
      report = await runInstall(
        // Built on the far side of the `vscode` line, where the params every
        // capability script requires are audited (memql#3560). A repair
        // replays the recorded checkout, images and lane (memql#3605,
        // #3901, #4068, #4430); an install honours the version chosen.
        installSessionOptions({
          root: this.deps.installRoot,
          receiptFile: this.deps.receiptFile,
          domain: inputs.domain,
          ownerEmail: inputs.ownerEmail,
          ownerFirstName: inputs.ownerFirstName,
          ownerLastName: inputs.ownerLastName,
          tag: pin.tag,
          commit: pin.commit,
          imageTag: pin.imageTag,
          imagesFromSource: pin.imagesFromSource,
          timeoutMs: STEP_TIMEOUT_MS,
          env: this.sudoEnv(),
        }),
        this.hooks({
          onEvent: (event) => {
            this.foldInstallEvent(event);
            void recorder.apply(event);
          },
          signal: controller.signal,
        }),
      );
    } catch (err) {
      // A THROW IS NOT A FAILED STEP: the run could not be attempted at all --
      // a missing graph document, an unreadable script.
      const raw = err instanceof Error ? err.message : String(err);
      recordDiagnostic(this.deps.diagnostics, "the run could not be attempted", raw, new Date().toISOString());
      failure = redactForDisplay(raw, os.homedir());
    } finally {
      this.runAbort = undefined;
      await this.releaseSudoAgent();
    }

    await recorder.finish(
      controller.signal.aborted ? "cancelled" : failure !== undefined || report?.ok !== true ? "failed" : "succeeded",
    );
    this.deps.refreshTree();
    if (this.disposed) return;

    if (failure !== undefined) {
      this.runError = failure;
      this.state.finish({ ok: false });
      return;
    }

    // `ok` means nothing FAILED, which a cancelled run usually satisfies too.
    const succeeded = report?.ok === true && report?.cancelled !== true;

    // A FAILED STEP STAYS ON THE FAILURE: `finish()` would move to `done`,
    // and "done" after a failure was the confident-wrong "Finished / Nothing
    // further to do" (memql#5118 audit).
    if (!succeeded && this.state.failed !== undefined) return;

    this.state.finish({ ok: report?.ok === true, cancelled: report?.cancelled === true });
    if (!succeeded) {
      // Stopped: what ran is recorded and can be uninstalled.
      this.presence.invalidate();
      return;
    }

    // What the done screen needs from the run, read now because `report` is
    // local here: whether there is an owner account to enrol against
    // (memql#3906), the owner magic link when nobody owns the cluster
    // (memql#3884, #4622), and the one-time recovery key -- DISPLAY, not
    // storage (memql#4079).
    if (report !== undefined) {
      this.state.setOwnerAccountExists(ownerAccountExistsFrom(report));
      this.state.setClaimUrl(claimUrlFrom(report));
      this.state.setRecoveryKey(revealedRecoveryKeyFrom(report), recoveryKeyStateFrom(report));
    }
    this.doneKind = action === "repair" ? "repaired" : "installed";
    await this.handOffAfterInstall(inputs.domain);
  }

  /** One install event into the state machine, the log and the bar -- without a document. */
  private foldInstallEvent(event: ExecEvent): void {
    const before = this.state.failed?.id;
    this.state.apply(event);
    this.onRunEvent(event);
    // THE FIRST FAILURE CHANGES THE SCREEN: the notice, the acts, and the log
    // opened at the failed step.
    const failed = this.state.failed;
    if (failed !== undefined && failed.id !== before) this.openLogAtFailure(failed.id);
    else if (isStepBoundary(event)) this.onStepBoundary();
  }

  /**
   * A step started or settled: the regions are patched (the document is
   * not replaced) and the bar moves. Log lines and phases never come here;
   * they travel as their own messages.
   */
  private onStepBoundary(): void {
    this.render();
    this.pushProgress();
  }

  /**
   * What every run event does to the page beside the state machine: the log
   * line (and the MemQL Install output), and the bar on a step boundary.
   */
  private onRunEvent(event: ExecEvent): void {
    if (event.type === "stepLog") {
      const line = redactForDisplay(event.line, os.homedir());
      this.deps.diagnostics.appendLine(`[${event.step.id}] ${line}`);
      this.queueLine(this.runLog.add(event.step.id, event.step.label || event.step.id, line));
      return;
    }
    if (event.type === "stepPhase") {
      this.pushProgress();
      return;
    }
    if (event.type === "stepFinished" && event.outcome.status === "failed") {
      // THE EXIT CODE AND THE VERIFY DETAIL GO TO THE LOG, not the page's
      // sentence: "exit 4: ..." is an account for the record.
      const reason = redactForDisplay(event.outcome.reason ?? "", os.homedir());
      if (reason !== "") {
        this.deps.diagnostics.appendLine(`[${event.step.id}] ${reason}`);
        this.queueLine(this.runLog.add(event.step.id, event.step.label || event.step.id, reason, "error"));
      }
    }
  }

  /** The log opens at the failed step's first line; the screen changes. */
  private openLogAtFailure(stepId: string): void {
    this.pendingLines = [];
    this.clearFlushTimer();
    this.live.log(this.runLog.anchoredAt(stepId), { reset: true });
    this.render();
    this.pushProgress();
    void this.panel.webview.postMessage({ type: "setDisclosure", id: "run-logs", open: true });
  }

  private queueLine(line: LogLine): void {
    this.pendingLines.push(line);
    if (this.flushTimer !== undefined) return;
    this.flushTimer = setTimeout(() => {
      this.flushTimer = undefined;
      this.flushLog();
    }, LOG_FLUSH_MS);
    this.flushTimer.unref?.();
  }

  private flushLog(): void {
    this.clearFlushTimer();
    if (this.pendingLines.length === 0 || this.disposed) return;
    const lines = this.pendingLines;
    this.pendingLines = [];
    this.live.log(lines);
  }

  private clearFlushTimer(): void {
    if (this.flushTimer !== undefined) clearTimeout(this.flushTimer);
    this.flushTimer = undefined;
  }

  /** The bar moves with the clock inside a long step; the page ticks only the elapsed time. */
  private startTicker(): void {
    this.stopTicker();
    this.ticker = setInterval(() => this.pushProgress(), PROGRESS_TICK_MS);
    this.ticker.unref?.();
  }

  private stopTicker(): void {
    if (this.ticker !== undefined) clearInterval(this.ticker);
    this.ticker = undefined;
  }

  /**
   * The progress region's live values, without touching the document: the
   * run's while it runs, and the settled screen's once it is done -- so a
   * reloaded page is never handed a running bar over a finished run.
   */
  private pushProgress(): void {
    if (this.disposed) return;
    const done = this.doneInput();
    if (done !== undefined) {
      this.live.progress(doneProgressUpdate(done));
      return;
    }
    const uninstalled = this.uninstalledInput();
    if (uninstalled !== undefined) {
      this.live.progress(uninstalledProgressUpdate(uninstalled));
      return;
    }
    const input = this.runInput();
    if (input !== undefined) this.live.progress(runProgressUpdate(input));
  }

  /** Cancel, during a run: stops at the next wave boundary, and says so until it has. */
  private cancelRun(): void {
    if (this.state.screen === "uninstallPreview") {
      if (this.uninstall.phase !== "running") return;
      this.uninstallAbort?.abort();
      this.uninstall.requestStop();
    } else {
      if (!this.runInFlight || this.state.screen !== "running") return;
      // Abort FIRST: the executor stops at the next wave boundary and the
      // receipt has been written after every step that ran, so what was built
      // remains uninstallable -- which is what makes Cancel safe to offer.
      this.runAbort?.abort();
      this.state.requestStop();
    }
    this.render();
    this.pushProgress();
  }

  /**
   * Retry after a failure that has come to rest.
   *
   * ONLY THEN. Pressed while other steps were still finishing, Retry used to
   * be dropped -- the old run still held the lock -- and the page landed on
   * "Finished / Nothing further to do" when that run ended (memql#5118 audit).
   * The bar offers no acts until the run is over; this is the wall behind it.
   */
  private retry(): void {
    if (this.runBusy()) return;
    if (this.state.screen === "uninstallPreview") {
      if (this.uninstall.phase === "failed" && this.uninstallRetryable()) void this.startUninstall();
      return;
    }
    if (this.state.screen !== "failedStep" || !this.installRetryable()) return;
    this.state.retry();
    this.startFreshRun();
    this.render();
    void this.startRun();
  }

  /** Resume a run that was stopped: the same run again, which skips what already happened. */
  private resume(): void {
    if (this.runBusy()) return;
    if (this.state.screen === "uninstallPreview") {
      if (this.uninstall.phase === "stopped") void this.startUninstall();
      return;
    }
    if (this.state.screen !== "done" || !this.state.cancelled) return;
    if (this.state.beginRun()) {
      this.startFreshRun();
      void this.startRun();
    }
    this.render();
  }

  private installRetryable(): boolean {
    const failures = this.state.failures;
    return failures.length > 0 && failures.every((f) => guidanceFor(f).retryable);
  }

  private uninstallRetryable(): boolean {
    const failed = this.uninstall.failure;
    return failed !== undefined && guidanceFor(failed).retryable;
  }

  /** Puts the recorded answers into the repair form's boxes, never over what was typed (memql#3544). */
  private async prefillFromReceipt(): Promise<void> {
    const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    if (this.disposed || this.state.action !== "repair") return;
    const recordedHost = recordedDomain(receipt);
    if (this.state.inputs.domain === "" && recordedHost !== "") this.state.setInput("domain", recordedHost);
    const owner = recordedOwner(receipt);
    if (this.state.inputs.ownerEmail === "" && owner.email !== "") this.state.setInput("ownerEmail", owner.email);
    if (this.state.inputs.ownerFirstName === "" && owner.firstName !== "") {
      this.state.setInput("ownerFirstName", owner.firstName);
    }
    if (this.state.inputs.ownerLastName === "" && owner.lastName !== "") {
      this.state.setInput("ownerLastName", owner.lastName);
    }
    this.render();
  }

  /**
   * The release tags the version field offers (memql#3882), loaded in the
   * background: the field already holds a working default, so nobody waits on
   * a network call to answer it. The newest becomes the selection unless the
   * person has chosen (memql#4429).
   */
  private async loadVersionChoices(): Promise<void> {
    if (this.versionChoices.length > 0) return;
    const listing = await listReleaseTags({ cwd: process.cwd(), repo: DEFAULT_STACK_REPO });
    if (listing.tags.length === 0 || this.disposed) return;
    this.versionChoices = listing.tags;
    this.state.seedVersionFromListing(listing.tags[0] ?? "");
    this.render();
  }

  /**
   * The form's checks (memql#4195), from the same sources the run consults:
   * the lane's graph document, the `sudo -n` probe and the receipt.
   */
  private async computeChecks(action: "install" | "repair"): Promise<void> {
    this.checks = undefined;
    let graph: { ok: true; steps: number; needsElevation: boolean } | { ok: false; error: string };
    let needsElevation = false;
    try {
      // THE LANE'S OWN DOCUMENT (memql#4430): a from-source install has one
      // more step.
      const loaded = await loadGraphFile(
        installGraphPath(this.deps.installRoot, action === "install" && isMainBranchChoice(this.state.inputs.version)),
      );
      needsElevation = loaded.steps.some((step) => step.elevation !== "none");
      graph = { ok: true, steps: loaded.steps.length, needsElevation };
    } catch (err) {
      const detail = redactForDisplay(err instanceof Error ? err.message : String(err), os.homedir());
      this.deps.diagnostics.appendLine(`the installer's step list could not be read: ${detail}`);
      graph = { ok: false, error: detail };
    }
    let sudoFree = process.getuid?.() === 0;
    if (!sudoFree && needsElevation) {
      const isFree = this.deps.sudoIsFree ?? (() => sudoRunsWithoutAsking());
      sudoFree = await isFree().catch(() => false);
    }
    const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    if (this.disposed || this.state.screen !== "collect" || this.state.action !== action) return;
    this.checks = preflightChecks({
      action,
      graph,
      sudoFree,
      imageSource: recordedImageSource(receipt),
      releasedTag: recordedCheckout(receipt).tag,
    });
    this.render();
  }

  // ---------------------------------------------------------------------------
  // the password
  // ---------------------------------------------------------------------------

  /**
   * Collects the password ONCE, if this run needs one at all (memql#3568).
   *
   * sudo's cache is keyed by the parent process and every step is its own
   * process, so without this an install asked three times. Not asked when
   * already root or when sudo runs without asking. A wrong password is caught
   * here, with `sudo -A -v`, and asked again -- up to three times.
   *
   * DISMISSING THE PROMPT IS AN ANSWER: `cancelled`, and the run does not
   * start. Three refusals is `refused`, and it does not start either.
   */
  private async collectSudoPassword(kind: GraphKind): Promise<"ok" | "notNeeded" | "cancelled" | "refused"> {
    await this.releaseSudoAgent();
    if (!(await this.runNeedsAPassword(kind))) return "notNeeded";
    const purpose =
      kind === "uninstall"
        ? "To remove the local addresses and the certificate it trusted."
        : "To update the hosts file and trust a local certificate.";
    for (let attempt = 0; attempt < 3; attempt += 1) {
      const secret = await vscode.window.showInputBox({
        password: true,
        ignoreFocusOut: true,
        title: "MemQL needs your password",
        prompt: attempt === 0 ? purpose : "That password wasn't accepted. Try again.",
        placeHolder: "Your computer password",
      });
      if (secret === undefined || secret === "") return "cancelled";
      const agent = await startSudoAgent(secret, process.execPath);
      const accepts = this.deps.sudoAccepts ?? sudoAccepts;
      if (await accepts(agent.askpassPath)) {
        this.sudoAgent = agent;
        return "ok";
      }
      await agent.dispose();
    }
    return "refused";
  }

  /** Whether any step of this graph needs a privilege this process lacks. */
  private async runNeedsAPassword(kind: GraphKind): Promise<boolean> {
    if (process.getuid?.() === 0) return false;
    let graph: Graph;
    try {
      graph = await loadGraphFile(graphDocumentPath(kind, this.deps.installRoot));
    } catch {
      return false; // The run is about to fail on the same missing document.
    }
    if (!graph.steps.some((step) => step.elevation !== "none")) return false;
    const isFree = this.deps.sudoIsFree ?? (() => sudoRunsWithoutAsking());
    return !(await isFree());
  }

  /** The elevation environment every step is handed: ALWAYS set, agent or none (memql#3586). */
  private sudoEnv(): Record<string, string> {
    return elevationEnv(this.sudoAgent?.askpassPath);
  }

  /** Stops the agent and removes its socket. Idempotent. */
  private async releaseSudoAgent(): Promise<void> {
    const agent = this.sudoAgent;
    this.sudoAgent = undefined;
    if (agent !== undefined) await agent.dispose();
  }

  private platformSession(): SessionOptions {
    return {
      root: this.deps.installRoot,
      receiptFile: this.deps.receiptFile,
      skip: new Set<string>(),
      stepParams: {},
      timeoutMs: STEP_TIMEOUT_MS,
      // The same marker every step carries (memql#3586): detect is read-only,
      // but a probe without it would be free to draw a desktop dialog.
      env: elevationEnv(undefined),
    };
  }

  // ---------------------------------------------------------------------------
  // the hand-off: into the list, and the one next act
  // ---------------------------------------------------------------------------

  /**
   * Registers the cluster the run built (or the one already here), and moves
   * to the done screen. The ordering and failure semantics are
   * src/install/handoff.ts's, where they are tested.
   *
   * THE SELECTION IS QUIET. Selecting dials, and on a freshly installed
   * cluster the dial finds no credential and the select command used to put a
   * modal "Set up now" over this page -- in front of the one-time recovery
   * key, and repeating the page's own next act (memql#5118 audit). The page
   * owns the next step, so it asks the command not to offer one.
   */
  async handOffAfterInstall(domain: string): Promise<void> {
    if (this.handingOff) return;
    this.handingOff = true;
    this.handoffDomain = domain;
    try {
      await this.handOff(domain);
    } finally {
      this.handingOff = false;
    }
  }

  private async handOff(domain: string): Promise<void> {
    const result = await completeInstallHandoff(
      { domain },
      {
        // upsertCluster, never addCluster: a repair updates the entry.
        write: (update) => upsertCluster(this.deps.clustersPath, update),
        invalidatePresence: () => this.presence.invalidate(),
        refreshTree: () => void vscode.commands.executeCommand("memql.clusters.refresh"),
        select: async (cluster) => {
          await vscode.commands.executeCommand("memql.clusters.select", { cluster, selected: false, quiet: true });
        },
      },
    );
    if (!result.ok) this.deps.diagnostics.appendLine(`the cluster could not be added to the list: ${result.message}`);
    this.state.setHandoff(result);
    this.render();
  }

  /**
   * Adds the local cluster that is already here to the list, then signs in.
   *
   * THE SAME HAND-OFF AN INSTALL USES, from the domain the install recorded
   * (or the installer's default for a cluster `make up` built). Then the sign
   * in, because "Connect to it" is what was asked for and a cluster in the
   * list that nobody can reach is half of it.
   */
  private async reconnectLocal(): Promise<void> {
    const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    if (this.disposed) return;
    const plan = planLocalReconnect(receipt);
    this.doneKind = "added";
    this.runError = "";
    this.runStartedAt = undefined;
    this.runEndedAt = undefined;
    await this.handOffAfterInstall(plan.domain);
    const handoff = this.state.handoff;
    if (handoff !== undefined && handoff.ok && !this.signedIn(handoff.cluster.name)) void this.signIn();
  }

  private signedIn(name: string): boolean {
    return this.deps.isSignedIn?.(name) ?? false;
  }

  /** The cluster the done or added screen is about. */
  private currentCluster(): ClusterConfig | undefined {
    if (this.added !== undefined && this.state.screen === "connect") return this.added.cluster;
    const handoff = this.state.handoff;
    return handoff !== undefined && handoff.ok ? handoff.cluster : undefined;
  }

  /** Sign in to the cluster on screen, then look again at whether it worked. */
  private async signIn(): Promise<void> {
    const cluster = this.currentCluster();
    if (cluster === undefined) return;
    await vscode.commands.executeCommand("memql.clusters.signIn", { cluster, selected: true });
    this.render();
  }

  private async openOs(): Promise<void> {
    const cluster = this.currentCluster();
    if (cluster === undefined) return;
    await vscode.commands.executeCommand("memql.clusters.openConsole", { cluster, selected: true });
  }

  /** The landing's Sign in: the listed local cluster, as the list has it. */
  private async signInToLocal(): Promise<void> {
    const cluster = await this.localEntry();
    if (cluster === undefined) return;
    await vscode.commands.executeCommand("memql.clusters.signIn", { cluster, selected: true });
    if (!this.disposed && this.state.screen === "landing") void this.redetect();
  }

  private async openOsForLocal(): Promise<void> {
    const cluster = await this.localEntry();
    if (cluster !== undefined) await vscode.commands.executeCommand("memql.clusters.openConsole", { cluster, selected: true });
  }

  private async localEntry(): Promise<ClusterConfig | undefined> {
    const name = this.detected?.clusterName;
    if (name === undefined) return undefined;
    const result = await readClustersFileSafe(this.deps.clustersPath);
    return result.ok ? result.file.clusters.find((c) => c.name === name) : undefined;
  }

  /** A passkey enrolment link, minted on click by the one command that does it (memql#3408, #3906). */
  private async enrolPasskey(): Promise<void> {
    const cluster = this.currentCluster();
    if (cluster === undefined) return;
    await vscode.commands.executeCommand("memql.clusters.takeOwnership", { cluster, selected: true });
  }

  /**
   * Claims the cluster with the magic link the install recovered.
   *
   * The link authenticates as the OWNER, so it never enters the page (the act
   * carries no href) and `openClaimLink` re-validates it before anything opens.
   * A host that cannot open a browser gets the link to copy (memql#4618).
   */
  private async claimCluster(): Promise<void> {
    const url = this.state.claimUrl;
    if (url === "") return;
    try {
      await openClaimLink(url, {
        resolveExternalUri: async (target) => (await vscode.env.asExternalUri(vscode.Uri.parse(target))).toString(),
        openExternal: async (target) => await vscode.env.openExternal(vscode.Uri.parse(target)),
      });
    } catch (err) {
      const detail = err instanceof ClaimError ? err.message : String(err);
      recordDiagnostic(this.deps.diagnostics, "the claim link could not be opened", detail, new Date().toISOString());
      if (err instanceof ClaimError && err.reason === "browserUnavailable") {
        void (async () => {
          const chosen = await vscode.window.showErrorMessage("MemQL: couldn't open a browser.", COPY_LINK);
          if (chosen === COPY_LINK) await this.copyClaimLink(url);
        })();
        return;
      }
      void vscode.window.showErrorMessage("MemQL: couldn't open the claim link.");
    }
  }

  /** The owner link to the clipboard, for a host with no browser. Never logged. */
  private async copyClaimLink(url: string): Promise<void> {
    try {
      await vscode.env.clipboard.writeText(url);
    } catch (err) {
      recordDiagnostic(
        this.deps.diagnostics,
        "the claim link could not be copied to the clipboard",
        err instanceof Error ? err.message : String(err),
        new Date().toISOString(),
      );
      void vscode.window.showErrorMessage("MemQL: couldn't copy the link.");
      return;
    }
    void vscode.window.showInformationMessage("MemQL: link copied. It works once.");
  }

  /**
   * Copies the revealed recovery key (memql#4079), from panel state, never
   * from the message. Loud on failure; on success the page says "Copied".
   */
  private async copyRecoveryKey(): Promise<void> {
    const key = this.state.revealedRecoveryKey;
    if (key === "") return;
    try {
      await vscode.env.clipboard.writeText(key);
    } catch (err) {
      recordDiagnostic(
        this.deps.diagnostics,
        "the recovery key could not be copied to the clipboard",
        err instanceof Error ? err.message : String(err),
        new Date().toISOString(),
      );
      void vscode.window.showErrorMessage("MemQL: couldn't copy the recovery key. Select it on the page and copy it.");
      return;
    }
    // RECORDED ONLY ON SUCCESS (memql#4615): this is what tells the close
    // warning the person actually has the key.
    this.state.recordRecoveryKeyCopied();
    this.render();
  }

  /** The run's log, as text, to the clipboard. */
  private async copyLog(): Promise<void> {
    try {
      await vscode.env.clipboard.writeText(this.runLog.text());
    } catch {
      void vscode.window.showErrorMessage("MemQL: couldn't copy the log. Open it in Output instead.");
      return;
    }
    void vscode.window.showInformationMessage("MemQL: log copied.");
  }

  /**
   * Opens a terminal holding the failed step's remedy, TYPED, not executed.
   *
   * THE COMMAND COMES FROM THE RUN'S OWN RECORDS, NEVER FROM THE MESSAGE: the
   * page posts a step id and the command is looked up against the failures
   * this panel recorded -- for the install run or the uninstall run,
   * whichever is on screen. Accepting a command over postMessage would let
   * anything in that iframe choose what a person is invited to run as root.
   */
  private openRemedyTerminal(stepId: string): void {
    const failures =
      this.state.screen === "uninstallPreview"
        ? this.uninstall.steps.filter((step) => step.state === "failed")
        : this.state.failures;
    const failure = failures.find((f) => f.id === stepId);
    if (failure === undefined || failure.remedy === "") return;
    const terminal = vscode.window.createTerminal({ name: "MemQL fix" });
    terminal.show();
    terminal.sendText(failure.remedy, false);
  }

  /**
   * Back to the landing, with the one-time recovery key standing in the way
   * when it has not been copied (memql#4615): only its hash is stored, so
   * leaving the page destroys it. A refusal changes nothing.
   */
  private async leaveScreen(): Promise<void> {
    if (this.runBusy()) return;
    if (this.state.recoveryKeyWouldBeLost) {
      const proceed = await this.confirmDestructive({
        message: "Leave without saving the recovery key?",
        detail: "It can't be shown again. Copy it first if you haven't saved it.",
        proceed: "Leave",
      });
      if (!proceed || this.disposed) return;
    }
    this.state.back();
    this.uninstall.reset();
    this.toLanding();
  }

  /** The modal, or whatever a test injected. Dismissal means KEEP (isCloseAffordance). */
  private async confirmDestructive(prompt: DestructiveConfirmation): Promise<boolean> {
    const inject = this.deps.confirmDestructive;
    if (inject !== undefined) return inject(prompt);
    const go: vscode.MessageItem = { title: prompt.proceed };
    const keep: vscode.MessageItem = { title: "Cancel", isCloseAffordance: true };
    const chosen = await vscode.window.showWarningMessage<vscode.MessageItem>(
      prompt.message,
      { modal: true, detail: prompt.detail },
      go,
      keep,
    );
    return chosen === go;
  }

  // ---------------------------------------------------------------------------
  // connecting to a cluster elsewhere (memql#3475)
  // ---------------------------------------------------------------------------

  /** The registry the inline duplicate check reads; the write-time check is the wall. */
  private async loadRegistry(): Promise<void> {
    const result = await readClustersFileSafe(this.deps.clustersPath);
    if (result.ok) this.state.setRegistry(result.file);
  }

  /**
   * Validates, probes, then adds -- with `addCluster`, never upsert: this is an
   * ADD, and upsert would turn a name collision into an edit of the cluster
   * already there. A failed probe warns and never blocks: the next press is
   * "Add anyway".
   */
  private async saveConnect(): Promise<void> {
    if (this.saving || this.state.screen !== "connect" || this.added !== undefined) return;
    this.saving = true;
    try {
      const pending = this.state.prepareConnectSave(this.deps.probeCluster ?? probeClusterOverHttps);
      this.render(); // the busy "Checking" button, while the probe runs
      const outcome = await pending;
      if (outcome !== "write") return;
      const draft = this.state.connectDraft();
      if (draft === undefined) return;
      try {
        await addCluster(this.deps.clustersPath, draft);
      } catch (err) {
        this.state.failConnect(err instanceof Error ? err.message : String(err));
        return;
      }
      this.deps.refreshTree();
      this.presence.invalidate();
      // THE PAGE STAYS, on what was added, with the one next act: Sign in.
      // It used to close and leave a toast carrying the button.
      const result = await readClustersFileSafe(this.deps.clustersPath);
      const cluster =
        (result.ok ? result.file.clusters.find((c) => c.name === draft.name) : undefined) ??
        ({ name: draft.name, endpoint: draft.endpoint, ...(draft.domain === undefined ? {} : { domain: draft.domain }) } as ClusterConfig);
      this.added = {
        cluster,
        name: cluster.name,
        address: cluster.endpoint,
        osUrl: composeConsoleUrl(cluster),
        hasToken: draft.token !== undefined,
        reachable: this.state.connectProbe.state === "passed",
      };
    } finally {
      this.saving = false;
      if (!this.disposed) this.render();
    }
  }

  // ---------------------------------------------------------------------------
  // uninstall (memql#3476)
  // ---------------------------------------------------------------------------

  /** A shared tool's switch: honoured only for a shared removal the preview offered. */
  private onSharedSwitch(msg: Record<string, unknown>): void {
    const step = typeof msg.value === "string" ? msg.value : "";
    const on = msg.checked === true;
    const tools = sharedToolRows(this.uninstallPreview ?? { removals: [] });
    const tool = tools.find((t) => t.id === step);
    if (tool === undefined || this.uninstall.phase !== "preview") return;
    if (on) {
      // A tool that cannot be removed without another is not switched on
      // alone: the switch is unavailable until that other one is on.
      if (tool.requires !== undefined && !this.removeShared.has(tool.requires)) {
        this.render();
        return;
      }
      this.removeShared.add(step);
    } else {
      this.removeShared.delete(step);
      // And turning that other one off turns its dependents off with it.
      for (const dependent of tools.filter((t) => t.requires === step)) this.removeShared.delete(dependent.id);
    }
    this.render();
  }

  private onDeleteDataSwitch(on: boolean): void {
    if (this.uninstall.phase !== "preview" || !this.keptClusterOffered()) return;
    this.deleteData = on;
    // TURNING IT OFF CLEARS THE PHRASE: a phrase left behind a switch somebody
    // turned off is consent this page no longer has.
    if (!on) this.deleteDataPhrase = "";
    this.render();
  }

  /** Whether a cluster MemQL did not make would be kept, so deleting its data can be offered. */
  private keptClusterOffered(): boolean {
    return (this.uninstallPreview?.preserved ?? []).some((step) => step.params["kind"] === "stack");
  }

  /** Whether the delete-data consent is whole: the switch and the exact phrase. */
  private deleteDataConfirmed(): boolean {
    return this.deleteData && phraseMatches(this.deleteDataPhrase);
  }

  /** Every consent the preview collected, gone: on Cancel, Back, a fresh preview and a finished uninstall. */
  private resetConsents(): void {
    this.removeShared.clear();
    this.deleteData = false;
    this.deleteDataPhrase = "";
  }

  private uninstallBack(): void {
    if (this.uninstalling) return;
    this.uninstall.reset();
    this.uninstallPreview = undefined;
    this.uninstallNothing = undefined;
    this.resetConsents();
    this.state.back();
    this.toLanding();
  }

  /**
   * The run-time inputs a removal needs; everything else comes off the receipt.
   *
   * `unreceiptedCluster` is set when there is no install record and the k3d
   * cluster is really here: the one artifact the removal can take without a
   * record, kept unless its data is deliberately deleted (memql#5118, D8).
   */
  private uninstallOptions(): SessionOptions {
    return {
      root: this.deps.installRoot,
      receiptFile: this.deps.receiptFile,
      ...(this.uninstallUnreceipted ? { unreceiptedCluster: LOCAL_CLUSTER_NAME } : {}),
      // THE SHARED TOOLS NOT SWITCHED ON are skipped (memql#3566).
      skip: this.skippedSharedRemovals(),
      // THE ONE DESTRUCTIVE ACT reaches the script only when the switch is on
      // and the phrase matches exactly; the script checks it again (D9).
      stepParams: this.deleteDataConfirmed() ? { removeCluster: { confirm: DELETE_DATA_CONFIRM_VALUE } } : {},
      timeoutMs: STEP_TIMEOUT_MS,
      env: this.sudoEnv(),
    };
  }

  private skippedSharedRemovals(): Set<string> {
    const skip = new Set<string>();
    for (const step of this.uninstallPreview?.removals ?? []) {
      if (step.shared && !this.removeShared.has(step.id)) skip.add(step.id);
    }
    return skip;
  }

  /**
   * Works out what an uninstall would do, and shows it. NOTHING RUNS HERE.
   *
   * A CLUSTER WITH NO INSTALL RECORD IS STILL UNINSTALLABLE. A `make up`
   * cluster, or one adopted into the list, has no receipt, and the preview
   * used to refuse it outright -- a dead end the landing then offered (the
   * owner's own machine; memql#5118 audit). When there is no receipt and k3d
   * lists the cluster, the preview plans that one removal: kept, as a cluster
   * MemQL did not make, unless its data is deliberately deleted. When there
   * is neither, there is nothing here, and the page says that instead.
   */
  private async loadUninstallPreview(): Promise<void> {
    this.uninstall.reset();
    this.uninstallPreview = undefined;
    this.uninstallNothing = undefined;
    this.uninstallUnreceipted = false;
    this.resetConsents();
    this.uninstallLoading = true;
    this.render();
    try {
      const presence = await this.presence.get().catch(() => undefined);
      this.localClusterName = presence?.clusterName;
      const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
      if (receipt === null) {
        const listed =
          presence?.verdict === "present-unreceipted" ||
          (await (this.deps.listLocalClusters?.() ?? Promise.resolve([])).catch(() => [] as string[])).some(
            (name) => name.trim() === LOCAL_CLUSTER_NAME,
          );
        if (!listed) {
          this.uninstallNothing = { removeFromList: this.localClusterName !== undefined };
          return;
        }
        this.uninstallUnreceipted = true;
      }
      // The PREVIEW plans every shared step -- they are what the switches
      // offer; only the RUN skips the ones left off.
      this.uninstallPreview = await previewUninstall(
        { ...this.uninstallOptions(), skip: new Set<string>() },
        this.hooks({}),
      );
    } catch (err) {
      const detail = redactForDisplay(err instanceof Error ? err.message : String(err), os.homedir());
      this.deps.diagnostics.appendLine(`the uninstall preview could not be read: ${detail}`);
      this.uninstallNothing = { removeFromList: this.localClusterName !== undefined };
    } finally {
      this.uninstallLoading = false;
      if (!this.disposed) this.render();
    }
  }

  /** "Remove from list", for a list entry with nothing on this computer behind it. */
  private async removeFromList(): Promise<void> {
    const name = this.localClusterName;
    if (name === undefined || this.uninstallNothing === undefined) return;
    try {
      await this.deps.removeRegistryEntry(name);
    } catch (err) {
      recordDiagnostic(this.deps.diagnostics, "the cluster could not be removed from the list", String(err), new Date().toISOString());
      void vscode.window.showErrorMessage("MemQL: couldn't remove it from the list.");
      return;
    }
    this.presence.invalidate();
    this.deps.refreshTree();
    this.uninstallBack();
  }

  /**
   * Runs the removal consented to on the preview.
   *
   * THE PREVIEW IS THE PRECONDITION: with no list on screen there is nothing
   * consent was given to. THE PHRASE IS A PRECONDITION when the data switch is
   * on (the act is absent until it matches; this is the wall behind it).
   */
  private async startUninstall(): Promise<void> {
    if (this.uninstalling || this.runInFlight || this.uninstallPreview === undefined) return;
    if (this.deleteData && !this.deleteDataConfirmed()) {
      this.render();
      return;
    }
    this.uninstalling = true;
    this.uninstall.begin();
    this.startFreshRun();
    this.startTicker();
    this.render();
    try {
      await this.runUninstallOnce();
    } finally {
      this.uninstalling = false;
      this.uninstallAbort = undefined;
      this.stopTicker();
      this.runEndedAt = Date.now();
      this.flushLog();
      await this.releaseSudoAgent();
      if (!this.disposed) {
        this.render();
        this.pushProgress();
      }
    }
  }

  private async runUninstallOnce(): Promise<void> {
    // Platform before the password (memql#4294).
    const refused = await refuseUnsupportedPlatform(this.uninstallOptions(), this.hooks({}));
    if (refused !== undefined) {
      for (const event of platformRefuseEvents(refused)) this.foldUninstallEvent(event);
      this.uninstall.finish(refused);
      return;
    }
    // DISMISSING THE PASSWORD STARTS NOTHING (memql#5118 audit): the removal
    // used to go ahead and delete the cluster, then fail on the hosts file.
    const password = await this.collectSudoPassword("uninstall");
    if (this.disposed) return;
    if (password === "cancelled" || password === "refused" || this.uninstall.stopping) {
      this.uninstall.reset();
      if (password === "refused") void vscode.window.showWarningMessage("MemQL: your password wasn't accepted. Nothing was removed.");
      return;
    }
    const controller = new AbortController();
    this.uninstallAbort = controller;
    // Read BEFORE the removal: the receipt is one of the things about to go.
    const uninstalledVersion = recordedStackTag(await readReceipt(this.deps.receiptFile).catch(() => null));
    this.uninstall.setStepWeights(
      await historicalWeights(this.deps.runsDir ?? defaultRunsDir(), { kinds: ["uninstall"] }),
    );
    const recorder = await RunRecorder.begin({
      dir: this.deps.runsDir ?? defaultRunsDir(),
      instance: LOCAL_INSTANCE_NAME,
      kind: "uninstall",
      ...(uninstalledVersion !== "" ? { fromVersion: uninstalledVersion } : {}),
    });
    this.deps.refreshTree();
    try {
      const report = await runUninstall(
        this.uninstallOptions(),
        this.hooks({
          onEvent: (event) => {
            this.foldUninstallEvent(event);
            void recorder.apply(event);
          },
          signal: controller.signal,
        }),
      );
      this.uninstall.finish(report);
      if (report.ok && report.cancelled !== true) {
        await this.completeUninstall(report.keptCluster === true);
        // Nothing left to consent to.
        this.resetConsents();
      } else {
        // A partial removal still changed the machine; the list entry stays,
        // naming a cluster some of whose artifacts are still here.
        this.presence.invalidate();
        this.deps.refreshTree();
      }
    } catch (err) {
      const detail = redactForDisplay(err instanceof Error ? err.message : String(err), os.homedir());
      this.deps.diagnostics.appendLine(`the uninstall could not run: ${detail}`);
      this.uninstall.fail(detail);
    } finally {
      // `preserved` reaches the record untranslated: the uninstall KEPT something.
      await recorder.finish(controller.signal.aborted ? "cancelled" : undefined);
      this.deps.refreshTree();
    }
  }

  private foldUninstallEvent(event: ExecEvent): void {
    const before = this.uninstall.failure?.id;
    this.uninstall.apply(event);
    this.onRunEvent(event);
    const failed = this.uninstall.failure;
    if (failed !== undefined && failed.id !== before) this.openLogAtFailure(failed.id);
    else if (isStepBoundary(event)) this.onStepBoundary();
  }

  /** The records that follow a clean removal (completeLocalUninstall). */
  private async completeUninstall(keptCluster: boolean): Promise<void> {
    const problem = await completeLocalUninstall({
      keptCluster,
      clusterName: this.localClusterName,
      removeEntry: (name) => this.deps.removeRegistryEntry(name),
      invalidatePresence: () => this.presence.invalidate(),
      refreshTree: () => this.deps.refreshTree(),
      deleteReceipt: () => deleteReceipt(this.deps.receiptFile),
    });
    if (problem !== "") this.uninstall.noteFollowUpProblem(problem);
  }

  // ---------------------------------------------------------------------------
  // rendering
  // ---------------------------------------------------------------------------

  private render(): void {
    if (this.disposed) return;
    const { key, parts } = this.screen();
    this.live.render(key, parts);
    // The progress values travel with every render, so the page's progress
    // region -- and a reload of it -- always shows the screen it is on.
    this.pushProgress();
  }

  /** The screen for the state the panel is in, and its LiveView key. */
  private screen(): { key: string; parts: RegionParts } {
    const s = this.state;
    switch (s.screen) {
      case "landing":
        return { key: "landing", parts: landingScreen({ view: this.currentLanding() }) };
      case "collect": {
        const action = s.action === "repair" ? "repair" : "install";
        const remote = localInstallRemoteProblem(vscode.env.remoteName);
        return {
          key: `collect-${action}`,
          parts: collectScreen({
            action,
            values: s.inputs,
            errors: s.errors,
            versionChoices: this.versionChoices,
            ...(this.checks === undefined ? {} : { checks: this.checks }),
            moreOpen: this.moreOpen,
            ...(remote === undefined ? {} : { remoteProblem: remote }),
            ...(this.passwordProblem === undefined ? {} : { passwordProblem: this.passwordProblem }),
          }),
        };
      }
      case "connect": {
        if (this.added !== undefined) return { key: "added", parts: addedScreen(this.added) };
        const values = s.connectInputs;
        return {
          key: "connect",
          parts: connectScreen({
            values,
            errors: s.connectErrors,
            failure: s.connectFailure,
            probe: s.connectProbe,
            derivation: derivationLine(values.domain),
            composedEndpoint: composeEndpointFromDomain(values.domain),
            moreOpen: this.connectMoreOpen,
          }),
        };
      }
      case "uninstallPreview":
        return this.uninstallScreenFor();
      case "running":
      case "failedStep":
      case "done":
        return this.runOrDoneScreen();
    }
  }

  /** The done screen's view model, when the page is on it. */
  private doneInput(): DoneInput | undefined {
    const s = this.state;
    const handoff = s.handoff;
    if (s.screen !== "done" || handoff === undefined || this.runError !== "" || s.cancelled) return undefined;
    const cluster = handoff.ok ? handoff.cluster : undefined;
    return {
      kind: this.doneKind,
      name: cluster?.name ?? "",
      address: cluster?.endpoint ?? (handoff.ok ? "" : handoff.reachableAt),
      osUrl: cluster === undefined ? "" : composeConsoleUrl(cluster),
      signedIn: cluster !== undefined && this.signedIn(cluster.name),
      canEnrol: s.canEnrol,
      claim: s.primaryHandoffAction === "claim",
      recoveryKey: {
        state: s.recoveryKeyState,
        value: s.revealedRecoveryKey,
        revealed: s.recoveryKeyRevealed,
        copied: s.recoveryKeyCopied,
      },
      ...(handoff.ok ? {} : { notListed: true }),
      ...(this.doneKind === "added" ? {} : { stepText: `${s.steps.length} steps` }),
      ...(this.runStartedAt === undefined ? {} : { startedAt: this.runStartedAt }),
      ...(this.runEndedAt === undefined ? {} : { endedAt: this.runEndedAt }),
      logsOpen: s.logsOpen,
    };
  }

  /** The finished uninstall's view model, when the page is on it. */
  private uninstalledInput(): UninstalledInput | undefined {
    const u = this.uninstall;
    if (this.state.screen !== "uninstallPreview" || u.phase !== "removed") return undefined;
    const steps = u.steps;
    return {
      removed: steps.filter((step) => step.state === "done").length,
      kept: steps.filter((step) => step.state === "preserved").length,
      followUpProblem: u.followUpProblem,
      ...(this.runStartedAt === undefined ? {} : { startedAt: this.runStartedAt }),
      ...(this.runEndedAt === undefined ? {} : { endedAt: this.runEndedAt }),
      logsOpen: u.logsOpen,
    };
  }

  private runOrDoneScreen(): { key: string; parts: RegionParts } {
    const done = this.doneInput();
    if (done !== undefined) {
      // A reconnect never ran anything, so its done screen is a document of
      // its own; after a run it is the run's own screen, settling.
      return { key: this.doneKind === "added" ? "done" : "run", parts: doneScreen(done) };
    }
    const input = this.runInput();
    return { key: "run", parts: runScreen(input ?? emptyRun()) };
  }

  private uninstallScreenFor(): { key: string; parts: RegionParts } {
    const u = this.uninstall;
    if (u.phase === "preview") {
      const preview = this.uninstallPreview;
      return {
        key: "uninstall",
        parts: uninstallPreviewScreen({
          loading: this.uninstallLoading || (preview === undefined && this.uninstallNothing === undefined),
          ...(this.uninstallNothing === undefined ? {} : { nothingHere: this.uninstallNothing }),
          rows: preview === undefined ? [] : removalRows(preview),
          sharedTools: preview === undefined ? [] : sharedToolRows(preview),
          chosen: this.removeShared,
          ...(this.keptClusterOffered() ? { deleteData: { on: this.deleteData, phrase: this.deleteDataPhrase } } : {}),
          clusterName: LOCAL_CLUSTER_NAME,
        }),
      };
    }
    const uninstalled = this.uninstalledInput();
    if (uninstalled !== undefined) return { key: "uninstall-run", parts: uninstalledScreen(uninstalled) };
    return { key: "uninstall-run", parts: runScreen(this.runInput() ?? emptyRun("uninstall")) };
  }

  /** The run screen's view model, for whichever run the page is showing; undefined off a run. */
  private runInput(): RunInput | undefined {
    if (this.state.screen === "uninstallPreview") {
      const u = this.uninstall;
      if (u.phase === "preview" || u.phase === "removed") return undefined;
      const steps = u.steps;
      const failed = u.failure;
      let phase: RunPhase;
      if (u.phase === "running") phase = u.stopping ? "stopping" : failed !== undefined ? "finishing" : "running";
      else if (u.phase === "stopped") phase = "stopped";
      else phase = this.uninstalling ? "finishing" : "failed";
      const failures: FailureView[] =
        failed !== undefined
          ? [failureView(failed, false)]
          : u.phase === "failed" && u.problem !== ""
            ? [{ id: "", line: "The uninstall couldn't start.", next: sentence(u.problem) }]
            : [];
      return this.runInputFrom("uninstall", phase, steps, u.progress(), failures, this.uninstallRetryable());
    }
    const s = this.state;
    if (s.screen !== "running" && s.screen !== "failedStep" && s.screen !== "done") return undefined;
    const mode = s.action === "repair" ? "repair" : "install";
    let phase: RunPhase;
    if (s.screen === "running") phase = s.stopping ? "stopping" : "running";
    else if (s.screen === "failedStep") phase = this.runInFlight ? "finishing" : "failed";
    else if (this.runError !== "") phase = "failed";
    else if (s.cancelled) phase = "stopped";
    // Finished, and the hand-off (the list entry, the selection) still going.
    else phase = "settling";
    const failures: FailureView[] =
      this.runError !== ""
        ? [{ id: "", line: `The ${mode} couldn't start.`, next: sentence(this.runError) }]
        : s.failures.map((f, _i, all) => failureView(f, all.length > 1));
    const retryable = this.runError === "" && this.installRetryable();
    return this.runInputFrom(mode, phase, s.steps, s.progress(), failures, retryable);
  }

  private runInputFrom(
    mode: RunInput["mode"],
    phase: RunPhase,
    steps: readonly StepProgress[],
    progress: { percent: number; status: string; stepText: string },
    failures: FailureView[],
    retryable: boolean,
  ): RunInput {
    const firstFailed = steps.find((step) => step.state === "failed");
    let status: string;
    switch (phase) {
      case "stopping":
        status = "Stopping after the current step";
        break;
      case "stopped":
        status = "Stopped";
        break;
      case "failed":
      case "finishing":
        status = firstFailed !== undefined ? failedLabel(firstFailed.label || firstFailed.id) : "Couldn't start";
        break;
      case "running":
        status = steps.length === 0 ? "Starting" : progress.status;
        break;
      case "settling":
        status = "Finishing";
        break;
    }
    // No plan yet: indeterminate while it is starting, an empty bar once it
    // could not start at all.
    const percent = steps.length > 0 ? progress.percent : phase === "running" ? undefined : 0;
    return {
      mode,
      phase,
      ...(percent === undefined ? {} : { percent }),
      status,
      stepText: progress.stepText,
      ...(this.runStartedAt === undefined ? {} : { startedAt: this.runStartedAt }),
      ...(this.runEndedAt === undefined ||
      phase === "running" ||
      phase === "stopping" ||
      phase === "finishing" ||
      phase === "settling"
        ? {}
        : { endedAt: this.runEndedAt }),
      failures,
      retryable,
      logsOpen: mode === "uninstall" ? this.uninstall.logsOpen : this.state.logsOpen,
    };
  }

  // ---------------------------------------------------------------------------
  // teardown
  // ---------------------------------------------------------------------------

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.stopTicker();
    this.clearFlushTimer();
    // ABORT EVERY RUN IN FLIGHT, FIRST (memql#4614). Closing the tab during an
    // install used to leave the graph running headless: a real cluster the
    // editor did not know about, and a recovery key generated and thrown
    // away. Aborting is safe at any point for the reason Cancel gives: the
    // executor stops at the next wave boundary and the receipt has been
    // written after every step that ran, so what was built can be uninstalled.
    const runInFlight = this.runAbort !== undefined || this.uninstallAbort !== undefined;
    this.runAbort?.abort();
    this.uninstallAbort?.abort();
    // THE PASSWORD GOES WITH THE PANEL -- but not out from under steps still
    // running (memql#4614): releasing the askpass socket mid-wave failed every
    // remaining privileged step. A run in flight releases its own agent when
    // the executor has come to rest.
    if (!runInFlight) void this.releaseSudoAgent();
    // AND SAY SO IF THE ONE-TIME RECOVERY KEY WENT WITH IT (memql#4615). Not a
    // confirmation: a webview has no cancellable close, so this can only say
    // what was lost and where to get another.
    if (this.state.recoveryKeyWouldBeLost) {
      void vscode.window.showWarningMessage(
        "MemQL: the recovery key wasn't saved, and it can't be shown again. Rotate it in MemQL OS, under Users.",
      );
    }
    if (AddClusterPanel.open_ === this) AddClusterPanel.open_ = undefined;
    for (const d of this.disposables.splice(0)) {
      try {
        d.dispose();
      } catch {
        // A disposable that is already gone needs no disposing.
      }
    }
    this.panel.dispose();
  }
}

/** The claim failure's fallback action, for a host that cannot open a browser. */
const COPY_LINK = "Copy link";

/** The flow an act belongs to, which names the tab. */
function flowOf(action: AddClusterAction): PanelFlow {
  switch (action) {
    case "install":
    case "installGuided":
      return "install";
    case "repair":
      return "repair";
    case "uninstall":
      return "uninstall";
    default:
      return "add";
  }
}

/** Which act Escape posts on a screen: leaving only where leaving loses nothing. */
function escapeActFor(screen: string): { escapeAct?: string } {
  if (screen === "connect") return { escapeAct: "connectEscape" };
  if (screen === "uninstall") return { escapeAct: "uninstallBack" };
  return {};
}

function isStepBoundary(event: ExecEvent): boolean {
  return event.type === "runStarted" || event.type === "stepStarted" || event.type === "stepFinished";
}

/** A script's own words as a sentence: a capital first, a full stop last. */
function sentence(text: string): string {
  const trimmed = text.trim();
  if (trimmed === "") return "";
  const capital = trimmed[0]!.toUpperCase() + trimmed.slice(1);
  return /[.!?]$/.test(capital) ? capital : `${capital}.`;
}

function guidanceFor(step: StepProgress) {
  return failureGuidance(step.exitCode, step.remedy, step.message || step.reason, {
    timeoutSeconds: step.timeoutSeconds ?? DEFAULT_STEP_TIMEOUT_SECONDS,
  });
}

/**
 * One failed step, as its notice says it: the script's own words when it gave
 * any, otherwise the plain-words guidance for how it failed, and the fix.
 */
function failureView(step: StepProgress, named: boolean): FailureView {
  const guidance = guidanceFor(step);
  const own = sentence(step.message ?? "");
  // The platform refusal's own words are detect.sh's; the guidance says them
  // better, with the supported list quoted from them.
  const platform = isUnsupportedPlatformRefuse(step.exitCode, step.message || step.reason);
  let line = platform || own === "" ? guidance.headline : own;
  if (named) line = `${failedLabel(step.label || step.id)}: ${lowerFirst(line)}`;
  const next = guidance.advice;
  return {
    id: step.id,
    line,
    next,
    ...(step.remedy === "" ? {} : { remedy: step.remedy }),
  };
}

function lowerFirst(text: string): string {
  return text === "" ? text : text[0]!.toLowerCase() + text.slice(1);
}

function emptyRun(mode: RunInput["mode"] = "install"): RunInput {
  return { mode, phase: "running", status: "Starting", stepText: "", failures: [], retryable: false, logsOpen: false };
}

// A CSP nonce is a security control, so it comes from a CSPRNG.
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
