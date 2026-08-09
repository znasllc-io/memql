// Named-call rendering.
//
// The engine PARSES this string, so every case here is a case where getting it
// wrong produces an engine-side error naming the construct rather than the
// argument -- or, worse, a call that parses into something the developer did
// not mean. The reference is sdk/go/client/support.go's renderMemQLValue +
// quoteMemQL, which the Go SDK's thousands of generated builders already
// exercise against the real parser.

import test from "node:test";
import assert from "node:assert/strict";

import { buildNamedCall, callKeywordFor, extractErrorId, renderMemqlValue } from "../src/run/call.js";

// -----------------------------------------------------------------------------
// The keyword mapping
// -----------------------------------------------------------------------------

test("callKeywordFor -- `mutate` is declared, `mutation` is invoked", () => {
  // The DSL declares with `mutate`; the engine's named-call parser wants
  // `mutation`. Passing the declaration keyword through produces a parse
  // failure at the engine with no hint about which side is confused.
  assert.equal(callKeywordFor("mutate"), "mutation");
  assert.equal(callKeywordFor("query"), "query");
  assert.equal(callKeywordFor("logic"), "logic");
});

test("callKeywordFor -- tool and automation have no ExecuteQueryMsg form", () => {
  assert.equal(callKeywordFor("tool"), undefined);
  assert.equal(callKeywordFor("automation"), undefined);
});

test("buildNamedCall -- refuses a kind that is not invoked by name", () => {
  assert.throws(() => buildNamedCall("tool", "searchUsers", [], {}), /CallToolMsg/);
});

// -----------------------------------------------------------------------------
// renderMemqlValue
// -----------------------------------------------------------------------------

test("renderMemqlValue -- scalars", () => {
  assert.equal(renderMemqlValue("abc"), '"abc"');
  assert.equal(renderMemqlValue(42), "42");
  assert.equal(renderMemqlValue(1.5), "1.5");
  assert.equal(renderMemqlValue(true), "true");
  assert.equal(renderMemqlValue(false), "false");
  assert.equal(renderMemqlValue(null), "nil");
  assert.equal(renderMemqlValue(undefined), "nil");
});

test("renderMemqlValue -- a string with quotes and newlines is escaped, not truncated", () => {
  // An unescaped quote would terminate the literal early and turn the rest of
  // the value into syntax -- the classic injection shape, here landing in a
  // string the engine parses.
  assert.equal(renderMemqlValue('say "hi"'), '"say \\"hi\\""');
  assert.equal(renderMemqlValue("a\nb"), '"a\\nb"');
  assert.equal(renderMemqlValue('back\\slash'), '"back\\\\slash"');
});

test("renderMemqlValue -- object keys are sorted", () => {
  // Determinism is not cosmetic here: the rendered call is compared against
  // the last session-defined bundle's cache key, and a run configuration is
  // diffed in a repository. Key order following whatever the form built would
  // churn both.
  assert.equal(renderMemqlValue({ b: 1, a: 2 }), "{a: 2, b: 1}");
  assert.equal(renderMemqlValue({ z: { y: 1, x: 2 } }), "{z: {x: 2, y: 1}}");
});

test("renderMemqlValue -- arrays render element-by-element", () => {
  assert.equal(renderMemqlValue(["a", 1, true]), '["a", 1, true]');
  assert.equal(renderMemqlValue([]), "[]");
});

test("renderMemqlValue -- a non-finite number is refused rather than emitted", () => {
  // `Infinity` and `NaN` have no MemQL literal. Emitting the JS spelling
  // produces a call the engine cannot parse, and its error names the
  // construct rather than the argument.
  assert.throws(() => renderMemqlValue(Number.POSITIVE_INFINITY), /no MemQL literal form/);
  assert.throws(() => renderMemqlValue(Number.NaN), /no MemQL literal form/);
});

// -----------------------------------------------------------------------------
// buildNamedCall
// -----------------------------------------------------------------------------

test("buildNamedCall -- renders the engine's call form", () => {
  const call = buildNamedCall("query", "spaceParticipants", ["spaceId"], { spaceId: "s1" });
  assert.equal(call, 'query spaceParticipants(spaceId: "s1")');
});

test("buildNamedCall -- a construct with no arguments renders empty parentheses", () => {
  assert.equal(buildNamedCall("query", "activeSpaces", [], {}), "query activeSpaces()");
});

test("buildNamedCall -- arguments follow the DECLARED order, not object key order", () => {
  // The values object comes from a form or a hand-edited JSON file, and JS
  // object key order is an implementation detail nobody should have to
  // reason about for a string the engine parses.
  const call = buildNamedCall(
    "mutate",
    "createSpace",
    ["spaceId", "name", "kind"],
    { name: "Ops", kind: "daily", spaceId: "s1" },
  );
  assert.equal(call, 'mutation createSpace(spaceId: "s1", name: "Ops", kind: "daily")');
});

test("buildNamedCall -- an argument that was not supplied is OMITTED, not sent as nil", () => {
  // Omitted and nil are different to the engine: an omitted optional argument
  // lets the body's `??` default apply and makes a `when(args.x)` guard drop
  // its block, while an explicit nil is a supplied value the guard treats as
  // PRESENT.
  const call = buildNamedCall("query", "q", ["a", "b"], { a: "x" });
  assert.equal(call, 'query q(a: "x")');
});

test("buildNamedCall -- a value for an undeclared argument is passed through, not dropped", () => {
  // This happens when a hand-authored run configuration outlives a change to
  // the construct's args. Dropping it silently would run something the file
  // does not say; passing it through lets the engine reject it by name.
  const call = buildNamedCall("query", "q", ["a"], { a: 1, stale: "x" });
  assert.equal(call, 'query q(a: 1, stale: "x")');
});

test("buildNamedCall -- nested objects and arrays round-trip", () => {
  const call = buildNamedCall("logic", "seed", ["event"], {
    event: { payload: { ids: ["a", "b"], count: 2 }, kind: "created" },
  });
  assert.equal(
    call,
    'logic seed(event: {kind: "created", payload: {count: 2, ids: ["a", "b"]}})',
  );
});

test("buildNamedCall -- refuses an empty name", () => {
  assert.throws(() => buildNamedCall("query", "", [], {}), /name is required/);
});

// -----------------------------------------------------------------------------
// extractErrorId
// -----------------------------------------------------------------------------

test("extractErrorId -- finds the engine's ERR- id", () => {
  // The id is the only handle the developer has on the server-side log entry
  // for their failure, so the result view renders it separately and copyably.
  assert.equal(extractErrorId("query failed: ERR-a1b2c3 internal"), "ERR-a1b2c3");
  assert.equal(extractErrorId("ERR-FFFFFF"), "ERR-FFFFFF");
});

test("extractErrorId -- absent for an ordinary message", () => {
  assert.equal(extractErrorId("no such construct: spaceParticipants"), "");
  // Not six hex digits: a near-miss must not be reported as an id an operator
  // would then fail to find in the logs.
  assert.equal(extractErrorId("ERR-12345"), "");
  assert.equal(extractErrorId("ERR-zzzzzz"), "");
});
