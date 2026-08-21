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
// THE SIXTH COMMAND IS NOT ONE OF THE FOUR, AND AT THIS COMMIT NOTHING HAS
// REGISTERED IT. `edited` offers Rebuild from checkout (memql#4244), which is
// `memql.deployments.rebuildFromCheckout` -- the Deployments surface's command,
// not `src/training/actions.ts`'s, contributed and registered by TASK 5 of the
// locality plan. It is offered only when the selected cluster is local.
//
// So the rule stated below is deliberately BENT here, within this branch: the
// lens is live ahead of the command, and a click lands on "command not found"
// until Task 5 merges. That is a property of the branch and not of a release --
// the two land in one PR -- and what has to hold it afterwards is
// `surfaceGuards.test.ts`, whose four-actions gate spells out each id it covers
// and must gain this one when the registration arrives. Do not register it from
// here: a training module registering a Deployments command is how an id ends
// up with two owners.
//
// THE ACTION LENSES ARE ON, as of #3763 -- which is the change that registers
// the four commands they post to, and they were switched on in it for that
// reason and no other. A Promote lens posting to an unregistered command fails
// with "command not found", and a click that does nothing teaches a developer
// the extension is broken, so the flip and the registration are one commit by
// construction. `src/training/actions.ts` is what they reach.
//
// Refs: #3763 #3761 #3745

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
  private cluster: { name: string; local: boolean } | undefined;

  /** Point at a language client, or at nothing. Refreshes either way. */
  setClient(client: TrainingStateClient | undefined): void {
    this.client = client;
    this.changed.fire();
  }

  /**
   * Point at a cluster, or at nothing. Refreshes either way.
   *
   * PUSHED, and it must be: `edited` renders a different sentence and a
   * different action per locality (memql#4244), and VS Code does not re-ask a
   * lens provider because something outside the document changed. Without the
   * refresh, selecting the local cluster would leave "seeded constructs change
   * by rollout" on screen beside a cluster that rebuilds on request -- until
   * the developer happened to type in the file.
   */
  setCluster(cluster: { name: string; local: boolean } | undefined): void {
    this.cluster = cluster;
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
    const plans = trainingLensPlans(parseTrainingConstructs(raw), {
      offerActions: true,
      cluster: this.cluster,
    });
    for (const plan of plans) {
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
