// The run form and the run result, built from the kit.
//
//   Run spaceParticipants                         Query
//   spaceId
//   [ 01J8...                       ]
//   Required
//   limit
//   [                               ]
//   ------------------------------------------------------------------
//   o Ready                                    Save as...   [ Run ]
//
// THE FORM IS FIELDS AND A FLOOR. Each argument is a kit field: its label, its
// control, and one hint line (what it is, whether it is optional, whether the
// cluster sets it). A closed value set is a select; a boolean the construct
// requires is a switch, an optional one a select with an empty choice, because
// "not supplied" is a real answer the engine treats differently; a JSON type
// is a text area. The act bar carries Run and "Save as...", which asks for the
// saved run's name when pressed rather than keeping a name field on the page.
//
// THE RESULT SAYS WHAT RAN IN ITS META LINE ("12 rows · From this editor"),
// not in a paragraph on every run; rows open in the concept's page on click or
// Enter; the raw value sits behind "Show JSON". It opens in its running shape
// the moment a run starts, so a slow cluster is visible from the click.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import {
  escapeHtml,
  renderToHtml,
  renderValueView,
  type ConceptLike,
} from "@znasllc-io/memql-view-kit";

import type { RunTarget } from "../constructs/runnable.js";
import type { RunOutcome } from "../run/orchestrator.js";
import type { ArgFieldModel } from "../state/argForm.js";
import { kindWord } from "../state/constructCatalog.js";
import { briefMessage } from "../state/diagnostics.js";
import { groupRowsByConcept, resultBannerFor } from "../state/runResult.js";
import { flattenForList } from "../state/rowProjection.js";
import {
  actionBar,
  disclosure,
  emptyState,
  field,
  head,
  notice,
  skeleton,
  subhead,
  switchRow,
  textInput,
  type Act,
} from "./ui/kit.js";
import type { RegionParts } from "./ui/liveView.js";
import { ROW_LIST_STYLES, rowListHtml } from "./rowListView.js";

// -----------------------------------------------------------------------------
// The form
// -----------------------------------------------------------------------------

/** The messages the form posts (plus the runtime's own `input`). */
export const RUN_FORM_ACTS = {
  run: "run",
  saveAs: "saveAs",
  openRuns: "openRuns",
} as const;

/** A note the form shows under its fields, after an act. */
export interface RunFormNote {
  tone: "info" | "error";
  line: string;
  /** Offer to open runs.json beside it. */
  openRuns?: boolean;
}

export interface RunFormInput {
  target: Pick<RunTarget, "kind" | "name">;
  fields: readonly ArgFieldModel[];
  /** Per-field errors from the last attempt, by argument name. */
  errors: Readonly<Record<string, string>>;
  /** Saved values this construct no longer declares: not shown, not sent. */
  orphans: readonly string[];
  busy: boolean;
  note?: RunFormNote;
}

function fieldId(name: string): string {
  return `arg-${name.replace(/[^A-Za-z0-9_-]/g, "_")}`;
}

/** One line under the control: what it is, then what is special about it. */
function hintFor(f: ArgFieldModel): string {
  const parts: string[] = [];
  if (f.description !== "") parts.push(f.description);
  if (f.type === "object" || f.type === "array") parts.push(f.type === "object" ? "A JSON object." : "A JSON array.");
  if (f.type === "any") parts.push("JSON, or plain text.");
  if (!f.required && !f.autoInjected) parts.push("Optional.");
  if (f.autoInjected) parts.push("Set by the cluster; anything entered here is ignored.");
  return parts.join(" ");
}

function controlFor(f: ArgFieldModel, id: string, invalid: boolean, describedBy: string): string {
  const aria = `${invalid ? ' aria-invalid="true"' : ""} aria-describedby="${escapeHtml(describedBy)}"`;
  if (f.enumValues.length > 0) {
    // An optional closed set keeps an empty choice: "not supplied" is a
    // distinct, reachable answer.
    const blank = f.required ? "" : `<option value=""${f.text === "" ? " selected" : ""}></option>`;
    const options = f.enumValues
      .map((v) => `<option value="${escapeHtml(v)}"${v === f.text ? " selected" : ""}>${escapeHtml(v)}</option>`)
      .join("");
    return `<select class="mq-input" id="${id}" data-field="${escapeHtml(f.name)}"${aria}>${blank}${options}</select>`;
  }
  if (f.type === "boolean" && !f.required) {
    const option = (value: string, label: string): string =>
      `<option value="${value}"${f.text === value ? " selected" : ""}>${label}</option>`;
    return (
      `<select class="mq-input" id="${id}" data-field="${escapeHtml(f.name)}"${aria}>` +
      `${option("", "")}${option("true", "true")}${option("false", "false")}</select>`
    );
  }
  if (f.type === "object" || f.type === "array" || f.type === "any") {
    return (
      `<textarea class="mq-input run-json" id="${id}" data-field="${escapeHtml(f.name)}" spellcheck="false"${aria}>` +
      `${escapeHtml(f.text)}</textarea>`
    );
  }
  return textInput({
    field: f.name,
    id,
    value: f.text,
    invalid,
    describedBy,
    enterAct: RUN_FORM_ACTS.run,
    type: "text",
  });
}

