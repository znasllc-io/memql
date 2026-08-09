// The two run tabs: the argument form, and the result.
//
// Both are ADAPTERS. The form model and its coercion live in
// state/argForm.ts, the result projection and banners in state/runResult.ts,
// and the run itself in run/orchestrator.ts; this file owns the webview HTML,
// the postMessage boundary, and nothing else.
//
// Rendering goes through view-kit for the same reason the concept browser's
// does: rows plus each concept's own @displayCard, no result-specific renderer
// and no concept-specific code anywhere, so a concept declared five minutes
// ago renders with no client change -- and the portal inherits the renderer
// rather than rebuilding it.
//
// The webview runs under a strict CSP with a per-load nonce. Result data is
// untrusted (it is whatever the cluster returned) and view-kit escapes it, but
// a CSP means an escaping bug cannot become script execution. The postMessage
// channel is untrusted too, so the handler validates shape at runtime rather
// than trusting the compile-time annotation.
//
// THE PAT IS NEVER RENDERED HERE. Nothing in this file reads a ClusterConfig;
// the orchestrator receives a RunCluster carrying only name, label and the
// local flag, precisely so a credential cannot reach a webview by accident.

import * as vscode from "vscode";
import { randomBytes } from "node:crypto";

import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";
import {
  escapeHtml,
  renderRowList,
  renderToHtml,
  viewKitStyles,
  type ConceptLike,
} from "@znasllc-io/memql-view-kit";

import type { RunTarget } from "../constructs/runnable.js";
import type { RunOutcome } from "../run/orchestrator.js";
import {
  AUTO_INJECTED_FIELD_NOTE,
  buildFields,
  coerceArgs,
  orphanedValueNames,
  type ArgFieldModel,
} from "../state/argForm.js";
import { groupRowsByConcept, resultBannerFor } from "../state/runResult.js";

/** What the arg form asks the extension to do when the user acts. */
export interface RunPanelHost {
  /** Run the construct with these values. */
  run(target: RunTarget, values: Record<string, unknown>): Promise<RunOutcome>;
  /** Persist a named run configuration in the workspace. */
  saveConfig(target: RunTarget, name: string, values: Record<string, unknown>): Promise<void>;
  /** The concept descriptors for result rendering; empty before the first list load. */
  concepts(): ReadonlyMap<string, ConceptLike>;
  /** Opens a row in the Concepts surface. */
  openRow(conceptId: string, rowId: string): void;
}

export function conceptMap(concepts: readonly Concept[]): Map<string, ConceptLike> {
  return new Map(concepts.map((c) => [c.id, c]));
}

// -----------------------------------------------------------------------------
// The argument form
// -----------------------------------------------------------------------------

export class RunPanel {
  // One panel per construct, keyed by uri+name: re-clicking Run with... on the
  // same signature reveals the tab already holding your half-typed arguments
  // rather than opening a second one that discards them.
  private static readonly open_ = new Map<string, RunPanel>();

  private readonly panel: vscode.WebviewPanel;
  private readonly disposables: vscode.Disposable[] = [];
  private fields: ArgFieldModel[];
  private errors: Record<string, string> = {};
  private notice = "";
  private busy = false;
  private disposed = false;

  static open(
    context: vscode.ExtensionContext,
    host: RunPanelHost,
    target: RunTarget,
    values: Record<string, unknown> = {},
  ): void {
    const key = panelKey(target);
    const existing = RunPanel.open_.get(key);
    if (existing !== undefined) {
      existing.adopt(target, values);
      existing.panel.reveal();
      return;
    }
    RunPanel.open_.set(key, new RunPanel(context, host, target, values, key));
  }

