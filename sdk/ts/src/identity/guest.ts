// Guest-invite lifecycle wrappers for the 5 client wire messages:
// sendGuestInvite, resolveGuestInvite, joinSpaceAsGuest,
// cancelGuestInvite, resendGuestInviteEmail.
//
// Errors land in two shapes:
//   1. Transport / unknown envelope -> thrown Error.
//   2. Typed application failures the server returns with
//      success=false and a machine-readable errorCode (e.g.
//      "invalid_email", "invitation_expired"). These are NOT
//      thrown -- they ride the typed result so the caller can
//      branch on errorCode without a try/catch. QueryError on
//      the dispatcher path is still thrown because the engine
//      treats it as a wire-level rejection.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface SendGuestInviteArgs {
  partitionId: string;
  spaceName: string;
  inviterName: string;
  email: string;
  joinUrlBase: string;
  guestName?: string;
  expiresInMinutes?: number;
  signal?: AbortSignal;
}

export interface SendGuestInviteResult {
  success: boolean;
  invitationId: string;
  errorCode: string;
  errorMessage: string;
}

// sendGuestInvite mints a pending v1:identity:invitation row and
// emails the guest. Authenticated space-owner op.
export async function sendGuestInvite(
  dispatcher: Dispatcher,
  args: SendGuestInviteArgs,
): Promise<SendGuestInviteResult> {
  if (!dispatcher) throw new Error("sendGuestInvite: dispatcher is required");
  if (!args.partitionId) throw new Error("sendGuestInvite: partitionId is required");
  if (!args.email) throw new Error("sendGuestInvite: email is required");
  if (!args.joinUrlBase) throw new Error("sendGuestInvite: joinUrlBase is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      sendGuestInvite: {
        requestId,
        partitionId: args.partitionId,
        spaceName: args.spaceName ?? "",
        inviterName: args.inviterName ?? "",
        email: args.email,
        joinUrlBase: args.joinUrlBase,
        ...(args.guestName ? { guestName: args.guestName } : {}),
        ...(args.expiresInMinutes != null ? { expiresInMinutes: args.expiresInMinutes } : {}),
      },
    },
    args.signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`sendGuestInvite: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "sendGuestInviteResult") {
    throw new Error("sendGuestInvite: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    invitationId: payload.value.invitationId ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export type GuestInviteStatus =
  | "ok"
  | "invalid"
  | "expired"
  | "already_accepted"
  | "cancelled"
  | (string & {});

export interface ResolveGuestInviteResult {
  status: GuestInviteStatus;
  invitationId: string;
  partitionId: string;
  spaceName: string;
  inviterName: string;
  inviteeEmail: string;
  inviteeName: string;
  expiresAt: string; // ISO8601
  errorMessage: string;
}

// resolveGuestInvite is the UNAUTHENTICATED lookup the /join/<token>
// landing page uses to render space + inviter metadata before the
// guest commits to a display name. status carries the validation
// outcome; the SDK does not promote non-"ok" statuses to thrown
// errors because the rendering code wants to branch on them.
export async function resolveGuestInvite(
  dispatcher: Dispatcher,
  token: string,
  signal?: AbortSignal,
): Promise<ResolveGuestInviteResult> {
  if (!dispatcher) throw new Error("resolveGuestInvite: dispatcher is required");
  if (!token) throw new Error("resolveGuestInvite: token is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    { resolveGuestInvite: { requestId, token } },
    signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`resolveGuestInvite: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "resolveGuestInviteResult") {
    throw new Error("resolveGuestInvite: unexpected reply envelope");
  }
  return {
    status: (payload.value.status ?? "invalid") as GuestInviteStatus,
    invitationId: payload.value.invitationId ?? "",
    partitionId: payload.value.partitionId ?? "",
    spaceName: payload.value.spaceName ?? "",
    inviterName: payload.value.inviterName ?? "",
    inviteeEmail: payload.value.inviteeEmail ?? "",
    inviteeName: payload.value.inviteeName ?? "",
    expiresAt: payload.value.expiresAt ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export interface JoinSpaceAsGuestArgs {
  participantId: string;
  displayName: string;
  signal?: AbortSignal;
}

export interface JoinSpaceAsGuestResult {
  success: boolean;
  participantId: string;
  partitionId: string;
  errorCode: string;
  errorMessage: string;
}

// joinSpaceAsGuest is the atomic accept-and-create op fired from
// the /join/<token> page once the guest confirms their display
// name. The stream interceptor has already validated
// "Authorization: Guest <token>" and stashed the invitation + space
// in the request claims; the server resolves partitionId from those
// claims and reflects it on the reply.
export async function joinSpaceAsGuest(
  dispatcher: Dispatcher,
  args: JoinSpaceAsGuestArgs,
): Promise<JoinSpaceAsGuestResult> {
  if (!dispatcher) throw new Error("joinSpaceAsGuest: dispatcher is required");
  if (!args.participantId) throw new Error("joinSpaceAsGuest: participantId is required");
  if (!args.displayName) throw new Error("joinSpaceAsGuest: displayName is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      joinSpaceAsGuest: {
        requestId,
        participantId: args.participantId,
        displayName: args.displayName,
      },
    },
    args.signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`joinSpaceAsGuest: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "joinSpaceAsGuestResult") {
    throw new Error("joinSpaceAsGuest: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    participantId: payload.value.participantId ?? "",
    partitionId: payload.value.partitionId ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export interface CancelGuestInviteResult {
  success: boolean;
  invitationId: string;
  errorCode: string;
  errorMessage: string;
}

// cancelGuestInvite revokes a still-pending guest invitation. No-op
// (success=true) if the invitation is already accepted or kicked.
export async function cancelGuestInvite(
  dispatcher: Dispatcher,
  invitationId: string,
  signal?: AbortSignal,
): Promise<CancelGuestInviteResult> {
  if (!dispatcher) throw new Error("cancelGuestInvite: dispatcher is required");
  if (!invitationId) throw new Error("cancelGuestInvite: invitationId is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    { cancelGuestInvite: { requestId, invitationId } },
    signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`cancelGuestInvite: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "cancelGuestInviteResult") {
    throw new Error("cancelGuestInvite: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    invitationId: payload.value.invitationId ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

export interface ResendGuestInviteEmailArgs {
  invitationId: string;
  joinUrlBase: string;
  signal?: AbortSignal;
}

export interface ResendGuestInviteEmailResult {
  success: boolean;
  invitationId: string;
  errorCode: string;
  errorMessage: string;
}

// resendGuestInviteEmail re-sends the invitation email for an
// existing guest invitation without creating a new record.
export async function resendGuestInviteEmail(
  dispatcher: Dispatcher,
  args: ResendGuestInviteEmailArgs,
): Promise<ResendGuestInviteEmailResult> {
  if (!dispatcher) throw new Error("resendGuestInviteEmail: dispatcher is required");
  if (!args.invitationId) throw new Error("resendGuestInviteEmail: invitationId is required");
  if (!args.joinUrlBase) throw new Error("resendGuestInviteEmail: joinUrlBase is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      resendGuestInviteEmail: {
        requestId,
        invitationId: args.invitationId,
        joinUrlBase: args.joinUrlBase,
      },
    },
    args.signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`resendGuestInviteEmail: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "resendGuestInviteEmailResult") {
    throw new Error("resendGuestInviteEmail: unexpected reply envelope");
  }
  return {
    success: payload.value.success === true,
    invitationId: payload.value.invitationId ?? "",
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}
