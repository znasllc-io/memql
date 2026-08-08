// The two automation tabs: the trigger-event form, and the step trace.
//
// Both are ADAPTERS, like their B2 counterparts in webview/runPanel.ts. The
// form's mode decision and payload validation live in state/automationForm.ts,
// the trace's ordering and refusal vocabulary in state/stepTrace.ts, and the
// run itself in run/automationRun.ts; this file owns webview HTML, the
// postMessage boundary, and nothing else.
//
// TWO THINGS MAKE THESE TABS DIFFERENT FROM B2's, and both are deliberate:
//
//  1. THE FORM HAS NO GENERATED FIELDS. There is no `args` block to generate
//     from, so instead of typed inputs there is a payload -- built by picking a
//     real row of the trigger concept, or by pasting JSON. The row picker is
//     the CONCEPTS BROWSER B1 ALREADY BUILT, reused piece for piece: the same
//     paged fetch through the host, the same ConceptPanelState guarding it,
//     the same flattenForList projection and the same view-kit renderRowList.
//     A second row browser would have been a second thing to keep correct
//     about paging, staleness and display cards.
//
//  2. THE RESULT IS A TIMELINE, NOT A ROW LIST. An automation returns no rows;
//     what a developer wants is the sequence -- which steps ran, in what
//     order, how long each took, which one broke. So StepTracePanel renders a
//     rail of ordered step markers and does not touch view-kit's row renderer
//     at all. It fills LIVE: the panel is opened on the accepted frame, before
//     any step exists, and repainted as each one lands.
//
// The webview runs under a strict CSP with a per-load nonce. Row data and step
// output are untrusted (whatever the cluster returned) and are escaped, but a
// CSP means an escaping bug cannot become script execution. The postMessage
// channel is untrusted too, so every handler validates shape at runtime.
//
// THE PAT IS NEVER RENDERED HERE. Nothing in this file reads a ClusterConfig.

import * as vscode from "vscode";
import { randomBytes } from "node:crypto";

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import {
  escapeHtml,
  renderRowList,
  renderToHtml,
  viewKitStyles,
  type ConceptLike,
} from "@znasllc-io/memql-view-kit";

import type { AutomationTarget } from "../constructs/runnable.js";
import type {
  AutomationRunOutcome,
  AutomationRunRequest,
} from "../run/automationRun.js";
import {
  TARGET_NODE_TYPE_NOTICE,
  automationFormPlan,
  definitionBanner,
  parsePayloadText,
  payloadTextForRow,
  type AutomationFormMode,
} from "../state/automationForm.js";
import { ConceptPanelState } from "../state/conceptPanelState.js";
import { flattenForList } from "../state/rowProjection.js";
import {
  StepTraceModel,
  describeRefusal,
  formatDuration,
} from "../state/stepTrace.js";

const ROW_PICKER_PAGE_SIZE = 100;

/** What the automation form asks the extension to do when the user acts. */
export interface AutomationPanelHost {
  /** Runs the automation, filling `trace` as frames land and calling onProgress after each. */
  run(
    target: AutomationTarget,
    request: AutomationRunRequest,
    trace: StepTraceModel,
    onProgress: () => void,
  ): Promise<AutomationRunOutcome>;
  /** Persists a named run configuration in the workspace. */
  saveConfig(target: AutomationTarget, name: string, request: AutomationRunRequest): Promise<void>;
  /** One page of the trigger concept's rows. Rejects when not connected. */
  browseRows(conceptId: string, cursor: string): Promise<{ rows: Row[]; nextCursor: string }>;
  /** The concept descriptor, so the picker renders the concept's own display card. Undefined before the first list load. */
  concept(conceptId: string): ConceptLike | undefined;
}

// -----------------------------------------------------------------------------
// The trigger-event form
// -----------------------------------------------------------------------------

export class AutomationRunPanel {
  // One panel per automation, keyed by uri+name: re-clicking the lens reveals
  // the tab already holding your half-built payload rather than opening a
  // second one that discards it.
  private static readonly open_ = new Map<string, AutomationRunPanel>();

  private readonly panel: vscode.WebviewPanel;
  private readonly disposables: vscode.Disposable[] = [];
  // The picker's row list, guarded exactly as the Concepts tab's is: a Reload
  // or a second "Load more" click landing before the first response must not
  // append the same page twice or paint a stale one.
  private readonly rows = new ConceptPanelState<Row>();