  private constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly host: RunPanelHost,
    private target: RunTarget,
    values: Record<string, unknown>,
    private readonly key: string,
  ) {
    this.fields = buildFields(target.args, values);
    this.notice = noticeFor(target, values);
    this.panel = vscode.window.createWebviewPanel(
      "memqlRun",
      `Run: ${target.name}`,
      vscode.ViewColumn.Active,
      { enableScripts: true },
    );
    this.disposables.push(
      this.panel.onDidDispose(() => this.dispose()),
      this.panel.webview.onDidReceiveMessage((msg: unknown) => this.onMessage(msg)),
    );
    this.render();
  }

  // adopt refreshes the target (the construct's args may have changed under an
  // edit) while keeping whatever the user has already typed.
  private adopt(target: RunTarget, values: Record<string, unknown>): void {
    this.target = target;
    const current = this.currentText();
    const merged: Record<string, unknown> = { ...values };
    for (const [name, text] of Object.entries(current)) {
      if (text !== "") merged[name] = text;
    }
    this.fields = buildFields(target.args, merged);
    this.notice = noticeFor(target, values);
    this.panel.title = `Run: ${target.name}`;
    this.render();
  }

  private currentText(): Record<string, string> {
    const out: Record<string, string> = {};
    for (const f of this.fields) out[f.name] = f.text;
    return out;
  }

  private onMessage(msg: unknown): void {
    if (msg === null || typeof msg !== "object") return;
    const { type, values, name } = msg as {
      type?: unknown;
      values?: unknown;
      name?: unknown;
    };
    if (!isStringMap(values)) return;
    // Keep what the user typed even when the action fails, so a validation
    // error does not blank the form.
    this.fields = buildFields(this.target.args, values as Record<string, unknown>);

    if (type === "run") {
      void this.doRun(values);
    } else if (type === "save" && typeof name === "string") {
      void this.doSave(name, values);
    }
  }

  private async doRun(raw: Record<string, string>): Promise<void> {
    const coerced = coerceArgs(this.target.args, raw);
    if (!coerced.ok) {
      this.errors = coerced.errors;
      this.render();
      return;
    }
    this.errors = {};
    this.busy = true;
    this.render();
    const outcome = await this.host.run(this.target, coerced.values);
    this.busy = false;
    this.render();
    ResultPanel.show(this.context, this.host, outcome);
  }

  private async doSave(name: string, raw: Record<string, string>): Promise<void> {
    const trimmed = name.trim();
    if (trimmed === "") {
      this.errors = {};
      this.notice = "ERROR: a run configuration needs a name.";
      this.render();
      return;
    }
    const coerced = coerceArgs(this.target.args, raw);
    if (!coerced.ok) {
      this.errors = coerced.errors;
      this.render();
      return;
    }
    this.errors = {};
    try {
      await this.host.saveConfig(this.target, trimmed, coerced.values);
      this.notice = `Saved "${trimmed}" to .memql/runs.json. It is plain text -- open it to edit or commit it.`;
    } catch (err) {
      this.notice = `ERROR: ${err instanceof Error ? err.message : String(err)}`;
    }
    this.render();
  }

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const d of this.disposables.splice(0)) d.dispose();
    RunPanel.open_.delete(this.key);
  }

  private render(): void {
    const nonce = nonceValue();
    const fieldsHtml = this.fields.map((f) => this.fieldHtml(f)).join("\n");
    const noticeHtml =
      this.notice === "" ? "" : `<div class="notice">${escapeHtml(this.notice)}</div>`;

    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>Run ${escapeHtml(this.target.name)}</title>
<style nonce="${nonce}">
${panelChrome()}
  .field { margin: 12px 0; }
  .field label { display: block; font-weight: 600; margin-bottom: 4px; }
  .field .req { color: var(--vscode-errorForeground); margin-left: 4px; }
  .field .type { color: var(--vscode-descriptionForeground); font-weight: 400; margin-left: 8px; }
  .field .desc { color: var(--vscode-descriptionForeground); margin: 2px 0 6px; }
  .field .auto { color: var(--vscode-descriptionForeground); font-style: italic; margin: 2px 0 6px; }
  .field .autotag { color: var(--vscode-descriptionForeground); font-weight: 400; margin-left: 8px; }
  .field .err { color: var(--vscode-errorForeground); margin-top: 4px; }
  input, select, textarea { width: 100%; box-sizing: border-box; font-family: var(--vscode-editor-font-family);
    background: var(--vscode-input-background); color: var(--vscode-input-foreground);
    border: 1px solid var(--vscode-input-border, transparent); padding: 4px 6px; }
  textarea { min-height: 5em; }
  .actions { display: flex; gap: 8px; align-items: center; margin-top: 16px; flex-wrap: wrap; }
  .actions input { width: auto; flex: 1 1 12em; }
</style>
</head>
<body>
<div class="toolbar">
  <strong>${escapeHtml(this.target.kind)} ${escapeHtml(this.target.name)}</strong>
  <span>${this.fields.length} argument${this.fields.length === 1 ? "" : "s"}</span>
</div>
${noticeHtml}
<div class="pane">
  <form id="form">
${fieldsHtml === "" ? '<div class="placeholder">This construct takes no arguments.</div>' : fieldsHtml}
    <div class="actions">
      <button id="run" type="button"${this.busy ? " disabled" : ""}>${this.busy ? "Running..." : "Run"}</button>
      <input id="config-name" type="text" placeholder="Name this run configuration">
      <button id="save" type="button">Save configuration</button>
    </div>
  </form>
</div>
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  function values() {
    const out = {};
    for (const el of document.querySelectorAll('[data-arg]')) out[el.dataset.arg] = el.value;
    return out;
  }
  document.getElementById('run').addEventListener('click', () =>
    vscode.postMessage({ type: 'run', values: values() }));
  document.getElementById('save').addEventListener('click', () =>
    vscode.postMessage({ type: 'save', name: document.getElementById('config-name').value, values: values() }));
</script>
</body>
</html>`;
  }

  private fieldHtml(f: ArgFieldModel): string {
    const id = `arg-${escapeHtml(f.name)}`;
    const required = f.required ? '<span class="req" title="required">*</span>' : "";
    const desc = f.description === "" ? "" : `<div class="desc">${escapeHtml(f.description)}</div>`;
    // The @autoInjected marker sits on the FIELD, not on the form. The label
    // gets the tag so it is visible without reading, and the caption below
    // says what it means. The input stays editable and the value is still
    // submitted -- the engine drops it, and pretending otherwise here would put
    // the extension and the engine out of step (memql#3333).
    const autoTag = f.autoInjected ? '<span class="autotag" title="@autoInjected">engine-supplied</span>' : "";
    const autoNote = f.autoInjected
      ? `<div class="auto">${escapeHtml(AUTO_INJECTED_FIELD_NOTE)}</div>`
      : "";
    const error = this.errors[f.name];
    const errorHtml = error === undefined ? "" : `<div class="err">${escapeHtml(error)}</div>`;

    let control: string;
    if (f.enumValues.length > 0) {
      // The DSL's own closed set becomes a dropdown. An optional enum keeps a
      // blank entry, because "not supplied" is a distinct, reachable choice.
      const blank = f.required ? "" : `<option value=""${f.text === "" ? " selected" : ""}></option>`;
      const options = f.enumValues
        .map(
          (v) =>
            `<option value="${escapeHtml(v)}"${v === f.text ? " selected" : ""}>${escapeHtml(v)}</option>`,
        )
        .join("");
      control = `<select id="${id}" data-arg="${escapeHtml(f.name)}">${blank}${options}</select>`;
    } else if (f.type === "object" || f.type === "array" || f.type === "any") {
      control = `<textarea id="${id}" data-arg="${escapeHtml(f.name)}" spellcheck="false">${escapeHtml(f.text)}</textarea>`;
    } else {
      control = `<input id="${id}" data-arg="${escapeHtml(f.name)}" type="text" value="${escapeHtml(f.text)}">`;
    }

    return `<div class="field">
  <label for="${id}">${escapeHtml(f.name)}${required}<span class="type">${escapeHtml(f.type)}</span>${autoTag}</label>
  ${desc}
  ${autoNote}
  ${control}
  ${errorHtml}
</div>`;
  }
}

function noticeFor(target: RunTarget, values: Record<string, unknown>): string {
  const orphans = orphanedValueNames(target.args, values);
  if (orphans.length === 0) return "";
  // Silently dropping them would run something the saved configuration does
  // not say; carrying them in a hidden field would be worse still.
  return `This run configuration sets ${orphans.join(", ")}, which ${target.name} no longer declares. Those values are not shown and will not be sent.`;
}

function panelKey(target: RunTarget): string {
  return `${target.uri} ${target.kind} ${target.name}`;
}

function isStringMap(v: unknown): v is Record<string, string> {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return false;
  return Object.values(v as Record<string, unknown>).every((x) => typeof x === "string");
}

// -----------------------------------------------------------------------------
// The result
// -----------------------------------------------------------------------------

export class ResultPanel {
  // A SINGLE result tab, reused. A tab per run would accumulate one per
  // iteration of an edit-run loop, and the developer only ever looks at the
  // newest.
  private static current: ResultPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly disposables: vscode.Disposable[] = [];
  private outcome: RunOutcome;
  private showRaw = false;
  private disposed = false;

  static show(context: vscode.ExtensionContext, host: RunPanelHost, outcome: RunOutcome): void {
    // A superseded run has nothing to show: a newer run is already in flight
    // and will paint over this the moment it lands.
    if (outcome.status === "superseded") return;
    if (ResultPanel.current !== undefined && !ResultPanel.current.disposed) {
      ResultPanel.current.outcome = outcome;
      ResultPanel.current.showRaw = false;
      ResultPanel.current.panel.title = resultTitle(outcome);
      ResultPanel.current.render();
      ResultPanel.current.panel.reveal(undefined, true);
      return;
    }
    ResultPanel.current = new ResultPanel(context, host, outcome);
  }

  private constructor(
    _context: vscode.ExtensionContext,
    private readonly host: RunPanelHost,
    outcome: RunOutcome,
  ) {
    this.outcome = outcome;
    this.panel = vscode.window.createWebviewPanel(
      "memqlRunResult",
      resultTitle(outcome),
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
    const { type, conceptId, rowId } = msg as {
      type?: unknown;
      conceptId?: unknown;
      rowId?: unknown;
    };
    if (type === "toggleRaw") {
      this.showRaw = !this.showRaw;
      this.render();
    } else if (type === "openRow" && typeof conceptId === "string" && typeof rowId === "string") {
      this.host.openRow(conceptId, rowId);
    }
  }

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const d of this.disposables.splice(0)) d.dispose();
    if (ResultPanel.current === this) ResultPanel.current = undefined;
  }

  private render(): void {
    const nonce = nonceValue();
    const o = this.outcome;

    let bodyHtml: string;
    let bannerHtml = "";
    let rawJson = "";

    if (o.status === "ok") {
      // Always present, on every run: a banner that only ever appears on the
      // bad case is one the reader learns to skip.
      bannerHtml = `<div class="${o.ranDeployedDefinition ? "warning" : "notice"}">${escapeHtml(resultBannerFor(o))}</div>`;
      rawJson = safeJson(o.toolContent ?? o.raw);
      bodyHtml =
        o.toolContent !== undefined ? toolContentHtml(o.toolContent) : this.rowsHtml(o.rows);
    } else if (o.status === "invalid") {
      bannerHtml = `<div class="warning">${escapeHtml(
        `The bundle did not compile (${o.phase}). Nothing ran. The failures are in the Problems panel, at their position in your buffer.`,
      )}</div>`;
      bodyHtml = `<ul class="diagnostics">${o.diagnostics
        .map(
          (d) =>
            `<li>${escapeHtml(d.message)}${d.fileLevel ? "" : ` <span class="muted">(${escapeHtml(d.path)}:${d.start.line + 1}:${d.start.character + 1})</span>`}</li>`,
        )
        .join("")}</ul>`;
      rawJson = safeJson(o.diagnostics);
    } else if (o.status === "declined") {
      bodyHtml = `<div class="placeholder">Cancelled. ${escapeHtml(o.target.name)} was not run.</div>`;
    } else if (o.status === "superseded") {
      // show() refuses a superseded outcome, so this is unreachable through
      // the normal path -- but the union carries the case, and an unhandled
      // one would fall into the error branch below and read the fields it does
      // not have.
      bodyHtml = '<div class="placeholder">Superseded by a newer run.</div>';
    } else {
      // The ERR- id is rendered SEPARATELY and selectably: it is the only
      // handle the developer has on the server-side log entry, and burying it
      // in prose is the difference between a support thread that resolves and
      // one that does not.
      const idHtml =
        o.errorId === ""
          ? ""
          : `<p>Error id: <code id="error-id" class="selectable">${escapeHtml(o.errorId)}</code></p>`;
      bodyHtml = `<div class="error">ERROR (${escapeHtml(o.phase)}): ${escapeHtml(o.message)}</div>${idHtml}`;
      rawJson = safeJson({ phase: o.phase, message: o.message, errorId: o.errorId });
    }

    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(resultTitle(this.outcome))}</title>
<style nonce="${nonce}">
${panelChrome()}
${viewKitStyles}
  .diagnostics { margin: 0; padding-left: 20px; }
  .muted { color: var(--vscode-descriptionForeground); }
  pre { white-space: pre-wrap; word-break: break-word; font-family: var(--vscode-editor-font-family); }
  code.selectable { user-select: all; }
  .group-title { font-weight: 600; margin: 12px 0 4px; }
</style>
</head>
<body>
<div class="toolbar">
  <strong>${escapeHtml(resultTitle(this.outcome))}</strong>
  <button id="raw" type="button">${this.showRaw ? "Show rows" : "Show raw JSON"}</button>
</div>
${bannerHtml}
<div class="pane">
${this.showRaw ? `<pre>${escapeHtml(rawJson)}</pre>` : bodyHtml}
</div>
<script nonce="${nonce}">
  const vscode = acquireVsCodeApi();
  document.getElementById('raw').addEventListener('click', () => vscode.postMessage({ type: 'toggleRaw' }));
  // One delegated listener: view-kit emits data attributes, never inline
  // handlers, which is what lets the CSP forbid them outright.
  document.querySelector('.pane').addEventListener('click', (e) => {
    const row = e.target.closest('[data-row-id]');
    if (!row) return;
    const group = row.closest('[data-concept-id]');
    vscode.postMessage({
      type: 'openRow',
      conceptId: group ? group.dataset.conceptId : '',
      rowId: row.dataset.rowId,
    });
  });
</script>
</body>
</html>`;
  }

  private rowsHtml(rows: readonly Row[]): string {
    if (rows.length === 0) {
      return '<div class="placeholder">The run succeeded and returned no rows.</div>';
    }
    const groups = groupRowsByConcept(rows, this.host.concepts());
    return groups
      .map((g) => {
        const title = groups.length > 1 ? `<div class="group-title">${escapeHtml(g.concept.entity)}</div>` : "";
        return `${title}<div data-concept-id="${escapeHtml(g.concept.id)}">${renderToHtml(
          renderRowList(g.rows, g.concept),
        )}</div>`;
      })
      .join("\n");
  }
}

function resultTitle(outcome: RunOutcome): string {
  if (outcome.status === "superseded") return "Result";
  return `Result: ${outcome.target.name}`;
}

function toolContentHtml(content: readonly { type: string; text: string }[]): string {
  if (content.length === 0) {
    return '<div class="placeholder">The tool returned no content.</div>';
  }
  return content
    .map((c) => `<pre>${escapeHtml(c.text === "" ? `(${c.type} content)` : c.text)}</pre>`)
    .join("\n");
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

// The chrome both panels share. Kept as one string rather than duplicated so
// the two tabs cannot drift apart visually.
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
  .error { color: var(--vscode-errorForeground); padding: 8px 0; }
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
