// The upgrade-barrier table (memql#3999).
//
// Two kinds of assertion live here and they guard different things.
//
// The BEHAVIOURAL half pins the crossing rule, including the two edges that are
// easy to get backwards: a move landing exactly ON the barrier's afterVersion
// crosses nothing, and a downgrade across it crosses the same barrier a
// upgrade does.
//
// The DATA half pins the table itself. It is hand-maintained, which is the
// right call for the reasons barriers.ts gives -- and the cost of that call is
// that an entry can be wrong in ways no type checks: a version nothing parses,
// a duplicate, an order that is not an order, a runbook nobody wrote, or an
// entry describing a release the installer's reviewed pin cannot even reach.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { UPGRADE_BARRIERS, barriersCrossed } from "../src/version/barriers.js";
import { compareVersions, isReleaseShaped } from "../src/version/compare.js";
import { DEFAULT_STACK_TAG } from "../src/install/stackPin.js";

// repoRoot walks up from this file until it finds the workspace marker. Walking
// beats a fixed "../../.." because the test runs from dist-test/ (esbuild
// output), not from test/. Same helper, same reason, as
// discoveryEndpointContract.test.ts.
function repoRoot(): string {
  let dir = __dirname;
  for (;;) {
    if (fs.existsSync(path.join(dir, "go.work"))) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) {
      throw new Error(`could not locate the repository root (no go.work above ${__dirname})`);
    }
    dir = parent;
  }
}

// --- the data ---------------------------------------------------------------

test("the newest barrier is not newer than the reviewed stack pin", () => {
  // THE POINT. DEFAULT_STACK_TAG is the release the installer actually puts on
  // a machine, and it moves only when someone reviews the move. A barrier entry
  // newer than it describes a crossing no operator can make yet -- which means
  // it was written from a plan rather than from a release, and nothing would
  // have caught that: the refusal it produces simply never fires, so the entry
  // looks correct for as long as it is wrong.
  //
  // Stated as "not ahead" rather than "behind", because the pin moving TO the
  // release that shipped a barrier is the normal, correct state.
  for (const barrier of UPGRADE_BARRIERS) {
    assert.notEqual(
      compareVersions(barrier.afterVersion, DEFAULT_STACK_TAG),
      "ahead",
      `barrier after ${barrier.afterVersion} is newer than DEFAULT_STACK_TAG (${DEFAULT_STACK_TAG}); ` +
        `add the entry in the release that ships it, not before`,
    );
  }
});

test("every afterVersion places on the release line", () => {
  // An entry whose version does not parse can never match anything: both sides
  // of the crossing test would be `undefined` and the barrier would silently
  // never fire. That is the same shape of failure as the pin check above --
  // an entry that is present, looks deliberate, and does nothing.
  for (const barrier of UPGRADE_BARRIERS) {
    assert.ok(
      isReleaseShaped(barrier.afterVersion),
      `barrier afterVersion ${JSON.stringify(barrier.afterVersion)} is not a release tag`,
    );
  }
});

test("the table is ordered oldest first, with no repeats", () => {
  // Order is part of the data, not a tidiness preference: a move crossing two
  // barriers hands the operator a sequence to perform, and a table in the wrong
  // order hands them the steps backwards.
  for (let i = 1; i < UPGRADE_BARRIERS.length; i++) {
    const previous = UPGRADE_BARRIERS[i - 1] as (typeof UPGRADE_BARRIERS)[number];
    const current = UPGRADE_BARRIERS[i] as (typeof UPGRADE_BARRIERS)[number];
    assert.equal(
      compareVersions(previous.afterVersion, current.afterVersion),
      "behind",
      `barriers are out of order or duplicated: ${previous.afterVersion} then ${current.afterVersion}`,
    );
  }
});

test("every runbook a barrier points at exists", () => {
  // A refusal that links to a document nobody wrote is worse than no refusal:
  // it reads as though someone thought the crossing through. This is why
  // docHref is a repo-relative path rather than a URL -- a URL cannot be
  // checked here, and this table's whole value is being checkable.
  const root = repoRoot();
  for (const barrier of UPGRADE_BARRIERS) {
    const file = path.join(root, barrier.docHref);
    assert.ok(
      fs.existsSync(file),
      `barrier after ${barrier.afterVersion} points at ${barrier.docHref}, which does not exist`,
    );
  }
});

test("every barrier says what the move changes", () => {
  for (const barrier of UPGRADE_BARRIERS) {
    assert.ok(
      barrier.summary.trim().length > 0,
      `barrier after ${barrier.afterVersion} has no summary; the refusal has nothing to say`,
    );
  }
});

