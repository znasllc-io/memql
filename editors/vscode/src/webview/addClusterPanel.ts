// The "Add a cluster" page.
//
// WHAT IT REPLACES. The "+" used to open a quick pick: a list of three
// sentences, no room to say what the machine actually is, and every local
// branch terminating in a message that named a shell command for the operator
// to copy. A palette entry is the wrong shape for a decision that depends on
// the state of the machine and is followed by ten minutes of work.
//
// WHAT DECIDES WHAT. The verdict comes from ClusterPresence and the CARDS come
// from addClusterMenu -- neither is restated here. That function already
// carries the rule that matters (install is offered for `absent` and for
// nothing else, uninstall for exactly its complement) and it is tested; a
// second copy in a webview would be a second place for it to be wrong. This
// file turns choices into cards and clicks into state transitions.
//
// THE PROGRESS REGION IS A PLAIN HTML SLOT. #3474 renders step progress and
// #3476 renders the uninstall preview; both hand this panel a string. That is
// deliberate -- view-kit's renderChecklist cannot carry the six step states a
// run needs (its `done` slot is boolean), so the element those issues use is
// theirs to choose, and this shell must not presume it.
//
// Refs: #3475 #3472 #3470 #3471 #3469 #3463

import { randomBytes } from "node:crypto";
import { constants as fsConstants } from "node:fs";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import * as vscode from "vscode";

import {
  escapeHtml,
  renderInstallSteps,
  renderRemovalPreview,
  renderToHtml,
  viewKitStyles,
} from "@znasllc-io/memql-view-kit";

import { addCluster, readClustersFileSafe } from "../clusters/file.js";
import {
  addClusterMenu,
  type AddClusterAction,
  type AddClusterChoice,
  type ClusterPresence,
  type PresenceVerdict,
} from "../clusters/presence.js";
import { upsertCluster } from "../clusters/file.js";
import { completeLocalUninstall } from "../clusters/registry.js";
import { completeInstallHandoff } from "../install/handoff.js";
import { offersReconnect, planLocalReconnect } from "../clusters/reconnect.js";
import { ClaimError, claimUrlFrom, openClaimLink } from "../install/claim.js";
import { recoveryKeyStateFrom, revealedRecoveryKeyFrom } from "../install/recoveryKey.js";
import { maskHomePath, redactForDisplay } from "../install/secrets.js";
import { brandStrip, brandStyleBlock } from "./brandTokens.js";
import { renderRunLogPane } from "./runLogPane.js";
import { LOG_PANE_SCRIPT, renderScreen } from "./screenLayout.js";
import { currentBodyThemeAttr, onAppearanceChange } from "./theme.js";
import { recordDiagnostic, type DiagnosticSink } from "../state/diagnostics.js";
import { preflightItems, type PreflightInputs, type PreflightItem } from "../state/preflight.js";
import { ownerAccountExistsFrom } from "../install/enrolment.js";
import {
  deleteReceipt,
  readReceipt,
  recordedDomain,
  recordedOwner,
  recordedProvider,
  recordedProviderKeyFile,
  recordedCheckout,
  recordedImageSource,
  recordedStackDir,
  recordedStackTag,
  type Receipt,
} from "../install/receipt.js";
import { REDACTED, looksLikeProviderKey } from "../install/secrets.js";
import {
  elevationEnv,
  startSudoAgent,
  sudoAccepts,
  sudoRunsWithoutAsking,
  type SudoAgent,
} from "../install/sudoAgent.js";
import {
  graphDocumentPath,
  installGraphPath,
  loadGraphFile,
  type Graph,
  type GraphKind,
} from "../install/graph.js";
import { removalPreviewItems } from "../install/removalPreview.js";
import type { RunScript } from "../install/runner.js";
import { platformRefuseEvents, refuseUnsupportedPlatform } from "../install/platform.js";
import {
  installSessionOptions,
  previewUninstall,
  runInstall,
  runUninstall,
  type SessionHooks,
  type SessionOptions,
  type UninstallPreview,
} from "../install/session.js";
import {
  AddClusterState,
  DERIVATION_PLACEHOLDER,
  SUPPORTED_PROVIDERS,
  derivationLine,
  type ConnectField,
  type ConnectProbeTargets,
  type ConnectProbeVerdict,
  type InputField,
} from "../state/addCluster.js";
import { composeEndpointFromDomain } from "../connection/endpoint.js";
import { failureGuidance, runIsSettled, toStepViews } from "../state/installProgress.js";
// EVERY LOCAL RUN WRITES A RECORD (memql#3739). The Deployments tree reads the
// run log as one history, so the wizard's install, repair and uninstall have to
// appear in it beside the deployments the instance page drives -- a history
// missing three of its five verbs is not a history.
import { RunRecorder } from "../state/runRecorder.js";
import { LOCAL_INSTANCE_NAME } from "../state/deployments.js";
import { defaultRunsDir } from "../state/runLog.js";
// The install-run screens, shared with Deployments (memql#3738). INPUT_FIELDS
// comes back with them because the message handler validates an incoming
// field name against the same list the form rendered from -- two lists would
// be two answers to "which fields exist".
import {
  INPUT_FIELDS,
  renderCollectScreen,
  renderFailedScreen,
  renderRunBlock,
  renderRunningScreen,
} from "./installScreens.js";
import type { ExecutionReport } from "../install/executor.js";
import { listReleaseTags } from "../install/tags.js";
import { DEFAULT_STACK_REPO, isMainBranchChoice } from "../install/stackPin.js";
import { UninstallRunState } from "../state/uninstallRun.js";
import { errorText } from "../auth/errors.js";

/** The ids the webview may send. A real guard, not a cast. */
const CHOICE_ACTIONS: readonly AddClusterAction[] = [
  "install",
  "installGuided",
  "connect",
  "reconnect",
  "repair",
  "uninstall",
];

/**
 * The registration form's fields, and its actions -- each a literal list of
 * its own (memql#3475).
 *
 * SEPARATE LISTS RATHER THAN A WIDER ONE. The postMessage channel is
 * untrusted: anything running in the webview can post any shape at all, so
 * every value that reaches the state machine has to be recognised by
 * comparison against a name written out here. Folding these into
 * INPUT_FIELDS/`data-act` would have been fewer lines and would also have
 * meant one list guarding two unrelated screens -- the point at which nobody
 * can say what widening it costs.
 */
const CONNECT_FIELDS: readonly ConnectField[] = ["name", "domain", "endpoint", "token"];

const CONNECT_ACTIONS = ["save", "discard"] as const;

/**
 * The uninstall screen's actions (memql#3476) -- a THIRD literal list, for the
 * reason CONNECT_FIELDS gives for being a second one.
 *
 * It matters more here than anywhere else on this page. Every other action on
 * this channel opens a screen or writes a registry entry; `uninstallStart`
 * deletes a k3d cluster, a block of /etc/hosts and a certificate authority the
 * operator's browsers trust. A value that reached that branch by being indexed
 * into a table, or cast, would be an irreversible operation started by whatever
 * the webview happened to post.
 */
const UNINSTALL_ACTIONS = ["uninstallStart", "uninstallCancel", "uninstallBack"] as const;

type UninstallAction = (typeof UNINSTALL_ACTIONS)[number];

/** The claim failure's fallback action, for a host that cannot open a browser. */
const COPY_CLAIM_LINK = "Copy link";

/** The registration toast's action. Named once so the offer and the check cannot drift. */
const SIGN_IN_ACTION = "Sign In";

const CONNECT_LABELS: Record<ConnectField, string> = {
  name: "Cluster name",
  domain: "Domain",
  endpoint: "gRPC endpoint",
  token: "Access token",
};

const CONNECT_HINTS: Record<ConnectField, string> = {
  name: "How this cluster is stored in clusters.yaml, and what every other MemQL command calls it.",
  domain:
    "The cluster's apex, e.g. example.com. Everything else is derived from it: the endpoint below, the identity service sign-in talks to, and the portal URL.",
  endpoint:
    "Derived from the domain. Change it only for a front door that is not at api.<domain>:443 -- an edit here wins.",
  token:
    'Optional, and rarely needed. The identity-issued JWT from POST <identity>/oauth/token. Leaving it empty and running "MemQL: Sign In" is the ordinary path -- the editor mints its own credential through your browser. A PAT (mql_pat_...) cannot work here.',
};

/**
 * The two questions the form actually asks (memql#4431).
 *
 * NAME AND DOMAIN, and everything else is derived from the second. The screen
 * used to ask four things, of which the endpoint was the DERIVED value -- so it
 * asked the operator for the answer and left the question beside it as optional.
 */
const CONNECT_PRIMARY_FIELDS: readonly ConnectField[] = ["name", "domain"];

/**
 * What Advanced holds: the derivation's override, and the credential nobody
 * ordinarily pastes.
 *
 * A DISCLOSURE, NOT A SECOND SCREEN. Both fields have correct answers already --
 * the endpoint is composed from the domain, and the token's ordinary value is
 * empty because "MemQL: Sign In" mints one. What they need is to be REACHABLE,
 * for the non-standard front door and the operator who genuinely has a token,
 * not to be asked.
 */
const CONNECT_ADVANCED_FIELDS: readonly ConnectField[] = ["endpoint", "token"];

/** The token is the one field on this page that should not render as plain text. */
const CONNECT_SECRET_FIELDS: readonly ConnectField[] = ["token"];

/**
 * How long any ONE step may take before it is killed (memql#3474).
 *
 * `runner.ts` documents its own default plainly: "0 or absent means NO
 * TIMEOUT". So omitting this does not pick a sensible ceiling -- it removes
 * the ceiling, and a step that never returns leaves the wizard reporting
 * `running` forever with Cancel as the only exit. That is exactly the "nothing
 * hangs indefinitely" requirement, and it is unmet by silence.
 *
 * Ten minutes PER STEP, not per run -- as the DEFAULT, which a step's own
 * `timeoutSeconds` in the graph document replaces (memql#4076). One flat
 * number was asked to both clear the genuinely slow step and kill a wedged
 * one fast, and clusterUp outgrew the compromise: on a fresh install it pulls
 * every image a new containerd has never seen and then waits out two inner
 * budgets (ArgoCD 300s + workloads 900s), which is more than ten minutes by
 * construction -- measured, this ceiling SIGKILLed it ~30s short of a cluster
 * that came up healthy, on the very install after memql#4073 fixed the inner
 * wait. So the slow step prices its slowness in the graph, beside the step it
 * belongs to, and this default stays tight so a hang in any OTHER step still
 * surfaces in minutes rather than inheriting the slowest step's patience.
 */
const STEP_TIMEOUT_MS = 600_000;

/**
 * What this page needs from the extension host to register a cluster.
 *
 * Passed in rather than reached for, because the two things a completed
 * registration touches -- the registry file and the tree that renders it --
 * are both owned by activation, and a panel that resolved them itself would be
 * a second opinion about where clusters.yaml lives.
 */
/**
 * The real reachability probe: does this cluster answer, and does its identity
 * service publish a keyset (memql#4432).
 *
 * WHAT IT PROVES AND WHAT IT DOES NOT. A 200 with a JSON body carrying `keys`
 * means the identity service is up AND its TLS chain verified AND the front door
 * routed it -- which is most of what "is this cluster reachable" means. It does
 * NOT prove the operator can sign in, and the form does not claim it does.
 *
 * TLS IS VERIFIED, NOT BYPASSED. Turning verification off would make the probe
 * pass for a cluster whose certificate nobody can trust, which is the one answer
 * worse than no probe at all. The mkcert false negative that would otherwise
 * force that choice is out of scope by construction: `validateConnect` refuses
 * the localhost family (memql#4431), so every domain reaching here is a public
 * one carrying a public chain.
 *
 * THE TIMEOUT IS THE POINT, not a detail. An unroutable host does not refuse a
 * connection, it hangs -- the operator would sit on a spinner with no verdict,
 * which is worse than the failure it is trying to report. Ten seconds matches
 * `TAG_LIST_TIMEOUT_MS`, the other network call this wizard makes.
 */
const CONNECT_PROBE_TIMEOUT_MS = 10_000;

export async function probeClusterOverHttps(
  targets: ConnectProbeTargets,
): Promise<ConnectProbeVerdict> {
  if (targets.jwksUrl === "") {
    return { ok: false, reason: "no identity host could be derived from that domain" };
  }
  const timer = new AbortController();
  const bell = setTimeout(() => timer.abort(), CONNECT_PROBE_TIMEOUT_MS);
  try {
    const res = await fetch(targets.jwksUrl, { signal: timer.signal, redirect: "follow" });
    if (!res.ok) {
      return { ok: false, reason: `${targets.jwksUrl} answered HTTP ${String(res.status)}` };
    }
    const body = (await res.json()) as { keys?: unknown };
    if (!Array.isArray(body.keys)) {
      return {
        ok: false,
        reason: `${targets.jwksUrl} answered, but not with a JWKS -- something else is serving that host`,
      };
    }
    return { ok: true };
  } catch (err) {
    // AbortError is the timeout, and it deserves its own sentence: "aborted"
    // tells an operator nothing about their cluster.
    if (err instanceof Error && err.name === "AbortError") {
      return {
        ok: false,
        reason: `no answer within ${String(CONNECT_PROBE_TIMEOUT_MS / 1000)}s (the host may not resolve, or a firewall may be dropping it)`,
      };
    }
    // The shared renderer, not err.message (memql#4619): a wrong hostname, a
    // firewall, an expired certificate and a self-signed CA all arrive here as
    // the bare string "fetch failed", and they are four different problems with
    // four different fixes.
    return { ok: false, reason: errorText(err) };
  } finally {
    clearTimeout(bell);
  }
}

/**
 * What a modal has to say before something irreversible happens.
 *
 * THREE STRINGS RATHER THAN ONE, because a VS Code modal has three slots and
 * collapsing them changes what an operator reads: `message` is the bold
 * headline, `detail` the smaller paragraph under it, and `proceed` the label on
 * the button that goes through with it. A prompt whose button says "OK" is a
 * prompt nobody has read -- the label is where the consequence gets stated a
 * second time, in the operator's own hand.
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
   * The MemQL Install output channel (memql#4194). Every line of capability
   * stderr is recorded here, redacted, as the run emits it -- the panel's step
   * list keeps only the short what-failed line, so this is where the whole
   * story lives.
   */
  diagnostics: DiagnosticSink;
  /** Repaints the Clusters view once an entry lands. */
  refreshTree: () => void;
  /**
   * Injected by tests; the real Node https probe when absent (memql#4432).
   *
   * The seam exists so the registration form's pass / fail / timeout behaviour
   * is testable without a network -- the same reason `sudoIsFree` and the graph
   * are injectable here.
   */
  probeCluster?: (targets: ConnectProbeTargets) => Promise<ConnectProbeVerdict>;
  /**
   * Where the graph documents and capability scripts are, from
   * `installRootFor` (memql#3487).
   *
   * Resolved by activation rather than here because the answer depends on
   * whether this extension was PACKAGED -- a .vsix carries a staged copy of
   * `scripts/`, a checkout has the real one two levels up -- and
   * `context.extensionPath` is the only input to that question.
   *
   * The install run (#3474) and the uninstall run (#3476) pass the same value
   * as `SessionOptions.root`; one root, resolved once.
   */
  installRoot: string;
  /**
   * ~/.memql/install-receipt.json, resolved once at activation.
   *
   * ONE VALUE, THREE READERS. The install writes it, the uninstall preview
   * reads it, and a repair reads the provider key path back out of it
   * (memql#3512). Each used to call `defaultReceiptPath()` for itself, which is
   * three independent answers to "where is the record of this install" -- and
   * the run that writes the receipt and the run that reverses it disagreeing
   * about that is the one way an uninstall can silently take nothing back.
   * Resolved beside `installRoot` for the same reason: it is activation's
   * answer, not the page's.
   */
  receiptFile: string;
  /**
   * Injected by tests; the real spawn-based runner when absent.
   *
   * The same seam `SessionHooks.run` and `ExecuteOptions.run` already carry,
   * threaded one layer further out so a case can drive this panel over the REAL
   * graph document, the real plan, the real executor and the real receipt with
   * only script EXECUTION faked (memql#3514). That is what makes the assertions
   * worth anything: the wave-2 provider-key gate, the params a step is handed
   * and the timeout it is given are all properties of the layers underneath,
   * and a fake that replaced any of them would be the test talking to itself.
   */
  runScript?: RunScript;
  /**
   * Injected by tests; `sudoRunsWithoutAsking` when absent.
   *
   * A SAFETY RAIL AS MUCH AS A SEAM. Without it a case that opens this panel
   * spawns the real sudo: on a desktop that risks a password dialog in front of
   * whoever ran the suite, and on a NOPASSWD CI runner it answers "free" and
   * quietly asserts nothing about the path that matters. It is also the only way
   * to drive both machines -- one that needs a password and one that does not --
   * from a single run (memql#3586).
   */
  sudoIsFree?: () => Promise<boolean>;
  /**
   * Injected by tests; `sudoAccepts` when absent. The other half of the rail
   * above -- validating a collected password means running `sudo -A -v`, and
   * `sudo -k` before it, which a test suite must not do to the machine it runs
   * on.
   */
  sudoAccepts?: (askpassPath: string) => Promise<boolean>;
  /**
   * Drops a cluster's registry entry, its stored credential and its live
   * connection, exactly as the "Remove from list" command does.
   *
   * Injected rather than called directly: the whole operation needs
   * SecretStorage and the ConnectionManager, and a panel that reached for
   * either would be a second opinion about state activation owns.
   */
  removeRegistryEntry: (name: string) => Promise<unknown>;
  /**
   * Where the run log lives. Optional, defaulting to ~/.memql/runs, so a test
   * can point a run at a temp directory without a real HOME.
   */
  runsDir?: string;
  /**
   * Asks the operator to confirm something irreversible; a modal
   * `window.showWarningMessage` when absent (memql#4615).
   *
   * INJECTED RATHER THAN CALLED INLINE, and the distinction is not stylistic.
   * This file already reaches for `window.showErrorMessage` and
   * `showInformationMessage` directly, because it is on the `vscode` allow-list
   * (cmd/memql-lsp/vscodeimportrule_test.go) and because nothing depends on
   * what those two ANSWER -- they are notifications. This one's answer decides
   * whether a break-glass credential survives, which makes it the same shape as
   * `sudoIsFree` and `sudoAccepts`: a host effect whose reply drives a branch,
   * and therefore a branch a test has to be able to drive both ways.
   */
  confirmDestructive?: (prompt: DestructiveConfirmation) => Promise<boolean>;
}

