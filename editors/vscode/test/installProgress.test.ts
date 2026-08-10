// The run's projection and its failure vocabulary (memql#3474).
//
// This is where what an operator is SHOWN during a run gets asserted. The panel
// that draws it imports `vscode` and so cannot be reached from this lane, which
// is why the judgement lives in state/installProgress.ts and the panel only
// calls it -- the same split that keeps state/topology.ts testable.
//
// The ones that carry weight:
//
//   - a stale exit code never rides on a step that did not fail;
//   - an unrecognised exit code is reported AS unrecognised rather than mapped
//     to the nearest known one, because confident wrong advice arrives exactly
//     when the operator is relying on it;
//   - order is the graph's, not this function's.

import test from "node:test";
import assert from "node:assert/strict";

import { failureGuidance, runIsSettled, toStepViews } from "../src/state/installProgress.js";
import type { StepProgress } from "../src/state/addCluster.js";

function step(over: Partial<StepProgress> = {}): StepProgress {
  return {
    id: "toolK3d",
    description: "Place the pinned k3d binary",
    state: "pending",
    reason: "",
    exitCode: null,
    log: "",
    guided: false,
    ...over,
  };
}

// ---------------------------------------------------------------------------
// projection
// ---------------------------------------------------------------------------

test("a step's description is its label, and the id stands in until one arrives", () => {
  // A blank row reads as a bug in the wizard rather than as a step whose first
  // event has not landed.
  assert.equal(toStepViews([step()])[0]?.label, "Place the pinned k3d binary");
  assert.equal(toStepViews([step({ description: "" })])[0]?.label, "toolK3d");
});

test("every state survives the projection unchanged", () => {
  // The projection must not quietly collapse states -- that is the failure the
  // whole two-type renderer exists to prevent.
  const states: StepProgress["state"][] = [
    "pending",
    "running",
    "done",
    "skipped",
    "preserved",
    "failed",
  ];
  const views = toStepViews(states.map((state, i) => step({ id: `s${i}`, state })));
  assert.deepEqual(
    views.map((v) => v.state),
    states,
  );
});

test("order is the caller's -- the graph's wave order, not a re-sort", () => {
  const views = toStepViews([
    step({ id: "clusterUp" }),
    step({ id: "toolK3d" }),
    step({ id: "seedBootstrap" }),
  ]);
  assert.deepEqual(
    views.map((v) => v.id),
    ["clusterUp", "toolK3d", "seedBootstrap"],
  );
});

test("an exit code rides only on a failure", () => {
  // The load-bearing one. AddClusterState.retry() clears the code, but a step
  // that succeeded while still carrying one would render "exit 5" beside a tick
  // -- alarming, and wrong.
  assert.equal(toStepViews([step({ state: "failed", exitCode: 4 })])[0]?.exitCode, 4);
  assert.equal(toStepViews([step({ state: "done", exitCode: 5 })])[0]?.exitCode, undefined);
  assert.equal(toStepViews([step({ state: "skipped", exitCode: 3 })])[0]?.exitCode, undefined);
});

test("the log is surfaced where something went wrong, and not on every green step", () => {
  // Attaching output to all twelve steps buries the one failure in a dozen
  // disclosures that all say "fine".
  assert.equal(toStepViews([step({ state: "failed", log: "boom" })])[0]?.error, "boom");
  assert.equal(toStepViews([step({ state: "preserved", log: "kept" })])[0]?.error, "kept");
  assert.equal(toStepViews([step({ state: "done", log: "chatter" })])[0]?.error, undefined);
});

test("a guided step says so, alongside any reason", () => {
  // "You run this one" changes what the operator is looking at more than the
  // reason does, so it leads.
  const view = toStepViews([step({ guided: true, reason: "needs sudo" })])[0];
  assert.match(view?.detail ?? "", /^guided/);
  assert.match(view?.detail ?? "", /needs sudo/);
});

test("a step with nothing to add carries no detail at all", () => {
  assert.equal(toStepViews([step()])[0]?.detail, undefined);
});

// ---------------------------------------------------------------------------
// the failure taxonomy
// ---------------------------------------------------------------------------

test("each contract exit code gets its own explanation", () => {
  // 2 bad param, 3 refused, 4 prerequisite missing, 5 operation failed. Four
  // codes, four different next actions.
  const headlines = [2, 3, 4, 5].map((c) => failureGuidance(c).headline);
  assert.equal(new Set(headlines).size, 4, `codes share wording: ${headlines.join(" | ")}`);
  assert.match(failureGuidance(4).advice, /prerequisite|missing/i);
  assert.match(failureGuidance(3).headline, /refus/i);
});

test("a bad parameter is named as memQL's fault, not the operator's", () => {
  // Exit 2 means the installer passed something wrong. Telling the operator to
  // check their answers would send them to fix something they did not break.
  const g = failureGuidance(2);
  assert.match(g.advice, /fault in memQL/i);
  assert.equal(g.retryable, false);
});

test("retryable distinguishes 'could differ' from 'will fail identically'", () => {
  // Not whether the button appears -- #3474 offers Retry on every failure,
  // since the operator may have fixed the cause in another window. This is
  // whether an UNCHANGED retry has any prospect of a different answer.
  assert.equal(failureGuidance(2).retryable, false);
  assert.equal(failureGuidance(3).retryable, false);
  assert.equal(failureGuidance(4).retryable, true);
  assert.equal(failureGuidance(5).retryable, true);
});

test("an unrecognised exit code is reported as unrecognised", () => {
  // Mapping it to the nearest known code would put confident wrong advice in
  // front of an operator at the moment they are relying on it.
  const g = failureGuidance(99);
  assert.match(g.headline, /99/);
  assert.match(g.advice, /not one of the codes|cannot say/i);
});

test("no exit code at all reads as stopped rather than failed", () => {
  const g = failureGuidance(null);
  assert.match(g.headline, /did not run to completion/i);
  assert.equal(g.retryable, true);
});

// ---------------------------------------------------------------------------
// settledness
// ---------------------------------------------------------------------------

test("a run is settled only when nothing is pending or running", () => {
  // Cancel is offered off this. Offering it on a finished run promises to stop
  // something there is nothing left to stop.
  assert.equal(runIsSettled([step({ state: "done" }), step({ state: "skipped" })]), true);
  assert.equal(runIsSettled([step({ state: "done" }), step({ state: "pending" })]), false);
  assert.equal(runIsSettled([step({ state: "running" })]), false);
  assert.equal(runIsSettled([step({ state: "failed" })]), true);
});

test("a run with no steps yet is NOT settled", () => {
  // Nothing reported is the START of a run, not the end of one -- and that is
  // exactly when an operator is most likely to want out. Reading "nothing
  // pending" off an empty list would withdraw Cancel for the whole opening
  // stretch of the longest operation the wizard performs.
  assert.equal(runIsSettled([]), false);
});
