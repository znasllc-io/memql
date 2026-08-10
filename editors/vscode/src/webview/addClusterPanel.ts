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
import { completeLocalUninstall } from "../clusters/registry.js";
import { defaultReceiptPath } from "../install/receipt.js";
import { removalPreviewItems } from "../install/removalPreview.js";
import {
  previewUninstall,
  runInstall,
  runUninstall,
  type SessionOptions,
  type UninstallPreview,
} from "../install/session.js";
import {
  AddClusterState,
  requiredFields,
  type ConnectField,
  type InputField,
} from "../state/addCluster.js";
import { failureGuidance, runIsSettled, toStepViews } from "../state/installProgress.js";
import type { ExecutionReport } from "../install/executor.js";
import { UninstallRunState } from "../state/uninstallRun.js";

/** The ids the webview may send. A real guard, not a cast. */
const CHOICE_ACTIONS: readonly AddClusterAction[] = [
  "install",
  "installGuided",
  "connect",
  "repair",
  "uninstall",
];

const INPUT_FIELDS: readonly InputField[] = [
  "domain",
  "ownerFirstName",
  "ownerLastName",
  "ownerEmail",
  "providerKeyFile",
];

/** The label each collected field carries. */
const FIELD_LABELS: Record<InputField, string> = {
  domain: "Domain",
  ownerFirstName: "First name",
  ownerLastName: "Last name",
  ownerEmail: "Email address",
  providerKeyFile: "AI provider key file",
};

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
  name: "How this cluster is stored in clusters.yaml, and what every other memQL command calls it.",
  domain:
    "Optional, e.g. staging.example.com. It names the identity service sign-in talks to, and composes the endpoint below when you leave that empty.",
  endpoint:
    "The gRPC front door as host:port -- cockpit.<domain>:443 for a cluster behind the usual ingress.",
  token:
    'Optional. The identity-issued JWT from POST <identity>/oauth/token. Leaving it empty and running "memQL: Sign In" is the ordinary path -- the editor mints its own credential through your browser. A PAT (mql_pat_...) cannot work here.',
};

/** The token is the one field on this page that should not render as plain text. */
const CONNECT_SECRET_FIELDS: readonly ConnectField[] = ["token"];

/** What each field is for, in one line. */
const FIELD_HINTS: Record<InputField, string> = {
  domain: "The cluster answers at cockpit.<domain>. Defaults are fine if you have no preference.",
  ownerFirstName: "The cluster owner -- you.",
  ownerLastName: "",
  ownerEmail: "Used to create the owner account. A local cluster sends no mail.",
  providerKeyFile:
    "A PATH to a file holding the key, never the key itself: a command line is readable by every process on this machine.",
};

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
   * Drops a cluster's registry entry, its stored credential and its live
   * connection, exactly as the "Remove from list" command does.
   *
   * Injected rather than called directly: the whole operation needs
   * SecretStorage and the ConnectionManager, and a panel that reached for
   * either would be a second opinion about state activation owns.
   */
  removeRegistryEntry: (name: string) => Promise<unknown>;
}