export class AddClusterPanel {
  private static open_: AddClusterPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly state = new AddClusterState();
  private readonly disposables: vscode.Disposable[] = [];
  private verdict: PresenceVerdict = "installed-unreachable";
  /**
   * Whether a `local: true` entry is already in the registry.
   *
   * Defaults TRUE, which withholds the reconnect card until the presence read
   * says otherwise. The safe direction: offering to compose an entry for a
   * cluster that already has one would quietly rewrite a row the operator may
   * have edited by hand.
   */
  private localRegistered = true;
  private disposed = false;
  private saving = false;
  /** Non-undefined exactly while a run is in flight; also the cancel handle. */
  private runAbort: AbortController | undefined;
  /** Why the run could not be attempted at all. Not a step failure. */
  private runError = "";
  /**
   * The receipt read once the run settles, so `doneHtml` needs no I/O of its
   * own (memql#4246). Read at the SAME moment `runError`, the hand-off and
   * the recovery-key facts are captured -- see `startRun` and
   * `reconnectLocal`, its two writers -- for the reason the recovery-key
   * comment nearby gives: `report` and a fresh read are both local to those
   * methods, and the done screen re-renders on its own (a click on Reveal,
   * for one) with no run in flight to read either from again.
   */
  private doneReceipt: Receipt | null = null;
  /** The "Before it runs" checklist for the collect screen (memql#4195). */
  private preflight: PreflightItem[] | undefined;

  /** The removal's own state (memql#3476). See state/uninstallRun.ts. */
  private readonly uninstall = new UninstallRunState();
  /** What an uninstall would do, from previewUninstall. Undefined until read. */
  private uninstallPreview: UninstallPreview | undefined;
  /**
   * Shared removals the operator has TICKED. Empty is the default and the safe
   * answer: everything not in here is skipped (memql#3566).
   */
  private readonly removeShared = new Set<string>();
  /** Answers sudo for every step of the run in flight. See sudoAgent.ts. */
  private sudoAgent: SudoAgent | undefined;
  /** Why the preview could not be produced -- most often: no receipt. */
  private uninstallProblem = "";
  private uninstallAbort: AbortController | undefined;
  private uninstalling = false;
  /**
   * The registry name of the local cluster, read BEFORE the removal runs.
   *
   * The entry is found by its `local: true` flag, and the uninstall is about to
   * make every other thing that flag refers to untrue. Reading the name while
   * the operator is still looking at what will go keeps the follow-up aimed at
   * the cluster they consented to remove.
   */
  private localClusterName: string | undefined;

  /**
   * Opens the page, or reveals the one already open.
   *
   * ONE PANEL. A second "Add a cluster" tab would be a second wizard over the
   * same machine, and two runs against one k3d cluster is not a state anything
   * downstream is prepared for.
   *
   * `initialAction` opens the page ON a branch instead of on the cards, for the
   * two affordances that name the branch themselves: the tree's "Uninstall
   * local cluster..." entry and the cluster panel's "Repair local cluster"
   * control (memql#3476). Making them route through the cards would ask the
   * operator to choose again something they have already chosen.
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

  /**
   * Puts the page on a named branch, as if the operator had clicked its card.
   *
   * Goes through `chooseAction` rather than setting a screen, so a branch
   * opened from a command is the same state as one opened from the cards --
   * there is no second route into a screen for the state machine to disagree
   * about.
   */
  private openOn(action: AddClusterAction | undefined): void {
    if (action === undefined) return;
    // A PAGE MID-RUN IS REVEALED, NEVER RE-ROUTED. The command may arrive while
    // this panel is executing a graph, and switching the screen out from under
    // a run would leave the operator with no view of work that is still
    // happening -- while the events it emits kept folding into a machine
    // nothing is drawing.
    if (this.uninstalling || this.state.screen === "running") return;
    this.state.chooseAction(action);
    if (action === "uninstall") void this.loadUninstallPreview();
    if (action === "repair") void this.prefillFromReceipt();
    // Nothing waits on it (memql#3882): the field already carries the pinned
    // default, so the form is answerable the instant it renders and the list
    // only widens what can be chosen.
    if (action === "install" || action === "installGuided") void this.loadVersionChoices();
    if (action === "install" || action === "installGuided" || action === "repair") {
      void this.computePreflight(action);
    }
    this.render();
  }

  private constructor(
    context: vscode.ExtensionContext,
    private readonly presence: ClusterPresence,
    private readonly deps: AddClusterDeps,
  ) {
    this.panel = vscode.window.createWebviewPanel(
      "memqlAddCluster",
      "Add a MemQL cluster",
      // Beside, not Active: the operator asked for this from a tree in the side
      // bar and is very likely reading something else. Taking over their editor
      // to ask five questions is not the same as opening beside it.
      vscode.ViewColumn.Beside,
      { enableScripts: true },
    );

    this.disposables.push(
      // The palette is a MemQL setting now, not the editor's theme, so an
      // OPEN panel repaints when either input moves (memql#4419).
      ...onAppearanceChange(() => this.render()),
      this.panel.webview.onDidReceiveMessage((msg: unknown) => this.onMessage(msg)),
    );
    this.panel.onDidDispose(() => this.dispose(), null, this.disposables);
    context.subscriptions.push(new vscode.Disposable(() => this.dispose()));

    this.render();
    void this.refreshVerdict();
  }

  /**
   * Asks the machine what it is, and repaints.
   *
   * A FAILED OR SLOW PROBE STILL OPENS THE PAGE. detectPresence answers rather
   * than rejects and enforces its own deadline, and this catch is the belt to
   * that braces. The direction it fails in is the one that cannot destroy
   * anything: `installed-unreachable` offers repair, uninstall and connect, and
   * never an install over a cluster that may already be there.
   */
  private async refreshVerdict(): Promise<void> {
    try {
      const presence = await this.presence.get();
      this.verdict = presence.verdict;
      // `clusterName` is set exactly when a `local: true` entry was found, so
      // it IS the registration fact -- read from the same pass rather than
      // from a second read of clusters.yaml that could disagree with it.
      this.localRegistered = presence.clusterName !== undefined;
    } catch {
      this.verdict = "installed-unreachable";
      this.localRegistered = true;
    }
    this.render();
  }

  // ---------------------------------------------------------------------------
  // messages
  // ---------------------------------------------------------------------------

  private onMessage(msg: unknown): void {
    if (msg === null || typeof msg !== "object") return;
    const { type, value, fields } = msg as {
      type?: unknown;
      value?: unknown;
      fields?: unknown;
    };

    if (type === "choose" && typeof value === "string") {
      const action = CHOICE_ACTIONS.find((known) => known === value);
      if (action === undefined) return;
      // RECONNECT IS GATED ON THE VERDICT, not merely on the id. The
      // postMessage channel is untrusted, and this action WRITES a registry
      // entry pointing at a front door -- on a machine with no cluster that is
      // a row for an address nothing serves, and on one already registered it
      // would quietly rewrite an entry the operator may have edited by hand.
      // The other cards ask for confirmation or collect fields before they
      // change anything; this one does not, so the check is here.
      if (action === "reconnect" && !offersReconnect(this.verdict, this.localRegistered)) return;
      this.state.chooseAction(action);
      // The duplicate-name check needs a registry to check against, and this
      // is the moment it becomes worth reading one. Nothing waits on it: the
      // read is fast, the operator has four fields to fill first, and a
      // registry that never arrives costs the inline refusal but not the
      // write-time one.
      if (action === "connect") void this.loadRegistry();
      if (action === "install" || action === "installGuided") void this.gateCreateDeployment();
      // A repair now COLLECTS the key fields (memql#3544), so it opens with the
      // recorded answers already in the boxes -- nobody retypes a good path,
      // and a bad one is finally reachable.
      if (action === "repair") void this.prefillFromReceipt();
      if (action === "install" || action === "installGuided" || action === "repair") {
        void this.computePreflight(action);
      }
      // The itemized list is the confirmation, so it is read the moment the
      // branch opens rather than behind a further click: there is nothing on
      // this screen for the operator to do until it is on screen.
      if (action === "uninstall") void this.loadUninstallPreview();
      // NOTHING TO ASK. The domain comes off the receipt (or the installer's
      // default), the entry is composed from it, and the hand-off screen the
      // action lands on is the same one a finished install reaches -- so the
      // operator's next click is "Sign in as owner" either way (memql#3741).
      if (action === "reconnect") void this.reconnectLocal();
      this.render();
      return;
    }
    // The registration form's own channel (memql#3475), recognised against
    // CONNECT_ACTIONS -- see that list for why it is a second one rather than
    // a wider first.
    if (typeof type === "string") {
      const connectAction = CONNECT_ACTIONS.find((known) => known === type);
      if (connectAction !== undefined) {
        // Absorb the whole form BEFORE acting on the action. Every message
        // from this screen carries every field, because render() replaces the
        // webview's HTML wholesale and the DOM is therefore not where form
        // state lives; taking the values first is what stops a click on Save
        // from acting on a form one keystroke out of date.
        this.absorbConnectFields(fields);
        if (connectAction === "discard") {
          this.state.discardConnect();
          this.render();
          return;
        }
        void this.saveConnect();
        return;
      }
    }
    if (type === "back") {
      void this.leaveScreen();
      return;
    }
    // THE LOG DISCLOSURE (memql#4455). A repaint is exactly what this one
    // wants -- the open/closed flag is what the next render reads -- which is
    // what makes it different from the keystroke and scroll messages below,
    // and the reason it is a message at all rather than a `<details>`.
    if (type === "toggleLogs") {
      // WHICH RUN'S LOG. This panel drives two: an install/repair, whose steps
      // live on `state`, and an uninstall, whose steps live on `uninstall`.
      // They are separate runs with separate step lists, so they hold the
      // disclosure separately -- and routing on the SCREEN is what keeps the
      // toggle acting on the pane the operator is actually looking at.
      this.runStateForScreen().toggleLogs();
      this.render();
      return;
    }
    // RECORDED, NEVER REPAINTED. Answering a scroll with a render would
    // replace the document under the operator's scrollbar, which is the
    // failure this whole state-held arrangement exists to avoid -- inverted.
    if (type === "logsFollow" && typeof value === "boolean") {
      this.runStateForScreen().setLogsFollow(value);
      return;
    }
    // RECORDED, NEVER REPAINTED (memql#3538).
    //
    // This message arrives once per KEYSTROKE, and `render()` assigns
    // `webview.html` -- which replaces the entire document. There is no focused
    // element, no caret and no selection on the other side of that, so
    // answering a keystroke with a render meant every character typed anywhere
    // on this page threw the operator out of the box they were in. The form
    // could be filled only one click-and-character at a time.
    //
    // The message itself STAYS. The DOM is discarded on every repaint, so the
    // extension is where form state lives; recording the value is what lets a
    // later render -- an action, a verdict arriving late -- redraw the form with
    // everything typed since the last one still in it.
    //
    // What is lost is per-keystroke validation feedback: `setInput` recomputes
    // this field's error, and nothing draws it until the next render. That is
    // the same trade the registration form states plainly in `connectHtml`, and
    // for the same reason -- on a surface whose only repaint is a full document
    // reload, validating as someone types means reloading the page under their
    // cursor. Every problem is still reported, together, when Start is pressed.
    if (type === "input" && typeof value === "object" && value !== null) {
      const { field, text } = value as { field?: unknown; text?: unknown };
      const known = INPUT_FIELDS.find((f) => f === field);
      if (known === undefined || typeof text !== "string") return;
      this.state.setInput(known, text);
      return;
    }
    // A TICK ON THE UNINSTALL SCREEN (memql#3566). Recorded and not repainted,
    // like every other field on this surface -- a repaint replaces the whole
    // document and would drop the other ticks with it.
    //
    // The message names a STEP ID and the extension checks it against the steps
    // the preview actually planned. The webview is an untrusted channel; a
    // message naming a step that is not shared, or not in this preview at all,
    // could otherwise opt into a removal the operator was never shown.
    if (type === "shared" && typeof value === "object" && value !== null) {
      const { step, remove } = value as { step?: unknown; remove?: unknown };
      if (typeof step !== "string" || typeof remove !== "boolean") return;
      const offered = (this.uninstallPreview?.removals ?? []).some((s) => s.id === step && s.shared);
      if (!offered) return;
      if (remove) this.removeShared.add(step);
      else this.removeShared.delete(step);
      return;
    }
    if (type === "begin") {
      // Validation and the transition are this panel's, so an incomplete form
      // is refused here rather than nine minutes into a graph.
      //
      // No toast. The run screen itself says what state the run is in, and a
      // popup that announced a run which has not started would be the same lie
      // in a second place -- one the operator cannot dismiss by looking again.
      void this.begin();
      return;
    }
    // The two recoveries from a failed step (#3474). Both are transitions on
    // the state machine rather than behaviour local to this panel, so the CLI
    // and a future front end recover the same way.
    if (type === "retry") {
      // RE-INVOKE, do not merely repaint. `state.retry()` puts the failed step
      // back to `pending` and returns to the run screen -- but without
      // starting a run, the operator fixes the cause, presses Retry, and
      // watches a screen that never changes again. That is the whole of
      // "recoverable in place" (#3473) and "Retry is offered on every failure"
      // (#3474), and both were being satisfied by a repaint.
      //
      // Re-running the WHOLE graph is correct rather than lazy: every step
      // verifies first and skips when already satisfied, which is the same
      // property that makes repair an install re-run. The steps that passed
      // report `skipped`; only the one that failed does work.
      this.state.retry();
      this.render();
      void this.startRun();
      return;
    }
    if (type === "guided") {
      // Same re-invocation as Retry, for the same reason. The guided flag is
      // per-step and rides on the step's own record, so the run picks it up
      // rather than this needing a second code path.
      this.state.switchToGuided();
      this.render();
      void this.startRun();
      return;
    }
    if (type === "cancel") {
      // Abort FIRST, then transition. The executor stops at the next wave
      // boundary and the receipt has been written after every step that ran,
      // so the cancelled install remains fully uninstallable -- which is the
      // property that makes cancelling safe to offer at any point.
      this.runAbort?.abort();
      this.state.cancel();
      this.render();
      return;
    }
    if (type === "remedy" && typeof value === "string") {
      this.openRemedyTerminal(value);
      return;
    }
    if (type === "browseKeyFile") {
      void this.browseForKeyFile();
      return;
    }
    if (type === "signInAsOwner") {
      void this.signInAsOwner();
    }
    if (type === "enrolPasskey") {
      void this.enrolPasskey();
    }
    if (type === "claimCluster") {
      void this.claimCluster();
    }
    if (type === "openCheckout") {
      void this.openCheckout();
    }
    if (type === "openProviderSettings") {
      void this.openProviderSettings();
    }
    if (type === "copyRecoveryKey") {
      void this.copyRecoveryKey();
    }
    if (type === "revealRecoveryKey") {
      // THE FLAG LIVES ON THE STATE MACHINE, not here (memql#4616). It was a
      // panel field, where nothing reset it -- so the screen-share guard
      // memql#4194 added worked exactly once per panel lifetime, and a repair
      // after a Back rendered the NEW plaintext with no click at all. Beside
      // the key it guards, it is let go of by the same lines that let go of the
      // key, which is the only arrangement in which the two cannot drift.
      this.state.revealRecoveryKey();
      this.render();
    }
    // The uninstall screen's own channel (memql#3476), recognised against
    // UNINSTALL_ACTIONS -- see that list for why an irreversible operation in
    // particular is reached only by comparison against a name written out in
    // this file.
    if (typeof type === "string") {
      const uninstallAction = UNINSTALL_ACTIONS.find((known) => known === type);
      if (uninstallAction !== undefined) this.onUninstallAction(uninstallAction);
    }
  }

