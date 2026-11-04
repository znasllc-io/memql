import { useCallback, useMemo, useState } from "react";
import {
  IdentityAdminClient,
  IdentityAdminError,
  type UserInvitationResult,
} from "@znasllc-io/memql-sdk-core/identityadmin";

import { renderMemQLValue, type QueryClient, type Result } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { copyFor, refusalFrom } from "./refusals";

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
  /**
   * The TYPED CODE, for a refusal that carried one. "" for the admin ops,
   * which answer with a gRPC status and a sentence and no code of their own.
   *
   * The two halves of this app refuse differently and both are kept as they
   * arrive: `IdentityAdminMsg` answers `PERMISSION_DENIED` with a message,
   * while the group and role builtins answer `"<code>: <sentence>"` -- the
   * contract integrations/groups/guards.go states, and what `refusals.ts`
   * keys its copy on.
   */
  code?: string;
  /** The headline for `code`, or absent when this build does not know it. */
  title?: string;
  /** What to do about it. */
  next?: string;
}

export function describeRefusal(err: unknown): ActionRefusal {
  if (err instanceof IdentityAdminError) {
    return {
      detail: err.message,
      auditEventId: err.auditEventId,
      denied: err.isPermissionDenied,
    };
  }
  // A builtin refusal, which carries its code inside the message. `refusalFrom`
  // recognises the two frames it can arrive in and answers a blank code for
  // anything that is not one, so an ordinary failure keeps its own words.
  const refusal = refusalFrom(err);
  return {
    detail: refusal.detail,
    auditEventId: "",
    denied: refusal.code === "group_capability_missing" || refusal.code === "role_capability_missing",
    code: refusal.code,
    title: refusal.title,
    next: refusal.next,
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

  issueInvitation: (
    email: string,
    role: string,
    groupIds?: readonly string[],
  ) => Promise<UserInvitationResult | null>;
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
    groupIds?: readonly string[],
  ) => Promise<UserInvitationResult | null>;

  /** Suspend or reinstate a person -- Deactivate and Reactivate on their bar. */
  setSuspended: (userId: string, suspended: boolean) => Promise<boolean>;
  /** End one session of somebody else's, from their Sign-in panel. */
  endSession: (sessionId: string) => Promise<boolean>;

  // ---- groups (integrations/groups, hand-rendered until #5176 regenerates) --
  /**
   * The new group's id, or NULL when the write was refused.
   *
   * The two are different answers and the caller acts on the difference: a
   * refusal keeps the form up with the server's sentence beside it, while a
   * SUCCESS whose reply this build could not read is still a success -- the
   * row is written, it arrives on its own broadcast, and keeping the form open
   * would invite a second one.
   */
  groupCreate: (name: string, description: string, accountId: string) => Promise<string | null>;
  groupUpdate: (groupId: string, name: string, description: string) => Promise<boolean>;
  groupArchive: (groupId: string) => Promise<boolean>;
  groupMemberAdd: (groupId: string, userId: string) => Promise<boolean>;
  groupMemberRemove: (groupId: string, userId: string) => Promise<boolean>;

  // ---- roles (integrations/rbac) ------------------------------------------
  /** The new role's slug, or NULL when the write was refused. See groupCreate. */
  roleCreate: (input: RoleDraft) => Promise<string | null>;
  roleUpdate: (slug: string, grants: readonly string[]) => Promise<boolean>;
  roleDeactivate: (slug: string) => Promise<boolean>;
}

/**
 * The grid's `resource:verb` pairs as the `{ verb, resource }` objects roleCreate
 * and roleUpdate read (`grants []object!`). The engine refuses any entry that is
 * not an object, so sending the pairs as text made every role write fail.
 *
 * Split at the LAST colon: a verb never carries one, and a resource may
 * (`app:deployables`). A pair with no colon keeps an empty resource, which the
 * engine refuses by name rather than this file guessing.
 */
export function grantObjects(pairs: readonly string[]): { verb: string; resource: string }[] {
  return pairs.map((pair) => {
    const at = pair.lastIndexOf(":");
    return { verb: pair.slice(at + 1), resource: at < 0 ? "" : pair.slice(0, at) };
  });
}

