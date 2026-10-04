// How much of a run is left, as a number an operator can be shown.
//
// WHY THIS IS TESTED AT ALL. The bar is the most confident claim a run screen
// makes -- "you are two-thirds of the way through a ten-minute operation" --
// and it is made to somebody who has no other way to check it. A progress
// display that is decorative is worse than none: it teaches an operator to
// distrust the one signal they have while a real install is running.
//
// The panels that draw it import `vscode` and cannot be reached from this lane,
// which is why the arithmetic lives in state/runProgress.ts and the renderer
// only interpolates it.
//
// THE PROPERTIES THAT MATTER: the bar is weighted by how long each step takes,
// follows a step's own phase count when it reports one, drops skipped steps
// from the total instead of leaping over them, and never runs backwards within
// an attempt.

import test from "node:test";
import assert from "node:assert/strict";

import {
  DEFAULT_STEP_WEIGHTS,
  UNKNOWN_STEP_WEIGHT,
  computeRunProgress,
  defaultStepWeight,
  stepWeight,
  type ProgressStep,
  failedLabel,
  FAILED_LABEL_GERUNDS,
} from "../src/state/runProgress.js";
import { progressStepsOf, runProgressOf } from "../src/state/installProgress.js";
import type { StepProgress, StepState } from "../src/state/addCluster.js";

const NOW = 1_000_000;

function ps(id: string, status: ProgressStep["status"], weight: number, over: Partial<ProgressStep> = {}): ProgressStep {
  return { id, label: `${id} label`, weight, status, ...over };
}

function row(id: string, state: StepState, over: Partial<StepProgress> = {}): StepProgress {
  return {
    id,
    label: "",
    description: "",
    state,
    reason: "",
    exitCode: null,
    log: "",
    guided: false,
    remedy: "",
    ...over,
  };
}

// -----------------------------------------------------------------------------
// the number
// -----------------------------------------------------------------------------

test("before the plan arrives there is nothing to measure, and the line says Starting", () => {
  assert.deepEqual(computeRunProgress([], NOW), {
    percent: 0,
    status: "Starting",
    stepText: "",
    highWater: 0,
  });
});

test("the percent is the share of EXPECTED TIME behind the run, not of steps", () => {
  // Two quick checks done and the eight-minute step not started: by step count
  // that is two thirds; by the time it will take it is barely begun.
  const progress = computeRunProgress(
    [ps("detect", "ok", 5), ps("dockerAccess", "ok", 3), ps("clusterUp", "pending", 480)],
    NOW,
  );
  assert.equal(progress.percent, 1, "8 of 488 expected seconds");
  assert.equal(progress.stepText, "Step 3 of 3");
});

test("a running step earns its elapsed share of its weight", () => {
  const progress = computeRunProgress(
    [ps("a", "ok", 10), ps("b", "running", 90, { startedAt: NOW - 45_000 })],
    NOW,
  );
  // 10 + 90 * (45 / 90) = 55 of 100.
  assert.equal(progress.percent, 55);
});

test("elapsed time alone never claims more than 90% of a step", () => {
  // A step that overruns its estimate slows to a crawl rather than reporting
  // itself finished while it is still running.
  const progress = computeRunProgress([ps("a", "running", 10, { startedAt: NOW - 3_600_000 })], NOW);
  assert.equal(progress.percent, 90);
});

test("a step's own phase count outranks the clock", () => {
  const progress = computeRunProgress(
    [
      ps("a", "ok", 20),
      ps("clusterUp", "running", 80, {
        startedAt: NOW - 1_000,
        phase: { label: "Starting services", done: 5, total: 10 },
      }),
    ],
    NOW,
  );
  // 20 + 80 * 5/10 = 60 of 100, although only a second has passed.
  assert.equal(progress.percent, 60);
  assert.equal(progress.status, "Starting services 5 of 10");
});

test("a counted phase carries on from where the clock left the step", () => {
  // Creating the cluster spent 40 of its expected 100 seconds on uncounted
  // phases before its wait started counting services. 0 of 10 is where the
  // clock left it, not the start of the step; each service fills a tenth of
  // what was left.
  const step = (done: number): ProgressStep[] => [
    ps("clusterUp", "running", 100, {
      startedAt: NOW - 70_000,
      phase: { label: "Starting services", done, total: 10, since: NOW - 30_000 },
    }),
  ];
  assert.equal(computeRunProgress(step(0), NOW).percent, 40);
  assert.equal(computeRunProgress(step(5), NOW).percent, 70);
  assert.equal(computeRunProgress(step(10), NOW).percent, 99, "the whole step, and still not a finished run");
});