  /**
   * Opens the editor's own file dialog and puts the chosen path in the box
   * (memql#3547).
   *
   * THE DIALOG BELONGS TO THE HOST. A webview runs in an iframe with no
   * filesystem access at all: it cannot open a native picker, and an
   * `<input type="file">` would give it a File object with no path -- which is
   * not something `--key-file` can be handed. So the page posts, the extension
   * asks, and the answer comes back as a path.
   *
   * IT FILLS THE FIELD, IT DOES NOT START ANYTHING. Choosing a file is not
   * consent to install; the operator still reads the form and presses Start.
   *
   * `defaultUri` opens where the answer probably is: the directory of whatever
   * is already typed, else `~/.memql`, which is where the wizard's own hint
   * tells people to put the key. `canSelectMany: false` because the flag takes
   * one path, and `canSelectFolders: false` because a directory is not a key.
   *
   * No filter. Key files carry every extension and none -- `.txt`, `.key`,
   * bare `key` -- and a filter that guessed would hide the file the operator
   * came here to choose.
   */
  private async browseForKeyFile(): Promise<void> {
    const chosen = await vscode.window.showOpenDialog({
      canSelectMany: false,
      canSelectFiles: true,
      canSelectFolders: false,
      openLabel: "Use this key file",
      title: "Select the file holding your AI provider key",
      defaultUri: vscode.Uri.file(this.keyDialogStartDir()),
    });
    // Cancelled. The form is untouched, which is the whole of what "cancel"
    // should mean here.
    if (chosen === undefined || chosen.length === 0) return;
    if (this.disposed) return;

    const picked = chosen[0]!.fsPath;
    this.state.setInput("providerKeyFile", picked);
    // A path that came out of the picker exists by construction, so any
    // complaint left over from something typed earlier is now stale.
    const problem = await this.keyFileProblem(picked);
    if (problem !== "") this.state.noteFieldProblem("providerKeyFile", problem);
    if (!this.disposed) this.render();
  }

  /** Where the file dialog opens. See browseForKeyFile. */
  private keyDialogStartDir(): string {
    const typed = this.state.inputs.providerKeyFile.trim();
    if (typed !== "" && !looksLikeProviderKey(typed) && path.isAbsolute(typed)) {
      return path.dirname(typed);
    }
    return path.join(os.homedir(), ".memql");
  }

  /**
   * The form's last gate before a run: does the key file actually exist?
   *
   * WHY IT IS NOT IN `problemWith`. `state/addCluster.ts` is deliberately free
   * of `node:fs` -- it is the module the fast unit lane drives -- so its checks
   * are string shape only. Whether there is a readable file at the path is the
   * question that catches everything shape cannot: a typo, a `~` no shell ever
   * expanded, a file moved since the last install.
   *
   * WHY IT MATTERS MORE THAN A TIDY ERROR. Without it, the first thing to
   * notice is `verify-provider-key.sh`, which exits 2 -- and the wizard renders
   * exit 2 as "a fault in MemQL rather than in your machine or your answers".
   * That sentence is false here and points the operator away from the one thing
   * they can fix. Nine minutes of install can precede it.
   *
   * Returns the message, or "" when the path is fine.
   */
  private async keyFileProblem(pathValue: string): Promise<string> {
    const value = pathValue.trim();
    if (value === "") return "";
    if (looksLikeProviderKey(value)) {
      // Belt to the state layer's braces: the same refusal, in case a value
      // reached the inputs by a route that did not run `problemWith`.
      return (
        "That is the key itself. This field takes the PATH to a file holding it -- " +
        "save the key to a file (e.g. ~/.memql/key) and give that path."
      );
    }
    if (value.startsWith("~")) {
      // `~` is expanded by a SHELL, and the runner spawns scripts with
      // shell:false so that a `;` in a param stays inert. Nothing will expand
      // this, and the script would report a missing file naming a literal "~".
      return "A leading ~ is not expanded here. Give the full path, e.g. /home/you/.memql/key.";
    }
    try {
      const stat = await fs.stat(value);
      if (!stat.isFile()) return "That path is a directory. Give the file that holds the key.";
    } catch {
      return "No file exists at that path. Save the key to a file and give the path to it.";
    }
    try {
      await fs.access(value, fsConstants.R_OK);
    } catch {
      return "That file exists but cannot be read. Check its permissions.";
    }
    return "";
  }

  /**
   * Validates, then starts -- with the filesystem check the state machine
   * cannot make itself (memql#3544).
   *
   * Async, which is why it is a method rather than three lines in `onMessage`:
   * the shape checks are synchronous and the file check is not, and the
   * operator must see the result of both under the fields rather than as a
   * failed run.
   */
  private async begin(): Promise<void> {
    // BEFORE `beginRun()`, not after. That call transitions to the run screen,
    // and `back()` -- the only way off it -- returns to the CARDS, dropping the
    // chosen action and every field error with it. Checking first keeps a
    // refusal on the form the operator is already looking at, with the box they
    // need to edit still on screen and still holding what they typed.
    const problem = await this.keyFileProblem(this.state.inputs.providerKeyFile);
    if (problem !== "") {
      this.state.noteFieldProblem("providerKeyFile", problem);
      this.render();
      return;
    }
    if (this.state.beginRun()) void this.startRun();
    this.render();
  }

  /**
   * The hooks every session call gets: the caller's, plus the injected runner.
   *
   * One place, so a run started from Retry cannot end up on a different runner
   * from the one `begin` used.
   */
  private hooks(own: SessionHooks): SessionHooks {
    return { ...own, ...(this.deps.runScript ? { run: this.deps.runScript } : {}) };
  }

  /**
   * Drives `session.ts` and folds every event into the state machine.
   *
   * ONE RUN AT A TIME, guarded by `runAbort` rather than by the screen: the
   * operator can reach `begin` again through retry, and two concurrent graphs
   * against one k3d cluster is not a state anything downstream is prepared for.
   *
   * Repair uses the SAME call. Every step verifies first and skips when
   * satisfied, which is what makes re-running the graph a repair -- there is no
   * second code path here, only different wording on the screen.
   */
  private async startRun(): Promise<void> {
    if (this.runAbort !== undefined) return;
    const action = this.state.action;
    if (action !== "install" && action !== "installGuided" && action !== "repair") return;

    const inputs = this.state.inputs;
    // A NEW RUN OWNS THE SCREEN. Whatever stopped the last one being attempted
    // is about to be either fixed or repeated, and a stale sentence describing
    // the previous attempt is the one thing on this page an operator has no way
    // to tell apart from a current one.
    this.runError = "";

    // A REPAIR READS THE KEY OFF THE RECEIPT WHEN THE FIELD IS EMPTY
    // (memql#3512), which after epic memql#4440 is a PREFILL rather than a
    // rescue.
    //
    // The original reasoning: memql#3473's gate put `providerKey` in front of
    // every mutating step, and `session.ts` drops empty params, so a repair
    // reached wave 2 with no `--key-file` and died there with exit 2 on every
    // invocation. The receipt is the record of what the install did, and
    // `providerKey` writes an entry even though it leaves no artifact, so the
    // path is already on disk.
    //
    // That failure mode is gone: `installPlan` now SKIPS `providerKey`,
    // satisfied, when no key file is supplied, so a keyless repair passes
    // wave 2 rather than dying in it. What the receipt read still buys is the
    // thing it was always also doing -- a repair of a cluster that WAS
    // installed with a key re-verifies that same key against that same
    // vendor, instead of silently dropping provider seeding on the way
    // through.
    let providerKeyFile = inputs.providerKeyFile;
    let provider = inputs.provider;
    // Read ONCE, up here: the recorded tag is needed at the run call below, and
    // a second read could see a different file (memql#3605).
    const priorReceipt = await readReceipt(this.deps.receiptFile);
    if (providerKeyFile === "") {
      const receipt = priorReceipt;
      // `usablePath`, because the receipt can hold a value that is not one: a
      // redaction marker where a key was given instead of a path (memql#3545),
      // or -- on a receipt written before that guard existed -- the key itself.
      // Passing either as `--key-file` produces a confusing failure deep in the
      // run; treating it as "nothing recorded" produces the honest refusal
      // below, which names the one thing the operator can do about it.
      providerKeyFile = usablePath(recordedProviderKeyFile(receipt));
      // The recorded vendor travels with the recorded path, and for the same
      // reason: a repair that read the key file back but re-asserted the
      // wizard's DEFAULT vendor would verify an OpenAI key against Anthropic
      // and report a refusal (exit 3) -- "the key is bad", about a key that is
      // fine.
      provider = recordedProvider(receipt) || provider;
    }
    // NO REFUSAL HERE ANY MORE (epic memql#4440).
    //
    // This is where a repair used to stop dead: with no key path recorded and
    // none typed, the run could not pass wave 2, so the panel refused up front
    // with "Install rather than repair, so the key can be collected and
    // verified." That was the honest sentence for a graph in which every
    // mutating step waited on a vendor call.
    //
    // It is now the wrong sentence, and it would be the WORST one in the
    // product: after this epic the ordinary install supplies no key at all, so
    // "no record of an AI provider key" describes almost every cluster -- and
    // the remedy it names, reinstalling, is destructive advice for a machine
    // whose only problem was that a repair refused to run. A keyless repair is
    // just a repair; `providerKey` skips, satisfied, and every step behind it
    // proceeds.
    //
    // Nothing replaces it, deliberately. There is no degraded state to warn
    // about: a cluster with no provider configured is a working cluster whose
    // agents cannot think yet, and the place that says so is the portal page
    // the done screen links to.

    // Repair does not go through gateCreateDeployment, so it must refuse
    // here -- before sudo -- on an unsupported platform (memql#4294).
    // Install already refused at the tag list.
    if (action === "repair") {
      const refused = await refuseUnsupportedPlatform(this.platformSession(), this.hooks({}));
      if (refused !== undefined) {
        for (const event of platformRefuseEvents(refused)) this.state.apply(event);
        this.render();
        return;
      }
    }

    // ONE PASSWORD FOR THE WHOLE RUN (memql#3568). Collected before the first
    // step, served to every one of them; see collectSudoPassword.
    await this.collectSudoPassword("install");
    if (this.disposed) return;

    const controller = new AbortController();
    this.runAbort = controller;

    const recorder = await RunRecorder.begin({
      dir: this.deps.runsDir ?? defaultRunsDir(),
      instance: LOCAL_INSTANCE_NAME,
      kind: action === "repair" ? "repair" : "install",
      // A repair returns the cluster to the checkout its receipt names, so that
      // is both where it came from and where it is going. An install has
      // neither: nothing was here.
      //
      // The LABEL rather than the tag (memql#3901): a branch install records no
      // tag, and reading one would leave the run record blank for exactly the
      // installs whose version is hardest to know by other means.
      ...(action === "repair" && recordedCheckout(priorReceipt).label !== ""
        ? {
            fromVersion: recordedCheckout(priorReceipt).label,
            toVersion: recordedCheckout(priorReceipt).label,
          }
        : {}),
    });
    this.deps.refreshTree();

    let report: ExecutionReport | undefined;
    let failure: string | undefined;
    try {
      report = await runInstall(
        // BUILT ON THE OTHER SIDE OF THE `vscode` LINE (memql#3560). This was
        // an object literal here, where no unit test can reach it -- and it was
        // missing `tag`, so every install started from this page failed at
        // stackCheckout while the CLI's worked. `installSessionOptions` is
        // audited against what the capability scripts require.
        installSessionOptions({
          root: this.deps.installRoot,
          // The same value the uninstall side reads (#3476), so the run that
          // writes the receipt and the run that reverses it cannot disagree
          // about where it lives.
          receiptFile: this.deps.receiptFile,
          // COLLECTED (memql#3473), and the graph no longer pins it -- which
          // vendor a key belongs to is a fact about the operator's key, run
          // input like the path beside it, not policy the graph decides. On a
          // repair it comes off the receipt, with the key path.
          provider,
          domain: inputs.domain,
          ownerEmail: inputs.ownerEmail,
          ownerFirstName: inputs.ownerFirstName,
          ownerLastName: inputs.ownerLastName,
          // A PATH, never the key. argv is world-readable in `ps`. On a
          // repair this is the path the receipt recorded (memql#3512).
          providerKeyFile,
          // On a REPAIR, the checkout the receipt recorded (memql#3605). Without
          // it the run fell through to DEFAULT_STACK_TAG and a repair from a
          // newer extension silently upgraded the cluster.
          //
          // On a FRESH INSTALL the receipt records no checkout, so the answer is
          // whatever the version field says -- which starts on DEFAULT_STACK_TAG
          // and is an OVERRIDE, not a replacement (memql#3882). The receipt is
          // checked first so a repair can never be steered by a field it does
          // not collect.
          //
          // TWO FIELDS NOW (memql#3901), and `recordedCheckout` decides which
          // one carries the answer. A repair of a BRANCH install must replay the
          // resolved COMMIT: replaying `--branch=main` would check out wherever
          // main is today, which is memql#3605's failure by the one route that
          // reopens it.
          tag: recordedCheckout(priorReceipt).tag || inputs.version,
          commit: recordedCheckout(priorReceipt).commit,
          // AND THE IMAGES THAT CHECKOUT RAN AGAINST (memql#4068). The two lines
          // above replay the recorded CODE; without this one the node images
          // were derived again, from the empty tag a branch install's record
          // deliberately carries, and fell through to DEFAULT_STACK_TAG. So a
          // repair from a newer extension build reconciled the recorded commit's
          // manifests against a different release's engine -- an upgrade the
          // operator did not ask for, and a skew nobody chose.
          //
          // Empty on a fresh install, where there is nothing recorded and
          // `installPlan`'s own derivation from the chosen version is the right
          // answer.
          imageTag: recordedCheckout(priorReceipt).imageTag,
          // AND THE LANE (memql#4430), for the reason the line above exists. A
          // from-source install records no image tag, so replaying only the tag
          // would leave this run deriving the pin for a cluster whose images were
          // built from its own checkout. False on a fresh install, where
          // `installPlan` reads the lane off the chosen version instead.
          imagesFromSource: recordedCheckout(priorReceipt).fromSource,
          timeoutMs: STEP_TIMEOUT_MS,
          env: this.sudoEnv(),
        }),
        this.hooks({
          onEvent: (event) => {
            // The FULL log's home (memql#4194, audit 30): every stderr line,
            // redacted, as the run emits it. The step list keeps only the
            // short what-failed line (state/installProgress.ts).
            if (event.type === "stepLog") {
              this.deps.diagnostics.appendLine(
                `[${event.step.id}] ${redactForDisplay(event.line, os.homedir())}`,
              );
            }
            this.state.apply(event);
            void recorder.apply(event);
            this.render();
          },
          signal: controller.signal,
        }),
      );
    } catch (err) {
      // A THROW IS NOT A FAILED STEP. Everything a step can do wrong arrives as
      // an event and is already on screen; reaching here means the run could
      // not be attempted at all -- a missing graph document, an unreadable
      // script -- and the step list would otherwise sit empty with no account
      // of why.
      const raw = err instanceof Error ? err.message : String(err);
      recordDiagnostic(
        this.deps.diagnostics,
        "the run could not be attempted",
        raw,
        new Date().toISOString(),
      );
      failure = redactForDisplay(raw, os.homedir());
    } finally {
      this.runAbort = undefined;
      // The run is over either way; the password has no further use.
      await this.releaseSudoAgent();
    }

    await recorder.finish(
      controller.signal.aborted
        ? "cancelled"
        : failure !== undefined || report?.ok !== true
          ? "failed"
          : "succeeded",
    );
    this.deps.refreshTree();

    if (this.disposed) return;

    // THE CHECKOUT, read once here (memql#4246). Every branch `doneHtml` can
    // reach from this point on -- the refused start, the failed hand-off,
    // the successful one, and the plain terminal -- reads `this.doneReceipt`
    // synchronously instead of doing its own I/O.
    this.doneReceipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    if (this.disposed) return;

    if (failure !== undefined) {
      this.runError = failure;
      this.state.finish({ ok: false });
      this.render();
      return;
    }

    // `ok` means nothing FAILED, which a cancelled run usually satisfies -- so
    // "did the whole graph run?" needs both fields. Handing off on `ok` alone
    // would have the wizard claim an install the operator deliberately stopped.
    const succeeded = report?.ok === true && report?.cancelled !== true;

    // A FAILED STEP STAYS ON THE FAILED-STEP SCREEN. `finish()` moves to
    // `done`, which reads "Finished / Nothing further to do" -- and calling it
    // after a failure took the operator off the one screen carrying Retry and
    // Switch-to-Guided, then told them the run was over. Between them, that and
    // the inert handlers meant a failed install could not be recovered from at
    // all, while every unit underneath was green.
    //
    // The run is genuinely over either way; what differs is whether there is
    // something left to do about it. `state.failed` is the executor's answer to
    // that, and `retry()` / `switchToGuided()` clear it when the operator acts.
    if (!succeeded && this.state.failed !== undefined) {
      this.render();
      return;
    }

    this.state.finish({ ok: report?.ok === true, cancelled: report?.cancelled === true });

    // IS THERE AN OWNER ACCOUNT TO ENROL AGAINST (memql#3408, memql#3906).
    //
    // This used to lift the run's minted link onto the done screen so the
    // button could replay it. Two failures came out of that. The link is
    // single-use and expires in fifteen minutes, so an operator who stepped
    // away came back to a button that failed in a way indistinguishable from
    // the feature being broken; and a run whose enrolment step produced nothing
    // offered no ownership route at all, which is how a repair -- where
    // `magicLink` correctly reports `linkState=none`, because identity only
    // logs a link that was REQUESTED -- ended with no way in but a terminal.
    //
    // So the screen keeps the durable half of that step's answer and mints the
    // perishable half on click. Read here rather than in `doneHtml` because
    // `report` is local to this method and the screen re-renders many times.
    if (report !== undefined) this.state.setOwnerAccountExists(ownerAccountExistsFrom(report));

    // THE MAGIC LINK, the SECOND of the two routes in (memql#3884, and the
    // model here corrected by memql#4622).
    //
    // WHAT THIS COMMENT USED TO SAY. That a cluster is claimed by its FIRST
    // SIGN-IN, so on every first install there is no account to enrol a passkey
    // for, `enrolmentLink` correctly returns nothing and reports
    // `enrolmentState=awaitingFirstSignIn`, and the magic link is therefore
    // "the ONLY one of the two that exists". That was a fair description of an
    // earlier bootstrap, and it is no longer what this installer builds -- it
    // had this file contradicting `src/clusters/ownershipRoute.ts` and
    // `state/addCluster.ts`'s `primaryHandoffAction`, both of which already
    // carry the current model. Two contradictory models in one repository is
    // how the next regression gets written.
    //
    // WHAT ACTUALLY HAPPENS. `seedBootstrap` writes the owner ROW at identity
    // boot from the values the install seeds, so a cluster this extension
    // installed HAS an owner account before the operator opens a browser --
    // verified from the other side in ownershipRoute.ts, where `/setup` is
    // gated on `Store.HasOwnerUser` and answers 404 on a freshly installed
    // cluster. On a fresh install `enrolmentLink` therefore MINTS
    // (`enrolmentState=minted`, `ownerClaimed=true`), `canEnrol` is true, the
    // done screen leads with "Set up a passkey", and `recoveryKey` claims a key
    // that IS revealed on this screen. `awaitingFirstSignIn` and
    // `awaitingOwner` remain real states -- they describe the HAND-ROLLED
    // cluster, brought up with no bootstrap env, which is precisely the case
    // `primaryHandoffAction` leads with `claim`.
    //
    // SO WHY THE LINK IS STILL WORTH RECOVERING. The owner row exists with no
    // HUMAN credential on it, and the magic link is the one route that signs
    // that account in without one -- the fallback for a run whose enrolment
    // mint produced nothing, where the alternative offer, "Sign in as owner",
    // times out and falls back to a device code that cannot complete either.
    // It is single-use and authenticates as the OWNER, which is why it is
    // passed from the envelope to the opener and never rendered, and why
    // `back()` and `beginRun()` now let go of it (memql#4617): a spent link
    // re-offered on a repair lands the operator on an error page.
    if (report !== undefined) this.state.setClaimUrl(claimUrlFrom(report));

    // THE RECOVERY KEY, the run's third and most perishable product
    // (memql#4079). The claim ROTATED the key and revealed the plaintext into
    // this in-memory report and nowhere else -- the run log and the receipt
    // withhold it (memql#3908), correctly, which is exactly why a display had
    // to be built: with no surface reading the report, default-deny had
    // quietly become "revealed to no one", while the step's own description
    // promised "show it once". DISPLAY, NOT STORAGE: the value goes to the
    // done screen's state and dies with it.
    if (report !== undefined) {
      this.state.setRecoveryKey(revealedRecoveryKeyFrom(report), recoveryKeyStateFrom(report));
    }

    // THE HAND-OFF, and the gate in front of it is the whole point (#3477).
    //
    // `ok` alone is not enough twice over. A CANCELLED run is normally `ok` --
    // every step that ran, worked -- so handing off on `ok` would register the
    // cluster and offer "Sign in as owner" for an install the operator
    // deliberately stopped: worse than doing nothing, because it looks like
    // success. `AddClusterState` treats the two separately for this reason, and
    // a test pins `{ok: true, cancelled: true}` reporting `succeeded === false`.
    //
    // `ok` also does not mean the machine CHANGED: it is
    // `outcomes.every(o => o.status !== "failed")`, so a run where every step
    // verified as already-satisfied is `ok` with nothing done. Handing off
    // anyway is DELIBERATE, not incidental -- that is exactly the repair case,
    // and the write is `upsertCluster`, so registering a cluster that is
    // already registered updates the entry rather than duplicating it.
    if (succeeded) {
      const domain = inputs.domain;
      await this.handOffAfterInstall(domain);
      return;
    }

    this.render();
  }

