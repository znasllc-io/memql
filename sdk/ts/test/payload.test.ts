// Tests for the promoted payload / domain-id helpers (memql#2459).

import test from "node:test";
import assert from "node:assert/strict";

import { deepStripNulls } from "../src/client/payload.js";
import { displayDomainIds, isSyntheticDomainId } from "../src/client/domainIds.js";

test("deepStripNulls drops null and undefined at the top level", () => {
  const out = deepStripNulls({ a: 1, b: null, c: undefined, d: "x" });
  assert.deepEqual(out, { a: 1, d: "x" });
});

test("deepStripNulls recurses into nested objects", () => {
  const out = deepStripNulls({
    name: "s",
    meta: { savedAt: null, archivedAt: undefined, keep: true },
  });
  assert.deepEqual(out, { name: "s", meta: { keep: true } });
});

test("deepStripNulls recurses into arrays and their elements", () => {
  const out = deepStripNulls({
    rows: [
      { id: "1", expiresAt: null },
      { id: "2", expiresAt: "2026-01-01" },
    ],
  });
  assert.deepEqual(out, {
    rows: [{ id: "1" }, { id: "2", expiresAt: "2026-01-01" }],
  });
});

test("deepStripNulls preserves falsy-but-defined values", () => {
  const out = deepStripNulls({ zero: 0, empty: "", flag: false });
  assert.deepEqual(out, { zero: 0, empty: "", flag: false });
});

test("deepStripNulls returns primitives unchanged", () => {
  assert.equal(deepStripNulls(5), 5);
  assert.equal(deepStripNulls("x"), "x");
  assert.equal(deepStripNulls(null), null);
});

test("isSyntheticDomainId matches bridge- prefix only", () => {
  assert.equal(isSyntheticDomainId("bridge-0123456789abcdef"), true);
  assert.equal(isSyntheticDomainId("business_administration"), false);
  assert.equal(isSyntheticDomainId(""), false);
});

test("displayDomainIds drops synthetic ids and keeps order", () => {
  const out = displayDomainIds([
    "math_algebra",
    "bridge-deadbeefdeadbeef",
    "design_ux",
  ]);
  assert.deepEqual(out, ["math_algebra", "design_ux"]);
});
