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
import {
  AddClusterState,
  requiredFields,
  type ConnectField,
  type InputField,
} from "../state/addCluster.js";
import { failureGuidance, runIsSettled, toStepViews } from "../state/installProgress.js";

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
}

export class AddClusterPanel {
  private static open_: AddClusterPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly state = new AddClusterState();
  private readonly disposables: vscode.Disposable[] = [];
  private verdict: PresenceVerdict = "installed-unreachable";
  private disposed = false;
  private saving = false;

  /**
   * Opens the page, or reveals the one already open.
   *
   * ONE PANEL. A second "Add a cluster" tab would be a second wizard over the
   * same machine, and two runs against one k3d cluster is not a state anything
   * downstream is prepared for.
   */
  static show(
    context: vscode.ExtensionContext,
    presence: ClusterPresence,
    deps: AddClusterDeps,
  ): AddClusterPanel {
    const existing = AddClusterPanel.open_;
    if (existing !== undefined && !existing.disposed) {
      existing.panel.reveal(vscode.ViewColumn.Beside);
      return existing;
    }
    const panel = new AddClusterPanel(context, presence, deps);
    AddClusterPanel.open_ = panel;
    return panel;
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
      this.state.beginRun();
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
      this.state.cancel();
      this.render();
    }
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

    // NO STEPS MEANS NOTHING IS RUNNING, and this must not pretend otherwise.
    //
    // `AddClusterState.apply()` is what populates the list, and nothing calls
    // it yet -- driving `session.ts` needs an install root a packaged extension
    // does not have (memql#3487). An earlier version of this screen said
    // "Starting. The first step will appear here as it begins", which was a
    // claim about a run that had not begun and could not begin. A wizard that
    // reports work it is not doing is worse than one that reports nothing: the
    // operator waits on it.
    const body =
      steps.length === 0
        ? `<p class="lede">Nothing has been run. Starting an install from the editor is not wired up in this build -- see memql#3487.</p>`
        : renderToHtml(renderInstallSteps(toStepViews(steps)));

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