  // ---------------------------------------------------------------------------
  // registering an existing cluster (memql#3475)
  // ---------------------------------------------------------------------------

  /**
   * Reads the field values out of a message, BY THE NAMES IN CONNECT_FIELDS.
   *
   * The iteration direction is the security property. Walking the list and
   * asking the payload for each name means an extra key on the wire reaches
   * nothing at all -- there is no branch it can take. Walking the payload's own
   * keys instead would make the guard a filter over attacker-chosen input,
   * which is the same shape of mistake as casting it.
   */
  private absorbConnectFields(raw: unknown): void {
    if (raw === null || typeof raw !== "object") return;
    const supplied = raw as Record<string, unknown>;
    for (const field of CONNECT_FIELDS) {
      const text = supplied[field];
      if (typeof text === "string") this.state.setConnectInput(field, text);
    }
  }

  /**
   * Puts the recorded answers into the repair form's boxes (memql#3544).
   *
   * A DEFAULT, NOT A LOCK -- that distinction is the whole fix. memql#3512
   * taught the repair to read the key path off the receipt so wave 2 could
   * pass, which was right for a good path and a trap for a bad one: the run
   * re-used the recorded value, failed at the same step, and offered no field
   * in which to correct it. The value now lands in an editable box.
   *
   * A REDACTED entry is left blank on purpose. It records that a key was given
   * where a path belonged (see install/secrets.ts) -- it is not a path, and
   * pre-filling it would hand the operator the very value that has to change.
   *
   * A value already typed WINS. This is async and the operator is looking at
   * the form while it runs; overwriting what they have just entered with what
   * an old receipt says would be the worst possible moment to be helpful.
   */
  private async prefillFromReceipt(): Promise<void> {
    const receipt = await readReceipt(this.deps.receiptFile);
    if (this.disposed || this.state.action !== "repair") return;

    const recordedPath = usablePath(recordedProviderKeyFile(receipt));
    if (this.state.inputs.providerKeyFile === "" && recordedPath !== "") {
      this.state.setInput("providerKeyFile", recordedPath);
    }
    const recordedVendor = recordedProvider(receipt);
    if (recordedVendor !== "" && SUPPORTED_PROVIDERS.some((p) => p === recordedVendor)) {
      this.state.setInput("provider", recordedVendor);
    }

    // THE DOMAIN AND THE OWNER, which a repair needs and did not have
    // (znasllc-io#3888). `seedBootstrap` takes five values and refuses a partial
    // set; `domain` was collected and `registration-mode` has a default, so the
    // three owner fields were the whole of what was missing -- and the run died
    // at `exit 2` naming values this page gave the operator no way to supply.
    //
    // Each is filled only when the box is EMPTY, so a value the operator has
    // just typed to correct a bad recorded one is never overwritten by the bad
    // one. That is the same rule the provider key path above follows, and it is
    // what makes pre-filling safe on the very run whose purpose is to change an
    // answer.
    const recordedHost = recordedDomain(receipt);
    if (this.state.inputs.domain === "" && recordedHost !== "") {
      this.state.setInput("domain", recordedHost);
    }
    const owner = recordedOwner(receipt);
    if (this.state.inputs.ownerEmail === "" && owner.email !== "") {
      this.state.setInput("ownerEmail", owner.email);
    }
    if (this.state.inputs.ownerFirstName === "" && owner.firstName !== "") {
      this.state.setInput("ownerFirstName", owner.firstName);
    }
    if (this.state.inputs.ownerLastName === "" && owner.lastName !== "") {
      this.state.setInput("ownerLastName", owner.lastName);
    }

    this.render();
  }

  /**
   * Loads the registry the inline duplicate check reads.
   *
   * readClustersFileSafe, so a clusters.yaml that will not parse yields no
   * names rather than a rejection nobody is waiting on. The consequence is
   * exactly that the inline refusal cannot fire -- `addCluster` re-reads the
   * file at write time and refuses there, which is the wall that has to hold
   * anyway, since the Cockpit writes this file too.
   */
  private async loadRegistry(): Promise<void> {
    const result = await readClustersFileSafe(this.deps.clustersPath);
    if (result.ok) this.state.setRegistry(result.file);
  }

  /**
   * Validates the form and, if it holds, writes the entry.
   *
   * addCluster rather than upsertCluster, deliberately: this is an ADD, and
   * upsert would quietly turn a name collision into an edit of the cluster
   * already there -- deleting, as it went, every field this form does not
   * collect. That is the destructive case addCluster exists to refuse.
   *
   * THE PAGE CLOSES ON SUCCESS. Registering was the whole of what the operator
   * came here to do; leaving the filled-in form on screen afterwards invites a
   * second click on Save, which would now be refused as a duplicate of what it
   * just wrote.
   */
  private async saveConnect(): Promise<void> {
    // A second Save while the first is still in the filesystem would be two
    // read-modify-write passes over the same file racing each other.
    if (this.saving) return;

    // VALIDATE, THEN PROBE, THEN WRITE (memql#4432). The state machine owns all
    // three decisions -- which is what lets the pass / fail / timeout table run
    // under bare `node --test` -- and this supplies only the socket.
    //
    // A "warned" outcome has written nothing and has recorded WHY on the form;
    // rendering is the whole response, and the button it renders now says "Save
    // anyway". The second press comes back through here and returns "write".
    this.render(); // paint the "Checking..." line before the probe blocks on it
    const outcome = await this.state.prepareConnectSave(
      this.deps.probeCluster ?? probeClusterOverHttps,
    );
    if (outcome !== "write") {
      this.render();
      return;
    }

    const draft = this.state.connectDraft();
    if (draft === undefined) {
      this.render();
      return;
    }

    this.saving = true;
    try {
      await addCluster(this.deps.clustersPath, draft);
    } catch (err) {
      // The form is intact, so the operator revises and tries again. This is
      // the wall the inline check cannot be: between that check and this write
      // the Cockpit may have added the very name being registered.
      this.state.failConnect(err instanceof Error ? err.message : String(err));
      this.render();
      return;
    } finally {
      this.saving = false;
    }

    this.deps.refreshTree();
    // The verdict is memoized from evidence that includes clusters.yaml. A
    // remote registration does not change it today -- the entry carries no
    // `local` flag, which is what registry evidence looks for -- but the memo
    // is derived from a file this just wrote, and every other writer of that
    // file drops the memo rather than reasoning about which reads it affects.
    this.presence.invalidate();
    // A BUTTON, NOT THE NAME OF A COMMAND (memql#4621).
    //
    // This used to hand over the string `Run "MemQL: Sign In" to authenticate.`
    // and stop -- so the last step of registering a cluster was to open the
    // palette and type the sentence back. The action runs the same command the
    // ownership walk runs, against the entry that was just written.
    //
    // THE ENTRY IS RE-READ RATHER THAN SYNTHESISED. `draft` is a ClusterUpdate,
    // not a ClusterConfig, and inventing the difference is the placeholder
    // mistake HandoffEffects.select records: a fabricated `{ name, endpoint: "" }`
    // dials nowhere and reports it as the cluster's fault. Re-reading also
    // yields the file's own view after every default the writer applied. If the
    // entry is gone by then -- another writer, a removal -- there is nothing
    // honest to sign in to, so it does nothing rather than guess.
    //
    // Detached, and it outlives dispose() deliberately: nothing below touches
    // panel state, and a notification carrying a button must not be awaited by
    // a handler that is about to close the tab.
    const registeredName = draft.name;
    const needsSignIn = draft.token === undefined;
    void (async () => {
      const chosen = await vscode.window.showInformationMessage(
        `MemQL: registered "${registeredName}" at ${draft.endpoint}. ` +
          (needsSignIn
            ? "Sign in to authenticate."
            : "Connect to it from the Clusters view."),
        ...(needsSignIn ? [SIGN_IN_ACTION] : []),
      );
      if (chosen !== SIGN_IN_ACTION) return;
      const result = await readClustersFileSafe(this.deps.clustersPath);
      if (!result.ok) return;
      const cluster = result.file.clusters.find((c) => c.name === registeredName);
      if (cluster === undefined) return;
      await vscode.commands.executeCommand("memql.clusters.signIn", {
        cluster,
        selected: true,
      });
    })();
    this.dispose();
  }

  // ---------------------------------------------------------------------------
  // taking the cluster off the machine (memql#3476)
  // ---------------------------------------------------------------------------

  /** Routes one approved action off the uninstall screen. */
  private onUninstallAction(action: UninstallAction): void {
    switch (action) {
      case "uninstallStart":
        void this.startUninstall();
        return;
      case "uninstallCancel":
        // Stops at the next WAVE boundary, never mid-step: a capability script
        // is the thing removing a cluster or a hosts block, and killing one
        // partway leaves the artifact half-gone. See ExecuteOptions.signal.
        this.uninstallAbort?.abort();
        return;
      case "uninstallBack":
        // Escape and Cancel both land here, and both must leave the machine
        // untouched -- which they do by construction, since nothing on this
        // screen runs anything until `uninstallStart`.
        if (this.uninstalling) return;
        this.uninstall.reset();
        this.uninstallPreview = undefined;
        this.uninstallProblem = "";
        this.state.back();
        this.render();
        // The verdict may have changed under the page -- this is also the way
        // back from a COMPLETED removal, where the cards must no longer offer
        // to uninstall a cluster that is gone.
        void this.refreshVerdict();
        return;
    }
  }

  /**
   * The run-time inputs a removal needs.
   *
   * Everything else an uninstall step is given comes off the RECEIPT: where the
   * artifact landed, and whether the installer created it or merely found it.
   * That is why this carries no domain, no owner and no tag -- a removal is not
   * configured, it is remembered.
   */
  private uninstallOptions(): SessionOptions {
    return {
      root: this.deps.installRoot,
      receiptFile: this.deps.receiptFile,
      // THE SHARED TOOLS THE OPERATOR DID NOT TICK (memql#3566).
      //
      // This used to read "nothing is skipped", reasoning that narrowing an
      // uninstall would leave a machine in a state no receipt describes. That
      // holds for MemQL's OWN artifacts and not for the toolchain: k3d, kubectl,
      // mkcert and the local CA are general tools the operator may now depend on
      // for other work, and taking them away is a decision they get to make
      // rather than a consequence of uninstalling an application. The receipt
      // still describes the result exactly -- a skipped removal leaves its entry
      // intact, so a later uninstall can still take it.
      skip: this.skippedSharedRemovals(),
      // Required by the shape and meaningless to a removal: `provider` names
      // the AI vendor an install seeds a key for.
      provider: "",
      stepParams: {},
      // A removal can hang exactly as an install can -- `k3d cluster delete`
      // against a wedged daemon is the obvious way -- and an uninstall stuck
      // halfway is the worse of the two states to be left in.
      timeoutMs: STEP_TIMEOUT_MS,
      // The hosts block and the CA both need root on the way OUT as well.
      env: this.sudoEnv(),
    };
  }

  /**
   * The elevation environment every step of this run is handed.
   *
   * ALWAYS SET, agent or no agent (memql#3586). This used to return nothing when
   * no password had been collected, which left each capability script free to
   * draw its own desktop dialog -- and that is what an install did, three times,
   * while the uninstall asked once in a VS Code box. See `elevationEnv`.
   */
  private sudoEnv(): Record<string, string> {
    return elevationEnv(this.sudoAgent?.askpassPath);
  }

  /**
   * Works out what an uninstall would do, and shows it.
   *
   * NOTHING RUNS HERE. `previewUninstall` is pure over the receipt -- it plans
   * every step and executes none -- which is what makes an itemized
   * confirmation possible without a dry-run mode inside the scripts.
   *
   * A FAILURE IS A SENTENCE, NOT AN EMPTY LIST. The case that matters is a
   * missing receipt, where `previewUninstall` refuses rather than falling back
   * to the graph's own idea of what an install creates. Rendering that as "this
   * would remove nothing" would be the same claim an empty receipt makes, and
   * the two are opposite news.
   */
  private async loadUninstallPreview(): Promise<void> {
    this.uninstall.reset();
    this.uninstallPreview = undefined;
    this.uninstallProblem = "";
    this.render();

    try {
      this.localClusterName = (await this.presence.get()).clusterName;
    } catch {
      // A detection that will not answer costs the registry cleanup, not the
      // uninstall. The artifacts still go; the entry is left for the operator's
      // own "Remove from list".
      this.localClusterName = undefined;
    }
    try {
      // The PREVIEW plans every step, including the shared ones the operator has
      // not ticked -- they are what the list offers. Only the RUN skips them.
      this.uninstallPreview = await previewUninstall(
        { ...this.uninstallOptions(), skip: new Set<string>() },
        this.hooks({}),
      );
    } catch (err) {
      this.uninstallProblem = err instanceof Error ? err.message : String(err);
    }
    this.render();
  }

