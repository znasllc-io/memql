// The two run tabs: the argument form, and the result.
//
// Both are ADAPTERS. The form model and its coercion live in
// state/argForm.ts, the result projection and its provenance in
// state/runResult.ts, the markup in webview/runScreens.ts, and the run itself
// in run/orchestrator.ts; this file owns the webview, the postMessage
// boundary, and nothing else.
//
// Rendering goes through view-kit for the same reason the concept browser's
// does: rows plus each concept's own @displayCard, no result-specific renderer
// and no concept-specific code anywhere, so a concept declared five minutes
// ago renders with no client change.
//
// ON THE PAGE KIT (src/webview/ui). Each tab's document is assigned once per
// screen and patched after that, so the form keeps the caret in the field
// being typed in while its errors and its bar change, and the result keeps its
// scroll and the JSON disclosure's state while it fills in. The form's values
// arrive as the runtime's `input` messages, one per keystroke, and are held
// here -- the page never repaints for them.
//
// The webview runs under a strict CSP with a per-load nonce. Result data is
// untrusted (it is whatever the cluster returned) and view-kit escapes it, but
// a CSP means an escaping bug cannot become script execution. The postMessage
// channel is untrusted too, so every message is checked before it is acted on.
//
// THE CREDENTIAL IS NEVER RENDERED HERE. Nothing in this file reads a
// ClusterConfig; the orchestrator receives a RunCluster carrying only name,
// label and the local flag, precisely so a credential cannot reach a webview
// by accident.

import * as vscode from "vscode";
import { randomBytes } from "node:crypto";

import type { Concept } from "@znasllc-io/memql-sdk-core/client";
import { viewKitStyles, type ConceptLike } from "@znasllc-io/memql-view-kit";

import { currentBodyThemeAttr, onAppearanceChange } from "./theme.js";

import type { RunTarget } from "../constructs/runnable.js";
import type { RunOutcome } from "../run/orchestrator.js";
import { buildFields, coerceArgs, orphanedValueNames, type ArgFieldModel } from "../state/argForm.js";
import {
  RESULT_ACTS,
  RUN_FORM_ACTS,
  RUN_PAGE_STYLES,
  resultParts,
  runFormParts,
  type ResultInput,
  type RunFormNote,
} from "./runScreens.js";
import { pageDocument } from "./ui/document.js";
import { LiveView } from "./ui/liveView.js";
import { pageMessage } from "./ui/protocol.js";

/** What the arg form asks the extension to do when the user acts. */
export interface RunPanelHost {
  /** Run the construct with these values. */
  run(target: RunTarget, values: Record<string, unknown>): Promise<RunOutcome>;
  /** Persist a named saved run in the workspace. */
  saveConfig(target: RunTarget, name: string, values: Record<string, unknown>): Promise<void>;
  /** The concept descriptors for result rendering; empty before the first list load. */
  concepts(): ReadonlyMap<string, ConceptLike>;
  /** Opens a row in the concept's page, selected. */
  openRow(conceptId: string, rowId: string): void;
}

