// The automation form and the automation trace, built from the kit.
//
//   Run autoJoinSI                     On node.created · participant
//   > Pick a row                                              48 rows
//     ...rows...
//   Event payload
//   [ { "id": ... }                                                  ]
//   > Options
//   ------------------------------------------------------------------
//   o Ready  Runs the deployed version               Save as...  [ Run ]
//
// THE FORM IS WHAT WILL BE SENT. There is no declared argument list to
// generate fields from, so the payload is one JSON box; picking a row fills it
// with that row, whole, and it stays editable. The row picker is a disclosure
// rather than a mode: open, it is the quick way to a real row; closed, the box
// is the whole form. Where the run goes and whether step output comes back are
// options, behind their own disclosure, and the second one is a switch.
//
// THE DEPLOYED VERSION RUNS, and the bar says so before the click -- in five
// words beside the state, not a paragraph above the form.
//
// THE TRACE IS A TIMELINE. Steps in sequence order, each with its status,
// duration, error and output; the outcome as one notice when there is
// something to act on; the node facts and the run id behind Details; the raw
// frames behind "Show JSON".
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { escapeHtml, renderToHtml, renderValueView, type ConceptLike } from "@znasllc-io/memql-view-kit";

import type { RunnableTrigger } from "../constructs/runnable.js";
import { TARGET_NODE_TYPE_NOTICE, definitionBanner, type AutomationFormPlan } from "../state/automationForm.js";
import { briefMessage } from "../state/diagnostics.js";
import { describeRefusal, formatDuration, type StepTraceModel } from "../state/stepTrace.js";
import {
  actionBar,
  button,
  disclosure,
  emptyState,
  facts,
  field,
  head,
  notice,
  skeleton,
  subhead,
  switchRow,
  textInput,
  type Act,
  type FactRow,
} from "./ui/kit.js";
import type { RegionParts } from "./ui/liveView.js";
import { ROW_LIST_STYLES, rowListHtml } from "./rowListView.js";

// -----------------------------------------------------------------------------
// The form
// -----------------------------------------------------------------------------

/** The messages the form posts (plus the runtime's own `input`). */
export const AUTOMATION_ACTS = {
  run: "run",
  saveAs: "saveAs",
  openRuns: "openRuns",
  picker: "picker",
  options: "options",
  selectRow: "selectRow",
  loadMore: "loadMore",
  reload: "reload",
} as const;

/** The form's field names, as the runtime posts them in `input`. */
export const AUTOMATION_FIELDS = {
  payload: "payload",
  targetNodeType: "targetNodeType",
  includeStepOutput: "includeStepOutput",
} as const;

export interface PickerInput {
  open: boolean;
  /** The first page has answered. */
  settled: boolean;
  /** A page read is running. */
  loading: boolean;
  rows: readonly Record<string, unknown>[];
  concept: ConceptLike;
  selectedRowId?: string;
  more: boolean;
  /** A page read's failure, or "". */
  error: string;
}

export interface AutomationFormInput {
  name: string;
  trigger?: RunnableTrigger;
  plan: AutomationFormPlan;
  picker: PickerInput;
  payloadText: string;
  payloadError: string;
  targetNodeType: string;
  includeStepOutput: boolean;
  optionsOpen: boolean;
  busy: boolean;
  note?: { tone: "info" | "error"; line: string; openRuns?: boolean };
}

/** The last segment of a concept id, which is its name: `v1:cognition:space` -> `space`. */
export function conceptEntity(conceptId: string): string {
  const parts = conceptId.split(":");
  return parts[parts.length - 1] ?? conceptId;
}

/** What fires the automation, in a meta line's words -- not its annotation syntax. */
export function triggerMeta(trigger: RunnableTrigger | undefined): string {
  const schedule = trigger?.schedule ?? "";
  const event = trigger?.event ?? "";
  const concept = trigger?.concept ?? "";
  if (schedule !== "" && event === "") return `On schedule · ${schedule}`;
  if (event !== "") return concept === "" ? `On ${event}` : `On ${event} · ${conceptEntity(concept)}`;
  return "Run by hand";
}

