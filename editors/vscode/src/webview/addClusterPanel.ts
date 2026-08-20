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
import { ownerAccountExistsFrom } from "../install/enrolment.js";
import {
  deleteReceipt,
  readReceipt,
  recordedDomain,
  recordedOwner,
  recordedProvider,
  recordedProviderKeyFile,
  recordedCheckout,
  recordedStackTag,
} from "../install/receipt.js";
import { REDACTED, looksLikeProviderKey } from "../install/secrets.js";
import {
  elevationEnv,
  startSudoAgent,
  sudoAccepts,
  sudoRunsWithoutAsking,
  type SudoAgent,
} from "../install/sudoAgent.js";
import { graphDocumentPath, loadGraphFile, type Graph, type GraphKind } from "../install/graph.js";
import { removalPreviewItems } from "../install/removalPreview.js";
import type { RunScript } from "../install/runner.js";
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
  SUPPORTED_PROVIDERS,
  type ConnectField,
  type InputField,
} from "../state/addCluster.js";
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
  renderRunningScreen,
} from "./installScreens.js";
import type { ExecutionReport } from "../install/executor.js";
import { listReleaseTags } from "../install/tags.js";
import { DEFAULT_STACK_REPO, DEFAULT_STACK_TAG } from "../install/stackPin.js";
import { UninstallRunState } from "../state/uninstallRun.js";

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

const CONNECT_LABELS: Record<ConnectField, string> = {
  name: "Cluster name",
  domain: "Domain",
  endpoint: "gRPC endpoint",
  token: "Access token",
};

