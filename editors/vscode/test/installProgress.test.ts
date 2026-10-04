// The run's failure vocabulary (memql#3474).
//
// This is where what an operator is SHOWN during a run gets asserted. The panel
// that draws it imports `vscode` and so cannot be reached from this lane, which
// is why the judgement lives in state/installProgress.ts and the panel only
// calls it -- the same split that keeps state/topology.ts testable.
//
// The ones that carry weight:
//
//   - an unrecognised exit code is reported AS unrecognised rather than mapped
//     to the nearest known one, because confident wrong advice arrives exactly
//     when the operator is relying on it;
//   - every code the installer can produce is claimed.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { SYNTHESISED_EXIT_CODES } from "../src/install/runner.js";
import {
  failureGuidance,
  refusedPlatformGuidance,
} from "../src/state/installProgress.js";

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

// ---------------------------------------------------------------------------
// the failure taxonomy
// ---------------------------------------------------------------------------

test("each reachable exit code gets its own explanation", () => {
  // Six, not four. 2/3/4/5 are the contract's classifications; 0 and 1 reach a
  // failed outcome without being one, and both are real.
  const codes = [0, 1, 2, 3, 4, 5];
  // 1 and 5 may share a headline ("The step failed.") -- what differs, and
  // what the person acts on, is the ADVICE: a catch-all points at the log, an
  // operation failure says a retry often helps.
  const said = codes.map((c) => `${failureGuidance(c).headline} ${failureGuidance(c).advice}`);
  assert.equal(new Set(said).size, codes.length, `codes share wording: ${said.join(" | ")}`);
  assert.match(failureGuidance(4).headline, /missing/i);
  assert.match(failureGuidance(3).headline, /protect/i);
});

