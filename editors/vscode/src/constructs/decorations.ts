// The gutter, the signature decoration and the status-bar count, wired in.
//
// An ADAPTER: every decision lives in state/training.ts (which state gets which
// mark, which lens, what the count says) and constructs/gutterIcons.ts (the
// glyphs and the decoration visuals). This file converts LSP ranges to
// vscode.Range, holds the three TextEditorDecorationType handles, and owns the
// status-bar item.
//
// THE GUTTER AND THE STATUS BAR HAVE ONE OWNER ON PURPOSE. They answer the same
// question at two scales -- "what is the state of what I am looking at" -- and
// the defect worth designing out is the two of them disagreeing: three untrained
// icons in the gutter beside a status bar saying nothing, or the reverse. One
// owner, one refresh, one source, so a disagreement has nowhere to come from.
//
// NOTHING HERE RUNS ANYTHING, AND NOTHING HERE IS TRIGGERED BY A SAVE. There is
// no promote-on-save and no train-on-save; the extension already holds that line
// for runs, and promotion is a strictly larger commitment than a run. The
// refresh trigger is the active editor changing, the document changing, or the
// connection changing -- never `onDidSaveTextDocument`, which a guard test
// asserts because the absence is the feature.
//
// Refs: #3761 #3745

import * as vscode from "vscode";

import {
  TRAINING_STATE_CAPABILITY,
  TRAINING_STATE_METHOD,
  countStates,
  gutterMarkFor,
  parseTrainingConstructs,
  statusBarText,
  statusBarTooltip,
  type GutterMark,
  type TrainingConstruct,
} from "../state/training.js";
import { markDecoration } from "./gutterIcons.js";

const MARKS: readonly GutterMark[] = ["untrained", "drifted", "live"];

/** The slice of the language client this needs. Structural, so it is fakeable. */
export interface TrainingStateClient {
  sendRequest(method: string, params: unknown, token?: vscode.CancellationToken): Promise<unknown>;
  /** The server's advertised `capabilities.experimental`, or undefined before initialize. */
  experimentalCapabilities(): Record<string, unknown> | undefined;
}

export class TrainingDecorations {
  private readonly types = new Map<GutterMark, vscode.TextEditorDecorationType>();
  private readonly status: vscode.StatusBarItem;
  private readonly disposables: vscode.Disposable[] = [];
  private client: TrainingStateClient | undefined;

  constructor() {
    for (const mark of MARKS) {
      const visual = markDecoration(mark);
      this.types.set(
        mark,
        vscode.window.createTextEditorDecorationType({
          ...visual,
          gutterIconPath: vscode.Uri.parse(visual.gutterIconPath),
          overviewRulerLane: vscode.OverviewRulerLane.Right,
          // The decoration must not grow when a developer types at the end of a
          // signature: it marks a range the SERVER reported, and a range that
          // stretched under editing would claim to describe text the server has
          // not seen.
          rangeBehavior: vscode.DecorationRangeBehavior.ClosedClosed,
        }),
      );
    }

    this.status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 90);
    // Clicking through to the list is #3763's, which owns the actions. Until
    // then the item reports and does not pretend to navigate.
    this.disposables.push(this.status);
  }

  /** Point at a language client, or at nothing when there is no connection. */
  setClient(client: TrainingStateClient | undefined): void {
    this.client = client;
    void this.refresh(vscode.window.activeTextEditor);
  }

  /** Watch the editor for changes worth a re-ask. */
  activate(): void {
    this.disposables.push(
      vscode.window.onDidChangeActiveTextEditor((editor) => {
        void this.refresh(editor);
      }),
      vscode.workspace.onDidChangeTextDocument((event) => {
        const editor = vscode.window.activeTextEditor;
        if (editor !== undefined && event.document === editor.document) void this.refresh(editor);
      }),
    );
    void this.refresh(vscode.window.activeTextEditor);
  }

  /**
   * Re-ask the server and repaint.
   *
   * EVERY FAILURE PATH CLEARS rather than leaving what was there. A stale
   * decoration is worse than none: it describes a cluster this editor is no
   * longer talking to, and nothing on screen says so.
   */
  async refresh(editor: vscode.TextEditor | undefined): Promise<void> {
    if (editor === undefined || editor.document.languageId !== "memql") {
      this.clear(editor);
      this.status.hide();
      return;
    }
    const constructs = await this.fetch(editor.document);
    if (constructs === undefined) {
      this.clear(editor);
      this.status.hide();
      return;
    }
    this.paint(editor, constructs);
  }

  private async fetch(document: vscode.TextDocument): Promise<TrainingConstruct[] | undefined> {
    const client = this.client;
    if (client === undefined) return undefined;
    // Feature-detected, not called blind: before #3759 lands every server is an
    // older build, and this surface being simply absent is correct rather than
    // degraded.
    const caps = client.experimentalCapabilities();
    if (caps === undefined || caps[TRAINING_STATE_CAPABILITY] !== true) return undefined;
    try {
      const raw = await client.sendRequest(TRAINING_STATE_METHOD, {
        textDocument: { uri: document.uri.toString() },
      });
      return parseTrainingConstructs(raw);
    } catch {
      // A request that fails on a document somebody is still typing must not
      // surface as a popup. It clears, exactly as a disconnection does.
      return undefined;
    }
  }

  private paint(editor: vscode.TextEditor, constructs: readonly TrainingConstruct[]): void {
    const byMark = new Map<GutterMark, vscode.Range[]>();
    for (const mark of MARKS) byMark.set(mark, []);
    for (const construct of constructs) {
      const mark = gutterMarkFor(construct.state);
      // `unknown` returns undefined and is therefore painted nowhere. That is
      // the rule, and it is expressed as an absence rather than as a branch.
      if (mark === undefined) continue;
      byMark.get(mark)?.push(toRange(construct.signatureRange));
    }
    for (const mark of MARKS) {
      const type = this.types.get(mark);
      if (type !== undefined) editor.setDecorations(type, byMark.get(mark) ?? []);
    }

    const counts = countStates(constructs);
    const text = statusBarText(counts);
    if (text === "") {
      this.status.hide();
      return;
    }
    this.status.text = `$(circle-outline) ${text}`;
    this.status.tooltip = statusBarTooltip(counts);
    this.status.show();
  }

  /** Empty every decoration set. Empty, not undefined -- that is what removes them. */
  private clear(editor: vscode.TextEditor | undefined): void {
    if (editor === undefined) return;
    for (const type of this.types.values()) editor.setDecorations(type, []);
  }

  dispose(): void {
    for (const type of this.types.values()) type.dispose();
    this.types.clear();
    for (const d of this.disposables) d.dispose();
  }
}

function toRange(range: { start: { line: number; character: number }; end: { line: number; character: number } }): vscode.Range {
  return new vscode.Range(
    new vscode.Position(range.start.line, range.start.character),
    new vscode.Position(range.end.line, range.end.character),
  );
}
