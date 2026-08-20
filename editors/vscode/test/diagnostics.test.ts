// The information policy's display primitives (memql#4194).
//
// briefMessage and recordDiagnostic are the mechanical half of the rule
// README.md's Security section states: surfaces carry a short verdict, the
// channel keeps the record. argShapeLines is the Runs-tree half: shapes on the
// hover, never the values a developer typed.

import test from "node:test";
import assert from "node:assert/strict";

import {
  argShapeLines,
  briefMessage,
  recordDiagnostic,
  valueShape,
} from "../src/state/diagnostics.js";

test("briefMessage keeps a short first line untouched", () => {
  assert.equal(briefMessage("connection refused"), "connection refused");
});

test("briefMessage drops everything after the first line", () => {
  const raw = "dial failed\n    at Object.connect (net.js:12)\n    at process";
  assert.equal(briefMessage(raw), "dial failed");
});

test("briefMessage caps a runaway line and marks the cut", () => {
  const out = briefMessage("x".repeat(500));
  assert.equal(out.length, 140);
  assert.ok(out.endsWith("..."));
});

test("recordDiagnostic writes the dated headline, the detail, and a separator", () => {
  const lines: string[] = [];
  recordDiagnostic(
    { appendLine: (l) => lines.push(l) },
    "the dial failed",
    "dial tcp 10.0.0.4: refused",
    "2026-08-20T00:00:00.000Z",
  );
  assert.deepEqual(lines, [
    "[2026-08-20T00:00:00.000Z] the dial failed",
    "dial tcp 10.0.0.4: refused",
    "",
  ]);
});

test("recordDiagnostic does not repeat a detail that IS the headline", () => {
  const lines: string[] = [];
  recordDiagnostic({ appendLine: (l) => lines.push(l) }, "same", "same", "T");
  assert.deepEqual(lines, ["[T] same", ""]);
});

test("valueShape names the shape and never the value", () => {
  assert.equal(valueShape("hunter2hunter2"), "string(14)");
  assert.equal(valueShape(42), "number");
  assert.equal(valueShape(true), "boolean");
  assert.equal(valueShape(null), "null");
  assert.equal(valueShape([1, 2, 3]), "array[3]");
  assert.equal(valueShape({ a: 1, b: 2 }), "object{2}");
});

test("argShapeLines is sorted, one line per argument, and value-free", () => {
  const lines = argShapeLines({ zeta: "sk-secret-value", alpha: 7 });
  assert.deepEqual(lines, ["alpha: number", "zeta: string(15)"]);
  for (const line of lines) {
    assert.doesNotMatch(line, /sk-secret/);
  }
});
