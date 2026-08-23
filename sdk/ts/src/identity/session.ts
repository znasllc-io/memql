// Per-device and cross-device session revocation. Both ops read
// the caller's bearer token out of the gRPC metadata server-side
// (the SDK doesn't pass the token in the payload) and flip the
// matching v1:identity:authSession row(s) to revoked.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface RevokeCurrentSessionResult {
  success: boolean;
  sessionId: string;
  errorCode: string;
  errorMessage: string;
}

// revokeCurrentSession revokes the single session tied to the
// caller's current bearer token. Used by per-device "Sign out".
// Other sessions for the same user are untouched.
export async function revokeCurrentSession(
  dispatcher: Dispatcher,
  signal?: AbortSignal,
): Promise<RevokeCurrentSessionResult> {
  if (!dispatcher) throw new Error("revokeCurrentSession: dispatcher is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    { revokeCurrentSession: { requestId } },
    signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`revokeCurrentSession: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "revokeCurrentSessionResult") {
    throw new Error("revokeCurrentSession: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    sessionId: payload.value.sessionId ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export interface RevokeAllSessionsResult {
  success: boolean;
  revokedCount: number;
  errorCode: string;
  errorMessage: string;
}

// revokeAllSessions revokes every session owned by the caller --
// the "Sign out of all sessions" button. revokedCount counts only
// rows newly transitioning to revoked; already-revoked or
// already-expired rows are skipped.
export async function revokeAllSessions(
  dispatcher: Dispatcher,
  signal?: AbortSignal,
): Promise<RevokeAllSessionsResult> {
  if (!dispatcher) throw new Error("revokeAllSessions: dispatcher is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    { revokeAllSessions: { requestId } },
    signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`revokeAllSessions: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "revokeAllSessionsResult") {
    throw new Error("revokeAllSessions: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    revokedCount: typeof payload.value.revokedCount === "number" ? payload.value.revokedCount : 0,
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export interface RevokeSessionResult {
  success: boolean;
  sessionId: string;
  // True when the row revoked is the one backing THIS connection. A caller
  // that gets this back should clear its local credential and land on
  // sign-in rather than re-reading a list it can no longer read.
  wasCurrent: boolean;
  errorCode: string;
  errorMessage: string;
}

// revokeSession revokes ONE named session of the caller's own -- what a
// sessions list needs and the two calls above cannot express: "current" is
// the row you are sitting in and "all" takes you out with it.
//
// sessionId must come from the caller's own authSessionsForSelf read. The
// server resolves it against that same set before writing, so an id from
// anywhere else comes back "not_found" -- which is also the answer for an id
// that does not exist, deliberately: distinguishing them would tell a caller
// whether any given session id is real.
export async function revokeSession(
  dispatcher: Dispatcher,
  sessionId: string,
  signal?: AbortSignal,
): Promise<RevokeSessionResult> {
  if (!dispatcher) throw new Error("revokeSession: dispatcher is required");
  if (!sessionId) throw new Error("revokeSession: sessionId is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait({ revokeSession: { requestId, sessionId } }, signal);
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`revokeSession: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "revokeSessionResult") {
    throw new Error("revokeSession: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    sessionId: payload.value.sessionId ?? "",
    wasCurrent: payload.value.wasCurrent === true,
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export type SignInPolicy = "any" | "passkey_only";

export interface SetSignInPolicyResult {
  success: boolean;
  // The policy in force AFTER the call -- on refusals too, so a caller
  // re-renders the truth rather than the state it optimistically flipped to.
  // Empty when the server could not read it back.
  policy: string;
  // "" | "invalid" | "no_passkey" | "precondition_unknown" | "not_found"
  // | "unauthenticated" | "unavailable".
  //
  // no_passkey and precondition_unknown are BOTH preconditions and they mean
  // different things: the first is a fact about the account (enrol a passkey),
  // the second a fact about the moment (try again). errorMessage carries the
  // sentence to show.
  errorCode: string;
  errorMessage: string;
}

// setSignInPolicy sets the CALLER'S OWN sign-in policy. There is no user id:
// the account written is the caller's, and the absence of a target is the
// authorization.
//
// passkey_only disables sign-in LINKS for the account. The server refuses it
// when no active passkey is enrolled, and refuses it too when it cannot READ
// the passkey list -- a blip and "no passkeys" must not reach the same
// decision when the difference is a lockout.
export async function setSignInPolicy(
  dispatcher: Dispatcher,
  policy: SignInPolicy,
  signal?: AbortSignal,
): Promise<SetSignInPolicyResult> {
  if (!dispatcher) throw new Error("setSignInPolicy: dispatcher is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait({ setSignInPolicy: { requestId, policy } }, signal);
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`setSignInPolicy: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "setSignInPolicyResult") {
    throw new Error("setSignInPolicy: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    policy: payload.value.policy ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}