  /**
   * Runs the removal the operator has just approved.
   *
   * THE PREVIEW IS THE PRECONDITION, not merely the confirmation: with no
   * preview on screen there is no itemized list for consent to have been given
   * to, so this refuses rather than running an unseen one.
   *
   * IT DOES NOT ASK THE CLUSTER ANYTHING. An uninstall reverses a receipt, and
   * a cluster that stopped answering is one of the two states this page is most
   * likely opened in -- gating the removal on reachability would strand exactly
   * the machine that most needs cleaning.
   */
  private async startUninstall(): Promise<void> {
    // A second click is not a second uninstall: two graph runs over one machine
    // would have each step racing the other's removal of the same artifact.
    if (this.uninstalling || this.uninstallPreview === undefined) return;

    this.uninstalling = true;
    // Platform before sudo (memql#4294). Asking for a password so we can then
    // say the wizard cannot run here is the wrong order.
    const refused = await refuseUnsupportedPlatform(this.uninstallOptions(), this.hooks({}));
    if (refused !== undefined) {
      this.uninstall.begin();
      for (const event of platformRefuseEvents(refused)) this.uninstall.apply(event);
      this.uninstall.finish(refused);
      this.render();
      this.uninstalling = false;
      return;
    }
    // A removal takes root too -- the hosts block, and the CA if it is ticked.
    // Asked once, here, for the same reason the install asks once.
    await this.collectSudoPassword("uninstall");
    if (this.disposed) return;
    const controller = new AbortController();
    this.uninstallAbort = controller;
    this.uninstall.begin();
    this.render();

    // Read BEFORE the removal, for the same reason localClusterName is: the
    // receipt is one of the things about to go.
    const uninstalledVersion = recordedStackTag(
      await readReceipt(this.deps.receiptFile).catch(() => null),
    );
    const recorder = await RunRecorder.begin({
      dir: this.deps.runsDir ?? defaultRunsDir(),
      instance: LOCAL_INSTANCE_NAME,
      kind: "uninstall",
      // What it removed, not where it went: there is no version afterwards.
      ...(uninstalledVersion !== "" ? { fromVersion: uninstalledVersion } : {}),
    });
    this.deps.refreshTree();

    try {
      const report = await runUninstall(
        this.uninstallOptions(),
        this.hooks({
          onEvent: (event) => {
            // The FULL log's home (memql#4194, audit 30): every stderr line,
            // redacted, as the run emits it. The step list keeps only the
            // short what-failed line (state/installProgress.ts).
            if (event.type === "stepLog") {
              this.deps.diagnostics.appendLine(
                `[${event.step.id}] ${redactForDisplay(event.line, os.homedir())}`,
              );
            }
            this.uninstall.apply(event);
            void recorder.apply(event);
            this.render();
          },
          signal: controller.signal,
        }),
      );
      this.uninstall.finish(report);
      if (report.ok && report.cancelled !== true) {
        await this.completeUninstall();
      } else {
        // A partial removal still changed the machine, so the memo describing
        // it has to go. The registry entry does NOT: it still names a cluster
        // some of whose artifacts are there, and dropping it would leave those
        // with nothing in the editor that can see them.
        this.presence.invalidate();
        this.deps.refreshTree();
      }
    } catch (err) {
      this.uninstall.fail(err instanceof Error ? err.message : String(err));
    } finally {
      this.uninstalling = false;
      this.uninstallAbort = undefined;
      // The run is over either way; the password has no further use. The same
      // line the install run has always had, and the other half of memql#4614's
      // teardown ordering: `dispose()` now DEFERS the release while either run
      // is in flight, which is only correct if both runs release their own
      // agent when they come to rest. Without this the agent would outlive a
      // panel closed mid-uninstall.
      await this.releaseSudoAgent();
      // `preserved` reaches the record UNTRANSLATED, which is the point: it
      // says the uninstall KEPT something the operator already had, and a
      // history that rounded it to "ok" would report an artifact as gone while
      // it is still on the machine.
      await recorder.finish(controller.signal.aborted ? "cancelled" : undefined);
      this.deps.refreshTree();
      this.render();
    }
  }

  /** The three things that follow a clean removal. See completeLocalUninstall. */
  private async completeUninstall(): Promise<void> {
    const problem = await completeLocalUninstall({
      clusterName: this.localClusterName,
      removeEntry: (name) => this.deps.removeRegistryEntry(name),
      invalidatePresence: () => this.presence.invalidate(),
      refreshTree: () => this.deps.refreshTree(),
      // The receipt goes with the artifacts it describes (memql#3544) --
      // otherwise the next thing the operator sees is a wizard still offering
      // to repair and uninstall a cluster that is no longer here.
      deleteReceipt: () => deleteReceipt(this.deps.receiptFile),
    });
    if (problem !== "") this.uninstall.noteFollowUpProblem(problem);
  }

  // ---------------------------------------------------------------------------
  // rendering
  // ---------------------------------------------------------------------------

  private render(): void {
    if (this.disposed) return;
    const nonce = nonceValue();
    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>Add a MemQL cluster</title>
<style nonce="${nonce}">
${brandStyleBlock()}
${viewKitStyles}

  body { max-width: 780px; }
  .card { display: block; width: 100%; text-align: left; cursor: pointer;
          border: 1px solid var(--memql-border); border-radius: 6px;
          background: var(--memql-surface); color: var(--memql-fg);
          padding: 10px 12px; margin-bottom: 8px; font: inherit; }
  .card:hover { background: var(--memql-raised); border-color: var(--memql-accent); }
  .card-label { font-weight: 600; }
  .card-detail { color: var(--memql-muted); margin-top: 2px; }
  .card[data-tone="destructive"] .card-label { color: var(--memql-danger); }
  .field { margin-bottom: 12px; }
  .field label { display: block; margin-bottom: 3px; }
  .field input, .field select { width: 100%; box-sizing: border-box; padding: 4px 6px; font: inherit;
                 color: var(--memql-fg);
                 background: var(--memql-surface);
                 border: 1px solid var(--memql-border-strong); border-radius: 3px; }
  .field input:focus, .field select:focus { outline: 1px solid var(--memql-accent); }
  /* The box and its Browse button read as one control (memql#3547): the button
     is an alternative way to fill the field beside it, not a separate action. */
  .control-row { display: flex; gap: 6px; align-items: stretch; }
  .control-row input { flex: 1 1 auto; min-width: 0; }
  button.browse { flex: 0 0 auto; white-space: nowrap; }
  /* What the step itself said, as opposed to the generic advice for its code. */
  .said { margin: 0 0 8px; }
  .remedy { font-family: var(--vscode-editor-font-family, monospace);
            background: var(--memql-raised);
            border: 1px solid var(--memql-border);
            border-radius: 4px; padding: 8px 10px; margin: 6px 0 0;
            overflow-x: auto; white-space: pre; }
  .error { color: var(--memql-danger); margin-top: 3px; }
  /* A refusal that belongs to the whole form rather than to one box, so it
     sits away from the fields instead of looking like the last one's. */
  .form-error { margin: 14px 0 0; }
  .field[data-invalid="true"] input, .field[data-invalid="true"] select {
    border-color: var(--memql-danger); }
  /* The one-time recovery key reveal (memql#4079). Monospace and boxed like
     .remedy so the value reads as the credential it is; user-select: all so a
     single click selects the whole 47 characters for the operator who prefers
     the keyboard to the Copy button. Scoped classes, not a global h2 rule --
     the uninstall screen's h2 keeps its own look. */
  .recovery-heading { font-size: 1.05em; margin: 16px 0 4px; }
  .recovery-key-row { display: flex; gap: 6px; align-items: center; margin: 6px 0; }
  details.advanced { margin: 12px 0; }
  details.advanced > summary { cursor: pointer; user-select: none;
                               color: var(--memql-accent); padding: 4px 0; }
  details.advanced[open] > summary { margin-bottom: 4px; }
  .warning { border-left: 3px solid var(--memql-accent);
             background: var(--memql-raised); padding: 8px 10px; border-radius: 4px; }
  .probe-ok { color: var(--memql-data-number); }
  .recovery-key { font-family: var(--vscode-editor-font-family, monospace);
                  background: var(--memql-raised);
                  border: 1px solid var(--memql-accent);
                  border-radius: 4px; padding: 6px 8px; overflow-x: auto;
                  user-select: all; color: var(--memql-data-number); }
</style>
</head>
<body${currentBodyThemeAttr()}>
${brandStrip("MemQL")}
${this.bodyHtml()}
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  // The registration form's values, ALL of them, on every message it sends.
  // Setting webview.html repaints the whole document, so the DOM cannot be
  // where form state lives -- an action that carried only its own field would
  // hand the extension a form missing everything typed since the last repaint.
  function connectFields() {
    const out = {};
    for (const el of document.querySelectorAll('[data-connect-field]')) {
      out[el.dataset.connectField] = el.value;
    }
    return out;
  }
  document.addEventListener('click', (e) => {
    const card = e.target.closest('[data-choose]');
    if (card) { vscode.postMessage({ type: 'choose', value: card.dataset.choose }); return; }
    const connect = e.target.closest('[data-connect-act]');
    if (connect) {
      vscode.postMessage({ type: connect.dataset.connectAct, fields: connectFields() });
      return;
    }
    const remedy = e.target.closest('[data-remedy]');
    if (remedy) {
      // The STEP ID travels, never the command: the extension looks the command
      // up in its own state. See openRemedyTerminal.
      vscode.postMessage({ type: 'remedy', value: remedy.dataset.remedy });
      return;
    }
    const act = e.target.closest('[data-act]');
    if (act) vscode.postMessage({ type: act.dataset.act });
  });
  // BOTH events, because the collect screen now carries a select as well as
  // text boxes. A select fires an input event in every browser this runs in,
  // but change is the one it is specified around, and this handler is
  // idempotent -- the extension only records the value, so arriving twice
  // costs a duplicate message and nothing else. Recording is ALL it does: the
  // host does not repaint on a keystroke, because a repaint here replaces the
  // whole document and would take the caret with it (memql#3538). (No
  // backticks in here: this script is itself inside a template literal.)
  function sendField(e) {
    const shared = e.target.closest('[data-shared]');
    if (shared) {
      // A tick is state, not an action: the host records it and does NOT
      // repaint, for the same reason a keystroke does not (memql#3538) -- a
      // repaint replaces the whole document and would drop every other tick.
      vscode.postMessage({
        type: 'shared', value: { step: shared.dataset.shared, remove: shared.checked } });
      return;
    }
    const field = e.target.closest('[data-field]');
    if (field) vscode.postMessage({
      type: 'input', value: { field: field.dataset.field, text: field.value } });
  }
  document.addEventListener('input', sendField);
  document.addEventListener('change', sendField);
  // THE DERIVATION, LIVE, WITH NO SECOND COPY OF THE CONVENTION (memql#4431).
  // The host renders the composition ALREADY PERFORMED over a placeholder --
  // api.%DOMAIN%:443, produced by composeEndpointFromDomain itself -- so all
  // this does is substitute. It does not know the convention and cannot drift
  // from it. Nothing is posted: a repaint here replaces the whole document, so
  // updating one node's text is precisely what a keystroke may do and a render
  // is what it must not.
  //
  // No regex: this source is itself inside a template literal, where a
  // backslash escape is consumed before the browser ever sees it, so a
  // character class here is one silent mangling away from matching everything.
  const derivedHint = document.querySelector('[data-derived-endpoint]');
  if (derivedHint) {
    const template = derivedHint.dataset.endpointTemplate || '';
    document.addEventListener('input', (e) => {
      const el = e.target.closest('[data-connect-field="domain"]');
      if (!el) return;
      // The same normalization normalizeDomain applies: trim, then drop leading
      // and trailing dots.
      let typed = el.value.trim();
      while (typed.startsWith('.')) typed = typed.slice(1);
      while (typed.endsWith('.')) typed = typed.slice(0, -1);
      derivedHint.textContent = typed === ''
        ? 'MemQL will connect to api.<domain>:443.'
        : 'Will connect to ' + template.replace('%DOMAIN%', typed) + '.';
    });
  }
  // Escape acts only where a screen has ASKED for it. A page-wide handler
  // would also cancel a screen that never opted in, and "the keystroke did
  // something the screen never offered" is the failure this attribute avoids.
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    const owner = document.querySelector('[data-escape-act]');
    if (owner) vscode.postMessage({ type: owner.dataset.escapeAct });
  });
${LOG_PANE_SCRIPT}
</script>
</body>
</html>`;
  }

  /**
   * Which of this panel's two run states owns the log pane on screen now.
   *
   * NARROWED TO THE INTERSECTION the disclosure needs, so this cannot become a
   * general "give me a state" accessor that hands an uninstall screen the
   * install's step list.
   */
  private runStateForScreen(): {
    toggleLogs(): void;
    setLogsFollow(follow: boolean): void;
  } {
    return this.state.screen === "uninstallPreview" ? this.uninstall : this.state;
  }

  private bodyHtml(): string {
    switch (this.state.screen) {
      case "landing":
        return this.landingHtml();
      case "collect":
        return this.collectHtml();
      case "running":
        return this.runHtml();
      case "failedStep":
        return this.failedHtml();
      case "connect":
        return this.connectHtml();
      case "uninstallPreview":
        return this.uninstallHtml();
      case "done":
        return this.doneHtml();
    }
  }

  /** The run in progress. Lifted to webview/installScreens.ts (memql#3738). */
  private runHtml(): string {
    return renderRunningScreen({
      steps: this.state.steps,
      logsOpen: this.state.logsOpen,
      logsFollow: this.state.logsFollow,
      mode: this.state.action === "repair" ? "repair" : "install",
      // `runAbort` is set for exactly as long as a run is in flight, which is
      // what distinguishes "starting, no step has reported yet" from "nothing
      // has been run".
      running: this.runAbort !== undefined,
    });
  }

  /** A step failed. Lifted to webview/installScreens.ts (memql#3738). */
  private failedHtml(): string {
    return renderFailedScreen({
      failures: this.state.failures,
      steps: this.state.steps,
      logsOpen: this.state.logsOpen,
      logsFollow: this.state.logsFollow,
      mode: this.state.action === "repair" ? "repair" : "install",
      running: this.runAbort !== undefined,
    });
  }

  /**
   * Opens a terminal holding the failed step's remedy.
   *
   * THE COMMAND COMES FROM THE PANEL'S OWN STATE, NEVER FROM THE MESSAGE. The
   * webview posts a step ID and nothing else, and the command is looked up
   * here against the failures this panel recorded. Accepting a command string
   * over postMessage would mean anything running in that iframe could choose
   * what the operator is invited to run as root -- which is the whole reason
   * the runner spawns with `shell:false` in the first place.
   */
  private openRemedyTerminal(stepId: string): void {
    const failure = this.state.failures.find((f) => f.id === stepId);
    if (failure === undefined || failure.remedy === "") return;

    const terminal = vscode.window.createTerminal({ name: "MemQL install -- fix" });
    terminal.show();
    // false: typed, NOT executed. See remedyHtml.
    terminal.sendText(failure.remedy, false);
  }

  /** The cards, straight from addClusterMenu. */
  private landingHtml(): string {
    // `registered` is what decides whether the reconnect card appears at all:
    // a cluster already in the list has nothing to compose (memql#3741).
    const choices = addClusterMenu(this.verdict, this.localRegistered);
    const cards = choices.map((choice) => this.cardHtml(choice)).join("");
    // THE CARDS ARE THE ACTIONS on this screen, so the ordering doctrine is
    // already satisfied by putting them under the lede: there is no separate
    // buttons row to hoist. Composed through `renderScreen` regardless, so the
    // screen list in test/screenOrdering.test.ts has nothing to exempt.
    return renderScreen({
      title: "Add a MemQL cluster",
      status: `<p class="lede">${escapeHtml(VERDICT_LEDE[this.verdict])}</p>`,
      details: cards,
    });
  }

  private cardHtml(choice: AddClusterChoice): string {
    // Uninstall is the one irreversible entry here, and it is toned so it does
    // not read as a peer of "connect". The confirmation is still the itemized
    // preview (#3476); this is only so the card is not mistaken for a routine
    // one at a glance.
    const tone = choice.action === "uninstall" ? "destructive" : "normal";
    return `<button class="card" type="button" data-tone="${tone}" data-choose="${escapeHtml(
      choice.action,
    )}">
  <div class="card-label">${escapeHtml(choice.label)}</div>
  <div class="card-detail">${escapeHtml(choice.detail)}</div>