// A step that needs root classifies as exit 4 -- correctly, since nothing is
// half-done -- but the generic exit-4 advice sends the operator hunting for a
// package to install when the wizard is already holding the exact command and
// what the step actually wants is a password (memql#3560).
test("exit 4 carrying a remedy is explained as elevation, not as a missing package", () => {
  const bare = failureGuidance(4);
  const withRemedy = failureGuidance(4, "sudo /path/to/hosts-entries.sh --action=add");

  assert.notEqual(withRemedy.headline, bare.headline);
  assert.match(withRemedy.headline, /administrator access/);
  // The operator has to be told to go and RUN the thing, not to go and find one.
  assert.equal(withRemedy.advice, "Run the command in a terminal, then retry.");
  assert.doesNotMatch(withRemedy.advice, /install the missing/i);
  // THE CONTRADICTION (memql#5118 audit): the advice used to say the
  // installer "cannot ask for your password" -- on a page that had just
  // asked for it.
  assert.doesNotMatch(`${withRemedy.headline} ${withRemedy.advice}`, /cannot ask for your password|background process/);
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
  // MemQL "cannot say what it means" about the case it understands best.
  const g = failureGuidance(0);
  assert.match(g.headline, /check didn't pass/);
  assert.ok(!/doesn't know/i.test(g.advice), "exit 0 still reaching the unknown-code branch");
  assert.equal(g.retryable, true);
});

test("exit 1 is explained as the catch-all, not as unknown", () => {
  // `cap_fail` clamps any out-of-range code to 1 and capability.sh's EXIT trap
  // emits a failure envelope for any non-zero abort, so 1 is where an
  // unclassified failure and a `set -e` death both land.
  const g = failureGuidance(1);
  assert.ok(!/doesn't know/i.test(g.advice), "exit 1 still reaching the unknown-code branch");
  assert.equal(g.retryable, true);
});

test("a bad parameter is named as MemQL's fault, not the operator's", () => {
  // Exit 2 means the installer passed something wrong. Telling the operator to
  // check their answers would send them to fix something they did not break.
  const g = failureGuidance(2);
  assert.match(g.advice, /fault in MemQL, not your computer/);
  assert.equal(g.retryable, false);
});

test("exit 3 with an unsupported-platform sentence is refused-platform, not artifact protection", () => {
  // Generic exit 3 stays "The step refused to act" -- hosts-block protecting a
  // pre-existing artifact, a bad key. Detect's refuse is a different next
  // action: the wizard cannot run here, and Repair / another tag will not help
  // (memql#4294).
  const generic = failureGuidance(3);
  assert.match(generic.headline, /protect something already on this computer/);
  assert.match(generic.advice, /log says what it kept/);
  assert.equal(generic.retryable, false);

  const detail =
    "unsupported platform darwin/amd64: the local cluster installer targets linux/amd64, darwin/arm64";
  const platform = failureGuidance(3, "", detail);
  assert.notEqual(platform.headline, generic.headline);
  assert.match(platform.headline, /can't run a local MemQL cluster/);
  assert.match(platform.advice, /Retrying won't change that/);
  assert.doesNotMatch(platform.advice, /picking another tag will help/i);
  assert.equal(platform.retryable, false);
  assert.deepEqual(platform, refusedPlatformGuidance(detail));

  // THE SUPPORTED SET IS QUOTED FROM DETECT, NOT RESTATED HERE. The advice
  // used to hardcode "targets linux/amd64 only. On macOS use make up", which
  // survived darwin/arm64 becoming supported and then told Mac operators the
  // installer could not run on their machine at all. Both halves are pinned:
  // detect's own sentence reaches the operator, and no macOS disclaimer is
  // reintroduced.
  assert.match(platform.advice, /linux\/amd64, darwin\/arm64/);
  assert.doesNotMatch(platform.advice, /make up/i);
  assert.doesNotMatch(refusedPlatformGuidance("").advice, /linux\/amd64/);
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
  assert.match(g.advice, /doesn't know what that code means/);
  assert.doesNotMatch(g.advice, /capability|contract/);
});

test("no exit code at all reads as stopped rather than failed", () => {
  const g = failureGuidance(null);
  assert.match(g.headline, /didn't finish/);
  assert.equal(g.retryable, true);
});

// ---------------------------------------------------------------------------
// the codes MemQL synthesises for itself (memql#3474 review)
// ---------------------------------------------------------------------------

test("a timed-out step is explained, not disclaimed", () => {
  // 124 is OURS: runner.ts kills a step that outruns its timeout and reports
  // 124/SIGKILL. Letting it fall to the default branch had MemQL say it
  // "cannot say what it means" about a code MemQL assigned itself -- to an
  // operator whose install just stopped dead after ten minutes.
  const g = failureGuidance(124);
  assert.match(g.headline, /timed out after 10 minutes/, "the default ceiling, when a step names none");
  assert.ok(!/doesn't know/i.test(g.advice), "124 still reaching the unknown-code branch");
  assert.equal(g.retryable, true);
});

test("a timeout names the step's OWN ceiling, not a figure written in the guidance", () => {
  // THE DEFECT (memql#5118 audit): the advice said "the ten minutes any one
  // step is allowed" for the cluster step (thirty minutes) and the image build
  // (forty-five).
  assert.match(failureGuidance(124, "", "", { timeoutSeconds: 1800 }).headline, /30 minutes/);
  assert.match(failureGuidance(124, "", "", { timeoutSeconds: 2700 }).headline, /45 minutes/);
  assert.doesNotMatch(failureGuidance(124, "", "", { timeoutSeconds: 1800 }).headline, /ten|10 minutes/);
});

test("a step that could not be started is named as an installer fault", () => {
  // 127 is also ours: runner.ts reports it when the script cannot be launched.
  // That is a broken package, not a broken machine, and retrying cannot help.
  const g = failureGuidance(127);
  assert.ok(!/doesn't know/i.test(g.advice), "127 still reaching the unknown-code branch");
  assert.match(g.advice, /Reinstall the MemQL extension/);
  assert.equal(g.retryable, false);
});

test("every code the installer can produce is claimed -- derived, not listed", () => {
  // THE SET IS DERIVED. Both halves of this used to be a hand-written array,
  // which asserts that the array is complete rather than that the function is:
  // a code added to the contract, or a fourth one synthesised by runner.ts,
  // would keep the test green while an operator read "MemQL cannot say what it
  // means" about a number MemQL had chosen.
  //
  // Two sources, because there are two: the capability contract's own table
  // (parsed from the document that defines it) and runner.ts's own constants.
  const reachable = new Set<number>([...contractExitCodes(), ...Object.values(SYNTHESISED_EXIT_CODES)]);
  assert.ok(reachable.has(2) && reachable.has(5), "the contract table did not parse");
  assert.ok(reachable.size >= 8, `only ${reachable.size} codes derived -- the sources did not both parse`);

  for (const code of reachable) {
    assert.ok(
      !/doesn't know/i.test(failureGuidance(code).advice),
      `exit ${code} is reachable but unexplained`,
    );
  }

  // And the branch still exists for a code that genuinely is not ours: guessing
  // would put confident wrong advice in front of an operator relying on it.
  assert.ok(!reachable.has(99));
  assert.match(failureGuidance(99).advice, /doesn't know/i);
});

test("no guidance says what a reader cannot act on", () => {
  // The words that used to reach the page: the contract, the executor's
  // verify, a retired portal, a double hyphen for a dash.
  for (const code of [null, 0, 1, 2, 3, 4, 5, 124, 127, 128, 99]) {
    for (const remedy of ["", "sudo x"]) {
      const g = failureGuidance(code, remedy);
      assert.doesNotMatch(`${g.headline} ${g.advice}`, /capability|contract|verify|envelope|exit code|portal|--/);
    }
  }
});
