// Who owns the asking (memql#3586).
//
// The uninstall asked for the password ONCE, in a VS Code input box. The install
// asked THREE TIMES, with a zenity dialog drawn by the desktop -- from the same
// wizard, over the same plumbing, on the same machine. Three dialogs means no
// agent existed for that run, and every capability script then built its own
// prompt (`scripts/lib/elevate.sh`), which is the right fallback for a human
// running the script by hand and the wrong one for a wizard that has an input
// box of its own.
//
// TWO THINGS ARE UNDER TEST, and the second is the one that matters.
//
//   1. THE PROBE. `sudo -n true` from the extension host answers a question
//      about the WRONG PROCESS. From `man sudo`: the sudoers policy keys a
//      credential-cache record to the terminal, "or parent process ID if no
//      terminal is present". The extension host has no terminal and neither does
//      any capability script, so each gets a record keyed to its own parent -- a
//      cached credential the extension host can see is one no script can use.
//      `-k` makes the probe ignore the cache, and with a command it does not
//      destroy it.
//
//   2. THE INVARIANT. Fixing the probe removes the symptom and leaves the shape
//      that produced it: consistency depending on the agent always existing.
//      So the wizard says so in the child environment, and elevate.sh offers no
//      dialog of its own when it sees that -- asserted here on the environment
//      every step is handed, INCLUDING the run where no password was collected,
//      which is precisely the run that popped three dialogs.
//
// NOTHING HERE CAN REACH THE REAL SUDO. The probe cases inject the runner; the
// panel cases inject the probe. A case that spawned sudo would, on a desktop,
// interrupt whoever ran `npm test` with a password dialog, and on a NOPASSWD CI
// runner it would answer "free" and silently assert nothing.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";

import { ClusterPresence } from "../src/clusters/presence.js";
import { graphDocumentPath, loadGraphFile, type Verify } from "../src/install/graph.js";
import type { ScriptOutcome, ScriptRun } from "../src/install/runner.js";
import { ELEVATE_DIALOG_ENV, elevationEnv, sudoRunsWithoutAsking } from "../src/install/sudoAgent.js";
import { AddClusterPanel, type AddClusterDeps } from "../src/webview/addClusterPanel.js";
import {
  recorded,
  resetRecorded,
  setNextInputBoxResult,
  type StubWebviewPanel,
} from "./support/vscodeStub.js";

// dist-test/test/<bundle>.js -> the repository root. The same four levels
// addClusterPanel.test.ts walks, and for the same reason: the graph under test is
// the SHIPPED scripts/install/graph/install.json, not a fixture that agrees.
const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), "memql-elevation-"));
const KEY_FILE = path.join(HOME, "provider.key");
fs.writeFileSync(KEY_FILE, "sk-test\n");

// -----------------------------------------------------------------------------
// the probe
// -----------------------------------------------------------------------------

test("the probe ignores the credential cache, because the cache it can see is not the one the scripts get", async () => {
  const asked: string[][] = [];
  // A machine that is NOT NOPASSWD but has a cached credential for THIS process:
  // `-n true` succeeds, `-n -k true` does not. That is the exact machine state
  // that skipped the agent and produced three desktop dialogs.
  const runner = async (args: string[]): Promise<number> => {
    asked.push(args);
    return args.includes("-k") ? 1 : 0;
  };

  const free = await sudoRunsWithoutAsking(runner);

  assert.equal(
    free,
    false,
    "the probe reported that sudo runs without asking because a credential was cached for the\n" +
      "EXTENSION HOST. Every capability script is its own process with its own cache record, so\n" +
      "that credential is unusable by any of them and the password still has to be collected.",
  );
  assert.ok(
    asked.every((args) => args.includes("-k")),
    `every probe must pass -k so a cached credential cannot answer for a NOPASSWD one: ${JSON.stringify(asked)}`,
  );
  assert.ok(
    asked.every((args) => args.includes("-n")),
    `every probe must pass -n so the probe itself can never block: ${JSON.stringify(asked)}`,
  );
});

test("a genuinely NOPASSWD machine is still not asked for a password", async () => {
  const free = await sudoRunsWithoutAsking(async () => 0);
  assert.equal(free, true, "sudo runs without asking here, so a prompt would be a question with no consequence");
});

// -----------------------------------------------------------------------------
// the environment every step is handed
// -----------------------------------------------------------------------------

