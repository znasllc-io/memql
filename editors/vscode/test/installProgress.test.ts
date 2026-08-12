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
import * as fs from "node:fs";
import * as path from "node:path";

import { SYNTHESISED_EXIT_CODES } from "../src/install/runner.js";
import { failureGuidance, runIsSettled, toStepViews } from "../src/state/installProgress.js";
import type { StepProgress } from "../src/state/addCluster.js";

// dist-test/test -> dist-test -> editors/vscode -> editors -> the repository.
const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

/**
 * The exit codes the capability-script contract defines, READ OFF THE CONTRACT.
 *
 * Parsed from the document rather than copied out of it, so a code added to the
 * standard table without a case in `failureGuidance` fails here. A list written
 * out beside the function asserts that the list is complete, which is not the
 * thing anyone wants to know.
 */
function contractExitCodes(): number[] {
  const doc = fs.readFileSync(
    path.join(REPO_ROOT, "docs", "internal", "design", "capability-script-contract.md"),
    "utf8",
  );
  const table = doc.slice(doc.indexOf("## Standard exit codes"));
  const codes: number[] = [];
  for (const line of table.split("\n")) {
    // The table's rows, and only those: `| 4    | precondition failed ... |`.
    const match = /^\|\s*(\d+)\s*\|/.exec(line);
    if (match) codes.push(Number(match[1]));
    else if (codes.length > 0 && line.startsWith("#")) break;
  }
  return codes;
}

function step(over: Partial<StepProgress> = {}): StepProgress {
  return {
    id: "toolK3d",
    description: "Place the pinned k3d binary",
    state: "pending",
    reason: "",
    exitCode: null,
    remedy: "",
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

test("each reachable exit code gets its own explanation", () => {
  // Six, not four. 2/3/4/5 are the contract's classifications; 0 and 1 reach a
  // failed outcome without being one, and both are real.
  const codes = [0, 1, 2, 3, 4, 5];
  const headlines = codes.map((c) => failureGuidance(c).headline);
  assert.equal(new Set(headlines).size, codes.length, `codes share wording: ${headlines.join(" | ")}`);
  assert.match(failureGuidance(4).advice, /prerequisite|missing/i);
  assert.match(failureGuidance(3).headline, /refus/i);
});

// A step that needs root classifies as exit 4 -- correctly, since nothing is
// half-done -- but the generic exit-4 advice sends the operator hunting for a
// package to install when the wizard is already holding the exact command and
// what the step actually wants is a password (memql#3560).
test("exit 4 carrying a remedy is explained as elevation, not as a missing package", () => {
  const bare = failureGuidance(4);
  const withRemedy = failureGuidance(4, "sudo /path/to/hosts-entries.sh --action=add");

  assert.notEqual(withRemedy.headline, bare.headline);
  assert.match(withRemedy.headline, /privilege|password|root|administrator/i);
  // The operator has to be told to go and RUN the thing, not to go and find one.
  assert.match(withRemedy.advice, /terminal/i);
  assert.doesNotMatch(withRemedy.advice, /install the missing prerequisite/i);
  assert.equal(withRemedy.retryable, true);
});

test("an empty remedy leaves exit 4 alone", () => {
  // No remedy means there is nothing to open a terminal for, and the ordinary
  // prerequisite advice is the right advice.
  assert.deepEqual(failureGuidance(4, ""), failureGuidance(4));
});

test("exit 0 is explained, because it is how most real failures arrive", () => {
  // THE ONE THIS FUNCTION GOT WRONG. executor.ts records a FAILED outcome
  // carrying exit 0 whenever a script exits cleanly and its verify does not
  // hold -- "an exit code of 0 is a precondition and nothing more". All 13
  // install steps verify a result field, so this is the common path, not an
  // edge case. It previously fell to the default branch and told the operator
  // memQL "cannot say what it means" about the case it understands best.
  const g = failureGuidance(0);
  assert.match(g.headline, /ran without error|not in the state/i);
  assert.ok(!/cannot say/i.test(g.advice), "exit 0 still reaching the unknown-code branch");
  assert.equal(g.retryable, true);
});

test("exit 1 is explained as the catch-all, not as unknown", () => {
  // `cap_fail` clamps any out-of-range code to 1 and capability.sh's EXIT trap
  // emits a failure envelope for any non-zero abort, so 1 is where an
  // unclassified failure and a `set -e` death both land.
  const g = failureGuidance(1);
  assert.ok(!/cannot say/i.test(g.advice), "exit 1 still reaching the unknown-code branch");
  assert.equal(g.retryable, true);
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

// ---------------------------------------------------------------------------
// the codes memQL synthesises for itself (memql#3474 review)
// ---------------------------------------------------------------------------

test("a timed-out step is explained, not disclaimed", () => {
  // 124 is OURS: runner.ts kills a step that outruns its timeout and reports
  // 124/SIGKILL. Letting it fall to the default branch had memQL say it
  // "cannot say what it means" about a code memQL assigned itself -- to an
  // operator whose install just stopped dead after ten minutes.
  const g = failureGuidance(124);
  assert.match(g.headline, /ran out of time|stopped/i);
  assert.ok(!/cannot say/i.test(g.advice), "124 still reaching the unknown-code branch");
  assert.equal(g.retryable, true);
});

test("a step that could not be started is named as an installer fault", () => {
  // 127 is also ours: runner.ts reports it when the script cannot be launched.
  // That is a broken package, not a broken machine, and retrying cannot help.
  const g = failureGuidance(127);
  assert.ok(!/cannot say/i.test(g.advice), "127 still reaching the unknown-code branch");
  assert.match(g.advice, /fault in this memQL build|memQL build/i);
  assert.equal(g.retryable, false);
});

test("every code the installer can produce is claimed -- derived, not listed", () => {
  // THE SET IS DERIVED. Both halves of this used to be a hand-written array,
  // which asserts that the array is complete rather than that the function is:
  // a code added to the contract, or a fourth one synthesised by runner.ts,
  // would keep the test green while an operator read "memQL cannot say what it
  // means" about a number memQL had chosen.
  //
  // Two sources, because there are two: the capability contract's own table
  // (parsed from the document that defines it) and runner.ts's own constants.
  const reachable = new Set<number>([...contractExitCodes(), ...Object.values(SYNTHESISED_EXIT_CODES)]);
  assert.ok(reachable.has(2) && reachable.has(5), "the contract table did not parse");
  assert.ok(reachable.size >= 8, `only ${reachable.size} codes derived -- the sources did not both parse`);

  for (const code of reachable) {
    assert.ok(
      !/cannot say/i.test(failureGuidance(code).advice),
      `exit ${code} is reachable but unexplained`,
    );
  }

  // And the branch still exists for a code that genuinely is not ours: guessing
  // would put confident wrong advice in front of an operator relying on it.
  assert.ok(!reachable.has(99));
  assert.match(failureGuidance(99).advice, /cannot say/i);
});
