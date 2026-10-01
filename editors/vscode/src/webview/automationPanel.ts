// The two automation tabs: the trigger-event form, and the step trace.
//
// Both are ADAPTERS, like their counterparts in webview/runPanel.ts. The
// form's mode decision and payload validation live in state/automationForm.ts,
// the trace's ordering and refusal vocabulary in state/stepTrace.ts, the
// markup in webview/automationScreens.ts, and the run itself in
// run/automationRun.ts; this file owns the webview, the postMessage boundary,
// and nothing else. Both are on the page kit: assigned once per screen and
// patched after that, so a half-typed payload keeps its caret while the page
// changes around it, and the trace fills in without repainting.
//
// TWO THINGS MAKE THESE TABS DIFFERENT FROM B2's, and both are deliberate:
//
//  1. THE FORM HAS NO GENERATED FIELDS. There is no `args` block to generate
//     from, so instead of typed inputs there is a payload -- built by picking a
//     real row of the trigger concept, or by pasting JSON. The row picker is
//     the CONCEPTS BROWSER B1 ALREADY BUILT, reused piece for piece: the same
//     paged fetch through the host, the same ConceptPanelState guarding it,
//     the same flattenForList projection and the same keyboard-reachable row
//     list (webview/rowListView.ts).
//     A second row browser would have been a second thing to keep correct
//     about paging, staleness and display cards.
//
//  2. THE RESULT IS A TIMELINE, NOT A ROW LIST. An automation returns no rows;
//     what a developer wants is the sequence -- which steps ran, in what
//     order, how long each took, which one broke. So StepTracePanel renders a
//     rail of ordered step markers and does not touch view-kit's row renderer
//     at all. It fills LIVE: the panel is opened on the accepted frame, before
//     any step exists, and patched as each one lands.
//
// The webview runs under a strict CSP with a per-load nonce. Row data and step
// output are untrusted (whatever the cluster returned) and are escaped, but a
// CSP means an escaping bug cannot become script execution. The postMessage
// channel is untrusted too, so every handler validates shape at runtime.
//
// THE CREDENTIAL IS NEVER RENDERED HERE. Nothing in this file reads a ClusterConfig.

import * as vscode from "vscode";
import { randomBytes } from "node:crypto";

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { viewKitStyles, type ConceptLike } from "@znasllc-io/memql-view-kit";

import { currentBodyThemeAttr, onAppearanceChange } from "./theme.js";

import type { AutomationTarget } from "../constructs/runnable.js";
import type { AutomationRunOutcome, AutomationRunRequest } from "../run/automationRun.js";
import { automationFormPlan, parsePayloadText, payloadTextForRow } from "../state/automationForm.js";
import { ConceptPanelState } from "../state/conceptPanelState.js";
import { flattenForList } from "../state/rowProjection.js";
import { StepTraceModel } from "../state/stepTrace.js";
import {
  AUTOMATION_ACTS,
  AUTOMATION_FIELDS,
  AUTOMATION_PAGE_STYLES,
  TRACE_ACTS,
  automationFormParts,
  conceptEntity,
  traceParts,
} from "./automationScreens.js";
import { pageDocument } from "./ui/document.js";
import { LiveView } from "./ui/liveView.js";
import { pageMessage } from "./ui/protocol.js";

/** What the automation form asks the extension to do when the user acts. */
export interface AutomationPanelHost {
  /** Runs the automation, filling `trace` as frames land and calling onProgress after each. */
  run(
    target: AutomationTarget,
    request: AutomationRunRequest,
    trace: StepTraceModel,
    onProgress: () => void,
  ): Promise<AutomationRunOutcome>;
  /** Persists a named saved run in the workspace. */
  saveConfig(target: AutomationTarget, name: string, request: AutomationRunRequest): Promise<void>;
  /** One page of the trigger concept's rows. Rejects when not connected. */
  browseRows(conceptId: string, cursor: string): Promise<{ rows: Row[]; nextCursor: string }>;
  /** The concept descriptor, so the picker renders the concept's own display card. Undefined before the first list load. */
  concept(conceptId: string): ConceptLike | undefined;
}

