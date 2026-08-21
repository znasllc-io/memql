// Account tokens (memql#3322): a credential an operator mints against a
// managed customer account. The plain token is returned ONCE in the mint
// reply and never persisted server-side -- only the SHA-256 hash lands on
// a v1:identity:identity row of identityType="account_token". Same custody
// rule, same module shape, as workerToken.ts next door.
//
// This module is the typed home the portal's accounts/wire.ts pointed at
// while it crossed the wire union with a documented cast (memql#4234): the
// envelopes are modelled in wire.ts now, so no consumer composes or casts a
// raw payload.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface MintAccountTokenArgs {
  accountId: string;
  label: string;
  expiresAt?: string; // ISO8601; empty/absent = no auto-expiry
  signal?: AbortSignal;
}

export interface AccountTokenMintResult {
  success: boolean;
  // "mql_acct_<43 base64url>". Present only on the mint reply, only once.
  // Never write this to storage, a URL, or anything that outlives the
  // component that received it.
  plainToken: string;
  identityId: string;
  accountId: string;
  // The credential's authenticated SUBJECT: the operator user. Never the
  // account -- nothing authenticates as an account. The server sends it so a
  // client cannot render otherwise without contradicting a field it was
  // handed.
  subjectUserId: string;
  auditEventId: string;
  errorCode: string;
  errorMessage: string;
}

// mintAccountToken mints a fresh account token. The plain bearer is
// returned in plainToken exactly once -- the SDK does not retain it.
export async function mintAccountToken(
  dispatcher: Dispatcher,
  args: MintAccountTokenArgs,
): Promise<AccountTokenMintResult> {
  if (!dispatcher) throw new Error("mintAccountToken: dispatcher is required");
  if (!args.accountId) throw new Error("mintAccountToken: accountId is required");
  if (!args.label) throw new Error("mintAccountToken: label is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      createAccountToken: {
        requestId,
        accountId: args.accountId,
        label: args.label,
        ...(args.expiresAt ? { expiresAt: args.expiresAt } : {}),
      },
    },
    args.signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`mintAccountToken: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "createAccountTokenResult") {
    throw new Error("mintAccountToken: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    plainToken: payload.value.plainToken ?? "",
    identityId: payload.value.identityId ?? "",
    accountId: payload.value.accountId ?? "",
    subjectUserId: payload.value.subjectUserId ?? "",
    auditEventId: payload.value.auditEventId ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export interface AccountTokenRevokeResult {
  success: boolean;
  auditEventId: string;
  errorCode: string;
  errorMessage: string;
}

// revokeAccountToken flips active=false on the account_token identity row.
// Audited; the audit id comes back on the reply.
export async function revokeAccountToken(
  dispatcher: Dispatcher,
  identityId: string,
  signal?: AbortSignal,
): Promise<AccountTokenRevokeResult> {
  if (!dispatcher) throw new Error("revokeAccountToken: dispatcher is required");
  if (!identityId) throw new Error("revokeAccountToken: identityId is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    { revokeAccountToken: { requestId, identityId } },
    signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`revokeAccountToken: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "revokeAccountTokenResult") {
    throw new Error("revokeAccountToken: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    auditEventId: payload.value.auditEventId ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}
