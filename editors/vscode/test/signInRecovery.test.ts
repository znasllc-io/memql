// What a failed sign-in leaves one click away (memql#4621).
//
// `describeSignInFailure` computes `retryable` and documents it -- "A UI may
// offer a retry affordance on true; it must not on false" -- and the editor read
// `level` and `message` and threw it away, raising every failure toast with no
// actions at all. After the browser flow ran out its ten-minute deadline the
// operator read a sentence naming `MemQL: Sign In With a Device Code` and had to
// open the palette and type it.
//
// The cases below are paired with the REAL `describeSignInFailure` wherever the
// claim is about a kind, so the two modules cannot drift into disagreeing about
// which kinds are retryable -- which is the only way this decision can be wrong
// without either file looking wrong on its own.

import test from "node:test";
import assert from "node:assert/strict";

import { AuthFlowError, type AuthFlowErrorKind } from "../src/auth/errors.js";
import { describeSignInFailure } from "../src/auth/signin.js";
import {
  SIGN_IN_DEVICE_CODE,
  SIGN_IN_EDIT_CLUSTER,
  SIGN_IN_RETRY,
  signInRecoveryActions,
} from "../src/clusters/signInRecovery.js";

/** Every kind in the taxonomy, so a new one cannot slip past these cases. */
const ALL_KINDS: AuthFlowErrorKind[] = [
  "misconfigured",
  "registrationFailed",
  "bindFailed",
  "timeout",
  "cancelled",
  "browserUnavailable",
  "authorizationDenied",
  "stateMismatch",
  "invalidCallback",
  "exchangeRejected",
];

/** What the editor actually has in hand at the toast: a report and a rejection. */
function actionsFor(kind: AuthFlowErrorKind, flow: "auto" | "deviceCode" = "auto"): string[] {
  const err = new AuthFlowError(kind, `the ${kind} case`);
  const report = describeSignInFailure("staging", err);
  return signInRecoveryActions({ retryable: report.retryable, kind, flow });
}

test("a timeout offers a retry AND the device code the advice already names", () => {
  // The advice text for `timeout` says: run "MemQL: Sign In" again, and if the
  // page can never reach this machine run "MemQL: Sign In With a Device Code".
  // Both of those are commands; both are now buttons.
  assert.deepEqual(actionsFor("timeout"), [SIGN_IN_RETRY, SIGN_IN_DEVICE_CODE]);
});

test("a timeout of the DEVICE flow does not offer a device code", () => {
  // The one button that cannot be the remedy is the one that just failed.
  assert.deepEqual(actionsFor("timeout", "deviceCode"), [SIGN_IN_RETRY]);
});

test("misconfigured offers the edit, and only the edit", () => {
  // The cluster names no identity service, so there is nowhere to register or
  // authorize and nothing was attempted. A retry cannot change that; editing
  // the entry is the only thing that can, and putting a useless button first
  // would be worse than putting none there.
  assert.deepEqual(actionsFor("misconfigured"), [SIGN_IN_EDIT_CLUSTER]);
});

test("stateMismatch offers nothing, on purpose", () => {
  // The most serious kind here and deliberately not retryable: a forged or
  // replayed callback wants a human looking at it, not a second attempt one
  // click away. `retryable` says so; this must not second-guess it.
  assert.deepEqual(actionsFor("stateMismatch"), []);
});

test("browserUnavailable offers nothing, because the fallback has already run", () => {
  // It is a FALLBACK TRIGGER (auth/errors.ts): by the time a failure carrying
  // this kind reaches a toast, signInWithDeviceCodeFallback has already tried
  // the device grant. Offering it again would be offering what just happened.
  assert.deepEqual(actionsFor("browserUnavailable"), []);
});

test("every other retryable kind offers exactly the retry", () => {
  for (const kind of ["registrationFailed", "bindFailed", "invalidCallback", "exchangeRejected", "authorizationDenied"] as AuthFlowErrorKind[]) {
    assert.deepEqual(actionsFor(kind), [SIGN_IN_RETRY], `kind: ${kind}`);
  }
});

test("a rejection that is not an AuthFlowError earns no buttons", () => {
  // describeSignInFailure reports such a rejection as not retryable, and there
  // is no kind to reason from. Inventing a button for a failure with no
  // taxonomy is how a retry loop comes to retry a refusal.
  const report = describeSignInFailure("staging", new Error("something else went wrong"));
  assert.equal(report.retryable, false);
  assert.deepEqual(signInRecoveryActions({ retryable: false, kind: undefined, flow: "auto" }), []);
});

test("no kind produces an action describeSignInFailure has forbidden", () => {
  // The whole contract in one sweep: `retryable === false` must never yield a
  // retry, for any kind, in either flow.
  for (const kind of ALL_KINDS) {
    for (const flow of ["auto", "deviceCode"] as const) {
      const report = describeSignInFailure("staging", new AuthFlowError(kind, "x"));
      const actions = signInRecoveryActions({ retryable: report.retryable, kind, flow });
      if (!report.retryable) {
        assert.equal(
          actions.includes(SIGN_IN_RETRY),
          false,
          `${kind}/${flow} offers a retry the report forbids`,
        );
      }
      for (const action of actions) {
        assert.ok(
          [SIGN_IN_RETRY, SIGN_IN_DEVICE_CODE, SIGN_IN_EDIT_CLUSTER].includes(action),
          `${kind}/${flow} offers an action the editor has no binding for: ${action}`,
        );
      }
    }
  }
});

test("a silent failure is never given buttons to be silent with", () => {
  // `cancelled` is reported at level "silent" -- the operator already knows --
  // and it is nonetheless `retryable`, so this pins the pairing the editor
  // relies on: the level decides whether a toast appears at all, and these
  // actions only decorate one that does.
  const report = describeSignInFailure("staging", new AuthFlowError("cancelled", "x"));
  assert.equal(report.level, "silent");
  assert.equal(report.message, "");
});