test("a phase with no count names the phase and leaves the bar to the clock", () => {
  const progress = computeRunProgress(
    [ps("clusterUp", "running", 100, { startedAt: NOW - 30_000, phase: { label: "Installing ArgoCD" } })],
    NOW,
  );
  assert.equal(progress.status, "Installing ArgoCD");
  assert.equal(progress.percent, 30);
});

test("a count past its total is read as the whole step, never more", () => {
  const progress = computeRunProgress(
    [ps("a", "running", 10, { phase: { label: "Building images", done: 12, total: 9 } }), ps("b", "pending", 10)],
    NOW,
  );
  assert.equal(progress.percent, 50);
});

test("a failure holds the bar where the run broke", () => {
  // clusterUp failed 240s into its 480; everything behind it was then skipped
  // without running. Dropping those from the total the way a planned skip is
  // dropped would carry this run to a full bar.
  const steps = [
    ps("detect", "ok", 5, { finishedAt: NOW - 300_000 }),
    ps("providerFederation", "skipped", 1, { finishedAt: NOW - 299_000 }),
    ps("clusterUp", "failed", 480, { startedAt: NOW - 250_000, finishedAt: NOW - 10_000 }),
    ps("seedBootstrap", "skipped", 90, { finishedAt: NOW - 10_000 }),
    ps("frontDoor", "skipped", 20, { finishedAt: NOW - 9_000 }),
  ];
  const progress = computeRunProgress(steps, NOW);
  // (5 + 480 * 0.5) of (5 + 480 + 90 + 20): the planned skip left, the
  // consequences did not.
  assert.equal(progress.percent, 41);
  assert.equal(progress.stepText, "Step 2 of 4", "it stopped on the failed step, of the steps meant to run");
  assert.equal(progress.status, "clusterUp label", "it names where it broke, not 'Finishing'");
});

test("a failure never reads as a finished bar, even with every step settled", () => {
  const progress = computeRunProgress([ps("a", "ok", 99), ps("b", "failed", 1)], NOW);
  assert.ok(progress.percent < 100);
});

test("100 is reserved for a settled run", () => {
  const running = computeRunProgress(
    [ps("a", "ok", 99), ps("b", "running", 1, { phase: { label: "x", done: 1, total: 1 } })],
    NOW,
  );
  assert.equal(running.percent, 99, "a full bar would say the run is over while a step still runs");
  const settled = computeRunProgress([ps("a", "ok", 99), ps("b", "ok", 1)], NOW);
  assert.equal(settled.percent, 100);
  assert.equal(settled.status, "Finishing");
});

test("the percent is a whole number across awkward divisions", () => {
  const progress = computeRunProgress(
    Array.from({ length: 13 }, (_, i) => ps(`s${i}`, i < 4 ? "ok" : "pending", 7)),
    NOW,
  );
  assert.equal(progress.percent, 30);
  assert.equal(Number.isInteger(progress.percent), true);
});

// -----------------------------------------------------------------------------
// skipped steps
// -----------------------------------------------------------------------------

test("a skipped step leaves the total instead of counting as done", () => {
  // A repair skips most of an install. Counting a skipped cluster step as eight
  // minutes done would make the bar leap on a step that did nothing.
  const progress = computeRunProgress(
    [ps("a", "ok", 10), ps("clusterUp", "skipped", 480), ps("c", "pending", 10)],
    NOW,
  );
  assert.equal(progress.percent, 50);
  assert.equal(progress.stepText, "Step 2 of 2", "only the steps that will run are counted");
});

test("learning that a step is skipped never moves the bar backwards", () => {
  const before = computeRunProgress(
    [ps("a", "ok", 10), ps("b", "pending", 480), ps("c", "running", 10, { startedAt: NOW - 5_000 })],
    NOW,
  );
  const after = computeRunProgress(
    [ps("a", "ok", 10), ps("b", "skipped", 480), ps("c", "running", 10, { startedAt: NOW - 5_000 })],
    NOW,
    { highWater: before.highWater },
  );
  assert.ok(after.percent >= before.percent);
  assert.equal(after.percent, 75, "10 + 10 * 0.5 of 20");
});

test("a run where everything was skipped is complete, with no step to count", () => {
  const progress = computeRunProgress([ps("a", "skipped", 10), ps("b", "skipped", 10)], NOW);
  assert.equal(progress.percent, 100);
  assert.equal(progress.stepText, "");
});

// -----------------------------------------------------------------------------
// monotonic within an attempt
// -----------------------------------------------------------------------------