export class AddClusterPanel {
  private static open_: AddClusterPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly state = new AddClusterState();
  private readonly disposables: vscode.Disposable[] = [];
  private verdict: PresenceVerdict = "installed-unreachable";
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
    this.render();
  }

  private constructor(
    context: vscode.ExtensionContext,
    private readonly presence: ClusterPresence,
    private readonly deps: AddClusterDeps,
  ) {
    this.panel = vscode.window.createWebviewPanel(
      "memqlAddCluster",
      "Add a memQL cluster",
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
      this.verdict = (await this.presence.get()).verdict;
    } catch {
      this.verdict = "installed-unreachable";
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
      this.state.chooseAction(action);
      // The duplicate-name check needs a registry to check against, and this
      // is the moment it becomes worth reading one. Nothing waits on it: the
      // read is fast, the operator has four fields to fill first, and a
      // registry that never arrives costs the inline refusal but not the
      // write-time one.
      if (action === "connect") void this.loadRegistry();
      // The itemized list is the confirmation, so it is read the moment the
      // branch opens rather than behind a further click: there is nothing on
      // this screen for the operator to do until it is on screen.
      if (action === "uninstall") void this.loadUninstallPreview();
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
    if (type === "input" && typeof value === "object" && value !== null) {
      const { field, text } = value as { field?: unknown; text?: unknown };
      const known = INPUT_FIELDS.find((f) => f === field);
      if (known === undefined || typeof text !== "string") return;
      this.state.setInput(known, text);
      this.render();
      return;
    }
    if (type === "begin") {
      // Validation and the transition are this panel's, so an incomplete form
      // is refused here rather than nine minutes into a graph.
      //
      // No toast. The run screen itself says what state the run is in, and a
      // popup that announced a run which has not started would be the same lie
      // in a second place -- one the operator cannot dismiss by looking again.
      if (this.state.beginRun()) void this.startRun();
      this.render();
      return;
    }
    // The two recoveries from a failed step (#3474). Both are transitions on
    // the state machine rather than behaviour local to this panel, so the CLI
    // and a future front end recover the same way.
    if (type === "retry") {
      this.state.retry();
      this.render();
      return;
    }
    if (type === "guided") {
      this.state.switchToGuided();
      this.render();
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

    const controller = new AbortController();
    this.runAbort = controller;
    const inputs = this.state.inputs;

    let report: ExecutionReport | undefined;
    let failure: string | undefined;
    try {
      report = await runInstall(
        {
          root: this.deps.installRoot,
          // The same default the uninstall side reads (#3476), so the run that
          // writes the receipt and the run that reverses it cannot disagree
          // about where it lives.
          receiptFile: defaultReceiptPath(),
          skip: new Set<string>(),
          // The graph pins `anthropic` on the providerKey step; this is the
          // same value, passed for the seedBootstrap step that also needs it.
          provider: "anthropic",
          domain: inputs.domain,
          ownerEmail: inputs.ownerEmail,
          ownerFirstName: inputs.ownerFirstName,
          ownerLastName: inputs.ownerLastName,
          // A PATH, never the key. argv is world-readable in `ps`.
          providerKeyFile: inputs.providerKeyFile,
          stepParams: {},
        },
        {
          onEvent: (event) => {
            this.state.apply(event);
            this.render();
          },
          signal: controller.signal,
        },
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
    }

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
    this.state.finish({ ok: report?.ok === true, cancelled: report?.cancelled === true });
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
      `memQL: registered "${draft.name}" at ${draft.endpoint}. ` +
        (draft.token === undefined
          ? 'Run "memQL: Sign In" to authenticate.'
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
      receiptFile: defaultReceiptPath(),
      // Nothing is skipped. A skip list is how an operator narrows an INSTALL;
      // narrowing an uninstall would produce a machine in a state no receipt
      // describes, and this screen offers no such control.
      skip: new Set<string>(),
      // Required by the shape and meaningless to a removal: `provider` names
      // the AI vendor an install seeds a key for.
      provider: "",
      stepParams: {},
    };
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
      this.uninstallPreview = await previewUninstall(this.uninstallOptions());
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
    const controller = new AbortController();
    this.uninstallAbort = controller;
    this.uninstall.begin();
    this.render();

    try {
      const report = await runUninstall(this.uninstallOptions(), {
        onEvent: (event) => {
          this.uninstall.apply(event);
          this.render();
        },
        signal: controller.signal,
      });
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
<title>Add a memQL cluster</title>
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
  .field input { width: 100%; box-sizing: border-box; padding: 4px 6px; font: inherit;
                 color: var(--vscode-input-foreground);
                 background: var(--vscode-input-background);
                 border: 1px solid var(--vscode-input-border, var(--vscode-panel-border)); }
  .hint { color: var(--vscode-descriptionForeground); margin-top: 3px; }
  .error { color: var(--vscode-inputValidation-errorForeground,
                   var(--vscode-editorError-foreground)); margin-top: 3px; }
  /* A refusal that belongs to the whole form rather than to one box, so it
     sits away from the fields instead of looking like the last one's. */
  .form-error { margin: 14px 0 0; }
  .field[data-invalid="true"] input { border-color: var(--vscode-editorError-foreground); }
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
    const act = e.target.closest('[data-act]');
    if (act) vscode.postMessage({ type: act.dataset.act });
  });
  document.addEventListener('input', (e) => {
    const field = e.target.closest('[data-field]');
    if (field) vscode.postMessage({
      type: 'input', value: { field: field.dataset.field, text: field.value } });
  });
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
        return this.placeholderHtml();
    }
  }

  /**
   * The run in progress.
   *
   * The step list is view-kit's `renderInstallSteps`, fed by a projection that
   * lives in state/installProgress.ts -- this method adds no judgement of its
   * own, which is what lets what an operator sees be asserted in the unit lane
   * despite this file importing `vscode`.
   *
   * REPAIR IS THE SAME RUN WITH DIFFERENT WORDING. Every step verifies first
   * and skips when satisfied, so re-running the graph IS the repair; only the
   * heading and the lede differ, and there is no second code path below them.
   */
  private runHtml(): string {
    const steps = this.state.steps;
    const settled = runIsSettled(steps);
    const repair = this.state.action === "repair";

    // An empty list now means the run is starting for real -- `startRun()` is
    // in flight and the first `stepStarted` has not arrived. That claim was
    // false while the invocation was unwired (memql#3487), which is why this
    // text is tied to `runAbort` rather than asserted unconditionally: if no
    // run is in flight and no step has reported, nothing is happening and the
    // screen says so.
    const body =
      steps.length > 0
        ? renderToHtml(renderInstallSteps(toStepViews(steps)))
        : this.runAbort !== undefined
          ? `<p class="lede">Starting. The first step will appear here as it begins.</p>`
          : `<p class="lede">Nothing has been run.</p>`;

    // A run that could not be ATTEMPTED gets its own line. It is not a step
    // failure -- there is no step to retry and no stderr to disclose -- so it
    // must not be dressed as one.
    const errorHtml =
      this.runError === ""
        ? ""
        : `<p class="error">The install could not be started: ${escapeHtml(this.runError)}</p>`;

    // Cancel is offered for exactly as long as there is something to stop.
    // A cancelled run leaves a valid receipt -- what ran, ran, and an uninstall
    // can still take it back -- so this is safe at any point.
    const actions = settled
      ? `<button class="secondary" type="button" data-act="back">Back</button>`
      : `<button class="secondary" type="button" data-act="cancel">Cancel</button>`;

    return `<h1>${escapeHtml(repair ? "Repairing the local cluster" : "Installing a local cluster")}</h1>
<p class="lede">${escapeHtml(
      repair
        ? "Every step checks first and is skipped when it is already satisfied, so only what is actually missing runs."
        : "Each step proves itself before the next one starts.",
    )}</p>
${errorHtml}
${body}
<div class="actions">${actions}</div>`;
  }

  /**
   * A step failed, and what that means.
   *
   * BOTH RECOVERIES ARE ALWAYS OFFERED. `failureGuidance().retryable` says
   * whether an UNCHANGED retry could plausibly differ -- it does not gate the
   * button, because the operator may have fixed the cause in another window
   * while this panel sat here, and we cannot know that.
   */
  private failedHtml(): string {
    const failed = this.state.failed;
    if (failed === undefined) return this.runHtml();
    const guidance = failureGuidance(failed.exitCode);

    return `<h1>${escapeHtml(failed.description === "" ? failed.id : failed.description)} failed</h1>
<p class="lede">${escapeHtml(guidance.headline)}</p>
<p>${escapeHtml(guidance.advice)}</p>
${renderToHtml(renderInstallSteps(toStepViews(this.state.steps)))}
<div class="actions">
  <button class="primary" type="button" data-act="retry">Retry this step</button>
  <button class="secondary" type="button" data-act="guided">Switch this step to guided</button>
  <button class="secondary" type="button" data-act="cancel">Cancel</button>
</div>`;
  }

  /** The cards, straight from addClusterMenu. */
  private landingHtml(): string {
    const choices = addClusterMenu(this.verdict);
    const cards = choices.map((choice) => this.cardHtml(choice)).join("");
    return `<h1>Add a memQL cluster</h1>
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
    const required = new Set(requiredFields(action));
    const values = this.state.inputs;
    const errors = this.state.errors;

    const fields = INPUT_FIELDS.filter((field) => required.has(field))
      .map((field) => {
        const error = errors.find((e) => e.field === field);
        const hint = FIELD_HINTS[field];
        return `<div class="field" data-invalid="${error !== undefined}">
  <label for="f-${field}">${escapeHtml(FIELD_LABELS[field])}</label>
  <input id="f-${field}" data-field="${field}" value="${escapeHtml(values[field])}">
  ${hint === "" ? "" : `<div class="hint">${escapeHtml(hint)}</div>`}
  ${error === undefined ? "" : `<div class="error">${escapeHtml(error.message)}</div>`}
</div>`;
      })
      .join("");

    return `<h1>${escapeHtml(COLLECT_TITLE[action] ?? "Install a local cluster")}</h1>
<p class="lede">Everything is collected before any work starts, so the long part runs unattended.</p>
${fields}
<div class="actions">
  <button class="primary" type="button" data-act="begin">Start</button>
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
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
<p class="lede">memQL cannot say what an uninstall would remove, so it will not run one.</p>
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
            "The marked steps interrupt the run to ask for something outside memQL's own " +
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
${renderToHtml(renderRemovalPreview(items))}
${elevationNote}
<div class="actions">
  <button class="primary" type="button" data-act="uninstallStart">Uninstall -- remove the items above</button>
  <button class="secondary" type="button" data-act="uninstallBack">Cancel</button>
</div>
</div>`;
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
   * The slot the remaining screens fill.
   *
   * Left as a plain region on purpose: #3474 (progress) and #3476 (the
   * uninstall preview) each render their own HTML into it. Guessing at their
   * markup here would be a second thing for them to unpick. The remote form
   * has since claimed its own screen off this slot (connectHtml, #3475).
   */
  private placeholderHtml(): string {
    return `<h1>${escapeHtml(this.state.action ?? "")}</h1>
<p class="lede">This screen lands with its own issue. Nothing has been run.</p>
<div class="actions">
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
  }

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
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

const COLLECT_TITLE: Partial<Record<AddClusterAction, string>> = {
  install: "Install a local cluster",
  installGuided: "Install a local cluster -- guided",
  repair: "Repair the local cluster",
};

// A CSP nonce is a security control, so it comes from a CSPRNG.
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