function pickerBody(picker: PickerInput): string {
  if (!picker.settled) return skeleton({ shape: "list", rows: 4, label: `Loading ${picker.concept.entity} rows` });
  if (picker.rows.length === 0) {
    if (picker.error !== "") {
      return notice({
        tone: "error",
        line: "Couldn't load rows.",
        next: briefMessage(picker.error, 120),
        acts: [{ act: AUTOMATION_ACTS.reload, label: "Try again" }],
      });
    }
    return emptyState({ line: `No ${picker.concept.entity} rows yet. Paste a payload below.` });
  }
  const list =
    rowListHtml({
      rows: picker.rows,
      concept: picker.concept,
      selectedRowId: picker.selectedRowId,
      act: AUTOMATION_ACTS.selectRow,
      label: `Rows of ${picker.concept.entity}`,
    }) ?? "";
  const acts: Act[] = [];
  if (picker.error !== "") acts.push({ act: AUTOMATION_ACTS.loadMore, label: "Try again" });
  else if (picker.more) acts.push({ act: AUTOMATION_ACTS.loadMore, label: "Load more", busy: picker.loading });
  acts.push({ act: AUTOMATION_ACTS.reload, label: "Reload" });
  const tail =
    picker.error === "" ? "" : `<p class="mq-field-error">Couldn't load more rows.</p>`;
  return `<div class="automation-picker">${list}</div>${tail}<div class="mq-acts automation-picker-acts">${acts
    .map((a) => button({ ...a, tone: "text" }))
    .join("")}</div>`;
}

function stateOf(input: AutomationFormInput): { state: string; tone: "idle" | "busy" | "warn" } {
  if (input.busy) return { state: "Running", tone: "busy" };
  if (input.payloadError !== "") return { state: "Fix the payload", tone: "warn" };
  return { state: "Ready", tone: "idle" };
}

export function automationFormParts(input: AutomationFormInput): RegionParts {
  const headHtml = head({ title: `Run ${input.name}`, meta: triggerMeta(input.trigger) });
  let body = "";
  const schedule = input.plan.modes.includes("schedule");

  if (input.plan.modes.includes("row")) {
    const count = input.picker.settled && input.picker.rows.length > 0 ? `${input.picker.rows.length}${input.picker.more ? "+" : ""} rows` : "";
    body += disclosure({
      act: AUTOMATION_ACTS.picker,
      label: "Pick a row",
      open: input.picker.open,
      meta: count,
      id: "automation-picker",
      bodyHtml: pickerBody(input.picker),
    });
  }

  if (schedule) {
    body += `<p class="mq-empty-line automation-schedule">${escapeHtml(input.plan.explanation)}</p>`;
  } else {
    const invalid = input.payloadError !== "";
    body += `<div class="automation-payload">${field({
      label: "Event payload",
      id: "automation-payload",
      controlHtml:
        `<textarea class="mq-input automation-json" id="automation-payload" data-field="${AUTOMATION_FIELDS.payload}"` +
        ` spellcheck="false" placeholder="{ }" aria-describedby="automation-payload-hint automation-payload-error"` +
        `${invalid ? ' aria-invalid="true"' : ""}>${escapeHtml(input.payloadText)}</textarea>`,
      hint: input.plan.modes.includes("row")
        ? "JSON. Picking a row fills this in. Leave it empty for an empty event."
        : `JSON. ${input.plan.explanation}`,
      error: input.payloadError,
    })}</div>`;
  }

  body += disclosure({
    act: AUTOMATION_ACTS.options,
    label: "Options",
    open: input.optionsOpen,
    id: "automation-options",
    bodyHtml:
      field({
        label: "Run on node type",
        id: "automation-node-type",
        controlHtml: textInput({
          field: AUTOMATION_FIELDS.targetNodeType,
          id: "automation-node-type",
          value: input.targetNodeType,
          placeholder: "Any",
          describedBy: "automation-node-type-hint",
        }),
        hint: TARGET_NODE_TYPE_NOTICE,
      }) +
      switchRow({
        id: "automation-step-output",
        label: "Include step output",
        note: "Adds each step's output to the trace. It can be large.",
        checked: input.includeStepOutput,
        data: { field: AUTOMATION_FIELDS.includeStepOutput },
      }),
  });

  if (input.note !== undefined) {
    const acts: Act[] = input.note.openRuns === true ? [{ act: AUTOMATION_ACTS.openRuns, label: "Open runs.json" }] : [];
    body += notice({ tone: input.note.tone, line: input.note.line, acts });
  }

  const { state, tone } = stateOf(input);
  const acts: Act[] = input.busy
    ? []
    : [
        { act: AUTOMATION_ACTS.saveAs, label: "Save as..." },
        { act: AUTOMATION_ACTS.run, label: "Run", tone: "primary" },
      ];
  return {
    head: headHtml,
    body,
    actions: actionBar({ state, tone, detail: "Runs the deployed version", acts }),
  };
}

// -----------------------------------------------------------------------------
// The trace
// -----------------------------------------------------------------------------