  private mode: AutomationFormMode;
  private payloadText = "";
  private targetNodeType = "";
  private includeStepOutput = false;
  private payloadError = "";
  private notice = "";
  private busy = false;
  private disposed = false;

  static open(
    context: vscode.ExtensionContext,
    host: AutomationPanelHost,
    target: AutomationTarget,
    initial?: AutomationRunRequest,
  ): void {
    const key = `${target.uri} automation ${target.name}`;
    const existing = AutomationRunPanel.open_.get(key);
    if (existing !== undefined) {
      if (initial !== undefined) existing.adopt(initial);
      existing.panel.reveal();
      return;
    }
    AutomationRunPanel.open_.set(
      key,
      new AutomationRunPanel(context, host, target, key, initial),
    );
  }

  private constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly host: AutomationPanelHost,
    private readonly target: AutomationTarget,
    private readonly key: string,
    initial: AutomationRunRequest | undefined,
  ) {
    const plan = automationFormPlan(target.name, target.trigger);
    this.mode = plan.defaultMode;
    if (initial !== undefined) this.applyRequest(initial);
    this.panel = vscode.window.createWebviewPanel(
      "memqlAutomationRun",
      `Run automation: ${target.name}`,
      vscode.ViewColumn.Active,
      { enableScripts: true },
    );
    this.disposables.push(
      this.panel.onDidDispose(() => this.dispose()),
      this.panel.webview.onDidReceiveMessage((msg: unknown) => this.onMessage(msg)),
    );
    this.render();
    // The picker's first page is fetched on open rather than on a click: the
    // whole point of row mode is that a real row is one click away, and a
    // picker that starts empty until you ask it to load is a picker nobody
    // uses.
    if (this.mode === "row") void this.loadPage();
  }

  private get plan() {
    return automationFormPlan(this.target.name, this.target.trigger);
  }

  // adopt refills the form from a saved run configuration.
  private adopt(request: AutomationRunRequest): void {
    this.applyRequest(request);
    this.render();
  }

  private applyRequest(request: AutomationRunRequest): void {
    if (request.payload !== undefined) {
      this.payloadText = payloadTextForRow(request.payload);
      // A saved configuration carries the payload, not how it was built, so
      // the form opens on the mode that shows the payload as text -- which is
      // also the mode in which every character of it is editable.
      if (this.plan.modes.includes("json")) this.mode = "json";
    }
    this.targetNodeType = request.targetNodeType ?? "";
    this.includeStepOutput = request.includeStepOutput === true;
  }

  private onMessage(msg: unknown): void {
    if (msg === null || typeof msg !== "object") return;
    const m = msg as Record<string, unknown>;
    const type = m.type;

    // Every message carries the current field values, because render()
    // replaces the webview HTML wholesale and the DOM is therefore not where
    // form state lives. Absorb them first so an action never discards
    // something the user typed.
    if (typeof m.payloadText === "string") this.payloadText = m.payloadText;
    if (typeof m.targetNodeType === "string") this.targetNodeType = m.targetNodeType.trim();
    if (typeof m.includeStepOutput === "boolean") this.includeStepOutput = m.includeStepOutput;

    if (type === "run") {
      void this.doRun();
      return;
    }
    if (type === "save" && typeof m.name === "string") {
      void this.doSave(m.name);
      return;
    }
    if (type === "mode" && typeof m.mode === "string") {
      const mode = m.mode as AutomationFormMode;
      if (!this.plan.modes.includes(mode)) return;
      this.mode = mode;
      this.render();
      if (mode === "row" && this.rows.nodes.length === 0) void this.loadPage();
      return;
    }
    if (type === "selectRow" && typeof m.rowId === "string") {
      this.selectRow(m.rowId);
      return;
    }
    if (type === "loadMore") {
      void this.loadPage();
      return;
    }
    if (type === "reload") {
      this.rows.reset();
      this.render();
      void this.loadPage();
    }
  }

  // selectRow copies the picked row INTO the payload box rather than holding a
  // hidden reference to it. What is on screen is then exactly what will be
  // sent, and "pick a row and change one field" needs no extra affordance.
  private selectRow(rowId: string): void {
    // beginSelection() marks the row so it highlights in the list. Its token is
    // deliberately dropped: the Concepts tab hands it back to
    // resolveSelection() after a detail round-trip, and this picker has no such
    // round-trip -- the loaded page already holds the whole row, which is the
    // only thing the payload needs.
    this.rows.beginSelection(rowId);
    const picked = this.rows.nodes.find((row) => String(row.id ?? "") === rowId);
    if (picked === undefined) {
      this.notice = `Row ${rowId} is no longer in the loaded page. Reload the picker and try again.`;
      this.render();
      return;
    }
    this.payloadText = payloadTextForRow(picked);
    this.payloadError = "";
    this.notice = "";
    this.render();
  }

  private async loadPage(): Promise<void> {
    const conceptId = this.plan.conceptId;
    if (conceptId === "") return;
    const cursor = this.rows.nextCursor;
    const changed = await this.rows.loadPage(() => this.host.browseRows(conceptId, cursor));
    if (changed) this.render();
  }

  private async doRun(): Promise<void> {
    const request = this.buildRequest();
    if (request === undefined) {
      this.render();
      return;
    }
    this.payloadError = "";
    this.notice = "";
    this.busy = true;
    this.render();

    const trace = new StepTraceModel();
    // The trace panel is opened BEFORE the run resolves and repainted on every
    // frame. That is the whole point of the streaming surface: `onAccepted`
    // fires ahead of any step, so the banner and header are on screen while
    // the automation is still running, and each step lands as it completes
    // rather than all at once at the end.
    const outcome = await this.host.run(this.target, request, trace, () => {
      StepTracePanel.show(this.context, this.target, trace);
    });
    this.busy = false;
    if (outcome.status === "declined") {
      this.notice = `Cancelled. ${this.target.name} was not run.`;
    }
    this.render();
    if (outcome.status !== "superseded" && outcome.status !== "declined") {
      StepTracePanel.show(this.context, this.target, trace);
    }
  }

  private async doSave(rawName: string): Promise<void> {
    const name = rawName.trim();
    if (name === "") {
      this.notice = "ERROR: a run configuration needs a name.";
      this.render();
      return;
    }
    const request = this.buildRequest();
    if (request === undefined) {
      this.render();
      return;
    }
    try {
      await this.host.saveConfig(this.target, name, request);
      this.notice = `Saved "${name}" to .memql/runs.json. It is plain text -- open it to edit or commit it.`;
    } catch (err) {
      this.notice = `ERROR: ${err instanceof Error ? err.message : String(err)}`;
    }
    this.render();
  }

  // buildRequest validates the payload and assembles the run request, or
  // returns undefined having recorded the parse error against the box. The
  // JSON is checked HERE, before anything is sent -- a typo is a form error,
  // not a failed run.
  private buildRequest(): AutomationRunRequest | undefined {
    const request: AutomationRunRequest = {};

    if (this.mode !== "schedule") {
      const parsed = parsePayloadText(this.payloadText);
      if (!parsed.ok) {
        this.payloadError = parsed.error;
        return undefined;
      }
      if (parsed.payload !== undefined) request.payload = parsed.payload;
      // The concept is sent alongside the payload so the engine can make a
      // glob trigger pattern concrete. Harmless when the pattern is already
      // concrete, and the difference between a run and an INVALID_ARGUMENT
      // refusal when it is not.
      if (this.plan.conceptId !== "") request.concept = this.plan.conceptId;
    }
    if (this.targetNodeType !== "") request.targetNodeType = this.targetNodeType;
    if (this.includeStepOutput) request.includeStepOutput = true;
    return request;
  }

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const d of this.disposables.splice(0)) d.dispose();
    AutomationRunPanel.open_.delete(this.key);
  }

  private render(): void {
    const nonce = nonceValue();
    const plan = this.plan;

    const modeTabs =
      plan.modes.length < 2
        ? ""
        : `<div class="modes">${plan.modes
            .map(
              (m) =>
                `<button type="button" data-mode="${m}" class="mode${m === this.mode ? " active" : ""}">${escapeHtml(modeLabel(m))}</button>`,
            )
            .join("")}</div>`;

    const pickerHtml = this.mode === "row" ? this.pickerHtml(plan.conceptId) : "";
    const payloadHtml =
      this.mode === "schedule"
        ? `<div class="placeholder">This automation fires with an EMPTY event, exactly as its schedule would deliver it. There is nothing to fill in.</div>`
        : `<div class="field">
  <label for="payload">Trigger event payload<span class="type">JSON object</span></label>
  <div class="desc">Leave empty to fire with an empty event. This is what the automation body reads as <code>args.payload.&lt;field&gt;</code>.</div>
  <textarea id="payload" spellcheck="false">${escapeHtml(this.payloadText)}</textarea>
  ${this.payloadError === "" ? "" : `<div class="err">${escapeHtml(this.payloadError)}</div>`}
</div>`;

    const noticeHtml = this.notice === "" ? "" : `<div class="notice">${escapeHtml(this.notice)}</div>`;

    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>Run automation ${escapeHtml(this.target.name)}</title>
<style nonce="${nonce}">
${panelChrome()}
${viewKitStyles}
  .field { margin: 12px 0; }
  .field label { display: block; font-weight: 600; margin-bottom: 4px; }
  .field .type { color: var(--vscode-descriptionForeground); font-weight: 400; margin-left: 8px; }
  .field .desc { color: var(--vscode-descriptionForeground); margin: 2px 0 6px; }
  .field .err { color: var(--vscode-errorForeground); margin-top: 4px; }
  input[type="text"], textarea { width: 100%; box-sizing: border-box; font-family: var(--vscode-editor-font-family);
    background: var(--vscode-input-background); color: var(--vscode-input-foreground);
    border: 1px solid var(--vscode-input-border, transparent); padding: 4px 6px; }
  textarea { min-height: 9em; }
  .modes { display: flex; gap: 4px; margin: 12px 0 4px; }
  .modes .mode { background: transparent; color: var(--vscode-foreground);
    border: 1px solid var(--vscode-panel-border); }
  .modes .mode.active { background: var(--vscode-button-background); color: var(--vscode-button-foreground);
    border-color: transparent; }
  .picker { border: 1px solid var(--vscode-panel-border); max-height: 40vh; overflow: auto; padding: 4px 8px; }
  .picker-bar { display: flex; gap: 8px; align-items: center; margin: 8px 0 4px; }
  .actions { display: flex; gap: 8px; align-items: center; margin-top: 16px; flex-wrap: wrap; }
  .actions input { width: auto; flex: 1 1 12em; }
  .check { display: flex; gap: 6px; align-items: center; margin: 12px 0; }
  .check input { width: auto; }
</style>
</head>
<body>
<div class="toolbar">
  <strong>automation ${escapeHtml(this.target.name)}</strong>
  <span>${escapeHtml(triggerSummary(this.target))}</span>
</div>
<div class="warning">${escapeHtml(DEPLOYED_FORM_WARNING)}</div>
<div class="notice">${escapeHtml(plan.explanation)}</div>
${noticeHtml}
<div class="pane">
${modeTabs}
${pickerHtml}
${payloadHtml}
  <div class="field">
    <label for="node-type">Run on node type<span class="type">optional</span></label>
    <div class="desc">${escapeHtml(TARGET_NODE_TYPE_NOTICE)}</div>
    <input id="node-type" type="text" value="${escapeHtml(this.targetNodeType)}" placeholder="(the node that receives the request)">
  </div>
  <div class="check">
    <input id="step-output" type="checkbox"${this.includeStepOutput ? " checked" : ""}>
    <label for="step-output">Include each step's output in the trace (can be large)</label>
  </div>
  <div class="actions">
    <button id="run" type="button"${this.busy ? " disabled" : ""}>${this.busy ? "Running..." : "Run automation"}</button>
    <input id="config-name" type="text" placeholder="Name this run configuration">
    <button id="save" type="button">Save configuration</button>
  </div>
</div>
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  function fields() {
    const payload = document.getElementById('payload');
    return {
      payloadText: payload ? payload.value : '',
      targetNodeType: document.getElementById('node-type').value,
      includeStepOutput: document.getElementById('step-output').checked,
    };
  }
  document.getElementById('run').addEventListener('click', () =>
    vscode.postMessage({ type: 'run', ...fields() }));
  document.getElementById('save').addEventListener('click', () =>
    vscode.postMessage({ type: 'save', name: document.getElementById('config-name').value, ...fields() }));
  for (const el of document.querySelectorAll('[data-mode]')) {
    el.addEventListener('click', () => vscode.postMessage({ type: 'mode', mode: el.dataset.mode, ...fields() }));
  }
  const picker = document.getElementById('picker');
  if (picker) {
    // One delegated listener: view-kit emits data attributes, never inline
    // handlers, which is what lets the CSP forbid them outright.
    picker.addEventListener('click', (e) => {
      const row = e.target.closest('[data-row-id]');
      if (row) vscode.postMessage({ type: 'selectRow', rowId: row.dataset.rowId, ...fields() });
    });
  }
  const more = document.getElementById('picker-more');
  if (more) more.addEventListener('click', () => vscode.postMessage({ type: 'loadMore', ...fields() }));
  const reload = document.getElementById('picker-reload');
  if (reload) reload.addEventListener('click', () => vscode.postMessage({ type: 'reload', ...fields() }));
</script>
</body>
</html>`;
  }

  private pickerHtml(conceptId: string): string {
    const concept: ConceptLike = this.host.concept(conceptId) ?? { id: conceptId, entity: conceptId };
    const errorHtml =
      this.rows.error === ""
        ? ""
        : `<div class="error">ERROR: ${escapeHtml(this.rows.error)}</div>`;
    const body =
      this.rows.nodes.length === 0 && this.rows.error === ""
        ? '<div class="placeholder">Loading rows...</div>'
        : renderToHtml(
            renderRowList(
              this.rows.nodes.map(flattenForList),
              concept,
              this.rows.selectedRowId,
            ),
          );
    const more =
      this.rows.nextCursor === ""
        ? ""
        : '<button id="picker-more" type="button">Load more</button>';
    return `<div class="picker-bar">
  <strong>${escapeHtml(conceptId)}</strong>
  <span>${this.rows.nodes.length} loaded</span>
  <button id="picker-reload" type="button">Reload</button>
  ${more}
</div>
${errorHtml}
<div class="picker" id="picker">${body}</div>`;
  }
}

/**
 * DEPLOYED_FORM_WARNING is the form's half of "the UI states that the deployed
 * definition ran".
 *
 * The trace renders the ENGINE's own note, which is the authority. This one is
 * on the form, before the click, for the same reason the tool lens carries its
 * caveat in the tooltip: by the time a banner is on the results surface the
 * developer has already run the thing.
 */
export const DEPLOYED_FORM_WARNING =
  "This runs the DEPLOYED automation on the selected cluster, NOT this buffer. Automations are dispatched by bus subscription rather than resolved by name, so they cannot be session-defined -- edits in your editor have no effect on what runs. Redeploy to run your edits.";

function modeLabel(mode: AutomationFormMode): string {
  switch (mode) {
    case "row":
      return "Pick an existing row";
    case "json":
      return "Paste JSON";
    default:
      return "Fire now";
  }
}

function triggerSummary(target: AutomationTarget): string {
  const t = target.trigger;
  if (t === undefined) return "no trigger reported";
  if (t.schedule !== undefined && t.schedule !== "" && (t.event ?? "") === "") {
    return `@trigger(schedule="${t.schedule}")`;
  }
  const parts: string[] = [];
  if (t.event !== undefined && t.event !== "") parts.push(`event="${t.event}"`);
  if (t.concept !== undefined && t.concept !== "") parts.push(`concept="${t.concept}"`);
  return parts.length === 0 ? "no trigger reported" : `@trigger(${parts.join(", ")})`;
}

// -----------------------------------------------------------------------------
// The step trace
// -----------------------------------------------------------------------------

export class StepTracePanel {
  // A SINGLE trace tab, reused -- the same choice ResultPanel makes, for the
  // same reason: a tab per run would accumulate one per iteration of an
  // edit-redeploy-run loop and the developer only ever looks at the newest.
  private static current: StepTracePanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly disposables: vscode.Disposable[] = [];
  private target: AutomationTarget;
  private trace: StepTraceModel;
  private showRaw = false;
  private disposed = false;

  static show(
    context: vscode.ExtensionContext,
    target: AutomationTarget,
    trace: StepTraceModel,
  ): void {
    const existing = StepTracePanel.current;
    if (existing !== undefined && !existing.disposed) {
      existing.target = target;
      // Re-showing the SAME trace object is the live-update path: the run
      // fills it frame by frame and calls back here to repaint. Only a
      // different trace resets the raw toggle, so toggling raw mid-run is not
      // undone by the next step landing.
      if (existing.trace !== trace) {
        existing.trace = trace;
        existing.showRaw = false;
      }
      existing.panel.title = traceTitle(target);
      existing.render();
      // preserveFocus: the developer is still in the form (or the editor), and
      // stealing focus on every step frame would make the tab unusable.
      existing.panel.reveal(undefined, true);
      return;
    }
    StepTracePanel.current = new StepTracePanel(context, target, trace);
  }

  private constructor(
    _context: vscode.ExtensionContext,
    target: AutomationTarget,
    trace: StepTraceModel,
  ) {
    this.target = target;
    this.trace = trace;
    this.panel = vscode.window.createWebviewPanel(
      "memqlAutomationTrace",
      traceTitle(target),
      { viewColumn: vscode.ViewColumn.Beside, preserveFocus: true },
      { enableScripts: true },
    );
    this.disposables.push(
      this.panel.onDidDispose(() => this.dispose()),
      this.panel.webview.onDidReceiveMessage((msg: unknown) => this.onMessage(msg)),
    );
    this.render();
  }

  private onMessage(msg: unknown): void {
    if (msg === null || typeof msg !== "object") return;
    if ((msg as { type?: unknown }).type === "toggleRaw") {
      this.showRaw = !this.showRaw;
      this.render();
    }
  }

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const d of this.disposables.splice(0)) d.dispose();
    if (StepTracePanel.current === this) StepTracePanel.current = undefined;
  }

  private render(): void {
    const nonce = nonceValue();
    const trace = this.trace;
    const accepted = trace.accepted;

    // The banner is the acceptance criterion made visible, and it is the
    // ENGINE's sentence (accepted.definitionNote), gated on its own
    // ranDeployedDefinition flag -- not a string this client decided on.
    const banner = accepted === undefined ? "" : definitionBanner(accepted);
    const bannerHtml = banner === "" ? "" : `<div class="warning">${escapeHtml(banner)}</div>`;

    const whereHtml = accepted === undefined ? "" : this.whereHtml();

    let outcomeHtml = "";
    if (trace.status === "refused") {
      const refusal = trace.refusal;
      // A REFUSAL IS NOT A FAILED RUN, and the panel says so in as many words:
      // an operator who reads "failed" concludes their automation is broken,
      // when in fact it never started.
      outcomeHtml = `<div class="error"><strong>REFUSED (${escapeHtml(refusal?.codeName ?? "")})</strong> -- the run never started, so there is no step trace.</div>
<p>${escapeHtml(refusal === undefined ? "" : describeRefusal(refusal))}</p>`;
    } else if (trace.status === "error") {
      outcomeHtml = `<div class="error">ERROR: ${escapeHtml(trace.error)}</div>`;
    } else if (trace.status === "failed") {
      const message = trace.complete?.error ?? "";
      outcomeHtml = `<div class="error"><strong>FAILED</strong> -- the automation ran and broke. The steps below are what it managed.${message === "" ? "" : ` ${escapeHtml(message)}`}</div>`;
    } else if (trace.status === "cancelled") {
      outcomeHtml = `<div class="warning">CANCELLED after ${escapeHtml(formatDuration(trace.complete?.durationMs ?? 0))}.</div>`;
    }

    const runIdHtml =
      trace.runId === ""
        ? ""
        : `<p>Run id: <code class="selectable">${escapeHtml(trace.runId)}</code></p>`;

    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(traceTitle(this.target))}</title>
<style nonce="${nonce}">
${panelChrome()}
  code.selectable { user-select: all; }
  pre { white-space: pre-wrap; word-break: break-word; font-family: var(--vscode-editor-font-family);
        margin: 4px 0 0; }
  /* The TIMELINE. Deliberately not a table and emphatically not view-kit's row
     list: an automation returns no rows, and the shape a developer needs to
     read here is a sequence with a spine, not a grid of records. */
  .timeline { list-style: none; margin: 0; padding: 0 0 0 20px; border-left: 2px solid var(--vscode-panel-border); }
  .timeline li { position: relative; padding: 0 0 16px 12px; }
  .timeline li::before { content: ""; position: absolute; left: -27px; top: 4px;
    width: 10px; height: 10px; border-radius: 50%; background: var(--vscode-panel-border); }
  .timeline li.success::before { background: var(--vscode-testing-iconPassed, var(--vscode-charts-green, #3fb950)); }
  .timeline li.failed::before { background: var(--vscode-errorForeground); }
  .timeline li.skipped::before { background: var(--vscode-descriptionForeground); }
  .step-head { display: flex; gap: 10px; align-items: baseline; flex-wrap: wrap; }
  .step-seq { color: var(--vscode-descriptionForeground); font-variant-numeric: tabular-nums; min-width: 2em; }
  .step-id { font-weight: 600; font-family: var(--vscode-editor-font-family); }
  .step-status { text-transform: uppercase; font-size: 0.85em; letter-spacing: 0.04em; }
  .step-status.failed { color: var(--vscode-errorForeground); }
  .step-status.skipped { color: var(--vscode-descriptionForeground); }
  .step-duration { color: var(--vscode-descriptionForeground); font-variant-numeric: tabular-nums; }
  .step-error { color: var(--vscode-errorForeground); margin-top: 4px; }
  .where { color: var(--vscode-descriptionForeground); padding: 4px 12px 8px; display: flex; gap: 16px; flex-wrap: wrap; }
  .summary { padding: 8px 12px; border-top: 1px solid var(--vscode-panel-border); color: var(--vscode-descriptionForeground); }
</style>
</head>
<body>
<div class="toolbar">
  <strong>${escapeHtml(this.target.name)}</strong>
  <span>${escapeHtml(statusLabel(this.trace))}</span>
  <button id="raw" type="button">${this.showRaw ? "Show trace" : "Show raw JSON"}</button>
</div>
${bannerHtml}
${whereHtml}
${outcomeHtml}
<div class="pane">
${this.showRaw ? `<pre>${escapeHtml(this.rawJson())}</pre>` : this.timelineHtml()}
${runIdHtml}
</div>
<div class="summary">${escapeHtml(this.summaryLine())}</div>
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  document.getElementById('raw').addEventListener('click', () => vscode.postMessage({ type: 'toggleRaw' }));
</script>
</body>
</html>`;
  }

  // whereHtml names the nodes. In a mesh this is not decoration: an automation
  // whose steps reach node-scoped integrations behaves differently depending on
  // where it ran, and "requested on X, executed on Y" is the fact that explains
  // an otherwise baffling NOT_FOUND.
  private whereHtml(): string {
    const a = this.trace.accepted;
    const c = this.trace.complete;
    if (a === undefined) return "";
    const parts = [
      `requested on ${a.requestedOnNodeId || "?"} (${a.requestedOnNodeType || "?"})`,
      a.targetNodeType === "" ? "target: the receiving node" : `target: ${a.targetNodeType}`,
    ];
    if (c !== undefined && c.executedOnNodeId !== "") {
      parts.push(`executed on ${c.executedOnNodeId} (${c.executedOnNodeType})`);
    }
    if (a.triggerTopic !== "") parts.push(`topic: ${a.triggerTopic}`);
    else if (a.triggerKind !== "") parts.push(`${a.triggerKind} run, empty event`);
    return `<div class="where">${parts.map((p) => `<span>${escapeHtml(p)}</span>`).join("")}</div>`;
  }

  private timelineHtml(): string {
    const steps = this.trace.steps;
    if (steps.length === 0) {
      if (this.trace.status === "refused") return "";
      return this.trace.settled
        ? '<div class="placeholder">The automation ran and recorded no steps.</div>'
        : '<div class="placeholder">Waiting for the first step...</div>';
    }
    // ORDER IS `sequence`, never arrival -- StepTraceModel.steps sorts, and
    // this renderer does not re-order it. See state/stepTrace.ts.
    return `<ol class="timeline">${steps
      .map((step) => {
        const cls = ["success", "failed", "skipped"].includes(step.status) ? step.status : "";
        const error =
          step.error === "" ? "" : `<div class="step-error">${escapeHtml(step.error)}</div>`;
        const output =
          step.output === undefined
            ? ""
            : `<pre>${escapeHtml(safeJson(step.output))}</pre>`;
        return `<li class="${cls}">
  <div class="step-head">
    <span class="step-seq">${step.sequence}</span>
    <span class="step-id">${escapeHtml(step.stepId === "" ? "(unnamed step)" : step.stepId)}</span>
    <span class="step-status ${cls}">${escapeHtml(step.status === "" ? "unknown" : step.status)}</span>
    <span class="step-duration">${escapeHtml(formatDuration(step.durationMs))}</span>
  </div>
  ${error}
  ${output}
</li>`;
      })
      .join("")}</ol>`;
  }

  private summaryLine(): string {
    const t = this.trace;
    const counts = t.counts;
    const parts = [`${t.steps.length} step${t.steps.length === 1 ? "" : "s"}`];
    if (counts.success > 0) parts.push(`${counts.success} ok`);
    if (counts.failed > 0) parts.push(`${counts.failed} failed`);
    if (counts.skipped > 0) parts.push(`${counts.skipped} skipped`);
    const complete = t.complete;
    if (complete !== undefined) parts.push(`total ${formatDuration(complete.durationMs)}`);
    else if (!t.settled) parts.push("running...");
    return parts.join(" | ");
  }

  private rawJson(): string {
    return safeJson({
      runId: this.trace.runId,
      status: this.trace.status,
      accepted: this.trace.accepted ?? null,
      steps: this.trace.steps,
      complete: this.trace.complete ?? null,
      refusal: this.trace.refusal ?? null,
      error: this.trace.error,
    });
  }
}

