// flattenForList tests.
//
// flattenForList lifts payload fields to the top level for the row-list
// display card. The one correctness rule that matters is that a payload
// field can never shadow a row intrinsic of the same name -- memQL concepts
// routinely carry payload fields, so a payload `id` (or `createdAt`,
// `type`, `concept`, ...) colliding with the intrinsic is reachable, and a
// payload value winning there breaks the row's data-row-id, so clicking it
// resolves the wrong row (or none at all).

import test from "node:test";
import assert from "node:assert/strict";

import { flattenForList } from "../src/webview/rowProjection.js";

test("flattenForList lifts payload fields to the top level", () => {
  const out = flattenForList({ id: "row-1", payload: { name: "Alice", active: true } });
  assert.deepEqual(out, { id: "row-1", name: "Alice", active: true });
});

test("a payload field named id does not shadow the row's id intrinsic", () => {
  const out = flattenForList({
    id: "row-1",
    createdAt: "2026-01-01T00:00:00Z",
    payload: { id: "payload-id-should-not-win", name: "Alice" },
  });
  assert.equal(out.id, "row-1", "the row intrinsic id must win over a payload field named id");
  assert.equal(out.name, "Alice");
});

test("a payload field named createdAt does not shadow the row's createdAt intrinsic", () => {
  const out = flattenForList({ createdAt: "intrinsic-value", payload: { createdAt: "payload-value" } });
  assert.equal(out.createdAt, "intrinsic-value");
});

test("a payload field named type does not shadow the row's type intrinsic", () => {
  const out = flattenForList({ type: "intrinsic-type", payload: { type: "payload-type" } });
  assert.equal(out.type, "intrinsic-type");
});

test("this holds regardless of whether payload appears before or after the colliding intrinsic in key order", () => {
  // Object key order in the source object must not matter -- a wire payload
  // that happens to serialize with "payload" ahead of "id" is exactly as
  // reachable as the reverse.
  const out = flattenForList({ payload: { id: "payload-id" }, id: "row-1" });
  assert.equal(out.id, "row-1");
});

test("flattenForList leaves a row with no payload key unchanged", () => {
  const out = flattenForList({ id: "row-1", name: "no payload key here" });
  assert.deepEqual(out, { id: "row-1", name: "no payload key here" });
});

test("flattenForList preserves a non-object payload value as-is (array)", () => {
  const out = flattenForList({ id: "row-1", payload: ["not", "an", "object"] });
  assert.deepEqual(out, { id: "row-1", payload: ["not", "an", "object"] });
});

test("flattenForList preserves a null payload value as-is", () => {
  const out = flattenForList({ id: "row-1", payload: null });
  assert.deepEqual(out, { id: "row-1", payload: null });
});