</button>`;
  }

  private collectHtml(): string {
    const action = this.state.action;
    if (action === undefined) return this.landingHtml();
    return renderCollectScreen({
      action,
      values: this.state.inputs,
      errors: this.state.errors,
      versionChoices: this.versionChoices,
      ...(this.preflight === undefined ? {} : { preflight: this.preflight }),
    });
  }

  /**
   * The release tags the version field offers (memql#3882).
   *
   * Filled ONCE, in the background, when the collect screen first opens. It
   * starts empty and the field renders as a text box until the listing lands,
   * which is the right order: an operator must never be blocked on a network
   * call to answer a question that already has a default.
   */
  private versionChoices: readonly string[] = [];

  /**
   * Loads the tag list and repaints if it found anything.
   *
   * FAILURE IS SILENT HERE, and deliberately: `listReleaseTags` already reports
   * why it came back empty, and the consequence -- typing the version instead
   * of picking it -- is a working path rather than a degraded one. An error
   * banner over a form whose default is already correct would be noise about a
   * choice most operators will not make.
   */
  /**
   * Create deployment on a machine with no wizard cluster (memql#4294).
   *
   * The tag list is a list of versions this wizard would then run. On an
   * unsupported platform that run is a refuse, so offering the list (or a
   * text box) would invite an operator to pick a tag that cannot install.
   */
  private async gateCreateDeployment(): Promise<void> {
    const refused = await refuseUnsupportedPlatform(this.platformSession(), this.hooks({}));
    if (refused !== undefined) {
      for (const event of platformRefuseEvents(refused)) this.state.apply(event);
      this.render();
      return;
    }
    await this.loadVersionChoices();
  }

  private platformSession(): SessionOptions {
    return {
      root: this.deps.installRoot,
      receiptFile: this.deps.receiptFile,
      skip: new Set<string>(),
      provider: "anthropic",
      stepParams: {},
      timeoutMs: STEP_TIMEOUT_MS,
      // Same marker the install run puts on every step (memql#3586): detect is
      // read-only, but it still goes through elevate.sh's environment, and a
      // probe that omitted this would be the one script free to draw a desktop
      // dialog.
      env: elevationEnv(undefined),
    };
  }

  private async loadVersionChoices(): Promise<void> {
    if (this.versionChoices.length > 0) return;
    // Asked of the REPOSITORY, not of a checkout: the wizard is choosing what
    // to clone, so there is no working tree yet to have an origin.
    const listing = await listReleaseTags({ cwd: process.cwd(), repo: DEFAULT_STACK_REPO });
    if (listing.tags.length === 0) return;
    this.versionChoices = listing.tags;
    // THE LISTING IS WHAT MAKES "Latest" A VALUE (memql#4429). The picker labels
    // the newest entry `Latest ... (recommended)`; this is what makes that entry
    // the one that is SELECTED. Both read the same head of the same list, so the
    // recommendation and the selection cannot disagree.
    //
    // The state machine drops this on the floor if the operator has already
    // chosen -- deciding that here would need form state the DOM no longer has.
    this.state.seedVersionFromListing(listing.tags[0] ?? "");
    this.render();
  }

  /**
   * The registration form (memql#3475).
   *
   * WHAT IT REPLACES: five input boxes shown one after another. That sequence
   * could not be navigated backwards -- seeing the endpoint question does not
   * let you fix the name you fumbled two boxes ago -- and Escape at any point
   * discarded every answer given so far. Both are properties of the widget, not
   * of the code behind it, which is why the fix is a form and not more
   * validation.
   *
   * VALIDATION RUNS ON SAVE, not on each keystroke, and that is a deliberate
   * consequence of the surface: a repaint here replaces the webview's whole
   * document, so validating as the operator types would reload the page under
   * their cursor. Checking everything at once is also what the argument form
   * does, and for the better reason -- all the problems arrive together
   * instead of one per attempt.
   */
  /**
   * The probe's verdict, on the form (memql#4432).
   *
   * A WARNING, NOT AN ERROR, and the markup says so: it is not attached to a
   * field, because no field is wrong. The cluster is unreachable from here right
   * now, which may be a typo and may equally be a VPN the operator has not
   * connected, a cluster that is scaled to zero, or a deploy in progress.
   *
   * The endpoint is NAMED in the message. "Could not reach the cluster" sends
   * somebody to check their cluster; "could not reach api.example.com:443" lets
   * them notice they typed exmaple.com.
   */
  private probeHtml(): string {
    const probe = this.state.connectProbe;
    switch (probe.state) {
      case "none":
        return "";
      case "running":
        return `<p class="hint probe-running">Checking that ${escapeHtml(
          this.state.connectProbeTargets().endpoint,
        )} answers...</p>`;
      case "passed":
        return `<p class="hint probe-ok">Reached ${escapeHtml(probe.endpoint)} and its sign-in service.</p>`;
      case "failed":
        return `<p class="warning probe-failed">Could not reach ${escapeHtml(
          probe.endpoint,
        )}: ${escapeHtml(probe.reason)}<br>
Nothing has been written. If the cluster is stopped, behind a VPN, or still deploying, this is expected -- press Save anyway to register it regardless.</p>`;
    }
  }

  private connectHtml(): string {
    const values = this.state.connectInputs;
    const errors = this.state.connectErrors;
    const failure = this.state.connectFailure;

    const renderField = (field: ConnectField): string => {
      const error = errors.find((e) => e.field === field);
      const secret = CONNECT_SECRET_FIELDS.includes(field);
      // The endpoint box shows the DERIVATION as its starting value, so opening
      // Advanced answers "what will this connect to" without anyone typing. An
      // edit replaces it and wins -- `validateConnect` and `connectDraft` both
      // prefer a non-empty box over the composition.
      const shown =
        field === "endpoint" && values.endpoint.trim() === ""
          ? composeEndpointFromDomain(values.domain)
          : values[field];
      // THE LIVE DERIVATION, WITHOUT A SECOND COPY OF THE CONVENTION
      // (memql#4431). `endpoint.ts` records that this composition was once
      // inlined in three places and that the drift is invisible -- each copy
      // produces a plausible hostname and the failure surfaces as a cluster that
      // will not dial. So the webview is handed a TEMPLATE produced by the real
      // function rather than a rule it re-implements: the script substitutes the
      // typed domain into it as the operator types, and the only thing on that
      // side is a string replace.
      const derived =
        field === "domain"
          ? `<div class="hint" data-derived-endpoint
       data-endpoint-template="${escapeHtml(composeEndpointFromDomain(DERIVATION_PLACEHOLDER))}">${escapeHtml(
              derivationLine(values.domain),
            )}</div>`
          : "";
      return `<div class="field" data-invalid="${error !== undefined}">
  <label for="c-${field}">${escapeHtml(CONNECT_LABELS[field])}</label>
  <input id="c-${field}" type="${secret ? "password" : "text"}"
         data-connect-field="${field}" value="${escapeHtml(shown)}">
  <div class="hint">${escapeHtml(CONNECT_HINTS[field])}</div>
  ${derived}
  ${error === undefined ? "" : `<div class="error">${escapeHtml(error.message)}</div>`}
</div>`;
    };

    const primary = CONNECT_PRIMARY_FIELDS.map(renderField).join("");
    // ADVANCED OPENS ITSELF WHEN IT HAS SOMETHING TO SAY. A validation error
    // inside a collapsed <details> is an error nobody can see, and a form that
    // refuses to save while showing no problem is the worst of both. It also
    // stays open when either field already carries an operator's answer, so a
    // repaint never hides what they typed.
    const advancedHasContent =
      CONNECT_ADVANCED_FIELDS.some((f) => errors.some((e) => e.field === f)) ||
      values.endpoint.trim() !== "" ||
      values.token.trim() !== "";
    const advanced = `<details class="advanced"${advancedHasContent ? " open" : ""}>
  <summary>Advanced</summary>
  <p class="hint">Both of these have correct answers already. Change the endpoint only for a front door that is not at api.&lt;domain&gt;:443.</p>
  ${CONNECT_ADVANCED_FIELDS.map(renderField).join("")}
</details>`;

    // data-escape-act is what makes Escape mean "discard" HERE and nowhere
    // else on this page: the key listener looks for the attribute rather than
    // acting on every Escape, so a screen that has not opted in -- a run in
    // progress, say -- is not cancelled by a keystroke aimed at a form.
    // THE ESCAPE OWNER MOVES OUT TO WRAP THE WHOLE SCREEN. It used to wrap the
    // form and its buttons together; with the buttons hoisted above the form
    // the two would be in different subtrees, and `[data-escape-act]` is found
    // by a page-wide query, so the wrapper has to contain both or Escape stops
    // meaning anything here.
    return `<div data-escape-act="discard">${renderScreen({
      title: "Connect to an existing cluster",
      actions: `<button class="primary" type="button" data-connect-act="save">${
        this.state.connectProbe.state === "failed" ? "Save anyway" : "Save cluster"
      }</button>
  <button class="secondary" type="button" data-connect-act="discard">Cancel</button>`,
      status: `<p class="lede">Registering a cluster records how to reach it. Nothing is installed and nothing on the cluster is touched.</p>
${failure === "" ? "" : `<p class="error form-error">${escapeHtml(failure)}</p>`}`,
      details: `${primary}
${advanced}
${this.probeHtml()}`,
    })}</div>`;
  }

  /**
   * Taking the local cluster off this machine (memql#3476).
   *
   * ONE SCREEN, FIVE PHASES, and they stay on this branch rather than borrowing
   * the install's `running` / `failedStep` screens. Those screens offer Retry
   * and Switch-to-Guided per step, which an uninstall has no version of, and
   * their wording is about building a cluster. What they DO share is the row
   * projection: `toStepViews` draws a removal's steps exactly as it draws an
   * install's, because a step is a step.
   */
  private uninstallHtml(): string {
    switch (this.uninstall.phase) {
      case "preview":
        return this.uninstallListHtml();
      case "running":
        return this.uninstallRunningHtml();
      case "removed":
        return this.uninstallRemovedHtml();
      case "stopped":
        return this.uninstallStoppedHtml();
      case "failed":
        return this.uninstallFailedHtml();
    }
  }

  /**
   * The itemized dry run -- and the confirmation.
   *
   * THERE IS NO SEPARATE YES/NO BOX (design D6). The list and the control that
   * acts on it are on one screen with nothing between them, because a modal
   * asking "are you sure?" after an itemized list adds a click and no
   * information: what the operator is consenting to is the list.
   *
   * BOTH KINDS RENDER IN ONE LIST. Hiding the preserved half behind a
   * disclosure would make "the uninstall leaves something behind" the one fact
   * an operator has to go looking for, and it is exactly the fact most likely
   * to change their mind.
   */
  private uninstallListHtml(): string {
    if (this.uninstallProblem !== "") {
      return renderScreen({
        title: "Uninstall the local cluster",
        actions: `<button class="secondary" type="button" data-act="uninstallBack">Back</button>`,
        status: `<p class="lede">MemQL cannot say what an uninstall would remove, so it will not run one.</p>
<p class="error">${escapeHtml(this.uninstallProblem)}</p>`,
      });
    }

    const preview = this.uninstallPreview;
    if (preview === undefined) {
      return renderScreen({
        title: "Uninstall the local cluster",
        status: `<p class="lede">Reading the install receipt to work out exactly what is on this machine.</p>`,
      });
    }

    // The projection is removalPreview.ts's, and it re-derives nothing: which
    // artifacts are preserved, in what order they read, and what each one is
    // called were all settled by previewUninstall against the receipt.
    const items = removalPreviewItems(preview);
    const privileged = items.filter(
      (item) => item.elevation !== undefined && item.elevation !== "none",
    );
    const elevationNote =
      privileged.length === 0
        ? ""
        : `<p class="hint">${escapeHtml(
            "The marked steps interrupt the run to ask for something outside MemQL's own " +
              "footprint: [sudo] needs your password to edit a system file, [user-trust] needs " +
              "your approval to withdraw a certificate authority your browsers trust.",
          )}</p>`;

    // data-escape-act, so Escape means "leave this alone" HERE and does not
    // reach a screen that never asked for it. Leaving costs nothing: not one
    // step has run.
    // THE BUTTON MOVES ABOVE THE LIST, AND THE LABEL FOLLOWS IT. "Remove the
    // items above" was true when the button sat under them and is a lie the
    // moment it sits over them -- so it names what it removes instead of
    // pointing at where they are. The list is still the confirmation and there
    // is still no second prompt; what changed is that an operator who has
    // already read it does not scroll back past it to act.
    return `<div data-escape-act="uninstallBack">${renderScreen({
      title: "Uninstall the local cluster",
      actions: `<button class="primary" type="button" data-act="uninstallStart">Uninstall -- remove the items listed below</button>
  <button class="secondary" type="button" data-act="uninstallBack">Cancel</button>`,
      status: `<p class="lede">${escapeHtml(
        "This list is the confirmation -- there is no second prompt. It is built from the " +
          "install receipt, so nothing this machine had before the install is touched.",
      )}</p>`,
      details: `${renderToHtml(renderRemovalPreview(items.filter((item) => !this.isShared(item.id))))}
${elevationNote}
${this.sharedToolsHtml()}`,
    })}</div>`;
  }

  /**
   * Every shared removal the operator left unticked.
   *
   * Computed from the PREVIEW rather than from a list here, so a tool marked
   * shared in the graph is offered and skipped without this file being edited.
   */
  private skippedSharedRemovals(): Set<string> {
    const skip = new Set<string>();
    for (const step of this.uninstallPreview?.removals ?? []) {
      if (step.shared && !this.removeShared.has(step.id)) skip.add(step.id);
    }
    return skip;
  }

  /**
   * Collects the password ONCE, if this run will need one at all.
   *
   * WHY IT IS ASKED HERE rather than left to each step. sudo caches an
   * authentication against a timestamp keyed by the terminal -- or, with no
   * terminal, by the PARENT PROCESS ID. Every step is its own process, so no
   * two share the cache: an install that touches the hosts file, installs the
   * NSS tools and trusts a CA asked three times for one install (memql#3568).
   *
   * NOT ASKED WHEN IT WOULD BE POINTLESS: already root, or a sudo that runs
   * without asking. A prompt in either case is a question with no consequence,
   * and the surest way to teach someone to type their password at anything.
   *
   * A wrong password is caught HERE, with `sudo -A -v`, rather than nine
   * minutes into a graph -- and re-asked, because a typo is the likeliest
   * reason and re-running the whole install is a poor way to fix one.
   */
  private async collectSudoPassword(kind: GraphKind): Promise<void> {
    await this.releaseSudoAgent();
    if (!(await this.runNeedsAPassword(kind))) return;

    for (let attempt = 0; attempt < 3; attempt += 1) {
      const secret = await vscode.window.showInputBox({
        password: true,
        ignoreFocusOut: true,
        title: "MemQL installer",
        prompt:
          attempt === 0
            ? "Your password, once. Some steps have to edit system files -- the hosts file, the certificate store your browsers read."
            : "That password was not accepted. Try again.",
        placeHolder: "Password for sudo",
      });
      // Dismissed. The run still starts: a step that needs root will refuse
      // with the exact command to run in a terminal, which is a better answer
      // than refusing to begin over a thing they may have already arranged.
      if (secret === undefined || secret === "") return;

      const agent = await startSudoAgent(secret, process.execPath);
      const accepts = this.deps.sudoAccepts ?? sudoAccepts;
      if (await accepts(agent.askpassPath)) {
        this.sudoAgent = agent;
        return;
      }
      await agent.dispose();
    }
  }

  /**
   * Gathers the "Before it runs" facts and repaints (memql#4195).
   *
   * The same sources the run itself consults -- the graph document, the
   * `sudo -n` probe (through the deps seam the elevation tests use), the
   * receipt's recorded key path -- so the checklist can never promise a
   * different run than the one Start begins. Stale answers are prevented by
   * re-checking the action when the async work lands.
   */
  private async computePreflight(action: "install" | "installGuided" | "repair"): Promise<void> {
    this.preflight = undefined;
    let graph: PreflightInputs["graph"];
    let needsElevation = false;
    try {
      // THE LANE'S OWN DOCUMENT (memql#4430). "Before it runs" states how many
      // steps a run has, and a from-source install has one more -- the build.
      // Counting install.json's steps for a `main` install would understate the
      // run on the one screen whose whole job is saying what it will cost.
      const loaded = await loadGraphFile(
        installGraphPath(this.deps.installRoot, isMainBranchChoice(this.state.inputs.version)),
      );
      needsElevation = loaded.steps.some((step) => step.elevation !== "none");
      graph = { ok: true, steps: loaded.steps.length, needsElevation };
    } catch (err) {
      graph = {
        ok: false,
        error: redactForDisplay(err instanceof Error ? err.message : String(err), os.homedir()),
      };
    }
    let sudoFree = process.getuid?.() === 0;
    if (!sudoFree && needsElevation) {
      const isFree = this.deps.sudoIsFree ?? (() => sudoRunsWithoutAsking());
      sudoFree = await isFree().catch(() => false);
    }
    const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    const recordedKeyPath = maskHomePath(
      usablePath(recordedProviderKeyFile(receipt)),
      os.homedir(),
    );
    if (this.disposed || this.state.action !== action || this.state.screen !== "collect") return;
    this.preflight = preflightItems({
      action,
      graph,
      sudoFree,
      recordedKeyPath,
      // WHICH LANE THIS MACHINE IS IN, from the same receipt every other fact
      // here comes from (memql#4246). A run over a cluster on checkout-built
      // images returns it to released ones, and this is what makes the
      // checklist say so before Start rather than leaving it to be noticed in
      // the Deployments row afterwards.
      imageSource: recordedImageSource(receipt),
      releasedTag: recordedCheckout(receipt).tag,
    });
    this.render();
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

  /** Stops the agent and removes its socket. Idempotent. */
  private async releaseSudoAgent(): Promise<void> {
    const agent = this.sudoAgent;
    this.sudoAgent = undefined;
    if (agent !== undefined) await agent.dispose();
  }

  /** Whether this removal step takes away something that is not MemQL-only. */
  private isShared(stepId: string): boolean {
    return (this.uninstallPreview?.removals ?? []).some((s) => s.id === stepId && s.shared);
  }

  /**
   * The shared tools, as an opt-in list.
   *
   * WHY THEY ARE NOT IN THE LIST ABOVE. Docker, k3d, kubectl, mkcert and the
   * local CA are general tools; the operator may be using them for other work
   * by now, and the mkcert CA may be signing certificates for half a dozen
   * other local stacks. "Uninstall MemQL" is consent to remove what MemQL put
   * there for itself -- the cluster, the checkout, the hosts block -- and is not
   * consent to take away the toolchain (memql#3566).
   *
   * So these are UNTICKED by default, and the run skips every one the operator
   * leaves alone. Skipping is the session's own `skip` set, which the executor
   * already honours, so nothing new decides anything here.
   *
   * Docker appears nowhere at all: MemQL did not install it and will not remove
   * it. See docs/public/operate/install-prerequisites.md.
   */
  private sharedToolsHtml(): string {
    const shared = (this.uninstallPreview?.removals ?? []).filter((s) => s.shared);
    if (shared.length === 0) return "";
    const rows = shared
      .map((step) => {
        const checked = this.removeShared.has(step.id) ? " checked" : "";
        return `<label class="shared-tool">
  <input type="checkbox" data-shared="${escapeHtml(step.id)}"${checked}>
  <span><strong>${escapeHtml(step.target !== "" ? step.target : step.description)}</strong>
  <em>${escapeHtml(step.sharedReason)}</em></span>