function fieldHtml(f: ArgFieldModel, error: string | undefined): string {
  const id = fieldId(f.name);
  // A boolean the construct REQUIRES is a switch: it always holds an answer,
  // and "true" typed into a text box was the old way to give it one.
  if (isSwitchField(f)) {
    const hint = hintFor(f);
    return (
      switchRow({
        id,
        label: f.name,
        note: [hint, error].filter((part) => part !== undefined && part !== "").join(" "),
        checked: f.text.trim().toLowerCase() === "true",
        data: { field: f.name },
      }) + `<div class="run-switch-gap"></div>`
    );
  }
  const invalid = error !== undefined && error !== "";
  const describedBy = `${id}-hint ${id}-error`;
  return field({
    label: f.name,
    id,
    controlHtml: controlFor(f, id, invalid, describedBy),
    hint: hintFor(f),
    error,
  });
}

/** Whether a field is drawn as a switch: a boolean the construct requires. */
export function isSwitchField(f: ArgFieldModel): boolean {
  return f.type === "boolean" && f.required && f.enumValues.length === 0;
}

/**
 * A switch always holds an answer, so a required boolean with no value yet is
 * `false` -- what the switch shows -- rather than an empty field the form would
 * then refuse as "Required" beside a switch that says off.
 */
export function withSwitchDefaults(fields: readonly ArgFieldModel[]): ArgFieldModel[] {
  return fields.map((f) => (isSwitchField(f) && f.text.trim() === "" ? { ...f, text: "false" } : f));
}

function formStateWord(input: RunFormInput): { state: string; tone: "idle" | "busy" | "warn" } {
  if (input.busy) return { state: "Running", tone: "busy" };
  const n = Object.keys(input.errors).length;
  if (n > 0) return { state: n === 1 ? "1 field to fix" : `${n} fields to fix`, tone: "warn" };
  return { state: "Ready", tone: "idle" };
}

export function runFormParts(input: RunFormInput): RegionParts {
  const headHtml = head({ title: `Run ${input.target.name}`, meta: kindWord(input.target.kind) });
  let body = "";
  if (input.orphans.length > 0) {
    body += notice({ tone: "info", line: `Not sent: saved values for arguments it no longer has (${input.orphans.join(", ")}).` });
  }
  body += input.fields.map((f) => fieldHtml(f, input.errors[f.name])).join("");
  if (input.fields.length === 0) body += emptyState({ line: "This takes no arguments." });
  if (input.note !== undefined) {
    const acts: Act[] = input.note.openRuns === true ? [{ act: RUN_FORM_ACTS.openRuns, label: "Open runs.json" }] : [];
    body += notice({ tone: input.note.tone, line: input.note.line, acts });
  }
  const { state, tone } = formStateWord(input);
  const acts: Act[] = input.busy
    ? []
    : [
        { act: RUN_FORM_ACTS.saveAs, label: "Save as..." },
        { act: RUN_FORM_ACTS.run, label: "Run", tone: "primary" },
      ];
  return { head: headHtml, body, actions: actionBar({ state, tone, acts }) };
}

// -----------------------------------------------------------------------------
// The result
// -----------------------------------------------------------------------------

/** The messages the result page posts. */
export const RESULT_ACTS = {
  openRow: "openRow",
  saveAs: "saveAs",
  json: "json",
  copyErrorId: "copyErrorId",
  showProblems: "showProblems",
  selectCluster: "selectCluster",
} as const;

export type ResultInput =
  | { state: "running"; target: Pick<RunTarget, "kind" | "name"> }
  // Never superseded (a newer run paints instead) and never declined (a run
  // that was not confirmed never started, and its form says so).
  | {
      state: "settled";
      outcome: Exclude<RunOutcome, { status: "superseded" | "declined" }>;
      concepts: ReadonlyMap<string, ConceptLike>;
      jsonOpen: boolean;
      /** The values it ran with are known, so the run can be saved from here. */
      canSave?: boolean;
    };

function rowsBody(rows: readonly Record<string, unknown>[], concepts: ReadonlyMap<string, ConceptLike>): string {
  if (rows.length === 0) return emptyState({ line: "No rows." });
  const groups = groupRowsByConcept(rows as Parameters<typeof groupRowsByConcept>[0], concepts);
  return groups
    .map((group) => {
      const title = groups.length > 1 ? subhead(group.concept.entity, String(group.rows.length)) : "";
      const list =
        rowListHtml({
          rows: group.rows.map(flattenForList),
          concept: group.concept,
          act: RESULT_ACTS.openRow,
          data: { "concept-id": group.concept.id },
          label: `Rows of ${group.concept.entity}`,
        }) ?? "";
      return title + list;
    })
    .join("");
}

function toolBody(content: readonly { type: string; text: string }[]): string {
  if (content.length === 0) return emptyState({ line: "The tool returned nothing." });
  return content
    .map((c) => `<pre class="result-text">${escapeHtml(c.text === "" ? `(${c.type})` : c.text)}</pre>`)
    .join("");
}

