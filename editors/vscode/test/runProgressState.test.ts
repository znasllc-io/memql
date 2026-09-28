// The progress model, wired into the machines that fold a run.
//
// `computeRunProgress` is pure and tested on its own (runProgress.test.ts).
// What is asserted here is the wiring that feeds it: that the install and
// uninstall machines record when each step started and finished, keep the
// phase a step last reported, start every step again on each attempt, and
// never let the bar run backwards within one -- and that the run records keep
// each step's start, so the next run's bar can be weighed by this one.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import type { ExecEvent, StepOutcome } from "../src/install/executor.js";
import type { Step } from "../src/install/graph.js";
import { AddClusterState } from "../src/state/addCluster.js";
import { UninstallRunState } from "../src/state/uninstallRun.js";
import { RunRecorder } from "../src/state/runRecorder.js";
import { readRun, runFilePath, writeRun } from "../src/state/runLog.js";
import { historicalWeights, weightsFromRuns } from "../src/state/runProgress.js";
import type { Run } from "../src/state/deployments.js";

function step(id: string, label = `${id} label`): Step {
  return {
    id,
    script: "install.binary",
    label,
    description: `${id} description`,
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  };
}

function plan(...ids: string[]): ExecEvent {
  return {
    type: "runStarted",
    steps: ids.map((id) => ({ id, label: `${id} label`, description: `${id} description` })),
  };
}

function started(id: string): ExecEvent {
  return { type: "stepStarted", step: step(id), params: {} };
}

function finished(id: string, status: StepOutcome["status"], over: Partial<StepOutcome> = {}): ExecEvent {
  return {
    type: "stepFinished",
    step: step(id),
    outcome: {
      id,
      script: "install.binary",
      status,
      exitCode: status === "failed" ? 5 : 0,
      envelope: null,
      verified: status === "ok",
      preExisting: false,
      params: {},
      startedAt: "",
      finishedAt: "",
      ...over,
    },
  };
}

/** A clock a test moves by hand, in epoch milliseconds. */
function clock(start = 1_000_000): { now: () => number; advance: (ms: number) => void } {
  let t = start;
  return { now: () => t, advance: (ms) => (t += ms) };
}

// -----------------------------------------------------------------------------
// AddClusterState
// -----------------------------------------------------------------------------

test("the install machine times each step from its own clock and the outcome's", () => {
  const c = clock();
  const s = new AddClusterState({ now: c.now });
  s.apply(plan("detect", "clusterUp"));
  s.apply(started("detect"));
  c.advance(4_000);
  s.apply(
    finished("detect", "ok", { startedAt: "2026-09-28T10:00:00.000Z", finishedAt: "2026-09-28T10:00:04.000Z" }),
  );
  const detect = s.steps.find((row) => row.id === "detect");
  assert.equal(detect?.label, "detect label");
  assert.equal(detect?.startedAt, 1_000_000, "the start is when the step started here");
  assert.equal(detect?.finishedAt, Date.parse("2026-09-28T10:00:04.000Z"), "the finish is the executor's");
});

test("a step that settles without starting starts and finishes at once", () => {
  const c = clock();
  const s = new AddClusterState({ now: c.now });
  s.apply(plan("providerFederation"));
  s.apply(finished("providerFederation", "skipped"));
  const row = s.steps[0];
  assert.equal(row?.startedAt, c.now());
  assert.equal(row?.finishedAt, c.now());
});

test("a stepPhase is kept on the running step, and cleared when it settles", () => {
  const s = new AddClusterState({ now: clock().now });
  s.apply(plan("clusterUp"));
  s.apply(started("clusterUp"));
  s.apply({ type: "stepPhase", step: step("clusterUp"), label: "Starting services", done: 4, total: 9 });
  assert.deepEqual(s.steps[0]?.phase, { label: "Starting services", done: 4, total: 9 });
  assert.equal(s.steps[0]?.log, "", "a phase is not a log line");
  assert.equal(s.progress().status, "Starting services 4 of 9");
  s.apply(finished("clusterUp", "ok"));
  assert.equal(s.steps[0]?.phase, undefined);
});

test("the copy a caller gets shares no phase object with the record", () => {
  const s = new AddClusterState({ now: clock().now });
  s.apply(plan("clusterUp"));
  s.apply(started("clusterUp"));
  s.apply({ type: "stepPhase", step: step("clusterUp"), label: "Installing ArgoCD" });
  const copy = s.steps[0];
  if (copy?.phase) copy.phase.label = "tampered";
  assert.equal(s.steps[0]?.phase?.label, "Installing ArgoCD");
});

