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

// ---------------------------------------------------------------------------
// the reveal that did not survive the journey (memql#4628)
// ---------------------------------------------------------------------------
//
// THE DEFECT THESE PIN. `recovery-key claim` used to stamp the row claimed
// before its plaintext left the process, so a perturbed capture spent the
// cluster's break-glass credential and showed it to nobody. Every later run
// then read the stamp, reported `alreadyClaimed`, and told the operator the
// key they held was still live. They held none.
//
// The script now reports that case as its own state, through `cap_fail` --
// a credential spent and delivered to nobody IS a failed install step. So this
// side has to read a state off a FAILED outcome, which it does for this one
// literal only.

test("recoveryKeyStateFrom reads revealLost from a failed outcome", () => {
  const rep = report(
    outcome({
      status: "failed",
      exitCode: 5,
      envelope: {
        ok: false,
        capability: "install.recoveryKey",
        changed: true,
        result: { recoveryKey: "", recoveryKeyState: "revealLost", ownerClaimed: true },
        error: { code: 5, message: "the recovery key was revealed but did not survive the capture" },
      },
    }),
  );
  assert.equal(
    recoveryKeyStateFrom(rep),
    "revealLost",
    "a lost reveal fell back to `none`, so the done screen renders nothing and the operator is " +
      "never told the cluster has no break-glass credential anyone holds",
  );
});

test("revealLost carries no plaintext to display", () => {
  const rep = report(
    outcome({
      status: "failed",
      exitCode: 5,
      envelope: {
        ok: false,
        capability: "install.recoveryKey",
        changed: true,
        // Even if a value were present, this state must never display one:
        // its entire content is that the value is gone.
        result: { recoveryKey: KEY, recoveryKeyState: "revealLost", ownerClaimed: true },
        error: { code: 5, message: "spent" },
      },
    }),
  );
  assert.equal(revealedRecoveryKeyFrom(rep), "");
});

// The narrowness is the safety. Reading state off a failed outcome is an
// exception made for exactly one literal; every other state still has to have
// succeeded to be believed.
test("a failed outcome reporting any other state is still none", () => {
  for (const state of ["claimed", "alreadyClaimed", "awaitingOwner"]) {
    const rep = report(
      outcome({
        status: "failed",
        exitCode: 5,
        envelope: {
          ok: false,
          capability: "install.recoveryKey",
          changed: false,
          result: { recoveryKey: KEY, recoveryKeyState: state, ownerClaimed: true },
          error: { code: 5, message: "boom" },
        },
      }),
    );
    assert.equal(
      recoveryKeyStateFrom(rep),
      "none",
      `a failed step claiming ${state} was believed; a step that did not succeed has not earned a ` +
        "claim about the cluster",
    );
  }
});
