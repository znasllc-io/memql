// How much of a run is left, as a number an operator can be shown (memql#4454).
//
// WHY THIS IS TESTED AT ALL. The bar is the most confident claim this wizard
// makes -- "you are two-thirds of the way through a ten-minute operation" --
// and it is made to somebody who has no other way to check it. A progress
// display that is decorative is worse than none: it teaches an operator to
// distrust the one signal they have while a real install is running.
//
// The panel that draws it imports `vscode` and cannot be reached from this
// lane, which is why the arithmetic lives in state/installProgress.ts and the
// renderer only interpolates it. Same split as `toStepViews`, same reason.
//
// THE ONE THAT MATTERS MOST IS THE RETRY. `AddClusterState.retry()` puts failed
// steps back to `pending`, so a retried run genuinely has more left to do than
// the moment before it -- and a bar written to only ever advance would have to
// either freeze or lie about which of the two happened.

import test from "node:test";
import assert from "node:assert/strict";

import { runNarration, runProgress } from "../src/state/installProgress.js";
import type { StepProgress, StepState } from "../src/state/addCluster.js";

function step(id: string, state: StepState, description = ""): StepProgress {
  return {
    id,
    description,
    state,
    reason: "",
    exitCode: null,
    log: "",
    guided: false,
    remedy: "",
  };
}

test("the pre-seed moment has no total, and says so rather than claiming 0 of 0", () => {
  // `runStarted` has not landed, so nothing is known about the size of the run.
  // The caller renders an INDETERMINATE bar off `total === 0`; a 0% one would
  // be a claim about a run whose length nobody has been told yet.
  const progress = runProgress([]);
  assert.deepEqual(progress, {
    settled: 0,
    total: 0,
    percent: 0,
    currentDescriptions: [],
  });
  assert.equal(runNarration(progress).position, "", "no total, no position");
});

test("settled counts every state that will not change again", () => {
  // `skipped` and `preserved` are the two that get miscounted: a step the run
  // verified it did not need is a step that is DONE WITH, and leaving them out
  // would park a repair's bar at a fraction of its real progress -- on the run
  // where almost every step skips.
  const progress = runProgress([
    step("a", "done"),
    step("b", "skipped"),
    step("c", "preserved"),
    step("d", "failed"),
    step("e", "running"),
    step("f", "pending"),
  ]);
  assert.equal(progress.total, 6);
  assert.equal(progress.settled, 4);
  assert.equal(progress.percent, 67);
});

test("a wave reports every running step, in graph order", () => {
  // The executor runs independent branches under Promise.all, so "the current
  // step" is regularly three steps. Naming one of them would be naming whichever
  // the projection reached first, which is a scheduling accident.
  const progress = runProgress([
    step("a", "done", "Downloading the release"),
    step("k3d", "running", "Installing k3d"),
    step("kubectl", "running", "Installing kubectl"),
    step("mkcert", "running", "Installing mkcert"),
    step("z", "pending", "Starting the cluster"),
  ]);
  assert.deepEqual(progress.currentDescriptions, [
    "Installing k3d",
    "Installing kubectl",
    "Installing mkcert",
  ]);
  const narration = runNarration(progress);
  assert.equal(narration.message, "Installing k3d -- and 2 more in progress");
  assert.equal(narration.position, "step 2 of 5");
});

test("a single running step is narrated by its own sentence, with no count", () => {
  const progress = runProgress([
    step("a", "done"),
    step("b", "running", "Creating the cluster and starting MemQL's services in it."),
    step("c", "pending"),
  ]);
  assert.equal(
    runNarration(progress).message,
    "Creating the cluster and starting MemQL's services in it.",
  );
  assert.equal(runNarration(progress).position, "step 2 of 3");
});

test("a step with no description yet is narrated by its id, never blank", () => {
  // The description arrives with the step's first event. A blank narration line
  // would read as a bug in the wizard rather than as an event in flight.
  const progress = runProgress([step("seedBootstrap", "running")]);
  assert.equal(runNarration(progress).message, "seedBootstrap");
});

test("between waves nothing is running, and the line is empty rather than invented", () => {
  const progress = runProgress([step("a", "done"), step("b", "pending")]);
  assert.deepEqual(progress.currentDescriptions, []);
  assert.equal(runNarration(progress).message, "");
  // The position still reads, because the run still has a size and a place in it.
  assert.equal(runNarration(progress).position, "step 2 of 2");
});

test("A RETRY MOVES THE BAR BACKWARDS, and that is the correct answer", () => {
  // The one this function exists to get right. Retry resets failed steps to
  // pending -- the run really does have more left than it did a moment ago --
  // and a bar that refused to go back would have to freeze or lie.
  const failed = [step("a", "done"), step("b", "failed"), step("c", "pending")];
  const before = runProgress(failed);
  assert.equal(before.settled, 2);

  const retried = [step("a", "done"), step("b", "pending"), step("c", "pending")];
  const after = runProgress(retried);
  assert.equal(after.settled, 1);
  assert.ok(after.percent < before.percent, "the bar is allowed to move backwards");
});

test("the position never runs past the end as the last wave settles", () => {
  // `settled + 1` is the step being waited on; unclamped, the final settle
  // would report "step 4 of 3".
  const progress = runProgress([step("a", "done"), step("b", "done"), step("c", "done")]);
  assert.equal(runNarration(progress).position, "step 3 of 3");
  assert.equal(progress.percent, 100);
});

test("percent is a whole number across the awkward divisions", () => {
  // 13 steps is what install.json actually ships, and none of its fractions
  // land on a round number.
  const steps = Array.from({ length: 13 }, (_, i) => step(`s${i}`, i < 4 ? "done" : "pending"));
  assert.equal(runProgress(steps).percent, 31);
  assert.equal(Number.isInteger(runProgress(steps).percent), true);
});

test("an embedded sentence loses its full stop, because it is no longer the whole line", () => {
  // FOUND BY RENDERING, not by reading. The descriptions are sentences and end
  // in a stop, so the naive join produced "...answer over https. -- and 1 more
  // in progress" on every concurrent wave -- which is exactly the sort of small
  // wrongness that makes a shipping product look unfinished.
  const progress = runProgress([
    step("a", "running", "Issuing the certificate that lets this cluster answer over https."),
    step("b", "running", "Setting up the tools your browsers need."),
  ]);
  assert.equal(
    runNarration(progress).message,
    "Issuing the certificate that lets this cluster answer over https -- and 1 more in progress",
  );
});

test("a single running step KEEPS its full stop, because it is the whole line", () => {
  const progress = runProgress([step("a", "running", "Downloading the MemQL release you chose.")]);
  assert.equal(runNarration(progress).message, "Downloading the MemQL release you chose.");
});