test("the bar never runs backwards when a phase count restarts below the clock", () => {
  // Elapsed time had carried clusterUp to 90% of its weight; its first phase
  // count then reports 1 of 9. The bar holds rather than dropping.
  const steps = (phase?: ProgressStep["phase"]): ProgressStep[] => [
    ps("clusterUp", "running", 100, { startedAt: NOW - 600_000, ...(phase ? { phase } : {}) }),
  ];
  const first = computeRunProgress(steps(), NOW);
  assert.equal(first.percent, 90);
  const second = computeRunProgress(steps({ label: "Starting services", done: 1, total: 9 }), NOW, {
    highWater: first.highWater,
  });
  assert.equal(second.percent, 90, "held at the high-water mark");
  assert.equal(second.status, "Starting services 1 of 9", "the words still move on");
  const later = computeRunProgress(steps({ label: "Starting services", done: 9, total: 9 }), NOW, {
    highWater: second.highWater,
  });
  assert.equal(later.percent, 99);
});

test("a new attempt, given no high-water mark, starts again from what is true", () => {
  const failed = computeRunProgress([ps("a", "ok", 10), ps("b", "failed", 10), ps("c", "skipped", 10)], NOW);
  assert.equal(failed.percent, 33, "the failed run stopped a third of the way in");
  const retry = computeRunProgress([ps("a", "pending", 10), ps("b", "pending", 10), ps("c", "pending", 10)], NOW);
  assert.equal(retry.percent, 0);
});

// -----------------------------------------------------------------------------
// the status line and the step count
// -----------------------------------------------------------------------------

test("the heaviest running step speaks for a wave", () => {
  const progress = computeRunProgress(
    [
      ps("toolK3d", "running", 15, { label: "Installing tools" }),
      ps("stackCheckout", "running", 30, { label: "Downloading MemQL" }),
      ps("hostsBlock", "running", 5, { label: "Adding local addresses" }),
    ],
    NOW,
  );
  assert.equal(progress.status, "Downloading MemQL");
});

test("equal weights go to graph order, so the line does not flicker", () => {
  const progress = computeRunProgress(
    [ps("a", "running", 15, { label: "First" }), ps("b", "running", 15, { label: "Second" })],
    NOW,
  );
  assert.equal(progress.status, "First");
});

test("between waves the line names what comes next", () => {
  const progress = computeRunProgress(
    [ps("a", "ok", 5), ps("b", "pending", 5, { label: "Checking Docker" }), ps("c", "pending", 5)],
    NOW,
  );
  assert.equal(progress.status, "Checking Docker");
  assert.equal(progress.stepText, "Step 2 of 3");
});

test("the step count never runs past the end as the last wave settles", () => {
  const progress = computeRunProgress([ps("a", "ok", 1), ps("b", "ok", 1), ps("c", "ok", 1)], NOW);
  assert.equal(progress.stepText, "Step 3 of 3");
});

test("a step with no label is named by its id rather than left blank", () => {
  const progress = computeRunProgress([ps("seedBootstrap", "running", 10, { label: "" })], NOW);
  assert.equal(progress.status, "seedBootstrap");
});

// -----------------------------------------------------------------------------
// weights
// -----------------------------------------------------------------------------

test("the default weights are the design record's table, and an unknown step weighs 10", () => {
  assert.equal(defaultStepWeight("clusterUp"), 480);
  assert.equal(defaultStepWeight("buildImages"), 900);
  assert.equal(defaultStepWeight("detect"), 5);
  assert.equal(defaultStepWeight("removeToolMkcert"), 3);
  assert.equal(defaultStepWeight("somethingNew"), UNKNOWN_STEP_WEIGHT);
  assert.equal(UNKNOWN_STEP_WEIGHT, 10);
  for (const [id, weight] of Object.entries(DEFAULT_STEP_WEIGHTS)) {
    assert.ok(weight > 0, `${id} has a positive weight`);
  }
});

test("a measured weight replaces the default, and a nonsense one does not", () => {
  assert.equal(stepWeight("clusterUp", { clusterUp: 212 }), 212);
  assert.equal(stepWeight("clusterUp", { clusterUp: 0 }), 480);
  assert.equal(stepWeight("clusterUp", { clusterUp: Number.NaN }), 480);
  assert.equal(stepWeight("clusterUp", {}), 480);
  assert.equal(stepWeight("clusterUp"), 480);
});

test("every step the shipped graphs declare has a default weight", async () => {
  // An unlisted step silently weighs 10 seconds, which is right for a check
  // and badly wrong for a build. A new step should arrive with its estimate.
  const fs = await import("node:fs/promises");
  const path = await import("node:path");
  const dir = path.resolve(__dirname, "..", "..", "..", "..", "scripts", "install", "graph");
  for (const name of (await fs.readdir(dir)).filter((n) => n.endsWith(".json"))) {
    const doc = JSON.parse(await fs.readFile(path.join(dir, name), "utf8")) as { steps: { id: string }[] };
    for (const step of doc.steps) {
      assert.ok(step.id in DEFAULT_STEP_WEIGHTS, `${name}: ${step.id} has no default weight`);
    }
  }
});

