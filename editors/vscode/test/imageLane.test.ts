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

import { rebuiltMessage, rebuiltNodes, releasedImages, returnsToReleasedImages } from "../src/state/imageLane.js";

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

test("the crossing says the operator's own build is what goes", () => {
  assert.equal(returnsToReleasedImages("local", "v0.17.0"), "Your own build is replaced with released v0.17.0 images.");
  assert.equal(returnsToReleasedImages("local", ""), "Your own build is replaced with released images.");
});

// -----------------------------------------------------------------------------
// what a finished rebuild reports
// -----------------------------------------------------------------------------

test("the finished sentence reads the envelope: the build that runs now", () => {
  assert.equal(
    rebuiltMessage("local", { nodes: "bff agent", commit: "abc1234def5678", dirtyCount: 2 }),
    "local now runs your build (abc1234, 2 uncommitted files).",
  );
  // A clean tree is the ordinary case and is not worth a clause.
  assert.equal(rebuiltMessage("local", { nodes: "bff", commit: "abc1234def5678", dirtyCount: 0 }), "local now runs your build (abc1234).");
  // No node list and no double hyphen: a full rebuild names nine services.
  assert.doesNotMatch(rebuiltMessage("local", { nodes: "bff agent", commit: "abc1234" }), /bff|--/);
});

test("one file is not '1 files'", () => {
  assert.match(rebuiltMessage("local", { nodes: "bff", commit: "abcdefg12", dirtyCount: 1 }), /1 uncommitted file\)/);
});

test("a fact the envelope did not carry is left out, never invented", () => {
  // THE PINNED DECISION. `Number(undefined)` is NaN and `Number(null)` is 0, so
  // a coercing reader prints either "NaN uncommitted files" or -- far worse --
  // "0 uncommitted files", a CLAIM that the tree was clean made from a field
  // that was never reported. Only an actual number counts.
  assert.equal(rebuiltMessage("local", { commit: "abc1234def" }), "local now runs your build (abc1234).");
  assert.equal(rebuiltMessage("local", { commit: "abc1234def", dirtyCount: null }), "local now runs your build (abc1234).");
  assert.equal(rebuiltMessage("local", { commit: "abc1234def", dirtyCount: "2" }), "local now runs your build (abc1234).");
  assert.equal(rebuiltMessage("local", { nodes: "bff" }), "local now runs your build.");
  assert.equal(rebuiltMessage("local", undefined), "local now runs your build.");
});

test("a partial rebuild names the services it built; a full one names none", () => {
  assert.equal(rebuiltNodes("bff, agent", { nodes: "bff agent" }), "bff, agent");
  assert.equal(rebuiltNodes("", { nodes: "bff agent cognition planner voice workbench mcp identity edge" }), "");
  assert.equal(rebuiltNodes("bff", undefined), "");
});