function traceTitle(target: AutomationTarget): string {
  return `Trace: ${target.name}`;
}

function statusLabel(trace: StepTraceModel): string {
  switch (trace.status) {
    case "running":
      return "running...";
    case "refused":
      return `refused (${trace.refusal?.codeName ?? ""})`;
    case "error":
      return "not run";
    default:
      return trace.status;
  }
}

function safeJson(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2) ?? "null";
  } catch (err) {
    // A cycle cannot come out of protojson, but the raw-JSON toggle must never
    // be the thing that takes the panel down.
    return `(not serialisable: ${err instanceof Error ? err.message : String(err)})`;
  }
}

// The chrome both automation tabs share, mirroring webview/runPanel.ts's so
// the four run-surface tabs read as one surface. Kept local rather than
// imported across the two files: runPanel.ts's copy is private to it, and
// exporting it would make a shared style token out of what is currently two
// independent adapters.
function panelChrome(): string {
  return `  :root {
    --vk-fg: var(--vscode-foreground);
    --vk-muted-fg: var(--vscode-descriptionForeground);
    --vk-border: var(--vscode-panel-border);
    --vk-hover-bg: var(--vscode-list-hoverBackground);
    --vk-selected-bg: var(--vscode-list-activeSelectionBackground);
    --vk-selected-fg: var(--vscode-list-activeSelectionForeground);
  }
  body { font-family: var(--vscode-font-family); color: var(--vscode-foreground);
         background: var(--vscode-editor-background); margin: 0; padding: 0; }
  .toolbar { padding: 8px 12px; border-bottom: 1px solid var(--vscode-panel-border);
             display: flex; gap: 12px; align-items: center; }
  .pane { overflow: auto; padding: 8px 12px; }
  .placeholder { color: var(--vscode-descriptionForeground); opacity: 0.6; padding: 8px 0; }
  .error { color: var(--vscode-errorForeground); padding: 8px 12px; }
  .warning { color: var(--vscode-editorWarning-foreground); padding: 8px 12px; }
  .notice { color: var(--vscode-descriptionForeground); padding: 8px 12px; }
  button { background: var(--vscode-button-background); color: var(--vscode-button-foreground);
           border: none; padding: 4px 10px; cursor: pointer; border-radius: 2px; }
  button:disabled { opacity: 0.6; cursor: default; }`;
}

// A CSP nonce is a security control, so it comes from a CSPRNG. Math.random()
// is not one -- its output is predictable from prior draws, which defeats the
// nonce's purpose.
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