// -----------------------------------------------------------------------------
// the wizard's rows, projected
// -----------------------------------------------------------------------------

test("the wizard's rows project onto the model, label first", () => {
  const steps = progressStepsOf(
    [
      row("detect", "done", { label: "Checking this computer", startedAt: 1, finishedAt: 2 }),
      row("clusterUp", "running", { description: "Creating the cluster and starting it.", startedAt: 3 }),
      row("seedBootstrap", "pending"),
    ],
    { detect: 7 },
  );
  assert.deepEqual(steps, [
    { id: "detect", label: "Checking this computer", weight: 7, status: "ok", startedAt: 1, finishedAt: 2 },
    { id: "clusterUp", label: "Creating the cluster and starting it.", weight: 480, status: "running", startedAt: 3 },
    { id: "seedBootstrap", label: "seedBootstrap", weight: 90, status: "pending" },
  ]);
});

test("runProgressOf is computeRunProgress over the projected rows", () => {
  const progress = runProgressOf(
    [
      row("a", "done", { label: "A" }),
      row("b", "running", { label: "Creating the cluster", phase: { label: "Installing ArgoCD" } }),
    ],
    NOW,
    undefined,
    { a: 10, b: 10 },
  );
  assert.equal(progress.status, "Installing ArgoCD");
  assert.equal(progress.stepText, "Step 2 of 2");
  assert.equal(progress.percent, 50);
});

// -----------------------------------------------------------------------------
// the failed step, in the negative
// -----------------------------------------------------------------------------

test("a failed step reads in the negative, the label's verb in its base form", () => {
  assert.equal(failedLabel("Creating the cluster"), "Couldn't create the cluster");
  assert.equal(failedLabel("Checking this computer"), "Couldn't check this computer");
  assert.equal(failedLabel("Setting up browser trust"), "Couldn't set up browser trust");
  assert.equal(failedLabel("Installing tools"), "Couldn't install tools");
  assert.equal(failedLabel("Adding local addresses"), "Couldn't add local addresses");
  assert.equal(failedLabel("Downloading MemQL"), "Couldn't download MemQL");
  assert.equal(failedLabel("Building MemQL"), "Couldn't build MemQL");
  assert.equal(failedLabel("Rebuilding MemQL"), "Couldn't rebuild MemQL");
  assert.equal(failedLabel("Preparing sign-in"), "Couldn't prepare sign-in");
  assert.equal(failedLabel("Removing the cluster"), "Couldn't remove the cluster");
});

test("a label that opens with no known gerund keeps its words, and grammar", () => {
  // THE DEFECT this replaces: "<long description>. failed".
  assert.equal(failedLabel("Deploy"), "Deploy failed");
  assert.equal(failedLabel("  "), "Something failed");
  assert.equal(failedLabel("Checking"), "Couldn't check");
  // A gerund that merely BEGINS a word is not the gerund.
  assert.equal(failedLabel("Checkingly odd"), "Checkingly odd failed");
});

test("every label the shipped graphs declare reads correctly in the negative", async () => {
  // A new step whose label opens with a gerund this table does not carry
  // would fall through to "<label> failed"; this makes that a failing test
  // rather than a sentence nobody reads until an install breaks.
  const fs = await import("node:fs/promises");
  const path = await import("node:path");
  const dir = path.resolve(__dirname, "..", "..", "..", "..", "scripts", "install", "graph");
  let seen = 0;
  for (const name of (await fs.readdir(dir)).filter((n) => n.endsWith(".json"))) {
    const doc = JSON.parse(await fs.readFile(path.join(dir, name), "utf8")) as { steps: { id: string; label: string }[] };
    for (const step of doc.steps) {
      seen += 1;
      const first = step.label.split(" ")[0] ?? "";
      assert.ok(
        FAILED_LABEL_GERUNDS.some((gerund) => step.label === gerund || step.label.startsWith(`${gerund} `)),
        `${name}: ${step.id}'s label "${step.label}" opens with "${first}", which failedLabel does not know`,
      );
      const said = failedLabel(step.label);
      assert.match(said, /^Couldn't [a-z]/, `${name}: ${step.id} reads "${said}"`);
      // The gerund becomes a verb and "Couldn't" is added: one word more, nothing lost.
      assert.equal(said.split(" ").length, step.label.split(" ").length + 1, `${name}: ${step.id} reads "${said}"`);
    }
  }
  assert.ok(seen > 20, `only ${seen} labels read -- the graph documents were not found`);
});
