// The training CodeLens: a construct's state, above its signature, beside the
// Run lens.
//
// An ADAPTER, holding no logic: which lens each state gets is
// `trainingLensPlans` in state/training.ts, and this file converts LSP ranges to
// vscode.Range and plans to vscode.CodeLens.
//
// NOTHING HERE EXECUTES ANYTHING. provideCodeLenses runs on open, on every edit,
// and whenever VS Code feels like refreshing -- so it is one read-only LSP
// request and nothing else. The commands the lenses carry fire only on a click,
// and promotion is a strictly larger commitment than a run, so the
// no-run-on-save line this extension already holds matters more here, not less.
//
// THE ACTION LENSES ARE OFF. They are #3763's, which registers the four
// commands; until then `offerActions` is false and only the STATE renders. A
// Promote lens posting to an unregistered command fails with "command not
// found", and a click that does nothing teaches a developer the extension is
// broken -- which is a worse outcome than the button not being there yet.
//
// Refs: #3761 #3745

import * as vscode from "vscode";

import {
  TRAINING_STATE_CAPABILITY,
  TRAINING_STATE_METHOD,
  parseTrainingConstructs,
  trainingLensPlans,
} from "../state/training.js";
import type { TrainingStateClient } from "./decorations.js";

export class TrainingCodeLensProvider implements vscode.CodeLensProvider {
  private readonly changed = new vscode.EventEmitter<void>();
  readonly onDidChangeCodeLenses = this.changed.event;

  private client: TrainingStateClient | undefined;

  /** Point at a language client, or at nothing. Refreshes either way. */
  setClient(client: TrainingStateClient | undefined): void {
    this.client = client;
    this.changed.fire();
  }

  async provideCodeLenses(
    document: vscode.TextDocument,
    token: vscode.CancellationToken,
  ): Promise<vscode.CodeLens[]> {
    const client = this.client;
    if (client === undefined) return [];
    const caps = client.experimentalCapabilities();
    if (caps === undefined || caps[TRAINING_STATE_CAPABILITY] !== true) return [];

    let raw: unknown;
    try {
      raw = await client.sendRequest(
        TRAINING_STATE_METHOD,
        { textDocument: { uri: document.uri.toString() } },
        token,
      );
    } catch {
      // Silent. A failed request on a document somebody is still typing is not
      // an event worth a popup, and no lens is the honest rendering of "this
      // editor does not currently know".
      return [];
    }

    const lenses: vscode.CodeLens[] = [];
    for (const plan of trainingLensPlans(parseTrainingConstructs(raw))) {
      const range = toRange(plan.construct.signatureRange);
      // The state label is NOT a command. It is a fact about the construct, and
      // giving it a command would make a developer wonder what clicking it does.
      lenses.push(
        new vscode.CodeLens(range, {
          title: plan.label,
          command: "",
          tooltip: plan.detail,
        }),
      );
      for (const action of plan.actions) {
        lenses.push(
          new vscode.CodeLens(range, {
            title: action.title,
            command: action.command,
            arguments: [{ uri: document.uri.toString(), name: plan.construct.name }],
          }),
        );
      }
    }
    return lenses;
  }

  dispose(): void {
    this.changed.dispose();
  }
}

function toRange(range: { start: { line: number; character: number }; end: { line: number; character: number } }): vscode.Range {
  return new vscode.Range(
    new vscode.Position(range.start.line, range.start.character),
    new vscode.Position(range.end.line, range.end.character),
  );
}