</label>`;
      })
      .join("");
    return `<h2>Also remove these?</h2>
<p class="hint">${escapeHtml(
      "These are general tools, not MemQL's own. They stay unless you tick them. Docker is " +
        "never touched -- MemQL did not install it.",
    )}</p>
${rows}`;
  }

  /**
   * The removal in flight.
   *
   * THE SAME RUN BLOCK AS AN INSTALL (memql#4454), from `renderRunBlock` with
   * `mode: "uninstall"`. The screen stays this panel's -- a removal has no
   * Retry, no guided mode and its own five phases -- but the mark, the bar and
   * the one-line narration are the shared ones, because a second progress
   * display for the one run that takes things away is where the two would
   * drift apart.
   */
  private uninstallRunningHtml(): string {
    const steps = this.uninstall.steps;
    const block = {
      steps,
      mode: "uninstall" as const,
      running: !runIsSettled(steps),
      logsOpen: this.uninstall.logsOpen,
      logsFollow: this.uninstall.logsFollow,
    };
    // Cancel stops at the next wave boundary, so it is offered only while there
    // is a wave left to stop.
    return renderScreen({
      title: "Removing the local cluster",
      actions: runIsSettled(steps)
        ? ""
        : `<button class="secondary" type="button" data-act="uninstallCancel">Cancel</button>`,
      status: `<p class="lede">${escapeHtml(
        "Each step reverses one entry in the receipt, in the order the graph gives -- each tool " +
          "outlives the artifact it is needed to remove.",
      )}</p>
${renderRunBlock(block)}`,
      details:
        steps.length === 0 ? "" : renderToHtml(renderInstallSteps(toStepViews(steps))),
      logs: renderRunLogPane({
        steps,
        open: this.uninstall.logsOpen,
        follow: this.uninstall.logsFollow,
      }),
    });
  }

  /** It is off the machine. */
  private uninstallRemovedHtml(): string {
    const steps = this.uninstall.steps;
    const removed = steps.filter((step) => step.state === "done").length;
    const kept = steps.filter((step) => step.state === "preserved").length;
    const summary =
      kept === 0
        ? `${removed} artifact${removed === 1 ? "" : "s"} removed.`
        : `${removed} artifact${removed === 1 ? "" : "s"} removed; ${kept} left in place because ` +
          `${kept === 1 ? "it was" : "they were"} already on this machine before the install.`;
    // The follow-up is reported as its own news. The cluster IS gone -- saying
    // the uninstall failed because a YAML write did would send the operator to
    // repeat a removal with nothing left to remove.
    const followUp =
      this.uninstall.followUpProblem === ""
        ? ""
        : `<p class="error">${escapeHtml(this.uninstall.followUpProblem)}</p>`;

    return renderScreen({
      title: "The local cluster is off this machine",
      actions: `<button class="secondary" type="button" data-act="uninstallBack">Back</button>`,
      status: `<p class="lede">${escapeHtml(summary)}</p>
${followUp}`,
      details: renderToHtml(renderInstallSteps(toStepViews(steps))),
      logs: renderRunLogPane({
        steps,
        open: this.uninstall.logsOpen,
        follow: this.uninstall.logsFollow,
      }),
    });
  }

  /** The operator stopped it. */
  private uninstallStoppedHtml(): string {
    return renderScreen({
      title: "Removal stopped",
      actions: `<button class="primary" type="button" data-act="uninstallStart">Run the rest</button>
  <button class="secondary" type="button" data-act="uninstallBack">Back</button>`,
      status: `<p class="lede">${escapeHtml(
        "What had already been removed is gone; everything else is still here. An uninstall " +
          "does not rewrite the receipt, so running it again takes up the rest -- the steps that " +
          "already ran find their artifact missing and do nothing.",
      )}</p>`,
      details: renderToHtml(renderInstallSteps(toStepViews(this.uninstall.steps))),
      logs: renderRunLogPane({
        steps: this.uninstall.steps,
        open: this.uninstall.logsOpen,
        follow: this.uninstall.logsFollow,
      }),
    });
  }

  /**
   * A step refused, and WHICH one.
   *
   * The failing step is named in the heading rather than left for the operator
   * to find in the list: a failed reversal means one specific artifact is still
   * on the machine, and "the uninstall failed" does not say which.
   */
  private uninstallFailedHtml(): string {
    const failed = this.uninstall.failure;
    if (failed === undefined) {
      // No step ever reported -- the run could not start. The sentence is the
      // whole of what there is to say.
      return renderScreen({
        title: "The uninstall did not run",
        actions: `<button class="secondary" type="button" data-act="uninstallBack">Back</button>`,
        status: `<p class="lede">${escapeHtml(
          this.uninstall.problem === ""
            ? "The removal ended without reporting a step."
            : this.uninstall.problem,
        )}</p>`,
      });
    }

    // THE LOG IS NO LONGER THE FALLBACK DETAIL (memql#4456). This used to pass
    // `failed.reason || failed.log`, which put verbatim stderr into the screen's
    // status area whenever a capability named no reason -- the one place D4 says
    // it must never be. The pane below carries it, opened already, because the
    // state module discloses on failure.
    const guidance = failureGuidance(failed.exitCode, failed.remedy, failed.reason);
    return renderScreen({
      title: `${failed.description === "" ? failed.id : failed.description} failed`,
      actions: `<button class="primary" type="button" data-act="uninstallStart">Try the removal again</button>
  <button class="secondary" type="button" data-act="uninstallBack">Back</button>`,
      status: `<p class="lede">${escapeHtml(guidance.headline)}</p>
