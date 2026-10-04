// The cluster page's long runs, held outside the page (deploy/localRun.ts).
//
// Three defects lived in the page's own run state and are driven here, where
// a fake runner can hold a step open and a test can look at the run mid-way:
//
//   RUN STATE BLED BETWEEN RUNS -- a second run opened on the first one's rows,
//   log and failure. Each run now has its own state.
//   CANCEL PROMISED WHAT IT COULD NOT DO -- a one-step rebuild cannot stop part
//   way, and a run that finished anyway was recorded as cancelled. Cancel is
//   offered only while a later step is ahead, and the record says what happened.
//   A SECOND RUN COULD START BESIDE THE FIRST -- the slot refuses it.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { LocalRuns, failedStatus, runWords, type LocalRunDeps } from "../src/deploy/localRun.js";
import { loadGraph, type Graph } from "../src/install/graph.js";
import type { RunScript, ScriptOutcome } from "../src/install/runner.js";
import { listRuns } from "../src/state/runLog.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

function step(id: string, script: string, label: string, dependsOn: string[] = []) {
  return {
    id,
    label,
    description: `${id} description`,
    script,
    ...(dependsOn.length === 0 ? {} : { dependsOn }),
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    receipt: id,
    preExistingPath: "none",
    verify: { kind: "resultTrue", field: "result.installed" },
  };
}

/** Two steps in two waves: a Cancel pressed during the first can stop before the second. */
const TWO_WAVES: Graph = loadGraph(
  JSON.stringify({
    name: "test-install",
    kind: "install",
    steps: [step("fetch", "install.binary", "Downloading MemQL"), step("apply", "k3d.up", "Creating the cluster", ["fetch"])],
  }),
  "test-fixture",
);

const OK: ScriptOutcome = {
  argv: [],
  exitCode: 0,
  signal: null,
  stdout: "",
  stderr: "",
  envelope: { ok: true, capability: "t", changed: true, result: { installed: true, imageSource: "checkout", commit: "abc1234def", dirtyCount: 0 }, error: null },
};

/** A runner whose steps wait until the test lets them go, and can be told to fail. */
function heldRunner() {
  const waiting = new Map<string, (outcome: ScriptOutcome) => void>();
  const started: string[] = [];
  const run: RunScript = async ({ capability, onLog }) => {
    const id = capability ?? "";
    started.push(id);
    onLog?.(`working on ${id}`);
    return new Promise<ScriptOutcome>((resolve) => waiting.set(id, resolve));
  };
  const release = (id: string, outcome: ScriptOutcome = OK): void => {
    const done = waiting.get(id);
    waiting.delete(id);
    done?.(outcome);
  };
  return { run, release, started };
}

async function until(check: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 400; i += 1) {
    if (check()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.fail(`timed out waiting for ${what}`);
}

async function deps(run: RunScript, over: Partial<LocalRunDeps> = {}): Promise<LocalRunDeps> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-localrun-"));
  return {
    installRoot: REPO_ROOT,
    receiptFile: path.join(dir, "install-receipt.json"),
    runsDir: path.join(dir, "runs"),
    runScript: run,
    graphs: { update: TWO_WAVES, changeVersion: TWO_WAVES },
    ...over,
  };
}

test("each run is said in its own words", () => {
  assert.equal(runWords({ kind: "update", instance: "local", from: "v1", to: "v2" }).title, "Updating MemQL");
  assert.equal(runWords({ kind: "changeVersion", instance: "local", from: "v1", to: "v2" }).title, "Changing version");
  assert.equal(runWords({ kind: "rebuild", instance: "local", checkout: "/c", nodes: "" }).title, "Rebuilding MemQL");
  assert.equal(
    runWords({ kind: "pullRebuild", instance: "local", checkout: "/c", nodes: "", branch: "main", strategy: "fastForward" }).busy,
    "Pulling and rebuilding",
  );
});

test("a failed step is said in the negative, with the label's verb in its base form", () => {
  assert.equal(failedStatus("Creating the cluster"), "Couldn't create the cluster");
  assert.equal(failedStatus("Rebuilding MemQL"), "Couldn't rebuild MemQL");
  assert.equal(failedStatus("Downloading updates"), "Couldn't download updates");
  assert.equal(failedStatus("Setting up browser trust"), "Couldn't set up browser trust");
  assert.equal(failedStatus("Adding local addresses"), "Couldn't add local addresses");
  assert.equal(failedStatus("Odd label"), "Odd label failed");
});

test("Cancel is offered while a later step is ahead, and stops before it -- recorded as cancelled", async () => {
  const runner = heldRunner();
  const slot = new LocalRuns();
  const d = await deps(runner.run);
  const run = slot.start({ kind: "update", instance: "local", from: "v0.23.5", to: "v0.24.0" }, d)!;
  await until(() => runner.started.includes("install.binary"), "the first step");
  assert.equal(run.status, "running");
  assert.equal(run.cancellable, true, "a later step is ahead, so Cancel can act");

  assert.equal(run.cancel(), true);
  assert.equal(run.status, "stopping");
  assert.equal(run.progress().status, "Stopping after the current step");
  runner.release("install.binary");
  await run.settled();

  assert.equal(run.status, "stopped");
  assert.equal(runner.started.includes("k3d.up"), false, "the second step ran after a Cancel");
  const [record] = await listRuns(d.runsDir);
  assert.equal(record?.status, "cancelled");
});

