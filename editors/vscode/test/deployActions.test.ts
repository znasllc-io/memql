// The deploy-action catalog: the role matrix mirror and the type-to-confirm
// rules (memql#3312).
//
// Nothing here is a gate -- the engine is, through the same authorize helpers
// the unary path runs. These tests pin the HINT so the buttons drawn match
// what the engine will actually allow, and so the confirmation cannot quietly
// become satisfiable without reading the target.

import test from "node:test";
import assert from "node:assert/strict";

import type { Role } from "@znasllc-io/memql-sdk-core/client";

import {
  DEPLOY_ACTIONS,
  actionById,
  confirmationMatches,
  confirmationPhrase,
  roleVisibility,
  rolloutRequiresConfirmation,
  satisfiesTier,
  tierDescription,
  visibleActions,
  type DeployActionId,
} from "../src/deploy/actions.js";

function ids(role: Role): DeployActionId[] {
  return visibleActions(roleVisibility(role)).map((a) => a.id);
}

// -----------------------------------------------------------------------------
// The catalog mirrors the service
// -----------------------------------------------------------------------------

test("the catalog carries exactly the five actions the surface drives", () => {
  assert.deepEqual(DEPLOY_ACTIONS.map((a) => a.id), [
    "cutVersion",
    "deploy",
    "promote",
    "rollback",
    "rolloutAction",
  ]);
});

test("each action's tier matches component/deploycontrol's authorize helper", () => {
  // Read off service.go: authorizeDeploy for cut + deploy, authorize for
  // promote + rollout_action, authorizeOwner for rollback_deployment.
  assert.deepEqual(
    Object.fromEntries(DEPLOY_ACTIONS.map((a) => [a.id, a.tier])),
    {
      cutVersion: "developer",
      deploy: "developer",
      promote: "admin",
      rollback: "owner",
      rolloutAction: "admin",
    },
  );
});

test("each action's verb matches the service's audit action suffix", () => {
  // So an operator can grep the audit log with exactly what the UI told them.
  assert.deepEqual(
    Object.fromEntries(DEPLOY_ACTIONS.map((a) => [a.id, a.verb])),
    {
      cutVersion: "cut_version",
      deploy: "deploy",
      promote: "promote",
      rollback: "rollback_deployment",
      rolloutAction: "rollout_action",
    },
  );
});

test("actionById throws rather than silently resolving to another action", () => {
  assert.throws(() => actionById("nope" as DeployActionId), /unknown deploy action/);
});

// -----------------------------------------------------------------------------
// Role visibility
// -----------------------------------------------------------------------------

test("an owner sees every action, including the owner-only rollback", () => {
  assert.deepEqual(ids("owner"), ["cutVersion", "deploy", "promote", "rollback", "rolloutAction"]);
});

test("an admin sees everything EXCEPT rollback -- owner-only by design", () => {
  const shown = ids("admin");
  assert.ok(!shown.includes("rollback"));
  assert.deepEqual(shown, ["cutVersion", "deploy", "promote", "rolloutAction"]);
});

test("a writer and a reader see NO actions -- the acceptance criterion", () => {
  assert.deepEqual(ids("writer"), []);
  assert.deepEqual(ids("reader"), []);
});

test("an indeterminate role is offered the actions, with a reason", () => {
  // The TS SDK's wire enum has no USER_ROLE_DEVELOPER, so roleFromWire maps a
  // developer to "". Hiding on that basis would lock a genuine developer out
  // of cut and deploy that the engine would have allowed -- and hiding is not
  // the gate anyway. The engine refuses, naming the role.
  const visibility = roleVisibility("");
  assert.equal(visibility.kind, "indeterminate");
  assert.equal(visibleActions(visibility).length, DEPLOY_ACTIONS.length);
  if (visibility.kind === "indeterminate") {
    assert.match(visibility.reason, /could not be determined/);
  }
});

test("an undefined role is indeterminate too, and takes a caller's reason", () => {
  const visibility = roleVisibility(undefined, "the access read failed");
  assert.equal(visibility.kind, "indeterminate");
  if (visibility.kind === "indeterminate") assert.equal(visibility.reason, "the access read failed");
});

test("satisfiesTier is narrower than the engine, never wider", () => {
  // Narrower hides a button a developer could have used, which the
  // indeterminate branch covers. Wider would show one that is certain to be
  // refused, which trains operators to ignore the hint.
  assert.equal(satisfiesTier("owner", "owner"), true);
  assert.equal(satisfiesTier("admin", "owner"), false);
  assert.equal(satisfiesTier("admin", "admin"), true);
  assert.equal(satisfiesTier("admin", "developer"), true);
  assert.equal(satisfiesTier("writer", "developer"), false);
  assert.equal(satisfiesTier("", "developer"), false);
});

test("tier descriptions read the way the service phrases the refusal", () => {
  assert.equal(tierDescription("owner"), "owner");
  assert.equal(tierDescription("admin"), "owner or admin");
  assert.equal(tierDescription("developer"), "developer, admin, or owner");
});

// -----------------------------------------------------------------------------
// Type-to-confirm
// -----------------------------------------------------------------------------

test("promote, rollback and rollout abort are the confirmed actions", () => {
  assert.equal(actionById("promote").typeToConfirm, true);
  assert.equal(actionById("rollback").typeToConfirm, true);
  assert.equal(actionById("cutVersion").typeToConfirm, false);
  assert.equal(actionById("deploy").typeToConfirm, false);
  // rolloutAction's requirement depends on the sub-action chosen.
  assert.equal(rolloutRequiresConfirmation("abort"), true);
  assert.equal(rolloutRequiresConfirmation("promote"), false);
});

test("the phrase is the action's TARGET, not a yes and not the action name", () => {
  // A confirmation satisfiable without reading the target confirms nothing.
  assert.equal(confirmationPhrase("promote", "2026.6.21"), "2026.6.21");
  assert.equal(confirmationPhrase("rollback", "d-100"), "d-100");
  assert.equal(confirmationPhrase("rolloutAction", "bff-rollout"), "bff-rollout");
  assert.notEqual(confirmationPhrase("promote", "2026.6.21"), "yes");
  assert.notEqual(confirmationPhrase("rollback", "d-100"), "rollback");
});

test("unconfirmed actions have no phrase", () => {
  assert.equal(confirmationPhrase("cutVersion", "1.0.0"), "");
  assert.equal(confirmationPhrase("deploy", "d-100"), "");
});

test("an empty expectation is never satisfiable", () => {
  // An action with nothing identifiable to re-type must not proceed
  // unchallenged, so "" must not read as "no confirmation needed".
  assert.equal(confirmationPhrase("promote", ""), "");
  assert.equal(confirmationMatches("", ""), false);
  assert.equal(confirmationMatches("", "anything"), false);
});

test("confirmation forgives surrounding whitespace and nothing else", () => {
  assert.equal(confirmationMatches("2026.6.21", "  2026.6.21 "), true);
  assert.equal(confirmationMatches("2026.6.21", "2026.6.2"), false);
  assert.equal(confirmationMatches("2026.6.21", "2026.6.211"), false);
  assert.equal(confirmationMatches("PROD", "prod"), false);
  assert.equal(confirmationMatches("2026.6.21", ""), false);
});