<p>${escapeHtml(guidance.advice)}</p>
<p>${escapeHtml(
        `The artifact this step names is still on this machine, and the receipt still records it ` +
          `-- an uninstall never rewrites the receipt, so once the cause is dealt with, running it ` +
          `again repeats exactly this list.`,
      )}</p>`,
      details: renderToHtml(renderInstallSteps(toStepViews(this.uninstall.steps))),
      logs: renderRunLogPane({
        steps: this.uninstall.steps,
        open: this.uninstall.logsOpen,
        follow: this.uninstall.logsFollow,
        focusStepId: failed.id,
      }),
    });
  }

  /**
   * Registers the cluster the run just built, and moves to the hand-off screen.
   *
   * THE SEAM #3474 CALLS when a run finishes successfully. The ordering and the
   * failure semantics are in src/install/handoff.ts, where they are tested; this
   * only supplies the effects, and it supplies them through COMMANDS rather than
   * through injected collaborators so the panel needs no new constructor
   * arguments to do it.
   */
  async handOffAfterInstall(domain: string): Promise<void> {
    const result = await completeInstallHandoff(
      { domain },
      {
        // upsertCluster, never addCluster: a repair or a second run over an
        // already-registered cluster must update the entry, and addCluster
        // refuses a duplicate name by design.
        write: (update) => upsertCluster(this.deps.clustersPath, update),
        invalidatePresence: () => this.presence.invalidate(),
        refreshTree: () => void vscode.commands.executeCommand("memql.clusters.refresh"),
        // The cluster the hand-off just wrote, not a placeholder built from its
        // name (#3905). The selection command dials what it is given, so the
        // `endpoint: ""` this used to pass produced "Cluster is not configured.
        // Set an endpoint" for a cluster whose endpoint had been written one
        // line earlier -- and suppressed the "Sign in" button, because
        // `notConfigured` is not a condition a credential can recover.
        select: async (cluster) => {
          await vscode.commands.executeCommand("memql.clusters.select", {
            cluster,
            selected: false,
          });
        },
      },
    );
    this.state.setHandoff(result);
    this.render();
  }

  /**
   * Registers the local cluster that is already on this machine.
   *
   * THE SAME HAND-OFF AN INSTALL USES, deliberately: write, invalidate the
   * presence memo, refresh, select, and report whether sign-in can be offered.
   * A cluster reconnected to is registered IDENTICALLY to one just built --
   * `local: true` and all -- because two spellings of that entry would be two
   * answers to what a local cluster's row looks like.
   *
   * The only thing this adds is where the domain comes from, and the whole
   * point of the action is that it comes from the machine rather than from the
   * operator. See clusters/reconnect.ts.
   */
  private async reconnectLocal(): Promise<void> {
    const receipt = await readReceipt(this.deps.receiptFile).catch(() => null);
    if (this.disposed) return;
    // The same receipt this call already read, held for `doneHtml` (memql#4246)
    // -- see `doneReceipt`'s own comment for why it is not read a second time.
    this.doneReceipt = receipt;
    const plan = planLocalReconnect(receipt);
    await this.handOffAfterInstall(plan.domain);
  }

  /**
   * Mints a passkey enrolment link and opens it (memql#3408, memql#3906).
   *
   * MINTS, rather than replaying the one the run produced. The install's link
   * is single-use and 15-minute, so this screen -- which an operator can leave
   * open indefinitely -- was the worst possible place to hold one. Minting on
   * click also means the button is offered whenever there is an ACCOUNT to
   * enrol against, instead of only when the run happened to produce a URL.
   *
   * Delegating to the command rather than calling `mintOwnershipLink` here is
   * deliberate: it is the same route the Clusters tree and the palette take, so
   * an operator cannot reach two implementations of "take ownership" that
   * behave differently, and the credential never enters this file at all.
   */
  private async enrolPasskey(): Promise<void> {
    const handoff = this.state.handoff;
    if (handoff === undefined || !handoff.ok) return;
    await vscode.commands.executeCommand("memql.clusters.takeOwnership", {
      cluster: handoff.cluster,
      selected: true,
    });
  }

  /**
   * Claims the cluster by opening the magic link the install recovered.
   *
   * The mirror of `enrolPasskey`, under the same rules, for a credential that
   * matters more: this link authenticates as the cluster OWNER. It never enters
   * the webview -- the button carries no href -- and `openClaimLink`
   * re-validates it (https, `/auth/complete?ml=`) before anything is opened, so
   * a value rewritten between the identity log and this click is refused rather
   * than followed.
   *
   * A failure names the recovery path rather than leaving the operator stuck,
   * because on a fresh install there IS no other route in: no passkey exists,
   * and sign-in cannot work until this has been done once.
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

      // A HEADLESS HOST GETS THE LINK, NOT AN APOLOGY (memql#4618).
      //
      // `browserUnavailable` means the machine could not open a browser, which
      // says nothing about the credential: the link in hand is live, and this
      // is the ONE route in on a fresh install -- no passkey exists yet, and
      // sign-in cannot work until this has been used once. Telling that
      // operator to go and recover the link from a pod log, while holding a
      // valid one, is the dead end the enrolment path had until this issue.
      //
      // Only that reason. `malformed` means the value is not an https
      // /auth/complete?ml= URL, and a value we refused to open is precisely the
      // value not to put on somebody's clipboard.
      if (err instanceof ClaimError && err.reason === "browserUnavailable") {
        void (async () => {
          const chosen = await vscode.window.showErrorMessage(detail, COPY_CLAIM_LINK);
          if (chosen === COPY_CLAIM_LINK) await this.copyClaimLink(url);
        })();
        return;
      }

      void vscode.window.showErrorMessage(
        `${detail} The install recovered the link from the identity service's log; you can recover it again there if this screen is gone.`,
      );
    }
  }

  /**
   * Puts the owner magic link on the clipboard, for a host with no browser.
   *
   * Follows copyRecoveryKey's shape: the exception detail is record material,
   * not toast material, and the LINK is never written to the diagnostics
   * channel -- it authenticates as the cluster owner, so the record says the
   * copy failed and not what it was copying.
   */
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
      vscode.window.showErrorMessage(
        "MemQL: the claim link could not be copied to the clipboard.",
      );
      return;
    }
    void vscode.window.showInformationMessage(
      "MemQL: claim link copied. Open it in a browser that can reach this cluster -- it signs you in as the owner, and it is single-use.",
    );
  }

  /**
   * Back, with the one-time recovery key standing in the way (memql#4615).
   *
   * THE DEFECT. `state.back()` deliberately lets go of the plaintext -- the
   * key's display lifetime IS the done screen -- and the done screen rendered
   * Back as an ordinary secondary button in the same actions row as "Set up a
   * passkey" and "Claim this cluster". Only the key's HASH is stored, so one
   * misclick permanently destroyed the cluster's break-glass credential, with
   * no confirmation and no "you have not copied this yet". The screen's own
   * copy already said "closing this screen is goodbye": the design knew the
   * stakes and did nothing about them.
   *
   * ASKED ONLY WHEN THERE IS SOMETHING TO LOSE. `recoveryKeyWouldBeLost` is
   * false for every screen that is not holding a plaintext and false once the
   * clipboard has taken it, so the prompt cannot become the thing an operator
   * clicks through on the way to somewhere else -- which is how a prompt stops
   * being read.
   *
   * A REFUSAL IS A NO-OP, deliberately: the operator stays exactly where they
   * were, with the key still on screen and the Copy button still under it.
   * There is nothing to undo because nothing was done.
   */
  private async leaveScreen(): Promise<void> {
    if (this.state.recoveryKeyWouldBeLost) {
      const proceed = await this.confirmDestructive({
        message: "Go back without keeping this cluster's recovery key?",
        detail:
          "The key is shown exactly once and only its hash is stored, so going back destroys " +
          "this copy of it for good. If you have not put it somewhere safe yet, copy it first " +
          "-- otherwise the only way to get one again is to rotate the key from the portal.",
        proceed: "Go back and lose the key",
      });
      // The panel can be closed while a modal is up, and `state.back()` on a
      // dead panel would be a transition nobody can see.
      if (!proceed || this.disposed) return;
    }
    this.state.back();
    this.render();
  }

  /**
   * The modal, or whatever a test injected in its place.
   *
   * `{ modal: true }` is not decoration. A notification toast can be answered
   * by ignoring it, and an ignored toast here reads as a Back that did nothing:
   * the operator presses it again, and again, while the answer they never gave
   * is the thing holding the transition. A modal is the one shape where being
   * asked and answering are the same event, which is what a question standing
   * in front of an irreversible act has to be.
   *
   * `isCloseAffordance` on the cancel item is what makes Escape and the window
   * chrome mean "keep the key" rather than "unanswered" -- an operator who
   * dismisses this without reading it must end up with their key, and
   * `undefined` is what a dismissal answers.
   */
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
    // Identity, not the title string: a dismissed modal answers `undefined`,
    // and `undefined` has to mean KEEP THE KEY. Comparing titles would give the
    // same answer today and quietly become a bug the first time a label is
    // reworded in one of the two places.
    return chosen === go;
  }

  /** Reaches the existing sign-in flow. No new credential path (#3401). */
  /**
   * Copies the revealed recovery key (memql#4079).
   *
   * THE KEY COMES FROM PANEL STATE, NEVER FROM THE MESSAGE -- the same rule the
   * remedy terminal enforces, for the same reason: the webview channel is
   * untrusted, and a value that came over it would let the page choose what
   * lands in the clipboard. Loud on failure, because an operator who believes
   * they copied a key they did not is worse off than one who was told to select
   * it by hand.
   */
  private async copyRecoveryKey(): Promise<void> {
    const key = this.state.revealedRecoveryKey;
    if (key === "") return;
    try {
      await vscode.env.clipboard.writeText(key);
    } catch (err) {
      // Generic sentence; the exception detail is record material, not toast
      // material (memql#4194, audit 3).
      recordDiagnostic(
        this.deps.diagnostics,
        "the recovery key could not be copied to the clipboard",
        err instanceof Error ? err.message : String(err),
        new Date().toISOString(),
      );
      vscode.window.showErrorMessage(
        "MemQL: the recovery key could not be copied -- select it in the panel and copy it by hand.",
      );
      return;
    }
    // RECORDED ONLY ON THE SUCCESS PATH (memql#4615). This is what tells Back
    // and the panel-close warning that the operator actually has the key, so a
    // failed write must leave it false -- every refusal above returns before
    // reaching here. Marking it on the CLICK instead would put the prompt away
    // for exactly the operator who needs it: the one whose copy did not happen.
    this.state.recordRecoveryKeyCopied();
    vscode.window.showInformationMessage(
      "MemQL: recovery key copied. Put it somewhere this machine is not -- it will not be shown again.",
    );
  }

  private async signInAsOwner(): Promise<void> {
    const handoff = this.state.handoff;
    if (handoff === undefined || !handoff.ok) return;
    await vscode.commands.executeCommand("memql.clusters.signIn", {
      cluster: handoff.cluster,
      selected: true,
    });
  }

  /**
   * Opens the local checkout the install cloned (memql#4246).
   *
   * Delegates to the shared command rather than reading the receipt and
   * opening the folder itself -- the same reason `enrolPasskey` delegates to
   * `memql.clusters.takeOwnership` rather than minting its own link: the
   * done screen, the Connection page and the Deployments tree must all open
   * the exact same directory the exact same way, not three implementations
   * of "where is the checkout".
   */
  private async openCheckout(): Promise<void> {
    await vscode.commands.executeCommand("memql.deployments.openCheckout");
  }

  /**
   * The done screen's "Open source checkout" button, offered wherever
   * `doneHtml` renders a terminal state (memql#4246) -- exactly the
   * `handoff.ok` branch and the plain terminal below it, per the receipt
   * read once in `startRun` / `reconnectLocal`. Empty when the receipt
   * records no checkout: a cluster registered by hand, or one whose install
   * never reached the clone step, has nowhere for the button to open.
   */
  /**
   * Opens the portal's AI-providers page (epic memql#4440).
   *
   * `asExternalUri` is deliberately NOT used, unlike the claim link: this is a
   * plain address an operator could type, carries no credential and no
   * single-use token, and remote-tunnel port mapping would only rewrite a
   * hostname the browser resolves perfectly well on its own.
   */
  private async openProviderSettings(): Promise<void> {
    const url = this.state.providerSetupUrl;
    if (url === "") return;
    await vscode.env.openExternal(vscode.Uri.parse(url));
  }

  /**
   * The done screen's "Configure AI providers" line, offered only when the run
   * seeded no key (epic memql#4440).
   *
   * The DECISION is `providerSetupUrl`'s, in state/addCluster.ts, where a test
   * can reach it; this method only renders what that getter returned. An empty
   * string renders nothing at all, which is the whole keyed path.
   */
  private providerSettingsBlock(): string {
    const url = this.state.providerSetupUrl;
    if (url === "") return "";
    return `<p>${escapeHtml(
      "No AI provider was configured, which is the ordinary way to install: nothing about running this cluster needs one. When you want agents to think, set a provider up in the portal -- workload identity federation is the recommended path for Anthropic, and needs no key at rest.",
    )}</p>`;
  }

  private providerSettingsButton(): string {
    if (this.state.providerSetupUrl === "") return "";
    return `<button class="secondary" type="button" data-act="openProviderSettings">Configure AI providers</button>`;
  }

  private openCheckoutButton(): string {
    if (recordedStackDir(this.doneReceipt) === "") return "";
    return `<button class="secondary" type="button" data-act="openCheckout">Open source checkout</button>`;
  }

  private doneHtml(): string {
    const handoff = this.state.handoff;

    // A RUN THAT WAS NEVER ATTEMPTED, and the reason it was not.
    //
    // This branch is first because BOTH paths that set `runError` end here:
    // the refusal ahead of the provider-key gate (memql#3512) and a throw out
    // of `runInstall` -- a missing graph document, an unreadable script -- each
    // call `finish()`, which is this screen. The sentence used to be rendered
    // only by `runHtml()`, which neither path can reach, so an operator
    // repairing a cluster with no recorded key was told "Finished / Nothing
    // further to do" -- the exact confident-wrong-report the honest message was
    // written to replace. It said the true thing to nobody.
    if (this.runError !== "") {
      return renderScreen({
        title:
          this.state.action === "repair"
            ? "The repair did not start"
            : "The install did not start",
        actions: `<button class="secondary" type="button" data-act="back">Back</button>`,
        status: `<p class="lede">${escapeHtml(this.runError)}</p>
<p>${escapeHtml(
          "Nothing has been changed on this machine -- the run was refused before its first step.",
        )}</p>`,
      });
    }

    if (handoff !== undefined && !handoff.ok) {
      // A failed registry write is NOT a failed install. Say where the cluster
      // answers, so ten minutes of work are not read as wasted.
      return renderScreen({
        title: "Installed, but not added to your list",
        actions: `<button class="secondary" type="button" data-act="back">Back</button>`,
        status: `<p class="lede">${escapeHtml(handoff.message)}</p>`,
      });
    }

    if (handoff !== undefined && handoff.ok) {
      // THE PASSKEY IS THE PRIMARY ROUTE, and the magic link steps down to
      // secondary when one exists (memql#3408). The install has just minted a
      // single-use enrolment link for this operator; sending them to a magic
      // link instead means waiting on a mailbox a local cluster does not have.
      //
      // The button carries NO href and no token -- the URL is a credential and
      // stays on the host side, read by `enrolPasskey` when this is clicked.
      // WHICH ACTION LEADS is `primaryHandoffAction`'s decision, not this
      // template's (memql#3884). It is the thing that was wrong, and a decision
      // written into an HTML string is one no test can reach -- this file
      // imports `vscode`, so nothing under `node --test` can render it.
      const primary = this.state.primaryHandoffAction;
      const cls = (name: string) => (primary === name ? "primary" : "secondary");
      const enrol = this.state.canEnrol
        ? `<button class="${cls("enrol")}" type="button" data-act="enrolPasskey">Set up a passkey</button>`
        : "";
      const claim = this.state.hasClaimLink
        ? `<button class="${cls("claim")}" type="button" data-act="claimCluster">Claim this cluster</button>`
        : "";
      const signIn = handoff.canSignIn
        ? `<button class="${cls("signIn")}" type="button" data-act="signInAsOwner">Sign in as owner</button>`
        : "";
      // Said only when there is a passkey to explain, and it no longer hurries
      // anybody (memql#3906). The old wording -- "do it now, the link expires"
      // -- was true of a link this screen was holding. Nothing is held now: the
      // link is minted when the button is pressed, so the honest thing to say
      // is what the click DOES, and that this account cannot be signed into
      // until it has been done once.
      const enrolNote = this.state.canEnrol
        ? `<p>${escapeHtml(
            "This cluster's owner account has no way to sign in yet. Setting up a passkey opens your browser once and gives it one; you can come back and do it whenever you like.",
          )}</p>`
        : "";
      // The claim note says what the click DOES, because "claim" is the one
      // word here an operator has no prior model for: it explains that this
      // cluster has no owner account yet and that signing in is what creates
      // one, so closing this screen is a decision rather than an oversight.
      const claimNote =
        primary !== "claim"
          ? ""
          : `<p>${escapeHtml(
              "Nobody owns this cluster yet -- signing in once is what creates your account. This opens your browser with a single-use link that expires shortly, so do it now. Afterwards you can add a passkey from the identity service's devices page.",
            )}</p>`;
      // THE RECOVERY KEY BLOCK, first after the lede (memql#4079). The reveal
      // outranks every offer below it in ordering: the passkey and the claim
      // link can be re-minted whenever the operator likes, while this value is
      // being shown for the only time there will ever be. The key itself is
      // interpolated ESCAPED and carries no control; the Copy button reads it
      // back out of panel state on the host side, so the message channel never
      // carries the credential. The two no-key states render one line each --
      // they ask the operator for different things, which is why the script
      // keeps them tellable apart.
      const recovery = this.recoveryKeyBlock();
      // LAST of the notes and last but one of the buttons (epic memql#4440).
      // It is the only line here that is not about getting INTO the cluster,
      // and an operator who cannot sign in yet has no use for a provider page.
      const providers = this.providerSettingsBlock();
      // THE RECOVERY BLOCK STAYS IN THE DETAILS, BELOW THE BUTTONS, and it is
      // the one place where actions-first needed thinking rather than moving.
      // It carries a credential shown exactly once and its own Reveal button,
      // so hoisting it into the top row would put a one-time secret above the
      // three ways of signing in -- and hoisting only its BUTTON would separate
      // the control from the sentence explaining what it reveals. The ordering
      // doctrine is about where the screen's actions go; this block is a piece
      // of content that happens to contain one.
      return renderScreen({
        title: "Your cluster is ready",
        actions: `${enrol}
  ${claim}
  ${signIn}
  ${this.providerSettingsButton()}
  ${this.openCheckoutButton()}
  <button class="secondary" type="button" data-act="back">Back</button>`,
        status: `<p class="lede">${escapeHtml(
          `"${handoff.cluster.name}" is registered and answers at ${handoff.cluster.endpoint}.`,
        )}</p>`,
        details: `${recovery}
${enrolNote}
${claimNote}
${providers}`,
      });
    }

    // Terminal without a hand-off: a cancel, or an action that registers
    // nothing. Saying "cancelled" plainly beats an empty success screen.
    return renderScreen({
      title: this.state.cancelled ? "Cancelled" : "Finished",
      actions: `${this.openCheckoutButton()}
  <button class="secondary" type="button" data-act="back">Back</button>`,
      status: `<p class="lede">${escapeHtml(
        this.state.cancelled
          ? "Whatever had already run is recorded, and can be uninstalled."
          : "Nothing further to do.",
      )}</p>`,
    });
  }

  /**
   * The recovery-key block of the done screen (memql#4079).
   *
   * One of four renderings, decided by the step's own state:
   *
   *   claimed         the reveal -- heading, the key, Copy, and what to do
   *                   with it. The only time the plaintext will ever be shown.
   *   alreadyClaimed  one line: the value stored earlier is still the live
   *                   one. What repair and upgrade find here.
   *   awaitingOwner   one line: the key is minted after the first sign-in.
   *                   What a fresh install finds, since a cluster is claimed
   *                   by that sign-in.
   *   none            nothing at all -- the step did not run or did not
   *                   succeed, and a block about it would be a guess.
   */
  private recoveryKeyBlock(): string {
    const state = this.state.recoveryKeyState;
    const key = this.state.revealedRecoveryKey;
    if (state === "claimed" && key !== "" && !this.state.recoveryKeyRevealed) {
      // THE REVEAL IS A CLICK, NOT A DEFAULT (memql#4194, audit 1). The value
      // stays out of the DOM entirely until asked for -- this branch renders a
      // button, not a hidden element a style could unhide.
      return `<h2 class="recovery-heading">${escapeHtml("Cluster recovery key -- shown once")}</h2>
<p>${escapeHtml(
        "This cluster minted its break-glass recovery key. It is shown exactly once, on this screen, when you choose to look.",
      )}</p>
<div class="actions">
  <button class="primary" type="button" data-act="revealRecoveryKey">Reveal the recovery key</button>
</div>`;
    }
    if (state === "claimed" && key !== "") {
      return `<h2 class="recovery-heading">${escapeHtml("Cluster recovery key -- shown once")}</h2>
<div class="recovery-key-row">
  <code class="recovery-key">${escapeHtml(key)}</code>
  <button class="secondary" type="button" data-act="copyRecoveryKey">Copy</button>
</div>
<p>${escapeHtml(
        "Store it somewhere this machine is not -- a password manager or a safe. " +
          "It is refused while you can still sign in normally, works exactly once, and can be " +
          "rotated later from the portal. Only its hash is stored, so it cannot be shown again: " +
          "closing this screen is goodbye.",
      )}</p>`;
    }
    if (state === "alreadyClaimed") {
      return `<p>${escapeHtml(
        "Recovery key: claimed earlier. If you no longer have it, rotate it from the portal.",
      )}</p>`;
    }
    if (state === "awaitingOwner") {
      return `<p>${escapeHtml(
        "Recovery key: minted after the first sign-in; claim it from the portal or CLI.",
      )}</p>`;
    }
    return "";
  }

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    // ABORT EVERY RUN IN FLIGHT, FIRST (memql#4614).
    //
    // THE DEFECT. This method released the sudo agent and never touched
    // `runAbort` -- compare the Cancel handler, which has aborted first and
    // transitioned second since the beginning. Closing the tab during a
    // twenty-minute install therefore left the graph running HEADLESS: k3d
    // still built a cluster, `seedBootstrap` still bootstrapped an owner,
    // `recoveryKey` still ROTATED the break-glass credential and revealed the
    // new plaintext into a report nobody would ever read. Then the
    // `if (this.disposed) return` guards further up skipped the hand-off, so no
    // registry entry was written either -- and reopening the page constructs a
    // fresh panel with empty state. The operator was left with a real cluster
    // on their machine that the editor did not know about, and whose recovery
    // key had been generated and thrown away.
    //
    // Aborting is safe to do at any point for the reason Cancel gives: the
    // executor stops at the next wave boundary and the receipt has been written
    // after every step that ran, so what was built remains fully uninstallable.
    const runInFlight = this.runAbort !== undefined || this.uninstallAbort !== undefined;
    this.runAbort?.abort();
    this.uninstallAbort?.abort();
    // THE PASSWORD GOES WITH THE PANEL. Closing the wizard is the clearest
    // possible statement that the operator is done, and a credential that
    // outlived the thing it was given to is a credential nobody is watching.
    //
    // BUT NOT OUT FROM UNDER STEPS THAT ARE STILL RUNNING (memql#4614).
    // `releaseSudoAgent()` `fs.rm`s the askpass socket directory, so releasing
    // it here while a wave was in flight handed every remaining
    // `elevation: "sudo"` step -- `hostsBlock`, `browserTrust`, `localCA` -- an
    // askpass helper pointing at a path that no longer existed, and each of
    // them failed on it. When a run is in flight its own `finally` releases the
    // agent once the executor has come to rest, which is both later and
    // correct; this branch covers the ordinary case of a panel closed with
    // nothing running.
    //
    // `dispose()` cannot await, so this is fire-and-forget either way -- the
    // agent's own dispose is idempotent and the failure mode is a stopped
    // server.
    if (!runInFlight) void this.releaseSudoAgent();
    // AND SAY SO IF THE ONE-TIME RECOVERY KEY WENT WITH IT (memql#4615).
    //
    // NOT A CONFIRMATION, and it cannot be one. `WebviewPanel.onDidDispose` is
    // the only close signal VS Code offers and it fires AFTER the tab is gone;
    // there is no cancellable before-close hook for a webview, so a modal here
    // would be asking permission for something that has already happened. Back
    // gets the confirmation because Back is preventable. What is left for this
    // path is the honest thing: say what was lost and name the one route to
    // another key, rather than letting a credential disappear in silence.
    if (this.state.recoveryKeyWouldBeLost) {
      void vscode.window.showWarningMessage(
        "MemQL: this cluster's recovery key was not copied, and closing the wizard let go of it. " +
          "Only its hash was ever stored, so it cannot be shown again -- rotate the recovery key " +
          "from the portal to get one you have.",
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

/** What the page says about the machine, before it says anything else. */
const VERDICT_LEDE: Record<PresenceVerdict, string> = {
  absent: "No local cluster was found on this machine.",
  "installed-healthy": "A local cluster is installed here and is answering.",
  "installed-unreachable": "A local cluster is installed here, but it is not answering.",
};

/**
 * A recorded key-file value, or "" when what was recorded is not a path.
 *
 * TWO WAYS THE RECEIPT CAN HOLD A NON-PATH, and both end here. Since
 * memql#3545 a key given where a path belonged is stored as `REDACTED`; on a
 * receipt written before that, the key itself is sitting there in plaintext.
 * Neither can be handed to `--key-file`, and "" is precisely what the callers
 * treat as "nothing to go on, ask" -- which is the true state of affairs.
 */
function usablePath(recorded: string): string {
  if (recorded === REDACTED) return "";
  return looksLikeProviderKey(recorded) ? "" : recorded;
}

// A CSP nonce is a security control, so it comes from a CSPRNG.
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
