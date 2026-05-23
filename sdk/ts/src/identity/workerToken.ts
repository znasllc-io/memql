// Worker tokens are bearer credentials issued to a user-owned
// machine running memql-cockpit-worker. The plain token is
// returned ONCE in the create reply (shown in the AddWorkerModal)
// and never persisted server-side -- only the SHA-256 hash lands
// on a v1:identity:identity row of identityType="worker_token".

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface CreateWorkerTokenArgs {
  name: string;
  expiresAt?: string; // ISO8601; empty/absent = no auto-expiry
  ownerUserId?: string; // admin override; empty = caller
  signal?: AbortSignal;
}

export interface CreateWorkerTokenResult {
  success: boolean;
  // "mql_wkr_<43 base64url>" -- shown to the user once; never
  // returned again. Caller MUST capture it on this call.
  plainToken: string;
  identityId: string;
  ownerUserId: string;
  errorCode: string;
  errorMessage: string;
}

// createWorkerToken mints a fresh worker token. The plain bearer is
// returned in plainToken exactly once -- the SDK does not retain it.
export async function createWorkerToken(
  dispatcher: Dispatcher,
  args: CreateWorkerTokenArgs,
): Promise<CreateWorkerTokenResult> {
  if (!dispatcher) throw new Error("createWorkerToken: dispatcher is required");
  if (!args.name) throw new Error("createWorkerToken: name is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      createWorkerToken: {
        requestId,
        name: args.name,
        ...(args.expiresAt ? { expiresAt: args.expiresAt } : {}),
        ...(args.ownerUserId ? { ownerUserId: args.ownerUserId } : {}),
      },
    },
    args.signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`createWorkerToken: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "createWorkerTokenResult") {
    throw new Error("createWorkerToken: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    plainToken: payload.value.plainToken ?? "",
    identityId: payload.value.identityId ?? "",
    ownerUserId: payload.value.ownerUserId ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export interface RevokeWorkerTokenResult {
  success: boolean;
  errorCode: string;
  errorMessage: string;
}

// revokeWorkerToken flips active=false on the worker_token identity
// row. The interceptor refuses any future connect attempt with this
// token; in-flight stream lifetimes are untouched.
export async function revokeWorkerToken(
  dispatcher: Dispatcher,
  identityId: string,
  signal?: AbortSignal,
): Promise<RevokeWorkerTokenResult> {
  if (!dispatcher) throw new Error("revokeWorkerToken: dispatcher is required");
  if (!identityId) throw new Error("revokeWorkerToken: identityId is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    { revokeWorkerToken: { requestId, identityId } },
    signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`revokeWorkerToken: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "revokeWorkerTokenResult") {
    throw new Error("revokeWorkerToken: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}
