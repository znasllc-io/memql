// What a failed sign-in leaves one click away (memql#4621).
//
// WHAT WAS WRONG. `describeSignInFailure` computes three things -- a level, a
// sentence, and `retryable`, documented as "A UI may offer a retry affordance on
// true; it must not on false" (src/auth/signin.ts). The editor read the first
// two and threw the third away: every failure toast was raised with NO actions.
// So after the browser flow ran out its ten-minute deadline the operator read a
// sentence naming `MemQL: Sign In With a Device Code` and had to open the
// palette and type it -- the command being named because it is the recovery, in
// a toast that could simply have offered it.
//
// A command name in prose is a dead end with extra steps. The whole of this
// epic is that a screen which knows the next step should carry it.
//
// WHY THE DECISION LIVES HERE AND NOT AT THE TOAST. `src/extension.ts` imports
// `vscode`, so nothing under `node --test` can reach a decision written into it
// -- which is how a computed, documented field came to be discarded at exactly
// one call site with nothing to notice. The mapping below is the whole of the
// decision; the extension binds each returned label to a command and does no
// branching of its own.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go),
// and its two imports are TYPES, erased at compile time -- so this adds no
// runtime edge from clusters/ to auth/.

import type { AuthFlowErrorKind } from "../auth/errors.js";
import type { SignInFlow } from "../auth/signin.js";

/** Run the same sign-in again. */
export const SIGN_IN_RETRY = "Try again";
/** Switch to the device grant, for a host the callback can never reach. */
export const SIGN_IN_DEVICE_CODE = "Use a device code";
/** Open the cluster's fields, because nothing named an identity service. */
export const SIGN_IN_EDIT_CLUSTER = "Edit cluster";

export interface SignInFailureFacts {
  /**
   * `describeSignInFailure`'s verdict on whether the SAME command run again
   * could work. Not a severity: `stateMismatch` is the most serious kind here
   * and is false precisely because a forged callback wants a human looking at
   * it, not a second attempt one click away.
   */
  retryable: boolean;
  /**
   * The failure's kind, or undefined when the rejection was not an
   * `AuthFlowError` at all.
   *
   * Undefined is the honest reading of "something threw and we do not know
   * what", and it earns no actions: `describeSignInFailure` already reports
   * such a rejection as not retryable, and inventing a button for a failure
   * with no taxonomy is how a retry loop comes to retry a refusal.
   */
  kind?: AuthFlowErrorKind;
  /** Which grant just failed. */
  flow: SignInFlow;
}

/**
 * The actions to raise the failure toast with, in the order they should appear.
 *
 * EMPTY IS A REAL ANSWER and the common one. `browserUnavailable`,
 * `stateMismatch` and an unrecognised rejection all get nothing, and that is
 * the contract `retryable` states rather than an omission -- a button whose
 * only outcome is the same failure teaches an operator to stop reading these.
 *
 * `misconfigured` RETURNS EARLY WITH ONE ACTION. The cluster names no identity
 * service, so there is nowhere to register or authorize and nothing was
 * attempted; retrying cannot change that, and the only thing that can is
 * editing the entry. Offering both would put the useless button first.
 *
 * THE DEVICE CODE IS OFFERED ON `timeout` AND ONLY THERE. A timeout means the
 * listener bound and the browser opened and no request ever arrived, which is
 * exactly the shape of a host the callback cannot reach -- the case the device
 * grant exists for, and the case the advice text already names. It is NOT
 * offered when the device grant is what just timed out: the same button, having
 * just failed, is the one thing that cannot be the remedy. Every other kind
 * either has its own answer or has already fallen back on its own
 * (`browserUnavailable` and `bindFailed` are what `signInWithDeviceCodeFallback`
 * triggers on, so by the time a failure carrying one of those reaches a toast,
 * the device grant has been tried).
 */
export function signInRecoveryActions(facts: SignInFailureFacts): string[] {
  if (facts.kind === "misconfigured") return [SIGN_IN_EDIT_CLUSTER];
  const actions: string[] = [];
  if (facts.retryable) actions.push(SIGN_IN_RETRY);
  if (facts.kind === "timeout" && facts.flow !== "deviceCode") {
    actions.push(SIGN_IN_DEVICE_CODE);
  }
  return actions;
}
