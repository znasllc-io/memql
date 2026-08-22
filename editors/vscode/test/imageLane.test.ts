// The sentences this extension says about WHICH IMAGES a local cluster runs
// (memql#4246).
//
// One wording, four surfaces. The lane crossing is stated by the install
// wizard's checklist, by the upgrade confirmation, by the Create-deployment tag
// screen and by the rebuild checklist -- and every one of them is a place an
// operator decides whether to proceed. Four copies of the sentence would drift,
// and a drifted copy is a surface that says the crossing is something slightly
// different from what the other three said.
//
// `rebuiltMessage` lives here for a duller reason: it was pure logic marooned
// inside a `vscode`-importing file, where the unit lane cannot reach it.

import test from "node:test";
import assert from "node:assert/strict";

import { rebuiltMessage, releasedImages, returnsToReleasedImages } from "../src/state/imageLane.js";

// -----------------------------------------------------------------------------
// the lane sentence
// -----------------------------------------------------------------------------

test("an unknown release tag drops the adjective rather than printing one", () => {
  // "released release images" is what the placeholder produced, and it reads as
  // a bug in front of an operator being asked to approve something.
  assert.equal(releasedImages("v0.17.0"), "released v0.17.0 images");
  assert.equal(releasedImages(""), "released images");
  assert.equal(releasedImages("   "), "released images");
});

test("the crossing names the instance, and says what it is running today", () => {
  assert.equal(
    returnsToReleasedImages("local", "v0.17.0"),
    "This returns local to released v0.17.0 images; it runs a checkout build today.",
  );
  assert.equal(
    returnsToReleasedImages("local", ""),
    "This returns local to released images; it runs a checkout build today.",
  );
});

// -----------------------------------------------------------------------------
// what a finished rebuild reports
// -----------------------------------------------------------------------------

test("the toast reads the envelope, and names the nodes it actually built", () => {
  // The SCRIPT's list, space-separated, not the operator's request: an empty
  // request expands to nine node types, and restating "" would say nothing.
  assert.equal(
    rebuiltMessage("local", { nodes: "bff agent", commit: "abc1234def5678", dirtyCount: 2 }),
    "Rebuilt bff, agent -- local now runs your checkout (abc1234, 2 uncommitted files).",
  );
  // Seven characters, the same short form the row and the receipt readers use.
  assert.match(
    rebuiltMessage("local", { nodes: "bff", commit: "abc1234def5678", dirtyCount: 0 }),
    /\(abc1234, 0 uncommitted files\)/,
  );
});

test("one file is not '1 files'", () => {
  assert.match(
    rebuiltMessage("local", { nodes: "bff", commit: "abcdefg12", dirtyCount: 1 }),
    /1 uncommitted file\)/,
  );
});

test("a fact the envelope did not carry is left out, never invented", () => {
  // THE PINNED DECISION. `Number(undefined)` is NaN and `Number(null)` is 0, so
  // a coercing reader prints either "NaN uncommitted files" or -- far worse --
  // "0 uncommitted files", which is a CLAIM that the tree was clean made from a
  // field that was never reported. Only an actual number counts.
  assert.equal(
    rebuiltMessage("local", { nodes: "bff", commit: "abc1234def" }),
    "Rebuilt bff -- local now runs your checkout (abc1234).",
  );
  assert.equal(
    rebuiltMessage("local", { nodes: "bff", commit: "abc1234def", dirtyCount: null }),
    "Rebuilt bff -- local now runs your checkout (abc1234).",
  );
  assert.equal(
    rebuiltMessage("local", { nodes: "bff", commit: "abc1234def", dirtyCount: "2" }),
    "Rebuilt bff -- local now runs your checkout (abc1234).",
  );
  // No commit either: no parenthetical at all, rather than an empty one.
  assert.equal(
    rebuiltMessage("local", { nodes: "bff" }),
    "Rebuilt bff -- local now runs your checkout.",
  );
  // And no envelope at all still names the instance and the default set.
  assert.equal(
    rebuiltMessage("local", undefined),
    "Rebuilt the app nodes -- local now runs your checkout.",
  );
});
