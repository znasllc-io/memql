// Badges are registered shared-terminal operator credentials
// (memql#2513): a badge/device id maps to a v1:identity:user so the
// identity service's POST /auth/badge/grant exchange can mint
// short-lived operator grant tokens. Only the SHA-256 hash of the
// badge id is persisted (identityType="badge"); the plaintext id lives
// on the physical card/device.
//
// These helpers cover the REGISTRATION lifecycle (create / revoke) on
// MemqlService.Stream. The GRANT exchange itself is an identity-HTTP
// call (POST /auth/badge/grant), not a stream message -- a kiosk hits
// that endpoint and then rotateAuth()s the returned token onto its
// live connection.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface CreateBadgeArgs {
  // Plaintext badge/device identifier. Hashed server-side; never
  // persisted or echoed back.
  badgeId: string;
  // Human label ("Alice -- floor badge").
  label: string;
  // Admin override; empty = the caller. Registering a badge for
  // another operator (the common shared-terminal flow) requires admin.
  ownerUserId?: string;
  signal?: AbortSignal;
}

export interface CreateBadgeResult {
  success: boolean;
  identityId: string;
  ownerUserId: string;
  errorCode: string;
  errorMessage: string;
}

// createBadge registers a badge. Unlike worker tokens there is no
// plaintext to capture -- the badge id is the caller-supplied physical
// identifier, and only its hash is stored.
export async function createBadge(
  dispatcher: Dispatcher,
  args: CreateBadgeArgs,
): Promise<CreateBadgeResult> {
  if (!dispatcher) throw new Error("createBadge: dispatcher is required");
  if (!args.badgeId) throw new Error("createBadge: badgeId is required");
  if (!args.label) throw new Error("createBadge: label is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      createBadge: {
        requestId,
        badgeId: args.badgeId,
        label: args.label,
        ...(args.ownerUserId ? { ownerUserId: args.ownerUserId } : {}),
      },
    },
    args.signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`createBadge: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "createBadgeResult") {
    throw new Error("createBadge: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    identityId: payload.value.identityId ?? "",
    ownerUserId: payload.value.ownerUserId ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export interface RevokeBadgeResult {
  success: boolean;
  errorCode: string;
  errorMessage: string;
}

// revokeBadge flips active=false on the badge identity row. Future
// grant exchanges are blocked immediately; outstanding short-TTL grant
// tokens expire on their own.
export async function revokeBadge(
  dispatcher: Dispatcher,
  identityId: string,
  signal?: AbortSignal,
): Promise<RevokeBadgeResult> {
  if (!dispatcher) throw new Error("revokeBadge: dispatcher is required");
  if (!identityId) throw new Error("revokeBadge: identityId is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    { revokeBadge: { requestId, identityId } },
    signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`revokeBadge: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "revokeBadgeResult") {
    throw new Error("revokeBadge: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}