test("no barrier summary names local-only machinery", () => {
  // A crossing is REFUSED ON THE REMOTE PATH TOO (memql#3997), so a staging
  // operator reads these sentences. The first draft of the v0.18.0 entry said
  // "the local cluster's database" and "moving the checkout does not carry the
  // data across" -- stackCheckout/clusterUp concepts that mean nothing to a
  // deploy-control move, and which sent a staging operator to a runbook whose
  // remedy is `make down PURGE=1`.
  //
  // A barrier is a fact about what a RELEASE changed. The machinery that would
  // have crossed it belongs to the caller and the runbook, not to the table.
  // The list is LOCAL-ONLY machinery, not "words that sound local". `overlay`
  // is deliberately absent: staging and production are overlays too, so a
  // remote operator reads that word correctly. `checkout`, `k3d`, `make up`
  // and "this machine" are the ones that mean nothing to them.
  const localOnly = /\bcheckout\b|\blocal cluster\b|\bmake (up|down)\b|\bk3d\b|\bthis machine\b/i;
  for (const barrier of UPGRADE_BARRIERS) {
    assert.doesNotMatch(
      barrier.summary,
      localOnly,
      `barrier after ${barrier.afterVersion} describes local-only machinery; ` +
        `the remote path is refused with this same sentence`,
    );
  }
});

// --- the crossing rule ------------------------------------------------------

test("a move that stays on one side of a barrier is clear", () => {
  assert.deepEqual(barriersCrossed("v0.17.0", "v0.17.1"), { kind: "clear" });
  assert.deepEqual(barriersCrossed("v0.18.0", "v0.18.0"), { kind: "clear" });
});

test("landing exactly on afterVersion crosses nothing", () => {
  // The interval is half-open at the right end, which is what `afterVersion`
  // means: the barrier sits in the gap AFTER that release, so arriving at it is
  // an ordinary retag. Getting this off by one turns every v0.17.x -> v0.18.0
  // upgrade into a refusal.
  assert.deepEqual(barriersCrossed("v0.16.1", "v0.18.0"), { kind: "clear" });
});

test("a move past a barrier is blocked, forwards", () => {
  const check = barriersCrossed("v0.18.0", "v0.19.0");
  assert.equal(check.kind, "blocked");
  if (check.kind !== "blocked") return;
  assert.equal(check.direction, "forward");
  assert.equal(check.barriers.length, 1);
  assert.equal(check.barriers[0]?.afterVersion, "v0.18.0");
});

test("a downgrade across a barrier is blocked too", () => {
  // The button is deliberately "Move this cluster to another release tag" and
  // its tag list is not filtered to newer tags (deploy/instanceActions.ts), so
  // a forward-only check would be blind to half of what it can do -- and this
  // is the more dangerous half: it puts a plain Postgres Deployment in front of
  // data that lives in a CloudNativePG cluster.
  const check = barriersCrossed("v0.19.0", "v0.18.0");
  assert.equal(check.kind, "blocked");
  if (check.kind !== "blocked") return;
  assert.equal(check.direction, "backward");
  assert.equal(check.barriers[0]?.afterVersion, "v0.18.0");
});

test("the v prefix does not decide anything", () => {
  // The two sides come from sources that disagree about the prefix: an install
  // receipt records one form, a deploy-control status another.
  assert.equal(barriersCrossed("0.18.0", "0.19.0").kind, "blocked");
  assert.equal(barriersCrossed("v0.18.0", "0.19.0").kind, "blocked");
  assert.equal(barriersCrossed("0.17.0", "v0.18.0").kind, "clear");
});

test("an unreadable version is undetermined and never clear", () => {
  // The fleet these barriers protect is largely the fleet whose version does
  // not place. Every engine shipped before memql#3998 reports the build stamp,
  // which compare.ts refuses to read as a release precisely so it cannot be
  // mistaken for one, and a cluster installed before memql#3990 records nothing
  // at all. If either quietly answered "clear", the table would be inert for
  // exactly the clusters it exists to protect while appearing to work.
  for (const unreadable of [undefined, "", "0.15.0-1737072000", "feat/some-branch", "9fd53842"]) {
    const check = barriersCrossed(unreadable, "v0.19.0");
    assert.equal(
      check.kind,
      "undetermined",
      `${JSON.stringify(unreadable)} as the recorded version answered ${check.kind}`,
    );
    if (check.kind !== "undetermined") continue;
    assert.equal(check.unreadable, "from");
    // The values travel with the verdict so the refusal can quote what it saw
    // rather than saying "unknown" about a string the operator can read.
    assert.equal(check.from, unreadable);
    assert.equal(check.to, "v0.19.0");
  }
});

test("undetermined names which side could not be read", () => {
  assert.equal(barriersCrossed("v0.18.0", "not-a-tag").kind, "undetermined");

  const target = barriersCrossed("v0.18.0", "not-a-tag");
  if (target.kind === "undetermined") assert.equal(target.unreadable, "to");

  const both = barriersCrossed(undefined, "not-a-tag");
  if (both.kind === "undetermined") assert.equal(both.unreadable, "both");
});