const CONNECT_HINTS: Record<ConnectField, string> = {
  name: "How this cluster is stored in clusters.yaml, and what every other MemQL command calls it.",
  domain:
    "Optional, e.g. example.com. It names the identity service sign-in talks to, and composes the endpoint below when you leave that empty.",
  endpoint:
    "The gRPC front door as host:port -- api.<domain>:443 for a cluster behind the usual ingress.",
  token:
    'Optional. The identity-issued JWT from POST <identity>/oauth/token. Leaving it empty and running "MemQL: Sign In" is the ordinary path -- the editor mints its own credential through your browser. A PAT (mql_pat_...) cannot work here.',
};

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
export interface AddClusterDeps {
  /** ~/.memql/clusters.yaml, resolved once at activation. */
  clustersPath: string;
  /** Repaints the Clusters view once an entry lands. */
  refreshTree: () => void;
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
      if (action === "install" || action === "installGuided") void this.loadVersionChoices();
      // A repair now COLLECTS the key fields (memql#3544), so it opens with the
      // recorded answers already in the boxes -- nobody retypes a good path,
      // and a bad one is finally reachable.
      if (action === "repair") void this.prefillFromReceipt();
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
      this.state.back();
      this.render();
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
    if (type === "copyRecoveryKey") {
      void this.copyRecoveryKey();
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

    // A REPAIR HAS NO KEY FIELD, SO IT READS ONE OFF THE RECEIPT (memql#3512).
    //
    // `requiredFields("repair")` is `["domain"]` on purpose -- a repair re-runs
    // the graph over a machine that already recorded these answers. But
    // memql#3473's gate put `providerKey` in front of every mutating step, and
    // `session.ts` drops empty params, so a repair reached wave 2 with no
    // `--key-file` and died there with exit 2 on every invocation. The receipt
    // is the record of what the install did, and `providerKey` writes an entry
    // even though it leaves no artifact, so the path is already on disk.
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
    if (providerKeyFile === "") {
      // REFUSE RATHER THAN START. Without a key path the run cannot pass wave
      // 2, and the failure it would produce is exit 2 -- whose guidance
      // correctly says "a fault in MemQL rather than in your machine", which
      // would be a lie here. Say the true thing before anything runs.
      this.runError =
        "MemQL has no record of an AI provider key for this machine. " +
        "Install rather than repair, so the key can be collected and verified.";
      this.state.finish({ ok: false });
      this.render();
      return;
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
          // Only ever consulted for a `main` install, to pick node images that
          // exist. See imageTagForVersion.
          latestRelease: this.latestRelease(),
          timeoutMs: STEP_TIMEOUT_MS,
          env: this.sudoEnv(),
        }),
        this.hooks({
          onEvent: (event) => {
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
      failure = err instanceof Error ? err.message : String(err);
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

    // THE MAGIC LINK, and on a fresh install it is the ONLY one of the two that
    // exists (memql#3884).
    //
    // The paragraph above describes the enrolment link as the thing an install
    // has just created for this operator. That is true of a re-run against a
    // cluster somebody already claimed, and false of every first install: a
    // cluster is claimed by its FIRST SIGN-IN, so at `enrolmentLink` time there
    // is no account to enrol a passkey for and the step correctly returns
    // nothing, reporting `enrolmentState=awaitingFirstSignIn` and telling the
    // operator to sign in with the magic link this install recovered.
    //
    // That link was recovered, put on the `magicLink` step's result, and then
    // read by nothing at all -- so the install ended by naming a credential it
    // had thrown away, and offered "Sign in as owner" for an account that did
    // not exist. The sign-in then times out and falls back to a device code
    // which cannot complete either, because there is still no account.
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
    void vscode.window.showInformationMessage(
      `MemQL: registered "${draft.name}" at ${draft.endpoint}. ` +
        (draft.token === undefined
          ? 'Run "MemQL: Sign In" to authenticate.'
          : "Select it in the Clusters view to connect."),
    );
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
  :root {
    --vk-fg: var(--vscode-foreground);
    --vk-muted-fg: var(--vscode-descriptionForeground);
    --vk-border: var(--vscode-panel-border);
    --vk-hover-bg: var(--vscode-list-hoverBackground);
    --vk-selected-bg: var(--vscode-list-activeSelectionBackground);
    --vk-selected-fg: var(--vscode-list-activeSelectionForeground);
  }

${viewKitStyles}

  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0;
         padding: 16px 20px; max-width: 780px; }
  h1 { font-size: 1.2em; margin: 0 0 4px; }
  .lede { color: var(--vscode-descriptionForeground); margin: 0 0 16px; }
  .card { display: block; width: 100%; text-align: left; cursor: pointer;
          border: 1px solid var(--vscode-panel-border); border-radius: 4px;
          background: transparent; color: var(--vscode-foreground);
          padding: 10px 12px; margin-bottom: 8px; font: inherit; }
  .card:hover { background: var(--vscode-list-hoverBackground); }
  .card-label { font-weight: 600; }
  .card-detail { color: var(--vscode-descriptionForeground); margin-top: 2px; }
  .card[data-tone="destructive"] .card-label { color: var(--vscode-editorWarning-foreground); }
  .field { margin-bottom: 12px; }
  .field label { display: block; margin-bottom: 3px; }
  .field input, .field select { width: 100%; box-sizing: border-box; padding: 4px 6px; font: inherit;
                 color: var(--vscode-input-foreground);
                 background: var(--vscode-input-background);
                 border: 1px solid var(--vscode-input-border, var(--vscode-panel-border)); }
  /* The box and its Browse button read as one control (memql#3547): the button
     is an alternative way to fill the field beside it, not a separate action. */
  .control-row { display: flex; gap: 6px; align-items: stretch; }
  .control-row input { flex: 1 1 auto; min-width: 0; }
  button.browse { flex: 0 0 auto; white-space: nowrap; }
  .hint { color: var(--vscode-descriptionForeground); margin-top: 3px; }
  /* What the step itself said, as opposed to the generic advice for its code. */
  .said { margin: 0 0 8px; }
  .remedy { font-family: var(--vscode-editor-font-family, monospace);
            background: var(--vscode-textCodeBlock-background, transparent);
            border: 1px solid var(--vscode-panel-border);
            border-radius: 4px; padding: 8px 10px; margin: 6px 0 0;
            overflow-x: auto; white-space: pre; }
  .error { color: var(--vscode-inputValidation-errorForeground,
                   var(--vscode-editorError-foreground)); margin-top: 3px; }
  /* A refusal that belongs to the whole form rather than to one box, so it
     sits away from the fields instead of looking like the last one's. */
  .form-error { margin: 14px 0 0; }
  .field[data-invalid="true"] input, .field[data-invalid="true"] select {
    border-color: var(--vscode-editorError-foreground); }
  /* The one-time recovery key reveal (memql#4079). Monospace and boxed like
     .remedy so the value reads as the credential it is; user-select: all so a
     single click selects the whole 47 characters for the operator who prefers
     the keyboard to the Copy button. Scoped classes, not a global h2 rule --
     the uninstall screen's h2 keeps its own look. */
  .recovery-heading { font-size: 1.05em; margin: 16px 0 4px; }
  .recovery-key-row { display: flex; gap: 6px; align-items: center; margin: 6px 0; }
  .recovery-key { font-family: var(--vscode-editor-font-family, monospace);
                  background: var(--vscode-textCodeBlock-background, transparent);
                  border: 1px solid var(--vscode-panel-border);
                  border-radius: 4px; padding: 6px 8px; overflow-x: auto;
                  user-select: all; }
  .actions { display: flex; gap: 8px; margin-top: 16px; }
  button.primary, button.secondary {
    font: inherit; padding: 4px 12px; cursor: pointer; border-radius: 2px;
    border: 1px solid transparent; }
  button.primary { background: var(--vscode-button-background);
                   color: var(--vscode-button-foreground); }
  button.secondary { background: var(--vscode-button-secondaryBackground);
                     color: var(--vscode-button-secondaryForeground); }
</style>
</head>
<body>
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
  // Escape acts only where a screen has ASKED for it. A page-wide handler
  // would also cancel a screen that never opted in, and "the keystroke did
  // something the screen never offered" is the failure this attribute avoids.
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    const owner = document.querySelector('[data-escape-act]');
    if (owner) vscode.postMessage({ type: owner.dataset.escapeAct });
  });
</script>
</body>
</html>`;
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
    return `<h1>Add a MemQL cluster</h1>
<p class="lede">${escapeHtml(VERDICT_LEDE[this.verdict])}</p>
${cards}`;
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
      latestRelease: this.latestRelease(),
    });
  }

  /**
   * The newest published release, or the compiled-in pin when nothing was
   * listed (memql#3901).
   *
   * `listReleaseTags` already returns newest-first and drops anything that is
   * not a `vX.Y.Z` release, so the head of the list IS the newest release.
   * Falling back to the pin rather than to "" matters: the pin is a real
   * published release, so a `main` install with no network still asks for node
   * images that exist instead of for none.
   *
   * NOTE THIS IS NOT "auto-select the newest", which stackPin.ts argues against
   * at length and which still holds -- nothing here changes what the version
   * field is SET to. It answers a different question: given that the operator
   * chose `main`, which images should it run.
   */
  private latestRelease(): string {
    return this.versionChoices[0] ?? DEFAULT_STACK_TAG;
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
  private async loadVersionChoices(): Promise<void> {
    if (this.versionChoices.length > 0) return;
    // Asked of the REPOSITORY, not of a checkout: the wizard is choosing what
    // to clone, so there is no working tree yet to have an origin.
    const listing = await listReleaseTags({ cwd: process.cwd(), repo: DEFAULT_STACK_REPO });
    if (listing.tags.length === 0) return;
    this.versionChoices = listing.tags;
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
  private connectHtml(): string {
    const values = this.state.connectInputs;
    const errors = this.state.connectErrors;
    const failure = this.state.connectFailure;

    const fields = CONNECT_FIELDS.map((field) => {
      const error = errors.find((e) => e.field === field);
      const secret = CONNECT_SECRET_FIELDS.includes(field);
      return `<div class="field" data-invalid="${error !== undefined}">
  <label for="c-${field}">${escapeHtml(CONNECT_LABELS[field])}</label>
  <input id="c-${field}" type="${secret ? "password" : "text"}"
         data-connect-field="${field}" value="${escapeHtml(values[field])}">
  <div class="hint">${escapeHtml(CONNECT_HINTS[field])}</div>
  ${error === undefined ? "" : `<div class="error">${escapeHtml(error.message)}</div>`}
</div>`;
    }).join("");

    // data-escape-act is what makes Escape mean "discard" HERE and nowhere
    // else on this page: the key listener looks for the attribute rather than
    // acting on every Escape, so a screen that has not opted in -- a run in
    // progress, say -- is not cancelled by a keystroke aimed at a form.
    return `<h1>Connect to an existing cluster</h1>
<p class="lede">Registering a cluster records how to reach it. Nothing is installed and nothing on the cluster is touched.</p>
<div data-escape-act="discard">
${fields}
${failure === "" ? "" : `<p class="error form-error">${escapeHtml(failure)}</p>`}
<div class="actions">
  <button class="primary" type="button" data-connect-act="save">Save cluster</button>
  <button class="secondary" type="button" data-connect-act="discard">Cancel</button>
</div>
</div>`;
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
      return `<h1>Uninstall the local cluster</h1>
<p class="lede">MemQL cannot say what an uninstall would remove, so it will not run one.</p>
<p class="error">${escapeHtml(this.uninstallProblem)}</p>
<div class="actions">
  <button class="secondary" type="button" data-act="uninstallBack">Back</button>
</div>`;
    }

    const preview = this.uninstallPreview;
    if (preview === undefined) {
      return `<h1>Uninstall the local cluster</h1>
<p class="lede">Reading the install receipt to work out exactly what is on this machine.</p>`;
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
    return `<h1>Uninstall the local cluster</h1>
<p class="lede">${escapeHtml(
      "This list is the confirmation -- there is no second prompt. It is built from the " +
        "install receipt, so nothing this machine had before the install is touched.",
    )}</p>
<div data-escape-act="uninstallBack">
${renderToHtml(renderRemovalPreview(items.filter((item) => !this.isShared(item.id))))}
${elevationNote}
${this.sharedToolsHtml()}
<div class="actions">
  <button class="primary" type="button" data-act="uninstallStart">Uninstall -- remove the items above</button>
  <button class="secondary" type="button" data-act="uninstallBack">Cancel</button>
</div>
</div>`;
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

  /** The removal in flight. */
  private uninstallRunningHtml(): string {
    const steps = this.uninstall.steps;
    const body =
      steps.length === 0
        ? `<p class="lede">Starting. The first step will appear here as it begins.</p>`
        : renderToHtml(renderInstallSteps(toStepViews(steps)));
    // Cancel stops at the next wave boundary, so it is offered only while there
    // is a wave left to stop.
    const actions = runIsSettled(steps)
      ? ""
      : `<div class="actions">
  <button class="secondary" type="button" data-act="uninstallCancel">Cancel</button>
</div>`;

    return `<h1>Removing the local cluster</h1>
<p class="lede">${escapeHtml(
      "Each step reverses one entry in the receipt, in the order the graph gives -- each tool " +
        "outlives the artifact it is needed to remove.",
    )}</p>
${body}
${actions}`;
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

    return `<h1>The local cluster is off this machine</h1>
<p class="lede">${escapeHtml(summary)}</p>
${renderToHtml(renderInstallSteps(toStepViews(steps)))}
${followUp}
<div class="actions">
  <button class="secondary" type="button" data-act="uninstallBack">Back</button>
</div>`;
  }

  /** The operator stopped it. */
  private uninstallStoppedHtml(): string {
    return `<h1>Removal stopped</h1>
<p class="lede">${escapeHtml(
      "What had already been removed is gone; everything else is still here. An uninstall " +
        "does not rewrite the receipt, so running it again takes up the rest -- the steps that " +
        "already ran find their artifact missing and do nothing.",
    )}</p>
${renderToHtml(renderInstallSteps(toStepViews(this.uninstall.steps)))}
<div class="actions">
  <button class="primary" type="button" data-act="uninstallStart">Run the rest</button>
  <button class="secondary" type="button" data-act="uninstallBack">Back</button>
</div>`;
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
      return `<h1>The uninstall did not run</h1>
<p class="lede">${escapeHtml(
        this.uninstall.problem === ""
          ? "The removal ended without reporting a step."
          : this.uninstall.problem,
      )}</p>
<div class="actions">
  <button class="secondary" type="button" data-act="uninstallBack">Back</button>
</div>`;
    }

    const guidance = failureGuidance(failed.exitCode);
    return `<h1>${escapeHtml(failed.description === "" ? failed.id : failed.description)} failed</h1>
<p class="lede">${escapeHtml(guidance.headline)}</p>
<p>${escapeHtml(guidance.advice)}</p>
<p>${escapeHtml(
      `The artifact this step names is still on this machine, and the receipt still records it ` +
        `-- an uninstall never rewrites the receipt, so once the cause is dealt with, running it ` +
        `again repeats exactly this list.`,
    )}</p>
${renderToHtml(renderInstallSteps(toStepViews(this.uninstall.steps)))}
<div class="actions">
  <button class="primary" type="button" data-act="uninstallStart">Try the removal again</button>
  <button class="secondary" type="button" data-act="uninstallBack">Back</button>
</div>`;
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
      void vscode.window.showErrorMessage(
        `${detail} The install recovered the link from the identity service's log; you can recover it again there if this screen is gone.`,
      );
    }
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
      vscode.window.showErrorMessage(
        "MemQL: the recovery key could not be copied -- select it in the panel and copy it " +
          `by hand (${err instanceof Error ? err.message : String(err)}).`,
      );
      return;
    }
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
      return `<h1>${escapeHtml(
        this.state.action === "repair" ? "The repair did not start" : "The install did not start",
      )}</h1>
<p class="lede">${escapeHtml(this.runError)}</p>
<p>${escapeHtml(
        "Nothing has been changed on this machine -- the run was refused before its first step.",
      )}</p>
<div class="actions">
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
    }

    if (handoff !== undefined && !handoff.ok) {
      // A failed registry write is NOT a failed install. Say where the cluster
      // answers, so ten minutes of work are not read as wasted.
      return `<h1>Installed, but not added to your list</h1>
<p class="lede">${escapeHtml(handoff.message)}</p>
<div class="actions">
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
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
      return `<h1>Your cluster is ready</h1>
<p class="lede">${escapeHtml(
        `"${handoff.cluster.name}" is registered and answers at ${handoff.cluster.endpoint}.`,
      )}</p>
${recovery}
${enrolNote}
${claimNote}
<div class="actions">
  ${enrol}
  ${claim}
  ${signIn}
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
    }

    // Terminal without a hand-off: a cancel, or an action that registers
    // nothing. Saying "cancelled" plainly beats an empty success screen.
    return `<h1>${escapeHtml(this.state.cancelled ? "Cancelled" : "Finished")}</h1>
<p class="lede">${escapeHtml(
      this.state.cancelled
        ? "Whatever had already run is recorded, and can be uninstalled."
        : "Nothing further to do.",
    )}</p>
<div class="actions">
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
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
    // THE PASSWORD GOES WITH THE PANEL. Closing the wizard is the clearest
    // possible statement that the operator is done, and a credential that
    // outlived the thing it was given to is a credential nobody is watching.
    // `dispose()` cannot await, so this is fire-and-forget -- the agent's own
    // dispose is idempotent and the failure mode is a stopped server.
    void this.releaseSudoAgent();
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