test("the wizard tells the scripts it owns the asking, agent or no agent", () => {
  const withAgent = elevationEnv("/tmp/agent/askpass");
  assert.equal(withAgent.SUDO_ASKPASS, "/tmp/agent/askpass");
  assert.equal(withAgent[ELEVATE_DIALOG_ENV], "never");

  // THE CASE THAT SHIPPED. No agent -- the operator dismissed the box, or the
  // probe answered wrongly -- and the scripts must still not draw a dialog. A
  // step that needs root refuses with the terminal remedy it already carries,
  // which is one answer rather than three prompts of a shape the wizard never
  // chose.
  const withoutAgent = elevationEnv(undefined);
  assert.equal(withoutAgent[ELEVATE_DIALOG_ENV], "never");
  assert.ok(
    !("SUDO_ASKPASS" in withoutAgent),
    "SUDO_ASKPASS must be absent rather than empty: sudo treats an empty value as a helper it\n" +
      "cannot execute, which is a different failure from having none",
  );
});

// -----------------------------------------------------------------------------
// the panel, end to end over the real graph
// -----------------------------------------------------------------------------

interface FakeRunner {
  run: (run: ScriptRun) => Promise<ScriptOutcome>;
  calls: ScriptRun[];
}

function satisfying(verify: Verify): Record<string, unknown> {
  const field = (verify.field ?? "").replace(/^result\./, "");
  if (field === "") return {};
  switch (verify.kind) {
    case "resultTrue":
      return { [field]: true };
    case "resultNonEmpty":
      return { [field]: `a-${field}` };
    case "resultEquals":
      return { [field]: verify.value ?? "" };
    default:
      return {};
  }
}

async function fakeRunner(): Promise<FakeRunner> {
  const graph = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const verifyFor = new Map<string, Verify>();
  for (const step of graph.steps) verifyFor.set(step.script, step.verify);

  const fake: FakeRunner = {
    calls: [],
    run: async (run: ScriptRun): Promise<ScriptOutcome> => {
      fake.calls.push(run);
      const capability = run.capability ?? "";
      const verify = verifyFor.get(capability);
      assert.ok(verify !== undefined, `unknown capability ${capability}`);
      return {
        argv: [run.scriptPath],
        exitCode: 0,
        signal: null,
        stdout: "",
        stderr: "",
        envelope: { ok: true, capability, changed: true, result: satisfying(verify), error: null },
      };
    },
  };
  return fake;
}

function presence(): ClusterPresence {
  return new ClusterPresence({
    clustersPath: path.join(HOME, "clusters.yaml"),
    receiptPath: path.join(HOME, "no-such-receipt.json"),
    readReceiptFile: async () => null,
    readClusters: async () => ({ ok: true as const, file: { selectedCluster: "", clusters: [] } }),
    probe: async () => false,
  });
}

function openPanel(options: {
  runner: FakeRunner;
  sudoIsFree: boolean;
  passwordAccepted?: boolean;
}): StubWebviewPanel {
  resetRecorded();
  const dir = fs.mkdtempSync(path.join(HOME, "case-"));
  const deps: AddClusterDeps = {
    diagnostics: { appendLine: () => {} },
    clustersPath: path.join(dir, "clusters.yaml"),
    receiptFile: path.join(dir, "install-receipt.json"),
    installRoot: REPO_ROOT,
    // Kept out of the developer's own ~/.memql/runs: every run writes a record
    // now (memql#3739), and the default is the real one.
    runsDir: path.join(dir, "runs"),
    refreshTree: () => undefined,
    removeRegistryEntry: async () => undefined,
    runScript: options.runner.run,
    // INJECTED, so no case here can reach the real sudo -- neither the probe nor
    // the `sudo -A -v` that validates a collected password. Also the only way to
    // drive the two machines that matter -- one that needs a password and one
    // that does not -- from a single test run.
    sudoIsFree: async () => options.sudoIsFree,
    sudoAccepts: async () => options.passwordAccepted ?? true,
  };
  AddClusterPanel.show({ subscriptions: [] } as unknown as ExtensionContext, presence(), deps, "install");
  return recorded.webviews[recorded.webviews.length - 1]!;
}

