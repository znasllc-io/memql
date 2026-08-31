import { useCallback, useMemo, useState } from "react";
import {
  IdentityAdminClient,
  IdentityAdminError,
  type UserInvitationResult,
} from "@znasllc-io/memql-sdk-core/identityadmin";

import { useOsConnection } from "../../live/connection";

// Every write the Users app makes, and the one busy/error pair they share.
//
// ===========================================================================
// NOTHING HERE CHECKS A ROLE, AND NOTHING HERE IS THE AUTHORIZATION
// ===========================================================================
// component/identity/adminops refuses every one of these below owner/admin,
// against the role the stream interceptor VERIFIED -- not against anything
// this file or its callers believe. The app hides controls its operator
// cannot use because showing a button that always fails teaches nobody who
// can; that is presentation, exactly as spec section E says, and editing a
// boolean in a browser changes nothing about the answer.
//
// ===========================================================================
// A REFUSAL IS THE SERVER'S OWN SENTENCE, AND IT RENDERS BESIDE THE CONTROL
// ===========================================================================
// Never a toast. A `role_above_inviter` refusal is the most useful thing this
// surface can say, and a toast moves it somewhere else on a timer -- somebody
// who looked away has lost the only account of what happened.
//
// `auditEventId` comes back on success AND on refusal, because a denial is
// audited too. It is surfaced rather than swallowed: it is what an operator
// quotes in a support thread.

/** A refusal, unpacked into what a surface actually renders. */
export interface ActionRefusal {
  /** The server's own message, verbatim and in the data voice. */
  detail: string;
  /** The `v1:identity:auditEvent` this attempt wrote. "" when unknown. */
  auditEventId: string;
  /** True when the refusal was the role gate rather than a failure. */
  denied: boolean;
}

export function describeRefusal(err: unknown): ActionRefusal {
  if (err instanceof IdentityAdminError) {
    return {
      detail: err.message,
      auditEventId: err.auditEventId,
      denied: err.isPermissionDenied,
    };
  }
  return {
    detail: err instanceof Error ? err.message : String(err),
    auditEventId: "",
    denied: false,
  };
}

export interface UsersActions {
  /** True while any write is in flight; the id it is for, or "". */
  busyKey: string;
  /** The last refusal, or null. Cleared when a write starts, so it is never
   *  read as belonging to the current attempt. */
  refusal: ActionRefusal | null;
  clearRefusal: () => void;

  setRole: (userId: string, role: string) => Promise<boolean>;
  resetSignInPolicy: (userId: string) => Promise<boolean>;
  issueEnrolmentLink: (userId: string) => Promise<string>;
  revokeEnrolmentLink: (enrolmentTokenId: string) => Promise<boolean>;

  issueInvitation: (email: string, role: string) => Promise<UserInvitationResult | null>;
  revokeInvitation: (invitationId: string) => Promise<boolean>;
  /**
   * Re-send: issue a FRESH invitation for the same address, then revoke the
   * stale row, so exactly one stays pending.
   *
   * There is no dedicated resend op on the IdentityAdminMsg oneof -- verified
   * against component/grpc/memql.proto -- and this order is the one that is
   * safe to interrupt. Revoking first and then failing to issue would leave
   * the person with nothing and no record of why; issuing first and then
   * failing to revoke leaves two live invitations, which is untidy and still
   * works, and the stale one expires on its own.
   */
  resendInvitation: (
    invitationId: string,
    email: string,
    role: string,
  ) => Promise<UserInvitationResult | null>;
}

export function useUsersActions(): UsersActions {
  const connection = useOsConnection();
  const [busyKey, setBusyKey] = useState("");
  const [refusal, setRefusal] = useState<ActionRefusal | null>(null);

  // The client is built from the Connection's dispatcher, exactly as the
  // portal's ClusterProvider builds it. `?? null` rather than a bare read: a
  // narrowed test double without a dispatcher must land on the null branch
  // that disables the writes, not on a constructor over undefined.
  const client = useMemo(() => {
    const transport = connection?.dispatcher ?? null;
    return transport === null ? null : new IdentityAdminClient(transport);
  }, [connection]);

  const run = useCallback(
    async <T,>(key: string, write: (c: IdentityAdminClient) => Promise<T>): Promise<T | null> => {
      if (client === null) {
        setRefusal({
          detail: "Not connected to the cluster, so nothing was written.",
          auditEventId: "",
          denied: false,
        });
        return null;
      }
      setBusyKey(key);
      setRefusal(null);
      try {
        return await write(client);
      } catch (err: unknown) {
        setRefusal(describeRefusal(err));
        return null;
      } finally {
        setBusyKey("");
      }
    },
    [client],
  );

  const setRole = useCallback(
    async (userId: string, role: string) =>
      (await run(userId, (c) => c.setUserRole(userId, role))) !== null,
    [run],
  );

  const resetSignInPolicy = useCallback(
    async (userId: string) =>
      (await run(userId, (c) => c.resetSignInPolicy(userId))) !== null,
    [run],
  );

  // The URL is a CREDENTIAL and this is the only place it exists: the server
  // persisted its SHA-256 hash and no later call can retrieve it. It is
  // returned to the caller to render ONCE and is deliberately not held here --
  // a hook that kept it would keep it for the life of the window.
  const issueEnrolmentLink = useCallback(
    async (userId: string) => {
      const result = await run(userId, (c) => c.issueEnrolmentLink(userId));
      return result?.url ?? "";
    },
    [run],
  );

  const revokeEnrolmentLink = useCallback(
    async (enrolmentTokenId: string) =>
      (await run(enrolmentTokenId, (c) => c.revokeEnrolmentLink(enrolmentTokenId))) !== null,
    [run],
  );

  const issueInvitation = useCallback(
    (email: string, role: string) =>
      run(`invite:${email}`, (c) => c.issueUserInvitation(email, role)),
    [run],
  );

  const revokeInvitation = useCallback(
    async (invitationId: string) =>
      (await run(invitationId, (c) => c.revokeUserInvitation(invitationId))) !== null,
    [run],
  );

  const resendInvitation = useCallback(
    (invitationId: string, email: string, role: string) =>
      run(invitationId, async (c) => {
        const issued = await c.issueUserInvitation(email, role);
        // The revoke is deliberately NOT awaited into the failure path: the
        // fresh invitation is the thing the operator asked for and it already
        // exists. A failure to tidy the stale row must not report the resend
        // as failed and invite a second one.
        await c.revokeUserInvitation(invitationId).catch(() => undefined);
        return issued;
      }),
    [run],
  );

  return {
    busyKey,
    refusal,
    clearRefusal: () => setRefusal(null),
    setRole,
    resetSignInPolicy,
    issueEnrolmentLink,
    revokeEnrolmentLink,
    issueInvitation,
    revokeInvitation,
    resendInvitation,
  };
}