/** What New role sends: the whole role in one call, grants included. */
export interface RoleDraft {
  slug: string;
  name: string;
  rank: number;
  description: string;
  /** `resource:verb` pairs, the grid's own vocabulary. */
  grants: readonly string[];
  /** The account a scoped role is confined to, or "" for everywhere. */
  accountId: string;
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

  const query = connection?.query ?? null;

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

  // THE SECOND WRITE PATH, and it is a different client on purpose.
  //
  // The admin ops ride `IdentityAdminMsg` and answer a gRPC status; the group
  // and role builtins ride the ordinary query path and answer `"<code>:
  // <sentence>"`. One `run` over both would have to invent a common failure
  // shape, and the shape it invented would drop the code -- which is the thing
  // `refusals.ts` keys every sentence in this app on.
  const runQuery = useCallback(
    async <T,>(key: string, write: (query: QueryClient) => Promise<T>): Promise<T | null> => {
      if (query === null) {
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
        return await write(query);
      } catch (err: unknown) {
        setRefusal(describeRefusal(err));
        return null;
      } finally {
        setBusyKey("");
      }
    },
    [query],
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
    (email: string, role: string, groupIds: readonly string[] = []) =>
      run(`invite:${email}`, (c) => c.issueUserInvitation(email, role, 0, groupIds)),
    [run],
  );

  const revokeInvitation = useCallback(
    async (invitationId: string) =>
      (await run(invitationId, (c) => c.revokeUserInvitation(invitationId))) !== null,
    [run],
  );

  const resendInvitation = useCallback(
    (invitationId: string, email: string, role: string, groupIds: readonly string[] = []) =>
      run(invitationId, async (c) => {
        const issued = await c.issueUserInvitation(email, role, 0, groupIds);
        // The revoke is deliberately NOT awaited into the failure path: the
        // fresh invitation is the thing the operator asked for and it already
        // exists. A failure to tidy the stale row must not report the resend
        // as failed and invite a second one.
        await c.revokeUserInvitation(invitationId).catch(() => undefined);
        return issued;
      }),
    [run],
  );

  const setSuspended = useCallback(
    async (userId: string, suspended: boolean) =>
      (await run(userId, (c) => c.setUserSuspended(userId, suspended))) !== null,
    [run],
  );

  const endSession = useCallback(
    async (sessionId: string) =>
      // `revokedReason: "admin"` is the enum value for somebody else ending it.
      // The other three are the person's own act, the all-sessions fan-out and
      // the rotator's reuse detection, and none of them is what this is.
      (await runQuery(sessionId, (q) =>
        q.revokeAuthSession({ sessionId, revokedReason: "admin" }),
      )) !== null,
    [runQuery],
  );

  // ---- groups -------------------------------------------------------------
  //
  // The GENERATED builders (`make sdk-gen`), which is what keeps the argument
  // names in step with the DSL: a builtin whose argument was renamed fails to
  // compile here rather than being refused at runtime for a reason no sentence
  // on screen would explain.

  const groupCreate = useCallback(
    async (name: string, description: string, accountId: string) => {
      const result: Result | null = await runQuery(`group:new:${name}`, (q) =>
        q.groupCreate({ name, description, accountId }),
      );
      if (result === null) return null;
      const row = result.rows()[0];
      return row ? String((row as Record<string, unknown>)["groupId"] ?? "") : "";
    },
    [runQuery],
  );

  const groupUpdate = useCallback(
    async (groupId: string, name: string, description: string) =>
      (await runQuery(groupId, (q) =>
        q.groupUpdate({ groupId, name, description }),
      )) !== null,
    [runQuery],
  );

  const groupArchive = useCallback(
    async (groupId: string) =>
      (await runQuery(groupId, (q) =>
        q.groupArchive({ groupId }),
      )) !== null,
    [runQuery],
  );