export function conceptMap(concepts: readonly Concept[]): Map<string, ConceptLike> {
  return new Map(concepts.map((c) => [c.id, c]));
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
const STYLES = `${viewKitStyles}\n${RUN_PAGE_STYLES}`;

// -----------------------------------------------------------------------------
// The argument form
// -----------------------------------------------------------------------------

export class RunPanel {
  // One panel per construct, keyed by uri+name: re-clicking Run... on the same
  // signature reveals the tab already holding your half-typed arguments rather
  // than opening a second one that discards them.
  private static readonly open_ = new Map<string, RunPanel>();

  private readonly panel: vscode.WebviewPanel;
  private readonly live: LiveView;
  private readonly disposables: vscode.Disposable[] = [];
  private fields: ArgFieldModel[];
  private errors: Record<string, string> = {};
  private orphans: string[];
  private note: RunFormNote | undefined;
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
    this.orphans = orphanedValueNames(target.args, values);
    this.panel = vscode.window.createWebviewPanel(
      "memqlRun",
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
  }

  // adopt refreshes the target (the construct's args may have changed under an
  // edit) while keeping whatever the user has already typed.
  private adopt(target: RunTarget, values: Record<string, unknown>): void {
    this.target = target;
    const merged: Record<string, unknown> = { ...values };
    for (const [name, text] of Object.entries(this.currentText())) {
      if (text !== "") merged[name] = text;
    }
    this.fields = buildFields(target.args, merged);
    this.orphans = orphanedValueNames(target.args, values);
    this.panel.title = `Run ${target.name}`;
    this.render();
  }

  private currentText(): Record<string, string> {
    const out: Record<string, string> = {};
    for (const f of this.fields) out[f.name] = f.text;
    return out;
  }

  private onMessage(raw: unknown): void {
    const message = pageMessage(raw);
    if (message === undefined) return;
    if (message.type === "input") {
      // One keystroke. Held, never rendered: the page already shows it, and a
      // repaint here would fight the caret.
      const { field, value } = message as { field?: unknown; value?: unknown };
      if (typeof field !== "string" || typeof value !== "string") return;
      this.fields = this.fields.map((f) => (f.name === field ? { ...f, text: value } : f));
      return;
    }
    if (message.type === RUN_FORM_ACTS.run) {
      void this.doRun();
    } else if (message.type === RUN_FORM_ACTS.saveAs) {
      void this.doSave();
    } else if (message.type === RUN_FORM_ACTS.openRuns) {
      void vscode.commands.executeCommand("memql.runs.open");
    }
  }

  private async doRun(): Promise<void> {
    if (this.busy) return;
    const coerced = coerceArgs(this.target.args, this.currentText());
    if (!coerced.ok) {
      this.errors = coerced.errors;
      this.render();
      return;
    }
    this.errors = {};
    this.note = undefined;
    this.busy = true;
    this.render();
    // The Result tab opens NOW, in its running shape, so a slow run is
    // visible from the click rather than only once it lands.
    ResultPanel.running(this.context, this.host, this.target);
    const outcome = await this.host.run(this.target, coerced.values);
    this.busy = false;
    this.render();
    ResultPanel.show(this.context, this.host, outcome);
  }

  private async doSave(): Promise<void> {
    const coerced = coerceArgs(this.target.args, this.currentText());
    if (!coerced.ok) {
      this.errors = coerced.errors;
      this.render();
      return;
    }
    this.errors = {};
    // The name is asked for when it is needed, not kept on the page as a
    // standing field beside a second button.
    const name = (
      await vscode.window.showInputBox({ prompt: "Name this saved run", placeHolder: this.target.name })
    )?.trim();
    if (name === undefined || name === "") return;
    try {
      await this.host.saveConfig(this.target, name, coerced.values);
      this.note = { tone: "info", line: `Saved as "${name}".`, openRuns: true };
    } catch (err) {
      this.note = { tone: "error", line: err instanceof Error ? err.message : String(err) };
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
    if (this.disposed) return;
    this.live.render(
      "form",
      runFormParts({
        target: this.target,
        fields: this.fields,
        errors: this.errors,
        orphans: this.orphans,
        busy: this.busy,
        note: this.note,
      }),
    );
  }
}

// The separator is NUL because no component of a RunTarget can contain one, so
// the joined key is unambiguous. It is written as an ESCAPE rather than as the
// raw byte it used to be (memql#4422): a raw NUL makes the whole file "binary"
// to the standard toolchain -- `file` reports "data", and `grep` skips it in
// silence, with no message and a zero exit. Same value, same key, greppable.
function panelKey(target: RunTarget): string {
  return `${target.uri}\u0000${target.kind}\u0000${target.name}`;
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
  private readonly live: LiveView;
  private readonly disposables: vscode.Disposable[] = [];
  private shown: ResultInput;
  private jsonOpen = false;
  /** Bumped per run, so each run's result is a new screen (a fresh scroll). */
  private run = 0;
  private disposed = false;

  /** Opens (or reuses) the tab in its running shape, as a run starts. */
  static running(context: vscode.ExtensionContext, host: RunPanelHost, target: RunTarget): void {
    ResultPanel.present(context, host, { state: "running", target });
  }

  static show(context: vscode.ExtensionContext, host: RunPanelHost, outcome: RunOutcome): void {
    // A superseded run has nothing to show: a newer run is already in flight
    // and will paint over this the moment it lands.
    if (outcome.status === "superseded") return;
    ResultPanel.present(context, host, { state: "settled", outcome, concepts: host.concepts(), jsonOpen: false });
  }

  private static present(context: vscode.ExtensionContext, host: RunPanelHost, shown: ResultInput): void {
    const existing = ResultPanel.current;
    if (existing !== undefined && !existing.disposed) {
      if (shown.state === "running") existing.run += 1;
      existing.shown = shown;
      existing.jsonOpen = false;
      existing.panel.title = titleOf(shown);
      existing.render();
      existing.panel.reveal(undefined, true);
      return;
    }
    ResultPanel.current = new ResultPanel(context, host, shown);
  }

  private constructor(
    _context: vscode.ExtensionContext,
    private readonly host: RunPanelHost,
    shown: ResultInput,
  ) {
    this.shown = shown;
    this.panel = vscode.window.createWebviewPanel(
      "memqlRunResult",
      titleOf(shown),
      { viewColumn: vscode.ViewColumn.Beside, preserveFocus: true },
      { enableScripts: true },
    );
    this.live = new LiveView(liveTarget(this.panel), (parts, screen) =>
      pageDocument({
        nonce: nonceValue(),
        title: titleOf(this.shown),
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
  }

  private onMessage(raw: unknown): void {
    const message = pageMessage(raw);
    if (message === undefined) return;
    const { conceptId, value } = message as { conceptId?: unknown; value?: unknown };
    switch (message.type) {
      case RESULT_ACTS.openRow:
        if (typeof conceptId === "string" && typeof value === "string") this.host.openRow(conceptId, value);
        return;
      case RESULT_ACTS.json:
        this.jsonOpen = message.open === true;
        return;
      case RESULT_ACTS.copyErrorId:
        if (this.shown.state === "settled" && this.shown.outcome.status === "error") {
          const id = this.shown.outcome.errorId;
          void vscode.env.clipboard.writeText(id).then(() => {
            void vscode.window.showInformationMessage(`MemQL: Copied ${id}.`);
          });
        }
        return;
      case RESULT_ACTS.showProblems:
        void vscode.commands.executeCommand("workbench.actions.view.problems");
        return;
      case RESULT_ACTS.selectCluster:
        void vscode.commands.executeCommand("memql.clusters.select");
        return;
    }
  }

  private dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const d of this.disposables.splice(0)) d.dispose();
    if (ResultPanel.current === this) ResultPanel.current = undefined;
  }

  private render(): void {
    if (this.disposed) return;
    const shown: ResultInput =
      this.shown.state === "settled" ? { ...this.shown, jsonOpen: this.jsonOpen } : this.shown;
    // The running shape and its result are ONE screen, so the result patches
    // into the page the run opened; the next run is a new one.
    this.live.render(`result:${this.run}`, resultParts(shown));
  }
}

function titleOf(shown: ResultInput): string {
  return shown.state === "running" ? shown.target.name : shown.outcome.target.name;
}

// A CSP nonce is a security control, so it comes from a CSPRNG. Math.random()
// is not one -- its output is predictable from prior draws, which defeats the
// nonce's purpose.
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}
