// The generated argument form.
//
// "Arg form generated from the real args block, including enums and required
// markers" is an acceptance criterion, and the interesting half of it is the
// coercion back out. A generated form's usual failure is guessing: a number
// box whose text is not a number silently becoming 0, or a string. The call
// string is parsed by the ENGINE, so a wrongly-typed argument surfaces as an
// engine error naming the construct rather than the field -- which is a far
// worse place for the developer to find out.

import test from "node:test";
import assert from "node:assert/strict";

import type { RunnableArg } from "../src/constructs/runnable.js";
import { buildFields, coerceArgs, orphanedValueNames } from "../src/state/argForm.js";

const ARGS: RunnableArg[] = [
  { name: "spaceId", type: "string", required: true, description: "The space." },
  { name: "limit", type: "number", required: false },
  { name: "active", type: "boolean", required: false },
  { name: "status", type: "string", required: false, enum: ["active", "left"] },
  { name: "filter", type: "object", required: false },
  { name: "ids", type: "array", required: false },
  { name: "extra", type: "any", required: false },
];

// -----------------------------------------------------------------------------
// buildFields
// -----------------------------------------------------------------------------

test("buildFields -- carries required markers, enums and descriptions through", () => {
  const fields = buildFields(ARGS);
  assert.equal(fields.length, ARGS.length);
  assert.equal(fields[0]?.required, true);
  assert.equal(fields[0]?.description, "The space.");
  assert.equal(fields[1]?.required, false);
  assert.deepEqual(fields[3]?.enumValues, ["active", "left"]);
  // Absent rather than undefined so the renderer can test `.length` without a
  // guard at every use.
  assert.deepEqual(fields[0]?.enumValues, []);
});

test("buildFields -- renders prior values back into editable text", () => {
  const fields = buildFields(ARGS, {
    spaceId: "s1",
    limit: 10,
    active: true,
    filter: { kind: "daily" },
  });
  // A string shows bare -- an input box showing "abc", not "\"abc\"".
  assert.equal(fields[0]?.text, "s1");
  assert.equal(fields[1]?.text, "10");
  assert.equal(fields[2]?.text, "true");
  // A composite shows as pretty JSON, because that is what gets edited.
  assert.equal(fields[4]?.text, '{\n  "kind": "daily"\n}');
});

test("buildFields -- an unset argument gets an empty box", () => {
  const fields = buildFields(ARGS, { spaceId: "s1" });
  assert.equal(fields[1]?.text, "");
});

test("orphanedValueNames -- names values the construct no longer declares", () => {
  // Happens when a hand-authored run configuration outlives a change to the
  // construct's args. Carrying the value in a hidden field would be worse
  // than losing it; the caller says what happened instead.
  assert.deepEqual(orphanedValueNames(ARGS, { spaceId: "s1", gone: 1 }), ["gone"]);
});

// -----------------------------------------------------------------------------
// coerceArgs
// -----------------------------------------------------------------------------

test("coerceArgs -- coerces each declared type", () => {
  const result = coerceArgs(ARGS, {
    spaceId: "s1",
    limit: "10",
    active: "true",
    status: "active",
    filter: '{"kind":"daily"}',
    ids: '["a","b"]',
    extra: "42",
  });
  assert.ok(result.ok);
  assert.deepEqual(result.values, {
    spaceId: "s1",
    limit: 10,
    active: true,
    status: "active",
    filter: { kind: "daily" },
    ids: ["a", "b"],
    extra: 42,
  });
});

test("coerceArgs -- an empty OPTIONAL field is omitted, not sent as null", () => {
  // Omitted and null are different to the engine: an omitted argument lets
  // the body's `??` default apply and makes a `when(args.x)` guard drop its
  // block, while an explicit null is a supplied value the guard treats as
  // PRESENT.
  const result = coerceArgs(ARGS, { spaceId: "s1", limit: "", active: "  " });
  assert.ok(result.ok);
  assert.deepEqual(result.values, { spaceId: "s1" });
  assert.equal("limit" in result.values, false);
});

test("coerceArgs -- an empty REQUIRED field is an error", () => {
  const result = coerceArgs(ARGS, { spaceId: "" });
  assert.equal(result.ok, false);
  assert.ok(!result.ok);
  assert.equal(result.errors.spaceId, "required");
});

test("coerceArgs -- a non-numeric number is refused, not silently zeroed", () => {
  const result = coerceArgs(ARGS, { spaceId: "s1", limit: "ten" });
  assert.ok(!result.ok);
  assert.match(result.errors.limit ?? "", /not a number/);
});

test("coerceArgs -- a non-boolean boolean is refused", () => {
  // "yes" coerced to true would be a guess, and the engine would receive a
  // value the developer did not type.
  const result = coerceArgs(ARGS, { spaceId: "s1", active: "yes" });
  assert.ok(!result.ok);
  assert.match(result.errors.active ?? "", /not true or false/);
});

test("coerceArgs -- an enum value outside the closed set is refused BY NAME", () => {
  // The set is the DSL's own, so the engine is guaranteed to refuse it. Saying
  // so here names the field; the engine's refusal would not.
  const result = coerceArgs(ARGS, { spaceId: "s1", status: "archived" });
  assert.ok(!result.ok);
  assert.match(result.errors.status ?? "", /must be one of: active, left/);
});

test("coerceArgs -- an object field rejects a JSON array", () => {
  const result = coerceArgs(ARGS, { spaceId: "s1", filter: "[1,2]" });
  assert.ok(!result.ok);
  assert.match(result.errors.filter ?? "", /must be a JSON object/);
});

test("coerceArgs -- an array field rejects a JSON object", () => {
  const result = coerceArgs(ARGS, { spaceId: "s1", ids: '{"a":1}' });
  assert.ok(!result.ok);
  assert.match(result.errors.ids ?? "", /must be a JSON array/);
});

test("coerceArgs -- malformed JSON names the field", () => {
  const result = coerceArgs(ARGS, { spaceId: "s1", filter: "{oops" });
  assert.ok(!result.ok);
  assert.match(result.errors.filter ?? "", /not valid JSON/);
});

test("coerceArgs -- an `any` field falls back to the literal text when it is not JSON", () => {
  // "any" is the degradation for a DSL type with no form equivalent, so the
  // box takes JSON -- but the user has no reason to think of it as a JSON
  // editor, and an unquoted word must work.
  const result = coerceArgs(ARGS, { spaceId: "s1", extra: "hello" });
  assert.ok(result.ok);
  assert.equal(result.values.extra, "hello");
});

test("coerceArgs -- every field is checked, so all problems surface at once", () => {
  const result = coerceArgs(ARGS, { spaceId: "", limit: "x", active: "maybe" });
  assert.ok(!result.ok);
  assert.deepEqual(Object.keys(result.errors).sort(), ["active", "limit", "spaceId"]);
});

test("coerceArgs -- a construct with no arguments coerces to an empty map", () => {
  const result = coerceArgs([], {});
  assert.ok(result.ok);
  assert.deepEqual(result.values, {});
});
