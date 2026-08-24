// conceptsFromWire -- the data-origins declaration (epic memql#4378).
//
// A client reads `dataState` BEFORE offering an edit: a concept whose
// state is "mirror" refuses every write that is not its connector's, so
// an editor rendered over one offers an action the server will refuse.
// These tests pin that the three fields survive the wire -> SDK
// projection, and that an older server's silence stays silence rather
// than being guessed into a state.

import test from "node:test";
import assert from "node:assert/strict";

import { conceptsFromWire } from "../src/client/types.js";

test("conceptsFromWire carries the declaration for all three states", () => {
  const out = conceptsFromWire([
    { id: "v1:shopify:product", dataState: "mirror", dataOrigin: "shopify" },
    {
      id: "v1:wholesale:priceList",
      dataState: "origin",
      dataOrigin: "memql",
      dataMirroredTo: ["shopify", "quickBooks"],
    },
    { id: "v1:planner:plan", dataState: "native", dataOrigin: "memql" },
  ]);

  assert.equal(out.length, 3);
  // Destructured so noUncheckedIndexedAccess sees defined values; the
  // length assertion above is what makes that safe.
  const [mirror, origin, native] = out as [typeof out[0], typeof out[0], typeof out[0]];

  assert.equal(mirror.dataState, "mirror");
  assert.equal(mirror.dataOrigin, "shopify");
  // Only an ORIGIN has mirror targets.
  assert.deepEqual(mirror.dataMirroredTo, []);

  assert.equal(origin.dataState, "origin");
  // Authored order, which is the order the outbox appends in.
  assert.deepEqual(origin.dataMirroredTo, ["shopify", "quickBooks"]);

  // The server sends the EFFECTIVE origin, so no client re-derives the
  // default.
  assert.equal(native.dataState, "native");
  assert.equal(native.dataOrigin, "memql");
});

test("conceptsFromWire says nothing rather than guessing when the server predates the fields", () => {
  const out = conceptsFromWire([{ id: "v1:planner:plan" }]);
  assert.equal(out.length, 1);
  const [plan] = out as [typeof out[0]];
  assert.equal(plan.dataState, "");
  assert.equal(plan.dataOrigin, "");
  assert.deepEqual(plan.dataMirroredTo, []);
});
