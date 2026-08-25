// Bare-vs-canonical id comparison at a client seam (memql#4581).
//
// These mirror component/memql/wire_bareids.go and the `sameRowAuthzOwner`
// rule in component/memql/rowauthz_enforce.go. The bug they exist for was a
// raw `!==` between a BARE row field and a CANONICAL MyAccess userId, which is
// always unequal -- so a goal the caller had just created reported "This goal
// belongs to someone else".

import test from "node:test";
import assert from "node:assert/strict";

import { bareShortId, sameEntityId } from "../src/client/id.js";

test("bareShortId strips a canonical node id to its short segment", () => {
  assert.equal(bareShortId("v1:identity:user:abc123"), "abc123");
  assert.equal(bareShortId("v1:planner:plan:p-9"), "p-9");
});

test("bareShortId admits a namespace containing a slash (memql#3898)", () => {
  assert.equal(bareShortId("v1:agents/tools:widget:abc"), "abc");
});

test("bareShortId is idempotent on an already-bare id", () => {
  assert.equal(bareShortId("abc123"), "abc123");
  assert.equal(bareShortId(bareShortId("v1:identity:user:abc123")), "abc123");
});

test("bareShortId preserves a 3-segment concept TYPE, which names no row", () => {
  // The Go pattern's trailing `.+` requires a fourth segment for exactly this
  // reason: a concept type must survive verbatim.
  assert.equal(bareShortId("v1:cognition:space"), "v1:cognition:space");
});

test("bareShortId returns anything it cannot decompose unchanged", () => {
  assert.equal(bareShortId(""), "");
  assert.equal(bareShortId("not-an-id"), "not-an-id");
  // A topic string embeds an id but is not one; anchoring keeps it whole.
  assert.equal(
    bareShortId("graph.node.created.v1:cognition:utterance"),
    "graph.node.created.v1:cognition:utterance",
  );
});

test("bareShortId keeps a shortId that itself contains colons", () => {
  assert.equal(bareShortId("v1:identity:user:a:b"), "a:b");
});

test("sameEntityId matches a canonical id against its bare form", () => {
  assert.equal(sameEntityId("user-1", "v1:identity:user:user-1"), true);
  assert.equal(sameEntityId("v1:identity:user:user-1", "user-1"), true);
});

test("sameEntityId matches identical ids in either shape", () => {
  assert.equal(sameEntityId("user-1", "user-1"), true);
  assert.equal(sameEntityId("v1:identity:user:u", "v1:identity:user:u"), true);
});

test("sameEntityId does not match different users", () => {
  assert.equal(sameEntityId("user-1", "v1:identity:user:user-2"), false);
  assert.equal(sameEntityId("user-1", "user-2"), false);
});

test("sameEntityId refuses an empty id rather than matching", () => {
  // A caller whose identity has not resolved has made no ownership claim.
  // Answering true would show one person another person's rows during the
  // window before MyAccess lands.
  assert.equal(sameEntityId("", "user-1"), false);
  assert.equal(sameEntityId("user-1", ""), false);
  assert.equal(sameEntityId("", ""), false);
  assert.equal(sameEntityId("   ", "user-1"), false);
});
