// The recovery key's one-time reveal, host side (znasllc-io/memql#4079).
//
// THE DEFECT THIS PINS. The install's `recoveryKey` step CLAIMS the cluster's
// break-glass key -- claiming rotates and reveals the plaintext exactly once,
// into the envelope's `result.recoveryKey` -- and in the extension NOBODY EVER SAW
// IT. Two requirements collided: memql#3908's default-deny credential
// withholding (a GATE, tested) and the step description's "show it once"
// (PROSE, untested). The gate won silently: the value was withheld from the
// run log and the receipt, both correctly, and no display surface was ever
// built, so default-deny quietly became "revealed to no one". The step's
// verify asserts `recoveryKeyState` -- the state machine, not the experience --
// so nothing could go red for it.
//
// The lesson these tests encode: every reveal-once credential needs a NAMED
// display surface and a test that the surface received it. This file tests the
// seam (the report -> display extraction); addClusterPanel.test.ts tests that
// the done screen -- the surface -- actually received the value.

import assert from "node:assert/strict";
import { test } from "node:test";

import type { ExecutionReport, StepOutcome } from "../src/install/executor.js";
import {
  RECOVERY_STEP_ID,
  recoveryKeyStateFrom,
  revealedRecoveryKeyFrom,
} from "../src/install/recoveryKey.js";

const KEY = `mql_rec_${"A".repeat(43)}`;

function outcome(overrides: Partial<StepOutcome> = {}): StepOutcome {
  return {
    id: RECOVERY_STEP_ID,
    script: "install.recoveryKey",
    status: "ok",
    exitCode: 0,
    envelope: {
      ok: true,
      capability: "install.recoveryKey",
      changed: true,
      result: { recoveryKey: KEY, recoveryKeyState: "claimed", ownerClaimed: true },
      error: null,
    },
    verified: true,
    preExisting: false,
    params: {},
    startedAt: "",
    finishedAt: "",
    ...overrides,
  };
}

function report(...outcomes: StepOutcome[]): ExecutionReport {
  return { graph: "install", ok: true, waves: [], outcomes };
}

// ---------------------------------------------------------------------------
// the reveal
// ---------------------------------------------------------------------------

test("a key the step claimed is lifted off the report for display", () => {
  assert.equal(revealedRecoveryKeyFrom(report(outcome())), KEY);
});

test("the seam is pinned to the STEP, not to a field name", () => {
  // The narrowness requirement. A generic "unredacted results" channel is the
  // hole memql#3908 closed; this seam must read exactly one field off exactly
  // one step. A different step whose envelope happens to carry a field named
  // `recoveryKey` -- a future script, a compromised one, a copy-paste -- must
  // reach no display through it.
  const impostor = outcome({ id: "frontDoor", script: "install.frontDoor" });
  assert.equal(revealedRecoveryKeyFrom(report(impostor)), "");

  // And the genuine step beside it still answers.
  assert.equal(revealedRecoveryKeyFrom(report(impostor, outcome())), KEY);
});

test("a step that did not succeed reveals nothing, whatever its envelope says", () => {
  for (const status of ["failed", "skipped", "preserved"] as const) {
    assert.equal(
      revealedRecoveryKeyFrom(report(outcome({ status }))),
      "",
      `status ${status} must not reveal`,
    );
  }
  assert.equal(revealedRecoveryKeyFrom(report(outcome({ envelope: null }))), "");
});

test("only the credential's own shape is displayed", () => {
  // The display is not a general stdout viewer. The script matches the key out
  // of pod output with an anchored mql_rec_ pattern before it ever reaches the
  // envelope; the display holds the same line, so a value that is NOT a bare
  // recovery key -- a stray log line, a wrapped sentence, an empty string --
  // renders nothing rather than something that merely contains one.
  const shapes = ["", "not-a-key", `the key is ${KEY}`, "mql_rec_tooshort", `${KEY}extra`];
  for (const value of shapes) {
    const o = outcome();
    o.envelope!.result = { recoveryKey: value, recoveryKeyState: "claimed" };
    assert.equal(revealedRecoveryKeyFrom(report(o)), "", `value ${JSON.stringify(value)}`);
  }
});

test("a state that is not `claimed` reveals nothing even if a value is present", () => {
  const o = outcome();
  o.envelope!.result = { recoveryKey: KEY, recoveryKeyState: "alreadyClaimed" };
  assert.equal(revealedRecoveryKeyFrom(report(o)), "");
});

// ---------------------------------------------------------------------------
// the state, for the block that renders when there is no key to show
// ---------------------------------------------------------------------------

test("the script's three states and absence each read distinctly", () => {
  const withState = (state: string): ExecutionReport => {
    const o = outcome();
    o.envelope!.result = { recoveryKey: "", recoveryKeyState: state };
    return report(o);
  };
  assert.equal(recoveryKeyStateFrom(withState("claimed")), "claimed");
  assert.equal(recoveryKeyStateFrom(withState("awaitingOwner")), "awaitingOwner");
  assert.equal(recoveryKeyStateFrom(withState("alreadyClaimed")), "alreadyClaimed");
  // A state this side has no model of renders nothing rather than a guess.
  assert.equal(recoveryKeyStateFrom(withState("somethingNew")), "none");
});

test("a report without the step, or with a failed one, carries no state to render", () => {
  assert.equal(recoveryKeyStateFrom(report()), "none");
  assert.equal(recoveryKeyStateFrom(report(outcome({ status: "failed" }))), "none");
});
