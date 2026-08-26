// The recovery key's one-time reveal, host side (znasllc-io/memql#4079).
//
// WHAT WAS MISSING, AND WHY IT STAYED MISSING. The install's `recoveryKey`
// step CLAIMS the cluster's break-glass key at install end -- claiming ROTATES
// and reveals the plaintext exactly once, into the envelope's
// `result.recoveryKey` -- and in the extension, nobody ever saw it. Two
// requirements collided: memql#3908's default-deny credential withholding (a
// GATE, tested) and the step description's "show it once" (PROSE, untested).
// The gate won silently. `recoveryKey` has head-noun `key` and an `mql_`
// value, so `redactResult` kept it out of the run log and `withholdResult`
// kept it out of the receipt -- both CORRECT, both about storage -- and no
// display surface was ever built, so default-deny quietly became "revealed to
// no one". Nothing could go red for it: the step's verify asserts
// `recoveryKeyState`, the state machine, not the experience. And the same
// script was complete in the terminal, where stdout IS the display
// (memql#3969 designed exactly that: `KEY=$(kubectl exec ...)`), so the lane
// CI exercises stayed green while the extension swallowed the envelope.
//
// THE LESSON, worth keeping where the seam lives: every reveal-once credential
// needs a NAMED display surface and a test that the surface received it. A
// redaction gate plus an untested promise equals a credential shown to no one.
//
// SO THIS IS A DISPLAY SEAM, NOT A STORAGE ONE, and it is NARROW on purpose:
// exactly one step id, exactly one field, exactly one value shape. A generic
// "unredacted results" channel is the hole memql#3908 closed, and this module
// must never widen into one -- the narrowness test in test/recoveryKey.test.ts
// fails the build if it does. The value read here goes to the done screen's
// in-memory state and nowhere else: never the run log, never the receipt,
// never any file. `redactResult` and `withholdResult` stay byte-for-byte as
// they are.
//
// `claim.ts`'s sibling in shape, like `enrolment.ts` before it: the plaintext
// already sits in the in-memory ExecutionReport (the executor withholds at the
// WRITE boundaries, not in memory), so surfacing it is a read, not a plumbing
// change. Deliberately free of `vscode` imports
// (cmd/memql-lsp/vscodeimportrule_test.go): the extraction is the part worth
// testing, and the webview panel supplies the rendering.
//
// Refs: #4079 #3969 #3970 #3908 #3958

import type { ExecutionReport, StepOutcome } from "./executor.js";

/** The graph step this module pairs with. */
export const RECOVERY_STEP_ID = "recoveryKey";

/** The envelope field the claimed plaintext arrives on. */
export const RECOVERY_RESULT_FIELD = "recoveryKey";

/**
 * The envelope field saying what became of the claim.
 *
 * `claimed`, `awaitingOwner`, `alreadyClaimed` or `revealLost` -- four
 * different facts the script keeps deliberately tellable apart (see
 * scripts/install/recovery-key.sh): "you were just handed a credential",
 * "there is nobody to hold one yet", "the credential exists and you already
 * have it", and "the credential was spent and nobody ever saw it".
 *
 * That fourth one exists because the third was doing duty for two facts
 * (memql#4628). A reveal that did not survive the journey out of the pod left
 * a cluster asserting an operator held a key that existed nowhere, and the
 * copy for `alreadyClaimed` told them to keep it. They are separate states
 * now because the remedies are opposite: `alreadyClaimed` means do nothing,
 * `revealLost` means rotate.
 *
 * `none` is this side's fifth: the step is not in the report, did not run, or
 * reported a state this module has no model of -- in every one of those cases
 * the done screen renders nothing rather than a guess.
 */
export const RECOVERY_STATE_FIELD = "recoveryKeyState";

export type RecoveryKeyState =
  | "claimed"
  | "awaitingOwner"
  | "alreadyClaimed"
  | "revealLost"
  | "none";

/**
 * Whether a value is a bare recovery key.
 *
 * The same anchored shape the capability script matches out of the pod output
 * (`mql_rec_` + 43 base64url chars), anchored at BOTH ends here: the display
 * is not a general stdout viewer, so a value that merely CONTAINS a key -- a
 * stray log line, a wrapped sentence -- renders nothing rather than something
 * that includes one.
 */
export function isRecoveryKey(candidate: string): boolean {
  return /^mql_rec_[A-Za-z0-9_-]{43}$/.test(candidate);
}

function recoveryOutcome(report: ExecutionReport): StepOutcome | undefined {
  return report.outcomes.find((o: StepOutcome) => o.id === RECOVERY_STEP_ID);
}

/**
 * What the claim reported, for the done screen's no-key renderings.
 *
 * Read off the step's own state field rather than inferred from the key's
 * presence, because the no-key states ask the operator for different things:
 * `alreadyClaimed` means "the value you stored earlier is still the live one",
 * `awaitingOwner` means "sign in first, then claim", `revealLost` means "the
 * key is spent and you must rotate".
 *
 * A FAILED OUTCOME IS READ, FOR EXACTLY ONE STATE (memql#4628). Every other
 * state requires `status === "ok"`, because a step that did not succeed has
 * not earned a claim about the cluster. `revealLost` is the exception by
 * construction: the script reports it through `cap_fail`, since a
 * break-glass credential that was spent and shown to nobody IS a failed
 * install step. Refusing to read it would drop the one message the operator
 * most needs and fall back to rendering nothing -- which is how this defect
 * stayed invisible in the first place. The narrowness is the safety: only
 * this literal, only from this step, and it reveals no value.
 */
export function recoveryKeyStateFrom(report: ExecutionReport): RecoveryKeyState {
  const outcome = recoveryOutcome(report);
  if (outcome === undefined || outcome.envelope === null) return "none";
  const state = outcome.envelope.result[RECOVERY_STATE_FIELD];
  if (state === "revealLost") return "revealLost";
  if (outcome.status !== "ok") return "none";
  if (state === "claimed" || state === "awaitingOwner" || state === "alreadyClaimed") return state;
  return "none";
}

/**
 * The key this run claimed, for one-time display -- or "".
 *
 * FOUR GATES, each refusing with "" rather than a guess: the outcome must be
 * THE recovery step's (id-pinned -- a different step carrying a field named
 * `recoveryKey` reaches no display through this), it must have succeeded, its
 * own state must say `claimed` (the one state that reveals), and the value
 * must be a bare key by shape. "" tells the caller to render the state line
 * instead, which `recoveryKeyStateFrom` decides.
 */
export function revealedRecoveryKeyFrom(report: ExecutionReport): string {
  if (recoveryKeyStateFrom(report) !== "claimed") return "";
  const outcome = recoveryOutcome(report);
  const value = outcome?.envelope?.result[RECOVERY_RESULT_FIELD];
  if (typeof value !== "string" || !isRecoveryKey(value)) return "";
  return value;
}
