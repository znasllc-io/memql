// The identity-administration surface (memql#3324): the writes the
// server-rendered /admin/* console owned, reachable from a browser.
//
// ===========================================================================
// AUTHORIZATION IS SERVER-SIDE, ALWAYS. NOTHING HERE CHECKS A ROLE.
// ===========================================================================
// Every method below is refused by the engine for a caller below OWNER or
// ADMIN, in component/identity/adminops, against the role the stream
// interceptor verified -- not against anything this file or its callers
// believe. A refusal arrives as an IdentityAdminError with
// CODE_PERMISSION_DENIED and carries the id of the `admin_auth_forbidden`
// audit event the refusal wrote.
//
// A console MAY hide an action its operator cannot take; that is presentation,
// and the honest kind, because showing a button that always fails teaches
// nobody who can. It is not a control, and editing a boolean in a browser
// changes nothing about the answer.
//
// EVERY CALL IS AUDITED, including the ones that fail. `auditEventId` comes
// back on success and refusal alike; surface it to the operator and quote it
// in support threads, exactly as the deploy console's ActionResult does.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";
import type {
  IdentityAdminClusterSettingsPayload,
  IdentityAdminRequestPayload,
  IdentityAdminResultPayload,
  IdentityAdminUserProfilePayload,
} from "../client/wire.js";

/** Canonical gRPC codes this surface returns. */
const GRPC_CODE_NAMES: Record<number, string> = {
  0: "OK",
  3: "INVALID_ARGUMENT",
  5: "NOT_FOUND",
  7: "PERMISSION_DENIED",
  12: "UNIMPLEMENTED",
  13: "INTERNAL",
  16: "UNAUTHENTICATED",
};

/** PERMISSION_DENIED -- the caller is below owner/admin. */
export const CODE_PERMISSION_DENIED = 7;
/** UNAUTHENTICATED -- the stream carries no resolvable actor. */
export const CODE_UNAUTHENTICATED = 16;
/** NOT_FOUND -- no such user, token, or node token. */
export const CODE_NOT_FOUND = 5;
/** INVALID_ARGUMENT -- the request was understood and is not valid. */
export const CODE_INVALID_ARGUMENT = 3;
/** UNIMPLEMENTED -- this node has no identity-admin service wired. */
export const CODE_UNIMPLEMENTED = 12;

/**
 * A refused or failed identity-admin call.
 *
 * `code` lets a console tell "you are not allowed" (CODE_PERMISSION_DENIED)
 * apart from "that row is gone" (CODE_NOT_FOUND) and from a genuine failure,
 * without string-matching a message. `auditEventId` is the event the attempt
 * wrote -- a denial is audited, so this is populated on a refusal.
 */
export class IdentityAdminError extends Error {
  readonly code: number;
  readonly codeName: string;
  readonly auditEventId: string;

  constructor(verb: string, code: number, message: string, auditEventId: string) {
    const name = GRPC_CODE_NAMES[code] ?? String(code);
    super(`identity admin: ${verb}: ${name}: ${message || "(no message)"}`);
    this.name = "IdentityAdminError";
    this.code = code;
    this.codeName = name;
    this.auditEventId = auditEventId;
  }

  /** True when the refusal was the role gate, not a failure. */
  get isPermissionDenied(): boolean {
    return this.code === CODE_PERMISSION_DENIED;
  }
}

/** What a successful write reports back. */
export interface AdminWriteResult {
  /** One line describing the change, for a status line. */
  message: string;
  /** The v1:identity:auditEvent this write recorded. */
  auditEventId: string;
}

/**
 * What `issueEnrolmentLink` reports back.
 *
 * `url` is a CREDENTIAL. It is the only value this surface returns that is
 * one, it exists nowhere else (the server persisted only its SHA-256 hash),
 * and no later call can retrieve it. Show it once, let the operator copy it,
 * and hold it nowhere else -- not in storage, not in a URL of your own, not in
 * a row.
 */
export interface EnrolmentLinkResult extends AdminWriteResult {
  url: string;
}

export interface IdentityAdminCallOptions {
  signal?: AbortSignal;
}

/** The directory fields an admin may set on a person. */
export type UserProfileEdit = IdentityAdminUserProfilePayload;

/** The editable slice of the cluster's runtime settings. */
export type ClusterSettingsEdit = IdentityAdminClusterSettingsPayload;

/**
 * A thin typed client over the bridged identity-admin surface.
 *
 * Construct it from a Dispatcher (the one Connection exposes) and call the
 * method you want; each sends a single IdentityAdminMsg and awaits its
 * correlated IdentityAdminResult.
 *
 * NOT here, deliberately: the READS. People, audit events, tokens and cluster
 * settings are ordinary rows behind ordinary named queries that carry their
 * own `requiresOwnerOrAdmin` conjunct -- reading them through a second,
 * divergent path would fork the contract for no gain. This client is the
 * writes and nothing else.
 */
export class IdentityAdminClient {
  constructor(private readonly dispatcher: Dispatcher) {
    if (!dispatcher) throw new Error("IdentityAdminClient: dispatcher is required");
  }

  /**
   * Set a person's directory fields.
   *
   * Every field is applied, empty ones included: the underlying write is a
   * shallow merge, so send the whole edit rather than a partial one. When
   * `displayName` is blank the server derives one from first + last name.
   */
  async updateUserProfile(
    edit: UserProfileEdit,
    opts: IdentityAdminCallOptions = {},
  ): Promise<AdminWriteResult> {
    requireArg("updateUserProfile", "userId", edit.userId);
    return this.call("editing a profile", { updateUserProfile: edit }, opts);
  }