/** The messages the trace page posts. */
export const TRACE_ACTS = {
  details: "details",
  json: "json",
} as const;

export type TraceView = Pick<
  StepTraceModel,
  "accepted" | "complete" | "refusal" | "error" | "runId" | "steps" | "status" | "settled"
>;

export interface TraceInput {
  name: string;
  trace: TraceView;
  detailsOpen: boolean;
  jsonOpen: boolean;
}

function stepsWord(n: number): string {
  return `${n} step${n === 1 ? "" : "s"}`;
}

/** The trace's state in its meta line. */
export function traceMeta(trace: TraceView): string {
  const n = trace.steps.length;
  const took = trace.complete === undefined ? "" : formatDuration(trace.complete.durationMs);
  switch (trace.status) {
    case "running":
      return n === 0 ? "Running" : `Running · ${stepsWord(n)}`;
    case "completed":
      return [`Succeeded`, stepsWord(n), took].filter((p) => p !== "").join(" · ");
    case "failed":
      return [`Failed`, stepsWord(n), took].filter((p) => p !== "").join(" · ");
    case "cancelled":
      return took === "" ? "Cancelled" : `Cancelled after ${took}`;
    case "refused":
      return "Didn't start";
    case "error":
      return "Not run";
  }
}

function statusWord(status: string): string {
  switch (status) {
    case "success":
      return "Succeeded";
    case "failed":
      return "Failed";
    case "skipped":
      return "Skipped";
    case "":
      return "Unknown";
    default:
      return status.charAt(0).toUpperCase() + status.slice(1);
  }
}

function outcomeHtml(trace: TraceView): string {
  switch (trace.status) {
    case "refused": {
      const refusal = trace.refusal;
      if (refusal === undefined) return "";
      // A REFUSAL IS NOT A FAILED RUN: the run never started, so there is no
      // step trace, and the sentence says what to do about it. The engine's
      // own words follow it; the code is in the details.
      return notice({
        tone: "error",
        line: describeRefusal(refusal),
        next: refusal.message.trim() === "" ? undefined : briefMessage(refusal.message, 300),
      });
    }
    case "error":
      return notice({ tone: "error", line: briefMessage(trace.error, 300) });
    case "failed": {
      const failedStep = trace.steps.find((step) => step.status === "failed");
      const message = trace.complete?.error ?? "";
      return notice({
        tone: "error",
        line: failedStep === undefined ? "The automation failed." : `Failed at step ${failedStep.sequence}.`,
        next: message === "" ? undefined : briefMessage(message, 300),
      });
    }
    case "cancelled":
      return notice({ tone: "warn", line: `Cancelled after ${formatDuration(trace.complete?.durationMs ?? 0)}.` });
    default:
      return "";
  }
}

function stepsHtml(trace: TraceView): string {
  if (trace.status === "refused" || trace.status === "error") return "";
  const steps = trace.steps;
  if (steps.length === 0) {
    if (!trace.settled) return subhead("Steps") + skeleton({ shape: "list", rows: 2, label: "Waiting for the first step" });
    return subhead("Steps") + emptyState({ line: "No steps were recorded." });
  }
  // ORDER IS `sequence`, never arrival -- StepTraceModel.steps sorts, and this
  // renderer does not re-order it. See state/stepTrace.ts.
  const items = steps
    .map((step) => {
      const tone = step.status === "success" || step.status === "failed" || step.status === "skipped" ? step.status : "other";
      const error = step.error === "" ? "" : `<p class="mq-field-error">${escapeHtml(step.error)}</p>`;
      // THE SAME RENDERER as every other value surface (memql#3754).
      const output =
        step.output === undefined ? "" : renderToHtml(renderValueView(step.output, { copy: false, expandDepth: 1 }));
      return (
        `<li class="trace-step" data-status="${tone}"><div class="trace-step-head">` +
        `<span class="mq-head-meta trace-seq">${step.sequence}</span>` +
        `<code class="trace-id">${escapeHtml(step.stepId === "" ? "(unnamed step)" : step.stepId)}</code>` +
        `<span class="trace-status">${escapeHtml(statusWord(step.status))}</span>` +
        `<span class="mq-head-meta">${escapeHtml(formatDuration(step.durationMs))}</span>` +
        `</div>${error}${output}</li>`
      );
    })
    .join("");
  const waiting = trace.settled ? "" : skeleton({ shape: "list", rows: 1, label: "Waiting for the next step" });
  return subhead("Steps", String(steps.length)) + `<ol class="trace-steps">${items}</ol>${waiting}`;
}