function beginInstall(panel: StubWebviewPanel): void {
  panel.send({ type: "choose", value: "install" });
  panel.send({ type: "input", value: { field: "domain", text: "memql.localhost" } });
  panel.send({ type: "input", value: { field: "ownerFirstName", text: "Ada" } });
  panel.send({ type: "input", value: { field: "ownerLastName", text: "Lovelace" } });
  panel.send({ type: "input", value: { field: "ownerEmail", text: "ada@example.com" } });
  panel.send({ type: "input", value: { field: "provider", text: "anthropic" } });
  panel.send({ type: "input", value: { field: "providerKeyFile", text: KEY_FILE } });
  panel.send({ type: "begin" });
}

async function until(condition: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 400; i += 1) {
    if (condition()) return;
    await new Promise((r) => setTimeout(r, 10));
  }
  assert.fail(`timed out waiting for ${what}`);
}

// The privileged steps of the install graph, as the graph itself declares them.
// Read rather than listed, so a step that starts needing root is covered without
// this file being edited.
async function privilegedCapabilities(): Promise<Set<string>> {
  const graph = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  return new Set(graph.steps.filter((s) => s.elevation !== "none").map((s) => s.script));
}

test("every step of an install is told the wizard owns the asking", async () => {
  const runner = await fakeRunner();
  const panel = openPanel({ runner, sudoIsFree: false });
  try {
    setNextInputBoxResult("hunter2");
    const privileged = await privilegedCapabilities();
    assert.ok(privileged.size > 0, "the install graph declares no privileged step at all");
    beginInstall(panel);
    // Waits for the LAST privileged step rather than a call count: the three do
    // not share a wave, so a count reached before mkcert ran would assert less
    // than this reads as.
    await until(
      () => privileged.size === new Set(runner.calls.map((c) => c.capability).filter((c) => privileged.has(c ?? ""))).size,
      "the run to reach every privileged step",
    );
    const sawPrivileged = new Set<string>();
    for (const call of runner.calls) {
      assert.equal(
        call.env?.[ELEVATE_DIALOG_ENV],
        "never",
        `${call.capability} was not told the wizard owns the asking, so it is free to draw its own\n` +
          `desktop dialog in front of an operator who has already answered one`,
      );
      if (privileged.has(call.capability ?? "")) {
        sawPrivileged.add(call.capability ?? "");
        assert.ok(
          (call.env?.SUDO_ASKPASS ?? "") !== "",
          `${call.capability} needs root and was given no way for sudo to ask, which is the state\n` +
            `that used to end in a terminal handoff`,
        );
      }
    }
    assert.deepEqual(
      [...sawPrivileged].sort(),
      [...privileged].sort(),
      "the run did not reach every privileged step, so this asserted less than it reads as",
    );
  } finally {
    panel.close();
  }
});

// THE RUN THAT SHIPPED THREE DIALOGS. No password reaches the extension -- the
// box was dismissed -- and the scripts must STILL not prompt. The password
// having been declined is an answer; asking again in a different shape is not
// what the wizard does with it.
test("a run that collected no password still does not let the scripts prompt", async () => {
  const runner = await fakeRunner();
  const panel = openPanel({ runner, sudoIsFree: false });
  try {
    setNextInputBoxResult(undefined); // dismissed
    beginInstall(panel);
    await until(() => runner.calls.length > 3, "the run to reach its privileged steps");

    for (const call of runner.calls) {
      assert.equal(call.env?.[ELEVATE_DIALOG_ENV], "never", `${call.capability} may draw its own dialog`);
      assert.ok(
        call.env?.SUDO_ASKPASS === undefined,
        `${call.capability} was given a SUDO_ASKPASS for an agent that does not exist`,
      );
    }
  } finally {
    panel.close();
  }
});

// A NOPASSWD machine is never asked, which is the case the probe exists to spot.
// The marker still travels: `elevate_method` answers `free` there and never
// consults it, so this is about the environment being one shape rather than two.
test("a machine that needs no password is not asked for one", async () => {
  const runner = await fakeRunner();
  const panel = openPanel({ runner, sudoIsFree: true });
  try {
    setNextInputBoxResult("should-never-be-read");
    beginInstall(panel);
    await until(() => runner.calls.length > 3, "the run to reach its privileged steps");

    assert.equal(
      recorded.inputBoxes.length,
      0,
      "the operator was asked for a password on a machine where sudo runs without one, which is the\n" +
        "surest way to teach someone to type their password at anything",
    );
  } finally {
    panel.close();
  }
});
