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
// THE CLICK-THROUGH (#3763) DOES NOT BREAK THAT. `showList` opens a picker and
// moves a cursor; it submits nothing, which is what makes it the one training
// command that can safely sit in the palette.
//
// AND IT LISTS THE CONSTRUCTS THE COUNT WAS COMPUTED FROM, not a fresh read.
// That is the "one owner" rule above applied to the click rather than to the
// paint: a re-fetch on click could answer differently from the number the
// developer just clicked -- a race in exactly the surface built to stop the two
// from disagreeing. So the last painted set is retained, and it is dropped by
// every path that clears, so a stale list can never outlive the count it belongs
// to.
//
// Refs: #3763 #3761 #3745

import * as vscode from "vscode";

import {
  COMMAND_SHOW_LIST,
  TRAINING_STATE_CAPABILITY,
  TRAINING_STATE_METHOD,
  countStates,
  gutterMarkFor,
  parseTrainingConstructs,
  statusBarText,
  statusBarTooltip,
  trainingListEntries,
  type GutterMark,
  type TrainingConstruct,
  type TrainingListEntry,
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
  /**
   * What the status bar is currently counting, and which document it counted.
   *
   * The document is carried with it because the two can come apart: the picker
   * opens on a click, and a click is a moment at which the active editor may
   * already be something else. Revealing into whatever is focused now would send
   * a developer to a line number in the wrong file.
   */
  private painted: { uri: vscode.Uri; constructs: TrainingConstruct[] } | undefined;

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
    // The click-through (#3763), which is what turns the count from a notice
    // into a way back to the constructs it is about. Set once, in the
    // constructor: the item is only ever shown when there is something to list,
    // so there is no state in which the command would open an empty picker.
    this.status.command = COMMAND_SHOW_LIST;
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

  /**
   * The status bar's click-through: the untrained and drifted constructs of the
   * document the count belongs to, and a way back to each one.
   *
   * A QUICKPICK rather than a tree view or a webview. The list is short, it is
   * per-document, and its whole job is to be dismissed a second after it opens
   * with the cursor somewhere useful -- which is the shape a picker has and a
   * persistent panel does not. It also inherits the palette's filtering for
   * free, so a file with forty untrained constructs stays navigable.
   *
   * The reveal is the extension's existing one (`webview/constructPanel.ts`):
   * open the document, show it non-preview in column one, set the selection and
   * `revealRange` it into the centre. What differs is where the position comes
   * from -- that path scans the text for a signature line because it has no
   * server answer, and this uses the SERVER'S signature range, the same one the
   * lens and the gutter anchor to. Three surfaces, one position.
   */
  async showList(): Promise<void> {
    const painted = this.painted;
    if (painted === undefined) {
      vscode.window.showInformationMessage(
        "memQL: no training state for the active editor. Open a .memql file with a cluster connected."
      );
      return;
    }
    const entries = trainingListEntries(painted.constructs);
    if (entries.length === 0) {
      // Reachable from the palette, where nothing guarantees the status bar is
      // showing. Worth its own sentence rather than an empty picker, because
      // "everything here is on the cluster" is the good news this whole surface
      // exists to tell somebody.
      vscode.window.showInformationMessage(
        "memQL: every construct in this file is on the cluster -- nothing untrained, nothing drifted."
      );
      return;
    }

    const picked = await vscode.window.showQuickPick(
      entries.map((entry) => ({
        label: entry.label,
        description: entry.description,
        detail: entry.detail,
        entry,
      })),
      {
        // "not in this version" rather than "does not have": a drifted construct
        // IS on the cluster, in an older version, and the two states are the
        // distinction this whole surface is built around. A placeholder that
        // collapsed them would undo it in the one place a developer reads while
        // deciding what to do next.
        placeHolder: "Constructs this cluster does not have in this version. Pick one to go to it.",
        matchOnDescription: true,
        matchOnDetail: true,
      }
    );
    if (picked === undefined) return;
    await revealConstruct(painted.uri, picked.entry);
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

    // Retained BEFORE the count is rendered and from the same array, so the list
    // a click produces cannot describe a different reading than the number that
    // was clicked.
    this.painted = { uri: editor.document.uri, constructs: [...constructs] };

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
    // Dropped on EVERY clearing path -- a non-memql editor, a lost connection, a
    // failed request. A retained list would otherwise outlive the count it was
    // taken with and offer navigation into a file nobody is looking at, on the
    // authority of a request that has since failed. Cleared first, so it happens
    // even when there is no editor to repaint.
    this.painted = undefined;
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

/**
 * revealConstruct puts the cursor on a construct's signature.
 *
 * The SELECTION is collapsed to the range's start rather than spanning it: the
 * point is to arrive at the construct, and arriving with its keyword and name
 * highlighted means the next keystroke replaces them.
 */
async function revealConstruct(uri: vscode.Uri, entry: TrainingListEntry): Promise<void> {
  const document = await vscode.workspace.openTextDocument(uri);
  const editor = await vscode.window.showTextDocument(document, {
    viewColumn: vscode.ViewColumn.One,
    preview: false,
  });
  const range = toRange(entry.construct.signatureRange);
  editor.selection = new vscode.Selection(range.start, range.start);
  editor.revealRange(range, vscode.TextEditorRevealType.InCenter);
}