test("progress(now) is weighted, and never runs backwards within an attempt", () => {
  const c = clock();
  const s = new AddClusterState({ now: c.now });
  s.setStepWeights({ detect: 10, clusterUp: 90 });
  s.apply(plan("detect", "clusterUp"));
  s.apply(started("detect"));
  s.apply(finished("detect", "ok"));
  s.apply(started("clusterUp"));
  c.advance(81_000);
  const timed = s.progress();
  assert.equal(timed.percent, 91, "10 + 90 * 0.9");
  assert.equal(timed.status, "clusterUp label");
  assert.equal(timed.stepText, "Step 2 of 2");

  // The step's first count is lower than the clock had it. The bar holds.
  s.apply({ type: "stepPhase", step: step("clusterUp"), label: "Starting services", done: 1, total: 9 });
  assert.equal(s.progress().percent, 91);
  assert.equal(s.progress().status, "Starting services 1 of 9");

  s.apply(finished("clusterUp", "ok"));
  assert.equal(s.progress().percent, 100);
});

test("each attempt's runStarted starts every step from pending, with a fresh bar", () => {
  const c = clock();
  const s = new AddClusterState({ now: c.now });
  s.chooseAction("install");
  s.beginRun();
  s.apply(plan("binary", "cluster"));
  s.apply(started("binary"));
  s.apply(finished("binary", "ok"));
  s.apply(started("cluster"));
  s.apply({ type: "stepLog", step: step("cluster"), line: "pulling" });
  s.apply(finished("cluster", "failed"));
  const failedRun = s.progress();
  assert.equal(failedRun.percent, 100, "a failed run has settled");

  s.retry();
  s.apply(plan("binary", "cluster"));
  assert.deepEqual(
    s.steps.map((row) => [row.id, row.state, row.previousState]),
    [
      ["binary", "pending", "done"],
      ["cluster", "pending", "failed"],
    ],
    "every step pending again; what it came to last time is kept for display",
  );
  assert.equal(s.steps[1]?.log, "", "the previous attempt's output is not this attempt's");
  assert.equal(s.steps[0]?.startedAt, undefined);
  assert.equal(s.progress().percent, 0, "the new attempt's bar starts from what is true");

  // And within the new attempt it only moves forward.
  s.apply(finished("binary", "skipped"));
  const afterSkip = s.progress().percent;
  s.apply(started("cluster"));
  assert.ok(s.progress().percent >= afterSkip);
});

test("a second graph on the same page does not inherit the first graph's steps", () => {
  // The deployment page holds one machine for its lifetime and runs a rebuild,
  // then an update. The rebuild's step must not sit pending in the update's
  // total forever.
  const s = new AddClusterState({ now: clock().now });
  s.apply(plan("rebuildFromCheckout"));
  s.apply(finished("rebuildFromCheckout", "ok"));
  s.apply(plan("updateCheckout", "rebuildFromCheckout"));
  assert.deepEqual(
    s.steps.map((row) => row.id),
    ["updateCheckout", "rebuildFromCheckout"],
  );
  s.apply(finished("updateCheckout", "ok"));
  s.apply(finished("rebuildFromCheckout", "ok"));
  assert.equal(s.progress().percent, 100);
});

test("a step switched to guided stays guided into the next attempt", () => {
  const s = new AddClusterState({ now: clock().now });
  s.chooseAction("install");
  s.beginRun();
  s.apply(plan("hostsBlock"));
  s.apply(finished("hostsBlock", "failed"));
  s.switchToGuided();
  s.apply(plan("hostsBlock"));
  assert.equal(s.steps[0]?.guided, true);
});

// -----------------------------------------------------------------------------
// UninstallRunState
// -----------------------------------------------------------------------------

test("the uninstall machine folds the same timings and phases", () => {
  const c = clock();
  const s = new UninstallRunState({ now: c.now });
  s.begin();
  s.setStepWeights({ removeCluster: 30, removeCheckout: 10 });
  s.apply(plan("removeCluster", "removeCheckout"));
  s.apply(started("removeCluster"));
  c.advance(15_000);
  s.apply({ type: "stepPhase", step: step("removeCluster"), label: "Deleting the cluster" });
  const mid = s.progress();
  assert.equal(mid.percent, 37, "30 * 0.5 of 40");
  assert.equal(mid.status, "Deleting the cluster");
  assert.equal(s.steps[0]?.startedAt, 1_000_000);
  s.apply(finished("removeCluster", "ok"));
  s.apply(finished("removeCheckout", "preserved"));
  assert.equal(s.progress().percent, 100);
  assert.equal(s.steps[1]?.state, "preserved");
});

test("the uninstall machine starts each attempt from pending, too", () => {
  const s = new UninstallRunState({ now: clock().now });
  s.begin();
  s.apply(plan("removeCluster"));
  s.apply(finished("removeCluster", "failed"));
  assert.equal(s.progress().percent, 100);
  s.apply(plan("removeCluster"));
  assert.equal(s.steps[0]?.state, "pending");
  assert.equal(s.steps[0]?.previousState, "failed");
  assert.equal(s.progress().percent, 0);
});