function detailRows(trace: TraceView): FactRow[] {
  const rows: FactRow[] = [];
  if (trace.runId !== "") rows.push({ label: "Run ID", value: trace.runId, mono: true });
  const a = trace.accepted;
  if (a !== undefined) {
    // In a mesh this is not decoration: an automation whose steps reach
    // node-scoped integrations behaves differently depending on where it ran.
    rows.push({ label: "Requested on", value: `${a.requestedOnNodeId || "?"} (${a.requestedOnNodeType || "?"})`, mono: true });
    rows.push({ label: "Node type", value: a.targetNodeType === "" ? "Any" : a.targetNodeType });
    if (a.triggerTopic !== "") rows.push({ label: "Topic", value: a.triggerTopic, mono: true });
    else if (a.triggerKind !== "") rows.push({ label: "Trigger", value: `${a.triggerKind}, empty event` });
  }
  const c = trace.complete;
  if (c !== undefined && c.executedOnNodeId !== "") {
    rows.push({ label: "Ran on", value: `${c.executedOnNodeId} (${c.executedOnNodeType})`, mono: true });
  }
  if (trace.refusal !== undefined && trace.refusal.codeName !== "") {
    rows.push({ label: "Refusal code", value: trace.refusal.codeName, mono: true });
  }
  return rows;
}

function rawValue(trace: TraceView): unknown {
  return {
    runId: trace.runId,
    status: trace.status,
    accepted: trace.accepted ?? null,
    steps: trace.steps,
    complete: trace.complete ?? null,
    refusal: trace.refusal ?? null,
    error: trace.error,
  };
}

export function traceParts(input: TraceInput): RegionParts {
  const { trace } = input;
  const headHtml = head({ title: input.name, meta: traceMeta(trace) });
  let body = "";
  // The ENGINE's sentence, gated on its own flag -- the authority on what ran.
  // Said once the run has run: not over a refusal (nothing ran), and not in
  // the past tense while it is still running.
  const ran = trace.status === "completed" || trace.status === "failed" || trace.status === "cancelled";
  const banner = trace.accepted === undefined || !ran ? "" : definitionBanner(trace.accepted);
  if (banner !== "") body += `<p class="mq-empty-line trace-note">${escapeHtml(banner)}</p>`;
  body += outcomeHtml(trace);
  body += stepsHtml(trace);
  const details = detailRows(trace);
  if (details.length > 0) {
    body += disclosure({ act: TRACE_ACTS.details, label: "Details", open: input.detailsOpen, id: "trace-details", bodyHtml: facts(details) });
  }
  if (trace.settled) {
    body += disclosure({
      act: TRACE_ACTS.json,
      label: "Show JSON",
      openLabel: "Hide JSON",
      open: input.jsonOpen,
      id: "trace-json",
      bodyHtml: renderToHtml(renderValueView(rawValue(trace), { copy: false })),
    });
  }
  return { head: headHtml, body, actions: "" };
}

/** Panel-local layout for the two automation tabs. */
export const AUTOMATION_PAGE_STYLES = `${ROW_LIST_STYLES}
  .automation-picker { max-height: 40vh; overflow: auto; max-width: 80ch; }
  .automation-picker-acts { margin: 6px 0 0 -6px; gap: 2px; }
  .automation-payload { margin-top: 20px; }
  textarea.mq-input.automation-json { height: auto; min-height: 9em; padding: 6px 8px; resize: vertical;
    font-family: var(--vscode-editor-font-family, ui-monospace, monospace); font-size: 0.923em; }
  .automation-schedule { margin: 0 0 8px; }
  .trace-note { margin: -8px 0 12px; }
  .trace-steps { list-style: none; margin: 0; padding: 0 0 0 18px; max-width: 90ch;
                 border-left: 2px solid var(--memql-border); }
  .trace-step { position: relative; padding: 0 0 14px 12px; }
  .trace-step::before { content: ""; position: absolute; left: -25px; top: 5px; width: 10px; height: 10px;
                        border-radius: 50%; background: var(--memql-border-strong); }
  .trace-step[data-status="success"]::before { background: var(--memql-accent); }
  .trace-step[data-status="failed"]::before { background: var(--memql-danger); }
  .trace-step-head { display: flex; flex-wrap: wrap; align-items: baseline; column-gap: 10px; }
  .trace-seq { min-width: 1.5em; }
  .trace-id { font-weight: 600; padding: 0; background: none; }
  .trace-step[data-status="failed"] .trace-status { color: var(--memql-danger); }
  .trace-step .mq-field-error { margin-top: 3px; }
`;