test("a one-step rebuild offers no Cancel once it is building, and is recorded as what it came to", async () => {
  const runner = heldRunner();
  const slot = new LocalRuns();
  const d = await deps(runner.run);
  const run = slot.start({ kind: "rebuild", instance: "local", checkout: "/home/me/.memql/stack", nodes: "" }, d)!;
  await until(() => runner.started.includes("k3d.dev"), "the rebuild step");
  // THE HONEST ANSWER: there is no boundary left for Cancel to stop at.
  assert.equal(run.cancellable, false);
  assert.equal(run.cancel(), false);
  assert.equal(run.status, "running");
  // "Step 1 of 1" counts nothing.
  assert.equal(run.progress().stepText, "");

  runner.release("k3d.dev");
  await run.settled();
  assert.equal(run.status, "done");
  assert.equal(run.result, "local now runs your build (abc1234).");
  const [record] = await listRuns(d.runsDir);
  assert.equal(record?.status, "succeeded");
});

test("a second run starts from nothing the first one left: no rows, no log, no failure", async () => {
  const runner = heldRunner();
  const slot = new LocalRuns();
  const d = await deps(runner.run);
  const first = slot.start({ kind: "update", instance: "local", from: "v0.23.5", to: "v0.24.0" }, d)!;
  await until(() => runner.started.includes("install.binary"), "the first run's step");
  runner.release("install.binary", { ...OK, exitCode: 5, envelope: { ...OK.envelope!, ok: false, result: {} } });
  await first.settled();
  assert.equal(first.status, "failed");
  assert.ok(first.log.length > 0);

  const second = slot.start({ kind: "rebuild", instance: "local", checkout: "/c", nodes: "" }, d)!;
  assert.notEqual(second, first);
  assert.deepEqual(second.state.failures, [], "the last run's failure bled into this one");
  assert.deepEqual(second.log, [], "the last run's log bled into this one");
  await until(() => runner.started.includes("k3d.dev"), "the second run's step");
  assert.deepEqual(second.state.steps.map((s) => s.id), ["rebuildFromCheckout"], "the last run's rows bled into this one");
  runner.release("k3d.dev");
  await second.settled();
  assert.equal(second.status, "done");
});

test("a run in flight holds the slot: a second start is refused and the first is what shows", async () => {
  const runner = heldRunner();
  const slot = new LocalRuns();
  const d = await deps(runner.run);
  const first = slot.start({ kind: "update", instance: "local", from: "v0.23.5", to: "v0.24.0" }, d)!;
  assert.equal(slot.inFlight, true);
  assert.equal(slot.start({ kind: "rebuild", instance: "local", checkout: "/c", nodes: "" }, d), undefined);
  assert.equal(slot.current, first);
  await until(() => runner.started.includes("install.binary"), "the step");
  runner.release("install.binary");
  await until(() => runner.started.includes("k3d.up"), "the next step");
  runner.release("k3d.up");
  await first.settled();
  assert.equal(slot.inFlight, false);
  // A settled run stays until it is dismissed, so a page reopened after it can
  // still show it -- and the slot is free for the next.
  assert.equal(slot.current, first);
  slot.dismiss(first);
  assert.equal(slot.current, undefined);
});

test("a failure leads with the step, its reason, and says it in the negative; Retry runs it again", async () => {
  const runner = heldRunner();
  const slot = new LocalRuns();
  const d = await deps(runner.run);
  const run = slot.start({ kind: "update", instance: "local", from: "v0.23.5", to: "v0.24.0" }, d)!;
  await until(() => runner.started.includes("install.binary"), "the first step");
  runner.release("install.binary");
  await until(() => runner.started.includes("k3d.up"), "the second step");
  runner.release("k3d.up", { ...OK, exitCode: 5, envelope: { ...OK.envelope!, ok: false, result: {} } });
  await run.settled();

  assert.equal(run.status, "failed");
  assert.equal(run.failure?.label, "Creating the cluster");
  assert.notEqual(run.failure?.reason, "");
  assert.equal(run.progress().status, "Couldn't create the cluster");
  assert.equal(run.progress().state, "failed");

  const retry = run.retry();
  assert.equal(run.status, "running");
  assert.deepEqual(run.log, [], "Retry kept the failed attempt's log");
  await until(() => runner.started.filter((s) => s === "install.binary").length === 2, "the retried first step");
  runner.release("install.binary");
  await until(() => runner.started.filter((s) => s === "k3d.up").length === 2, "the retried second step");
  runner.release("k3d.up");
  await retry;
  assert.equal(run.status, "done");
  assert.equal(run.progress().title, "local is on v0.24.0");
});

test("every log line is kept in order with its step's label, and mirrored to the Output channel", async () => {
  const runner = heldRunner();
  const mirrored: string[] = [];
  const slot = new LocalRuns();
  const d = await deps(runner.run, { log: (line) => mirrored.push(line) });
  const run = slot.start({ kind: "update", instance: "local", from: "v0.23.5", to: "v0.24.0" }, d)!;
  const streamed: string[] = [];
  run.onEvent((event) => {
    if (event.type === "log") for (const line of event.lines) streamed.push(line.text);
  });
  await until(() => runner.started.includes("install.binary"), "the first step");
  runner.release("install.binary");
  await until(() => runner.started.includes("k3d.up"), "the second step");
  runner.release("k3d.up");
  await run.settled();
  assert.deepEqual(run.log.map((l) => [l.label, l.text]), [
    ["Downloading MemQL", "working on install.binary"],
    ["Creating the cluster", "working on k3d.up"],
  ]);
  assert.deepEqual(streamed, ["working on install.binary", "working on k3d.up"]);
  assert.deepEqual(mirrored, ["Downloading MemQL: working on install.binary", "Creating the cluster: working on k3d.up"]);
});