  const groupMemberAdd = useCallback(
    async (groupId: string, userId: string) =>
      (await runQuery(userId, (q) =>
        q.groupMemberAdd({ groupId, userId }),
      )) !== null,
    [runQuery],
  );

  const groupMemberRemove = useCallback(
    async (groupId: string, userId: string) =>
      (await runQuery(userId, (q) =>
        q.groupMemberRemove({ groupId, userId }),
      )) !== null,
    [runQuery],
  );

  // ---- roles --------------------------------------------------------------
  //
  // `grants` crosses as the grid's own `resource:verb` pairs, which is what the
  // three builtins take: the whole grant set on every call, never a patch. A
  // patch would need the caller to send what to REMOVE, and a role edited from
  // two windows would then apply two half-answers.

  // ===========================================================================
  // THE ROLE BUILTINS ANSWER WITH A DECISION ROW, NOT AN ERROR
  // ===========================================================================
  // `integrations/groups` refuses by returning an error, so a refused group
  // write throws and `runQuery` catches it. `integrations/rbac` does NOT: its
  // three role handlers answer `{ok, slug, code, message}` as an ordinary
  // reply row (`decisionNodes`), so the call SUCCEEDS and the refusal rides
  // inside it.
  //
  // Read naively, that makes every refused role write report success -- the
  // form closes, nothing is written, and the person is told nothing at all.
  // This is the one place that difference is reconciled, and it reads the CODE
  // rather than `ok`: a scalar boolean crosses a builtin's reply row as the
  // STRING "true", so `rowBool` answers false for a successful write and
  // trusting it would invert the whole thing.
  const decisionRefusal = useCallback((result: Result | null): boolean => {
    if (result === null) return false;
    const row = result.rows()[0] as Record<string, unknown> | undefined;
    const code = typeof row?.["code"] === "string" ? (row["code"] as string) : "";
    if (code === "" || code === "ok") return true;
    const message = typeof row?.["message"] === "string" ? (row["message"] as string) : "";
    const copy = copyFor(code);
    setRefusal({
      detail: message,
      auditEventId: "",
      denied: code === "role_not_authorized",
      code,
      title: copy?.title ?? "",
      next: copy?.next ?? "",
    });
    return false;
  }, []);

  const roleCreate = useCallback(
    async (input: RoleDraft) => {
      const result: Result | null = await runQuery(`role:new:${input.slug}`, (q) =>
        q.executeNamed(
          "roleCreate",
          `builtin roleCreate(slug: ${renderMemQLValue(input.slug)}, name: ${renderMemQLValue(input.name)}, rank: ${renderMemQLValue(input.rank)}, description: ${renderMemQLValue(input.description)}, grants: ${renderMemQLValue(grantObjects(input.grants))}, accountId: ${renderMemQLValue(input.accountId)})`,
        ),
      );
      if (result === null) return null;
      if (!decisionRefusal(result)) return null;
      const row = result.rows()[0];
      return row ? String((row as Record<string, unknown>)["slug"] ?? "") : input.slug;
    },
    [runQuery, decisionRefusal],
  );

  const roleUpdate = useCallback(
    async (slug: string, grants: readonly string[]) => {
      const result: Result | null = await runQuery(slug, (q) =>
        q.executeNamed(
          "roleUpdate",
          `builtin roleUpdate(slug: ${renderMemQLValue(slug)}, grants: ${renderMemQLValue(grantObjects(grants))})`,
        ),
      );
      return decisionRefusal(result);
    },
    [runQuery, decisionRefusal],
  );

  const roleDeactivate = useCallback(
    async (slug: string) => {
      const result: Result | null = await runQuery(slug, (q) =>
        q.executeNamed("roleDeactivate", `builtin roleDeactivate(slug: ${renderMemQLValue(slug)})`),
      );
      return decisionRefusal(result);
    },
    [runQuery, decisionRefusal],
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
    setSuspended,
    endSession,
    groupCreate,
    groupUpdate,
    groupArchive,
    groupMemberAdd,
    groupMemberRemove,
    roleCreate,
    roleUpdate,
    roleDeactivate,
  };
}
