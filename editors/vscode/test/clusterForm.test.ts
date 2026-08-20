// The credential fields of the edit-cluster form (memql#4194, audit 7/8).
//
// THE NO-PREFILL RULE, MECHANICALLY CHECKED. The plans below are what
// src/extension.ts feeds the two password input boxes, so `value: ""` here IS
// the assertion that a stored credential never rides into an input. If a
// future edit reintroduces a prefill it has to widen the CredentialFieldPlan
// type or bypass the plan -- either is visible in review, and this file is the
// tripwire for the second.

import test from "node:test";
import assert from "node:assert/strict";

import {
  refreshTokenFieldPlan,
  resolveCredentialInput,
  tokenFieldPlan,
} from "../src/clusters/form.js";

test("the token field is never prefilled, whatever is stored", () => {
  assert.equal(tokenFieldPlan(undefined).value, "");
  assert.equal(tokenFieldPlan("").value, "");
  assert.equal(tokenFieldPlan("eyJhbGciOi.header.payload").value, "");
});

test("the refresh-token field is never prefilled either", () => {
  assert.equal(refreshTokenFieldPlan(undefined).value, "");
  assert.equal(refreshTokenFieldPlan("pending-refresh-token").value, "");
});

test("the prompt says a token is stored without saying the token", () => {
  const plan = tokenFieldPlan("eyJhbGciOi.header.payload");
  assert.match(plan.prompt, /stored/);
  assert.match(plan.prompt, /leave empty to keep/i);
  assert.doesNotMatch(plan.prompt, /eyJhbGciOi/);
});

test("an empty prompt for an empty store does not claim anything is stored", () => {
  assert.doesNotMatch(tokenFieldPlan("").prompt, /is stored/);
  assert.doesNotMatch(refreshTokenFieldPlan(undefined).prompt, /pending/i);
});

test("an empty submission keeps the stored credential", () => {
  assert.equal(resolveCredentialInput("", "stored-token"), "stored-token");
  assert.equal(resolveCredentialInput("   ", "stored-token"), "stored-token");
});

test("typed text replaces the stored credential", () => {
  assert.equal(resolveCredentialInput(" new-token ", "stored-token"), "new-token");
});

test("an empty submission over an empty store stays empty", () => {
  assert.equal(resolveCredentialInput("", undefined), "");
  assert.equal(resolveCredentialInput("", ""), "");
});

test("a cancelled input cancels", () => {
  assert.equal(resolveCredentialInput(undefined, "stored-token"), undefined);
});