  /** Change a person's cluster-wide role. */
  async setUserRole(
    userId: string,
    role: string,
    opts: IdentityAdminCallOptions = {},
  ): Promise<AdminWriteResult> {
    requireArg("setUserRole", "userId", userId);
    requireArg("setUserRole", "role", role);
    return this.call("changing a role", { setUserRole: { userId, role } }, opts);
  }

  /**
   * Suspend or reinstate a person. One call for both, because they are one
   * decision with two values.
   */
  async setUserSuspended(
    userId: string,
    suspended: boolean,
    reason = "",
    opts: IdentityAdminCallOptions = {},
  ): Promise<AdminWriteResult> {
    requireArg("setUserSuspended", "userId", userId);
    return this.call(
      suspended ? "suspending an account" : "reinstating an account",
      { setUserSuspended: { userId, suspended, reason } },
      opts,
    );
  }

  /** Revoke anyone's personal access token. */
  async revokePersonalAccessToken(
    identityId: string,
    opts: IdentityAdminCallOptions = {},
  ): Promise<AdminWriteResult> {
    requireArg("revokePersonalAccessToken", "identityId", identityId);
    return this.call("revoking a personal access token", { revokeUserToken: { identityId } }, opts);
  }

  /** Revoke a node's bootstrap credential. */
  async revokeNodeToken(
    identityId: string,
    opts: IdentityAdminCallOptions = {},
  ): Promise<AdminWriteResult> {
    requireArg("revokeNodeToken", "identityId", identityId);
    return this.call("revoking a node token", { revokeNodeToken: { identityId } }, opts);
  }

  /**
   * Edit the cluster's runtime settings.
   *
   * Only the fields on ClusterSettingsEdit are written; everything else on the
   * row (the cluster domain, the bootstrap stamp) inherits. TTLs are seconds
   * -- days for the invitation one -- and 0 means "use the boot default".
   */
  async updateClusterSettings(
    settings: ClusterSettingsEdit,
    opts: IdentityAdminCallOptions = {},
  ): Promise<AdminWriteResult> {
    requireArg("updateClusterSettings", "registrationMode", settings.registrationMode);
    requireArg("updateClusterSettings", "internalDefaultRole", settings.internalDefaultRole);
    return this.call("editing cluster settings", { updateClusterSettings: settings }, opts);
  }

  /**
   * Mint a single-use enrolment link for another person.
   *
   * This is how somebody gets their FIRST credential when there is no mailbox
   * in the loop: the link authorizes exactly one action -- register a passkey
   * as the named user -- and nothing else. It is spent the moment that
   * registration succeeds.
   *
   * `ttlSeconds` of 0 takes the server's 15-minute default; a larger value is
   * clamped down to the server's ceiling rather than refused.
   *
   * The returned `url` is shown ONCE. See EnrolmentLinkResult.
   */
  async issueEnrolmentLink(
    userId: string,
    ttlSeconds = 0,
    opts: IdentityAdminCallOptions = {},
  ): Promise<EnrolmentLinkResult> {
    requireArg("issueEnrolmentLink", "userId", userId);
    const { result, raw } = await this.callRaw(
      "issuing an enrolment link",
      { issueEnrolmentLink: { userId, ttlSeconds } },
      opts,
    );
    const url = raw.enrolmentUrl ?? "";
    if (url === "") {
      // A success with no link is not a success. Reporting it as one would
      // leave a live enrolment token in the cluster that nobody can use and
      // nobody knows exists.
      throw new IdentityAdminError(
        "issuing an enrolment link",
        13,
        "the server reported success but returned no link",
        result.auditEventId,
      );
    }
    return { ...result, url };
  }

  /** Kill an unused enrolment link before it expires. */
  async revokeEnrolmentLink(
    enrolmentTokenId: string,
    opts: IdentityAdminCallOptions = {},
  ): Promise<AdminWriteResult> {
    requireArg("revokeEnrolmentLink", "enrolmentTokenId", enrolmentTokenId);
    return this.call("revoking an enrolment link", { revokeEnrolmentLink: { enrolmentTokenId } }, opts);
  }

  // call sends one bridged request and unwraps its reply, turning a carried
  // gRPC code into a thrown IdentityAdminError. `verb` only labels the error.
  private async call(
    verb: string,
    request: IdentityAdminRequestPayload,
    opts: IdentityAdminCallOptions,
  ): Promise<AdminWriteResult> {
    const { result } = await this.callRaw(verb, request, opts);
    return result;
  }

  // callRaw is call() plus the untouched reply payload, for the one operation
  // whose product is a field call() deliberately does not model.
  private async callRaw(
    verb: string,
    request: IdentityAdminRequestPayload,
    opts: IdentityAdminCallOptions,
  ): Promise<{ result: AdminWriteResult; raw: IdentityAdminResultPayload }> {
    const requestId = newShortId();
    const reply = await this.dispatcher.sendAndWait(
      { identityAdmin: { requestId, ...request } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new IdentityAdminError(verb, 13, payload.value.error?.message ?? "", "");
    }
    if (payload?.kind !== "identityAdminResult") {
      throw new IdentityAdminError(verb, 13, "unexpected reply envelope", "");
    }
    const res: IdentityAdminResultPayload = payload.value;
    // protojson omits zero values, so an absent code is 0 (OK) and an absent
    // `ok` is false. Trusting `ok` alone would read a malformed reply as
    // success, so a non-zero code is authoritative.
    const code = res.errorCode ?? 0;
    if (code !== 0 || res.ok !== true) {
      throw new IdentityAdminError(verb, code, res.errorMessage ?? "", res.auditEventId ?? "");
    }
    return {
      result: { message: res.message ?? "Done.", auditEventId: res.auditEventId ?? "" },
      raw: res,
    };
  }
}

function requireArg(method: string, name: string, value: string): void {
  if (!value) throw new Error(`${method}: ${name} is required`);
}
