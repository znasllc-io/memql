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