function jsonDisclosure(value: unknown, open: boolean): string {
  return disclosure({
    act: RESULT_ACTS.json,
    label: "Show JSON",
    openLabel: "Hide JSON",
    open,
    id: "result-json",
    // THE VALUE, not a stringified copy (memql#3754): the viewer collapses,
    // badges and bounds it, which it can only do with the thing itself.
    bodyHtml: renderToHtml(renderValueView(value, { copy: false })),
  });
}

function fileName(filePath: string): string {
  const parts = filePath.split(/[\\/]/);
  return parts[parts.length - 1] ?? filePath;
}

function countText(n: number): string {
  return `${n} row${n === 1 ? "" : "s"}`;
}

export function resultParts(input: ResultInput): RegionParts {
  if (input.state === "running") {
    return {
      head: head({ title: input.target.name, meta: "Running" }),
      body: skeleton({ shape: "list", rows: 5, label: `Running ${input.target.name}` }),
      actions: "",
    };
  }
  const o = input.outcome;
  const title = o.target.name;
  switch (o.status) {
    case "ok": {
      const provenance = resultBannerFor({ ...o, kind: o.target.kind });
      // SAVE WHAT JUST WORKED. A record act (it writes runs.json, nothing on
      // the cluster), so it sits in the head. It is also the one way to save a
      // construct that takes no arguments: its lens runs it straight away,
      // with no form to save from.
      const asideActs: Act[] = input.canSave === true ? [{ act: RESULT_ACTS.saveAs, label: "Save as..." }] : [];
      if (o.toolContent !== undefined) {
        return {
          head: head({ title, meta: provenance, asideActs }),
          body: toolBody(o.toolContent) + jsonDisclosure(o.toolContent, input.jsonOpen),
          actions: "",
        };
      }
      return {
        // The count only when there are rows: "0 rows" over "No rows." is
        // the same fact twice.
        head: head({ title, meta: o.rows.length === 0 ? provenance : `${countText(o.rows.length)} · ${provenance}`, asideActs }),
        body: rowsBody(o.rows, input.concepts) + (o.rows.length === 0 ? "" : jsonDisclosure(o.raw, input.jsonOpen)),
        actions: "",
      };
    }
    case "invalid": {
      const items = o.diagnostics
        .map(
          (d) =>
            `<li>${escapeHtml(d.message)}${
              // The file's NAME and the position: the path is absolute, and the
              // Problems panel (one click away) carries all of it.
              d.fileLevel
                ? ""
                : ` <span class="mq-head-meta">${escapeHtml(`${fileName(d.path)}:${d.start.line + 1}:${d.start.character + 1}`)}</span>`
            }</li>`,
        )
        .join("");
      return {
        head: head({ title, meta: "Didn't compile" }),
        body:
          notice({
            tone: "error",
            line: "It didn't compile, so nothing ran.",
            acts: [{ act: RESULT_ACTS.showProblems, label: "Show Problems" }],
          }) + `<ul class="result-diagnostics">${items}</ul>`,
        actions: "",
      };
    }
    case "error": {
      // Refused before anything was sent: the fix is a connection.
      const disconnected = o.phase === "preflight" && /^Not connected/.test(o.message);
      const acts: Act[] = [];
      if (disconnected) acts.push({ act: RESULT_ACTS.selectCluster, label: "Select a cluster" });
      // The ERR- id is the only handle on the server-side log entry: shown
      // apart from the prose, and one click from the clipboard.
      if (o.errorId !== "") acts.push({ act: RESULT_ACTS.copyErrorId, label: "Copy error ID" });
      // The id is lifted OUT of the message (run/call.ts), so the message
      // already carries it; it is added only when the brief cut it off.
      const brief = briefMessage(o.message, 400);
      return {
        head: head({ title, meta: "Failed" }),
        body: notice({
          tone: "error",
          line: disconnected ? o.message : "The run failed.",
          next: disconnected ? undefined : o.errorId === "" || brief.includes(o.errorId) ? brief : `${brief} · ${o.errorId}`,
          acts,
        }),
        actions: "",
      };
    }
  }
}

/** Panel-local layout for the two run tabs. */
export const RUN_PAGE_STYLES = `${ROW_LIST_STYLES}
  textarea.mq-input.run-json { height: auto; min-height: 6.5em; padding: 6px 8px; resize: vertical;
    font-family: var(--vscode-editor-font-family, ui-monospace, monospace); font-size: 0.923em; }
  select.mq-input { padding-right: 4px; }
  .run-switch-gap { height: 10px; }
  .result-text { margin: 0 0 8px; white-space: pre-wrap; overflow-wrap: anywhere;
    font-family: var(--vscode-editor-font-family, ui-monospace, monospace); font-size: 0.923em; }
  .result-diagnostics { margin: 8px 0 0; padding-left: 20px; max-width: 80ch; }
  .result-diagnostics li { margin: 4px 0; }
`;