// -----------------------------------------------------------------------------
// RunRecorder keeps the start, and historicalWeights reads it back
// -----------------------------------------------------------------------------

function tmpdir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "memql-runprogress-"));
}

test("RunRecorder keeps each step's start beside its finish", async () => {
  const dir = await tmpdir();
  const times = ["2026-09-28T10:00:00.000Z", "2026-09-28T10:00:02.000Z", "2026-09-28T10:08:02.000Z"];
  let i = 0;
  const recorder = await RunRecorder.begin({
    dir,
    instance: "local",
    kind: "install",
    now: () => times[Math.min(i++, times.length - 1)]!,
    entropy: "aaaa1111",
  });
  await recorder.apply(plan("clusterUp"));
  await recorder.apply(started("clusterUp"));
  await recorder.apply(finished("clusterUp", "ok"));
  const run = await readRun(runFilePath(dir, recorder.current.id));
  const item = run?.items.find((it) => it.label === "clusterUp");
  assert.equal(item?.startedAt, "2026-09-28T10:00:02.000Z", "the start written at stepStarted survives the finish");
  assert.equal(item?.at, "2026-09-28T10:08:02.000Z");
});

test("RunRecorder prefers the executor's own start time when the outcome has one", async () => {
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({ dir, instance: "local", kind: "install", now: () => "t0", entropy: "bbbb2222" });
  await recorder.apply(started("detect"));
  await recorder.apply(
    finished("detect", "ok", { startedAt: "2026-09-28T10:00:00.000Z", finishedAt: "2026-09-28T10:00:05.000Z" }),
  );
  const item = recorder.current.items.find((it) => it.label === "detect");
  assert.equal(item?.startedAt, "2026-09-28T10:00:00.000Z");
  assert.equal(item?.at, "2026-09-28T10:00:05.000Z");
});

function run(id: string, startedAt: string, status: Run["status"], items: Run["items"], kind: Run["kind"] = "install"): Run {
  return { id, instance: "local", kind, startedAt, status, items };
}

test("weightsFromRuns reads each step's most recent successful duration", () => {
  const weights = weightsFromRuns([
    run("old", "2026-09-01T00:00:00Z", "succeeded", [
      { label: "clusterUp", status: "ok", startedAt: "2026-09-01T00:00:00Z", at: "2026-09-01T00:10:00Z" },
      { label: "detect", status: "ok", startedAt: "2026-09-01T00:00:00Z", at: "2026-09-01T00:00:06Z" },
    ]),
    run("new", "2026-09-20T00:00:00Z", "succeeded", [
      { label: "clusterUp", status: "ok", startedAt: "2026-09-20T00:00:00Z", at: "2026-09-20T00:05:00Z" },
      // Skipped: it took no time, and must not teach the bar that it is instant.
      { label: "detect", status: "skipped", startedAt: "2026-09-20T00:00:00Z", at: "2026-09-20T00:00:00Z" },
      // Written before starts were kept: no measurement rather than a guess.
      { label: "localCA", status: "ok", at: "2026-09-20T00:00:09Z" },
    ]),
    run("failed", "2026-09-27T00:00:00Z", "failed", [
      { label: "clusterUp", status: "ok", startedAt: "2026-09-27T00:00:00Z", at: "2026-09-27T00:00:30Z" },
    ]),
  ]);
  assert.deepEqual(weights, { clusterUp: 300, detect: 6 });
});

test("weightsFromRuns can be narrowed to the kind of run being weighed", () => {
  const runs = [
    run("repair", "2026-09-20T00:00:00Z", "succeeded", [
      { label: "clusterUp", status: "ok", startedAt: "2026-09-20T00:00:00Z", at: "2026-09-20T00:00:40Z" },
    ], "repair"),
    run("install", "2026-09-10T00:00:00Z", "succeeded", [
      { label: "clusterUp", status: "ok", startedAt: "2026-09-10T00:00:00Z", at: "2026-09-10T00:09:00Z" },
    ]),
  ];
  assert.deepEqual(weightsFromRuns(runs), { clusterUp: 40 });
  assert.deepEqual(weightsFromRuns(runs, { kinds: ["install"] }), { clusterUp: 540 });
});

test("historicalWeights reads the directory it is given, and an absent one is no history", async () => {
  const dir = await tmpdir();
  await writeRun(
    dir,
    run("install-20260920T000000Z-aaaa1111", "2026-09-20T00:00:00Z", "succeeded", [
      { label: "seedBootstrap", status: "ok", startedAt: "2026-09-20T00:00:00Z", at: "2026-09-20T00:01:12Z" },
    ]),
  );
  assert.deepEqual(await historicalWeights(dir), { seedBootstrap: 72 });
  assert.deepEqual(await historicalWeights(path.join(dir, "nope")), {});
});
