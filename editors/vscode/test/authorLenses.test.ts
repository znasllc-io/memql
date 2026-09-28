// Fewer, clearer lenses above a construct: one Run lens and one training lens.
//
// An untrained query used to carry Run · Run with... · untrained · Dry-run ·
// Try in session · Stage · Promote, and a session lens beside them -- up to
// eight on one line. It now carries two: "Run" (or "Run...", the form) and the
// state in words, whose click opens a quick pick of every act legal from it.
// No act was removed; each is one pick away.
//
// Also here, because they are wiring rather than rendering: the Constructs
// view is redrawn when a training act changes the catalog, and running a saved
// run without a language server says why instead of doing nothing.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import type { CancellationToken, TextDocument } from "vscode";

import { RunnableCodeLensProvider } from "../src/constructs/lensProvider.js";
import { TrainingCodeLensProvider } from "../src/constructs/trainingLens.js";
import {
  COMMAND_CHOOSE,
  COMMAND_DEMOTE,
  COMMAND_DRY_RUN,
  COMMAND_PROMOTE,
  COMMAND_STAGE,
  COMMAND_TRY_IN_SESSION,
  TRAINING_STATE_CAPABILITY,
  type TrainingChoice,
} from "../src/state/training.js";

const ROOT = path.resolve(__dirname, "..", "..");
const RANGE = { start: { line: 3, character: 0 }, end: { line: 3, character: 30 } };
const DOCUMENT = { uri: { toString: () => "file:///w/q.memql" } } as unknown as TextDocument;
const TOKEN = { isCancellationRequested: false } as unknown as CancellationToken;

function trainingClient(states: { name: string; state: string }[]) {
  return {
    experimentalCapabilities: () => ({ [TRAINING_STATE_CAPABILITY]: true }),
    sendRequest: async () => ({
      constructs: states.map((s) => ({ kind: "query", name: s.name, signatureRange: RANGE, state: s.state })),
    }),
  };
}

test("each construct gets ONE training lens: the state in words, opening the legal acts", async () => {
  const lens = new TrainingCodeLensProvider();
  lens.setClient(trainingClient([{ name: "spaceParticipants", state: "untrained" }]) as never);
  const lenses = await lens.provideCodeLenses(DOCUMENT, TOKEN);
  assert.equal(lenses.length, 1, "more than one training lens above a construct");
  const command = lenses[0]?.command;
  assert.equal(command?.title, "Not on cluster");
  assert.equal(command?.command, COMMAND_CHOOSE);
  const choice = command?.arguments?.[0] as TrainingChoice;
  assert.equal(choice.name, "spaceParticipants");
  // Every act the old lens line offered, in the order of the escalation.
  assert.deepEqual(
    choice.actions.map((a) => a.command),
    [COMMAND_DRY_RUN, COMMAND_TRY_IN_SESSION, COMMAND_STAGE, COMMAND_PROMOTE],
  );
});

test("a state with no act is a fact with no command, and a staged one offers promote and demote", async () => {
  const lens = new TrainingCodeLensProvider();
  lens.setClient(
    trainingClient([
      { name: "builtIn", state: "seeded" },
      { name: "mine", state: "staged" },
    ]) as never,
  );
  const [seeded, staged] = await lens.provideCodeLenses(DOCUMENT, TOKEN);
  assert.equal(seeded?.command?.title, "Built in");
  assert.equal(seeded?.command?.command, "", "a lens with nothing to do invites a click");
  const acts = (staged?.command?.arguments?.[0] as TrainingChoice).actions.map((a) => a.command);
  assert.deepEqual(acts, [COMMAND_STAGE, COMMAND_PROMOTE, COMMAND_DEMOTE]);
});

test("a construct defined for this session says so in its one lens, not a second one", async () => {
  const lens = new TrainingCodeLensProvider();
  lens.setClient(trainingClient([{ name: "spaceParticipants", state: "untrained" }]) as never);
  lens.setSessionLookup((name) => name === "spaceParticipants");
  const lenses = await lens.provideCodeLenses(DOCUMENT, TOKEN);
  assert.equal(lenses.length, 1);
  assert.equal(lenses[0]?.command?.title, "Not on cluster · this session");
  assert.match(lenses[0]?.command?.tooltip ?? "", /until you disconnect/);
});

test("the run lens is one lens per construct", async () => {
  const provider = new RunnableCodeLensProvider({
    experimentalCapabilities: () => ({ memqlRunnableConstructs: true }),
    sendRequest: async () => ({
      constructs: [
        { kind: "query", name: "a", signatureRange: RANGE, args: [] },
        { kind: "mutation", name: "b", signatureRange: RANGE, args: [{ name: "x", type: "string", required: true }] },
      ],
    }),
  });
  const lenses = await provider.provideCodeLenses(DOCUMENT, TOKEN);
  assert.deepEqual(
    lenses.map((l) => l.command?.title),
    ["Run", "Run..."],
  );
});

test("the chooser is contributed, palette-hidden and registered", () => {
  const manifest = JSON.parse(fs.readFileSync(path.join(ROOT, "package.json"), "utf8")) as {
    contributes: { commands: { command: string }[]; menus: { commandPalette: { command: string; when?: string }[] } };
  };
  assert.ok(manifest.contributes.commands.some((c) => c.command === COMMAND_CHOOSE));
  assert.equal(
    manifest.contributes.menus.commandPalette.find((e) => e.command === COMMAND_CHOOSE)?.when,
    "false",
  );
  const extension = fs.readFileSync(path.join(ROOT, "src", "extension.ts"), "utf8");
  assert.ok(extension.includes("registerCommand(COMMAND_CHOOSE"), "the chooser is never registered");
});

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

const EXTENSION = fs.readFileSync(path.join(ROOT, "src", "extension.ts"), "utf8");

test("a training act that changes the catalog redraws the Constructs view", () => {
  // THE BUG: catalogChanged refreshed the lenses and the gutter, and nothing
  // told the Constructs view, so a construct just promoted stayed out of it
  // until somebody pressed Refresh.
  const at = EXTENSION.indexOf("catalogChanged: () => {");
  assert.ok(at > 0, "catalogChanged no longer has a body");
  const body = EXTENSION.slice(at, EXTENSION.indexOf("},", at));
  assert.match(body, /refreshConstructsView\(\)/);
  assert.match(EXTENSION, /refreshConstructsView = \(\) => constructsTree\.refresh\(\)/);
});

test("running a saved run from a file without a language server says so", () => {
  // THE BUG: `if (client === undefined) return undefined;` -- Run on a saved
  // run then did nothing at all.
  const at = EXTENSION.indexOf("async function constructForConfig(");
  const body = EXTENSION.slice(at, EXTENSION.indexOf("\n}\n", at));
  assert.doesNotMatch(body, /if \(client === undefined\) return undefined;/);
  assert.match(body, /needs the MemQL language server/);
});

test("training acts show progress and end in one toast with the next act", () => {
  assert.match(EXTENSION, /window\.withProgress\(\{ location: ProgressLocation\.Window, title: `\$\{verb\} \$\{request\.name\}` \}/);
  // Dry run, try and stage offer Promote as their next act.
  for (const command of ["COMMAND_DRY_RUN", "COMMAND_TRY_IN_SESSION", "COMMAND_STAGE"]) {
    const at = EXTENSION.indexOf(`registerCommand(${command}`);
    const body = EXTENSION.slice(at, EXTENSION.indexOf("}),", at));
    assert.match(body, /promoteNext\(request\)/, `${command} ends without a next act`);
  }
});