/** The two ends of a live view over a panel's webview. */
function liveTarget(panel: vscode.WebviewPanel): { setHtml(html: string): void; postMessage(msg: unknown): unknown } {
  return {
    setHtml: (html) => {
      panel.webview.html = html;
    },
    postMessage: (message) => panel.webview.postMessage(message),
  };
}

/** Both tabs' styles: view-kit's rows and value viewer, and the tabs' layout. */
const STYLES = `${viewKitStyles}\n${AUTOMATION_PAGE_STYLES}`;

// -----------------------------------------------------------------------------
// The trigger-event form
// -----------------------------------------------------------------------------

export class AutomationRunPanel {
  // One panel per automation, keyed by uri+name: re-clicking the lens reveals
  // the tab already holding your half-built payload rather than opening a
  // second one that discards it.
  private static readonly open_ = new Map<string, AutomationRunPanel>();

  private readonly panel: vscode.WebviewPanel;
  private readonly live: LiveView;
  private readonly disposables: vscode.Disposable[] = [];
  // The picker's row list, guarded exactly as the concept page's is: a Reload
  // or a second "Load more" click landing before the first response must not
  // append the same page twice or paint a stale one.
  private readonly rows = new ConceptPanelState<Row>();

  private pickerOpen: boolean;
  private optionsOpen = false;
  private payloadText = "";
  private targetNodeType = "";
  private includeStepOutput = false;
  private payloadError = "";
  private note: { tone: "info" | "error"; line: string; openRuns?: boolean } | undefined;
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
    AutomationRunPanel.open_.set(key, new AutomationRunPanel(context, host, target, key, initial));
  }

  private constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly host: AutomationPanelHost,
    private readonly target: AutomationTarget,
    private readonly key: string,
    initial: AutomationRunRequest | undefined,
  ) {
    // The picker starts open when the trigger names a concept: a real row one
    // click away is what makes an automation genuinely testable.
    this.pickerOpen = this.plan.defaultMode === "row";
    if (initial !== undefined) this.applyRequest(initial);
    this.panel = vscode.window.createWebviewPanel(
      "memqlAutomationRun",
      `Run ${target.name}`,
      vscode.ViewColumn.Active,
      { enableScripts: true },
    );
    this.live = new LiveView(liveTarget(this.panel), (parts, screen) =>
      pageDocument({
        nonce: nonceValue(),
        title: `Run ${this.target.name}`,
        themeAttr: currentBodyThemeAttr(),
        screen,
        styles: STYLES,
        ...parts,
      }),
    );
    this.disposables.push(
      // The palette is a MemQL setting, not the editor's theme, so an OPEN
      // panel restyles when either input moves (memql#4419).
      ...onAppearanceChange(() => {
        this.live.invalidate();
        this.render();
      }),
      this.panel.onDidDispose(() => this.dispose()),
      this.panel.webview.onDidReceiveMessage((msg: unknown) => {
        if (this.live.handleMessage(msg)) return;
        this.onMessage(msg);
      }),
    );
    this.render();
    if (this.pickerOpen) void this.loadPage();
  }

  private get plan() {
    return automationFormPlan(this.target.name, this.target.trigger);
  }

  // adopt refills the form from a saved run.
  private adopt(request: AutomationRunRequest): void {
    this.applyRequest(request);
    this.render();
  }

  private applyRequest(request: AutomationRunRequest): void {
    if (request.payload !== undefined) {
      this.payloadText = payloadTextForRow(request.payload);
      // A saved run carries the payload, not how it was built, so the picker
      // starts closed: the box is the whole form, and every character of it is
      // editable.
      this.pickerOpen = false;
    }
    this.targetNodeType = request.targetNodeType ?? "";
    this.includeStepOutput = request.includeStepOutput === true;
    if (this.targetNodeType !== "" || this.includeStepOutput) this.optionsOpen = true;
  }

  private onMessage(raw: unknown): void {
    const message = pageMessage(raw);
    if (message === undefined) return;
    if (message.type === "input") {
      // One keystroke, or one switch flip. Held, never rendered: the page
      // already shows it.
      const { field, value } = message as { field?: unknown; value?: unknown };
      if (typeof field !== "string" || typeof value !== "string") return;
      if (field === AUTOMATION_FIELDS.payload) this.payloadText = value;
      else if (field === AUTOMATION_FIELDS.targetNodeType) this.targetNodeType = value.trim();
      else if (field === AUTOMATION_FIELDS.includeStepOutput) this.includeStepOutput = value === "true";
      return;
    }
    switch (message.type) {
      case AUTOMATION_ACTS.run:
        void this.doRun();
        return;
      case AUTOMATION_ACTS.saveAs:
        void this.doSave();
        return;
      case AUTOMATION_ACTS.openRuns:
        void vscode.commands.executeCommand("memql.runs.open");
        return;
      case AUTOMATION_ACTS.picker:
        this.pickerOpen = message.open === true;
        // The first page is fetched when the picker is first opened.
        if (this.pickerOpen && !this.rows.settled && !this.rows.loading) void this.loadPage();
        return;
      case AUTOMATION_ACTS.options:
        this.optionsOpen = message.open === true;
        return;
      case AUTOMATION_ACTS.selectRow:
        if (typeof message.value === "string") this.selectRow(message.value);
        return;
      case AUTOMATION_ACTS.loadMore:
        void this.loadPage();
        return;
      case AUTOMATION_ACTS.reload:
        this.rows.reset();
        this.render();
        void this.loadPage();
        return;
    }
  }

  // selectRow copies the picked row INTO the payload box rather than holding a
  // hidden reference to it. What is on screen is then exactly what will be
  // sent, and "pick a row and change one field" needs no extra affordance.
  private selectRow(rowId: string): void {
    const picked = this.rows.nodes.find((row) => String(row.id ?? "") === rowId);
    if (picked === undefined) {
      // Gone from the loaded page (a reload raced the click): the list is
      // re-read rather than the click answered with a sentence to act on.
      this.rows.reset();
      this.render();
      void this.loadPage();
      return;
    }
    // beginSelection() marks the row so it highlights in the list. Its token is
    // deliberately dropped: the loaded page already holds the whole row, which
    // is the only thing the payload needs.
    this.rows.beginSelection(rowId);
    this.payloadText = payloadTextForRow(picked);
    this.payloadError = "";
    this.note = undefined;
    this.render();
  }

  private async loadPage(): Promise<void> {
    const conceptId = this.plan.conceptId;
    if (conceptId === "") return;
    const cursor = this.rows.nextCursor;
    const pending = this.rows.loadPage(() => this.host.browseRows(conceptId, cursor));
    this.render();
    if (await pending) this.render();
  }

  private async doRun(): Promise<void> {
    if (this.busy) return;
    const request = this.buildRequest();
    if (request === undefined) {
      this.render();
      return;
    }
    this.payloadError = "";
    this.note = undefined;
    this.busy = true;
    this.render();

    const trace = new StepTraceModel();
    // The trace panel is opened BEFORE the run resolves and patched on every
    // frame. That is the whole point of the streaming surface: `onAccepted`
    // fires ahead of any step, so the header is on screen while the
    // automation is still running, and each step lands as it completes.
    const outcome = await this.host.run(this.target, request, trace, () => {
      StepTracePanel.show(this.context, this.target, trace);
    });
    this.busy = false;
    if (outcome.status === "declined") this.note = { tone: "info", line: "Cancelled. Nothing ran." };
    this.render();
    if (outcome.status !== "superseded" && outcome.status !== "declined") {
      StepTracePanel.show(this.context, this.target, trace);
    }
  }

  private async doSave(): Promise<void> {
    const request = this.buildRequest();
    if (request === undefined) {
      this.render();
      return;
    }
    // Asked for when it is needed, not kept on the page as a standing field.
    const name = (
      await vscode.window.showInputBox({ prompt: "Name this saved run", placeHolder: this.target.name })
    )?.trim();
    if (name === undefined || name === "") return;
    try {
      await this.host.saveConfig(this.target, name, request);
      this.note = { tone: "info", line: `Saved as "${name}".`, openRuns: true };
    } catch (err) {
      this.note = { tone: "error", line: err instanceof Error ? err.message : String(err) };
    }
    this.render();
  }

  // buildRequest validates the payload and assembles the run request, or
  // returns undefined having recorded the parse error against the box. The
  // JSON is checked HERE, before anything is sent -- a typo is a form error,
  // not a failed run.
  private buildRequest(): AutomationRunRequest | undefined {
    const request: AutomationRunRequest = {};
    if (!this.plan.modes.includes("schedule")) {
      const parsed = parsePayloadText(this.payloadText);
      if (!parsed.ok) {
        this.payloadError = parsed.error;
        return undefined;
      }
      this.payloadError = "";
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
    if (this.disposed) return;
    const plan = this.plan;
    const concept: ConceptLike =
      this.host.concept(plan.conceptId) ?? { id: plan.conceptId, entity: conceptEntity(plan.conceptId) };
    this.live.render(
      "form",
      automationFormParts({
        name: this.target.name,
        trigger: this.target.trigger,
        plan,
        picker: {
          open: this.pickerOpen,
          settled: this.rows.settled,
          loading: this.rows.loading,
          rows: this.rows.nodes.map(flattenForList),
          concept,
          selectedRowId: this.rows.selectedRowId,
          more: this.rows.nextCursor !== "",
          error: this.rows.listError,
        },
        payloadText: this.payloadText,
        payloadError: this.payloadError,
        targetNodeType: this.targetNodeType,
        includeStepOutput: this.includeStepOutput,
        optionsOpen: this.optionsOpen,
        busy: this.busy,
        note: this.note,
      }),
    );
  }
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
  private readonly live: LiveView;
  private readonly disposables: vscode.Disposable[] = [];
  private target: AutomationTarget;
  private trace: StepTraceModel;
  /** Bumped per trace, so each run is a new screen (a fresh scroll). */
  private run = 0;
  private detailsOpen = false;
  private jsonOpen = false;
  private disposed = false;

  static show(context: vscode.ExtensionContext, target: AutomationTarget, trace: StepTraceModel): void {
    const existing = StepTracePanel.current;
    if (existing !== undefined && !existing.disposed) {
      existing.target = target;
      // Re-showing the SAME trace object is the live-update path: the run
      // fills it frame by frame and calls back here to patch the page. Only a
      // different trace starts a new screen.
      if (existing.trace !== trace) {
        existing.trace = trace;
        existing.run += 1;
        existing.detailsOpen = false;
        existing.jsonOpen = false;
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

  private constructor(_context: vscode.ExtensionContext, target: AutomationTarget, trace: StepTraceModel) {
    this.target = target;
    this.trace = trace;
    this.panel = vscode.window.createWebviewPanel(
      "memqlAutomationTrace",
      traceTitle(target),
      { viewColumn: vscode.ViewColumn.Beside, preserveFocus: true },
      { enableScripts: true },
    );
    this.live = new LiveView(liveTarget(this.panel), (parts, screen) =>
      pageDocument({
        nonce: nonceValue(),
        title: traceTitle(this.target),
        themeAttr: currentBodyThemeAttr(),
        screen,
        styles: STYLES,
        ...parts,
      }),
    );
    this.disposables.push(
      // The palette is a MemQL setting, not the editor's theme, so an OPEN
      // panel restyles when either input moves (memql#4419).
      ...onAppearanceChange(() => {
        this.live.invalidate();
        this.render();
      }),
      this.panel.onDidDispose(() => this.dispose()),
      this.panel.webview.onDidReceiveMessage((msg: unknown) => {
        if (this.live.handleMessage(msg)) return;
        const message = pageMessage(msg);
        if (message?.type === TRACE_ACTS.details) this.detailsOpen = message.open === true;
        else if (message?.type === TRACE_ACTS.json) this.jsonOpen = message.open === true;
      }),
    );
    this.render();
  }

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const d of this.disposables.splice(0)) d.dispose();
    if (StepTracePanel.current === this) StepTracePanel.current = undefined;
  }

  private render(): void {
    if (this.disposed) return;
    this.live.render(
      `trace:${this.run}`,
      traceParts({ name: this.target.name, trace: this.trace, detailsOpen: this.detailsOpen, jsonOpen: this.jsonOpen }),
    );
  }
}

function traceTitle(target: AutomationTarget): string {
  return `Trace ${target.name}`;
}

// A CSP nonce is a security control, so it comes from a CSPRNG. Math.random()
// is not one -- its output is predictable from prior draws, which defeats the
// nonce's purpose.
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
