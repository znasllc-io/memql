// What the recorded checkout is RIGHT NOW (memql#4246).
//
// The receipt says what was cloned; this says what is there today, and the
// difference is the whole reason a rebuild preflight exists. A developer who
// checked out another branch, or who has four uncommitted files, is about to
// build images from THAT -- and the checklist has to name it before the build
// starts rather than after the cluster is running it.
//
// The parse is tested and the spawn is not: `parseCheckoutState` is where every
// decision lives (which ref wins, what counts as dirty, whether deploy/ is
// touched), and the git invocation around it is the same shape tags.ts already
// carries.

import test from "node:test";
import assert from "node:assert/strict";

import { parseCheckoutState, readCheckoutState } from "../src/install/checkoutState.js";

test("a tag checkout is named as one, with its dirtiness counted", () => {
  const s = parseCheckoutState({
    head: "abc1234def\n",
    tag: "v0.17.0\n",
    branch: "",
    status: " M dsl/cognition/queries.memql\n?? deploy/x.yaml\n",
  });
  assert.deepEqual(s, {
    commit: "abc1234def",
    ref: { kind: "tag", name: "v0.17.0" },
    dirtyCount: 2,
    deployDirty: true,
  });
});

test("a branch beats detached; detached is named when neither resolves", () => {
  assert.deepEqual(parseCheckoutState({ head: "a", tag: "", branch: "main\n", status: "" }).ref, {
    kind: "branch",
    name: "main",
  });
  assert.deepEqual(parseCheckoutState({ head: "a", tag: "", branch: "", status: "" }).ref, {
    kind: "detached",
    name: "",
  });
  assert.equal(
    parseCheckoutState({ head: "a", tag: "", branch: "", status: "" }).deployDirty,
    false,
  );
});

test("dirtiness is counted from the porcelain lines, not from their text", () => {
  // A blank trailing line is what `git status --porcelain` always ends with, and
  // counting it would report every clean checkout as carrying one edit.
  assert.equal(parseCheckoutState({ head: "a", tag: "", branch: "m", status: "\n" }).dirtyCount, 0);
  // A path that merely CONTAINS deploy/ is not deploy/ -- the status column is
  // two characters wide plus a space, and the path starts after it.
  const nested = parseCheckoutState({
    head: "a",
    tag: "",
    branch: "m",
    status: " M editors/vscode/deploy/x.ts\n",
  });
  assert.equal(nested.deployDirty, false);
});

test("git that cannot answer is undefined, never a half-read state", () => {
  // The preflight renders "git could not read the checkout" on this, which is a
  // different sentence from "clean at 0000000" -- and reporting the second when
  // the first is true would be the preflight inventing a fact.
  return readCheckoutState("/nonexistent", async () => {
    throw new Error("not a git repository");
  }).then((state) => {
    assert.equal(state, undefined);
  });
});

test("a describe that finds no tag is an ordinary answer, not a failure", async () => {
  // `git describe --exact-match` EXITS NON-ZERO on a commit that carries no tag,
  // which is the common case on a branch checkout. Treating that as "git is
  // unavailable" would lose the branch name and the dirt count with it.
  const state = await readCheckoutState("/somewhere", async (args) => {
    if (args[0] === "describe") throw new Error("no tag exactly matches");
    if (args[0] === "rev-parse") return "deadbeefcafe\n";
    if (args[0] === "symbolic-ref") return "feat/x\n";
    return " M a.ts\n";
  });
  assert.deepEqual(state, {
    commit: "deadbeefcafe",
    ref: { kind: "branch", name: "feat/x" },
    dirtyCount: 1,
    deployDirty: false,
  });
});
