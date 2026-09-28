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
// THE SIXTH COMMAND IS NOT ONE OF THE FOUR. `edited` offers Rebuild from
// checkout (memql#4244), which is `memql.deployments.rebuildFromCheckout` --
// the Deployments surface's command, not `src/training/actions.ts`'s. It is
// contributed in package.json, registered in `extension.ts`, and held by
// `surfaceGuards.test.ts`, which asserts the contribution, the registration and
// the palette gate. It is offered only when the selected cluster is local.
//
// Do not register it from here: a training module registering a Deployments
// command is how an id ends up with two owners.
//
// ONE LENS PER CONSTRUCT. The state, in words ("Not on cluster"), and a click
// that opens a quick pick of the acts legal from it -- Dry run, Try in this
// session, Stage, Promote, Demote, Rebuild from checkout. It used to be the state
// plus up to four act lenses plus a session lens, seven or eight on one line
// beside Run; every act is still one pick away, and still reachable through
// "Show CodeLens Commands for Current Line". The chooser
// (`memql.training.choose`) is registered in extension.ts beside the acts it
// runs; `src/training/actions.ts` is what they reach.
//
// Refs: #3763 #3761 #3745

import * as vscode from "vscode";

import {
  COMMAND_CHOOSE,
  TRAINING_STATE_CAPABILITY,
  TRAINING_STATE_METHOD,
  parseTrainingConstructs,
  trainingLensPlans,
  type TrainingChoice,
} from "../state/training.js";
import type { TrainingStateClient } from "./decorations.js";

export class TrainingCodeLensProvider implements vscode.CodeLensProvider {
  private readonly changed = new vscode.EventEmitter<void>();
  readonly onDidChangeCodeLenses = this.changed.event;

  private client: TrainingStateClient | undefined;
  private cluster: { name: string; local: boolean; label?: string } | undefined;
  private sessionDefined: ((name: string) => boolean) | undefined;

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
  setCluster(cluster: { name: string; local: boolean; label?: string } | undefined): void {
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
      sessionDefined: this.sessionDefined,
    });
    for (const plan of plans) {
      const range = toRange(plan.construct.signatureRange);
      // ONE LENS: the state in words. When the state has acts, clicking it
      // opens a quick pick of exactly those (Dry run, Try in this session,
      // Stage, Promote, Demote, Rebuild); when it has none it is a fact with
      // no command, so nothing invites a click that does nothing.
      if (plan.actions.length === 0) {
        lenses.push(new vscode.CodeLens(range, { title: plan.label, command: "", tooltip: plan.detail }));
        continue;
      }
      const choice: TrainingChoice = {
        uri: document.uri.toString(),
        name: plan.construct.name,
        actions: plan.actions,
      };
      lenses.push(
        new vscode.CodeLens(range, {
          title: plan.label,
          command: COMMAND_CHOOSE,
          tooltip: plan.detail,
          arguments: [choice],
        }),
      );
    }
    return lenses;
  }

  /**
   * Point at the session's definitions, or at nothing. Refreshes either way.
   *
   * A construct defined for this session only says so in its lens ("Not on
   * cluster · this session"). That used to be a second lens from a second
   * provider on the same line.
   */
  setSessionLookup(lookup: ((name: string) => boolean) | undefined): void {
    this.sessionDefined = lookup;
    this.changed.fire();
  }

  /** Redraw: the session's definitions changed, which is not a document change. */
  refresh(): void {
    this.changed.fire();
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
