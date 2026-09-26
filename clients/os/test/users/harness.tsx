import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { readiness, verdict } from "../setup/harness";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Users app's test harness: a connection-shaped double, and the session
// the app reads its actor from.
//
// CONNECTION-SHAPED rather than a mocked hook per call site, for the reason
// the Fleet's harness records: every read goes through
// `connection.query.<generated method>`, every subscription through
// `connection.subscriptions`, and every write through
// `connection.dispatcher`. A fake that answers those three exercises the real
// LiveCollection, the real retain/seed path, the real projections and the real
// IdentityAdminClient -- which is where the behaviour under test actually
// lives.

export function rowsResult(rows: Row[]): Result {
  // Result reads its rows off `data` -- the envelope the engine returns for a
  // shape-projected query -- so this is the wire's own shape rather than a
  // convenience the class does not have.
  return new Result({ data: rows } as never);
}

/**
 * A BUNDLE envelope, which is a different wire shape and not interchangeable.
 *
 * `getRowByConceptAndId` reads `rawNodes()`, which looks at
 * `payload.bundle.nodes` and NOTHING else -- a `data` envelope makes it return
 * an empty list, and the by-id re-read then answers null. That failure is
 * silent by design (a null re-read means "keep what you have"), so a harness
 * that returned the wrong envelope would make the detail panel look like it
 * was correctly falling back when in fact it had never read anything.
 *
 * The node is nested -- intrinsics on the envelope, concept fields under
 * `payload` -- because that is what a bundle node is, and it is exactly the
 * shape `rows.ts:flatten` exists to reconcile with the flat seed form.
 */
export function bundleResult(rows: Row[]): Result {
  const nodes = rows.map((row) => {
    const { id, createdAt, ...fields } = row as Record<string, unknown>;
    return { id, createdAt, payload: fields };
  });
  return new Result({ bundle: { nodes } } as never);
}

export interface FakeEvent {
  subscriptionId: string;
  kind: string;
  timestamp: Date | null;
  payload: Row | null;
  payloadOmitted: boolean;
  seq: number;
  gapBefore: boolean;
}

export interface FakeSubscriptions {
  subscribeGraph: (handler: (event: FakeEvent) => void, opts: { concept?: string }) => () => void;
  /** Push an event to every handler subscribed to `concept`. */
  emit: (concept: string, payload: Row, kind?: string) => void;
}

function fakeSubscriptions(): FakeSubscriptions {
  const handlers = new Map<string, Set<(event: FakeEvent) => void>>();
  return {
    subscribeGraph(handler, opts) {
      const concept = opts.concept ?? "*";
      const set = handlers.get(concept) ?? new Set();
      set.add(handler);
      handlers.set(concept, set);
      return () => set.delete(handler);
    },
    emit(concept, payload, kind = "NODE_UPDATED") {
      for (const handler of handlers.get(concept) ?? []) {
        handler({
          subscriptionId: "sub-1",
          kind,
          timestamp: new Date(),
          payload,
          payloadOmitted: false,
          seq: 0,
          gapBefore: false,
        });
      }
    },
  };
}

export interface FakeQuery {
  searchUsers: ReturnType<typeof vi.fn>;
  pendingUserInvitations: ReturnType<typeof vi.fn>;
  sessionsForSubjectAdmin: ReturnType<typeof vi.fn>;
  activeRoles: ReturnType<typeof vi.fn>;
  activeCapabilities: ReturnType<typeof vi.fn>;
  clientAccountsAll: ReturnType<typeof vi.fn>;
  revokeAuthSession: ReturnType<typeof vi.fn>;
  groupsAll: ReturnType<typeof vi.fn>;
  groupPeople: ReturnType<typeof vi.fn>;
  membersOfGroup: ReturnType<typeof vi.fn>;
  groupsForUser: ReturnType<typeof vi.fn>;
  groupsForAccount: ReturnType<typeof vi.fn>;
  groupCreate: ReturnType<typeof vi.fn>;
  groupUpdate: ReturnType<typeof vi.fn>;
  groupArchive: ReturnType<typeof vi.fn>;
  groupMemberAdd: ReturnType<typeof vi.fn>;
  groupMemberRemove: ReturnType<typeof vi.fn>;
  /**
   * The one entry point for everything without a generated builder yet: the
   * group reads and every group and role BUILTIN (`useGroups.ts` and
   * `actions.ts` both hand-render, because epic memql#5165's last task is what
   * regenerates the SDKs). `getRowByConceptAndId` lands here too, which is why
   * the fake dispatches on the NAME rather than pattern-matching the call.
   */
  executeNamed: ReturnType<typeof vi.fn>;
}

export interface FakeConnection {
  query: FakeQuery;
  subscriptions: FakeSubscriptions;
  dispatcher: { sendAndWait: ReturnType<typeof vi.fn> };
}

export interface FakeSeed {
  searchUsers?: Row[];
  pendingUserInvitations?: Row[];
  sessionsForSubjectAdmin?: Row[];
  groupsAll?: Row[];
  groupPeople?: Row[];
  activeRoles?: Row[];
  activeCapabilities?: Row[];
  clientAccountsAll?: Row[];
  /** Membership rows, keyed by group id -- what `membersOfGroup` answers. */
  membersOfGroup?: Record<string, Row[]>;
  /** Membership rows, keyed by user id -- what `groupsForUser` answers. */
  groupsForUser?: Record<string, Row[]>;
  /** Rows the by-id re-read answers with, keyed by row id. */
  byId?: Record<string, Row>;
  /** Builtin replies by name; a thrown value is the refusal path. */
  builtins?: Record<string, Row | Error>;
}

export function fakeConnection(seed: FakeSeed = {}): FakeConnection {
  const read = (
    key:
      | "searchUsers"
      | "pendingUserInvitations"
      | "sessionsForSubjectAdmin"
      | "activeRoles"
      | "activeCapabilities"
      | "clientAccountsAll",
  ) => vi.fn(async () => rowsResult(seed[key] ?? []));

  /** Pull the one argument a hand-rendered read passes, e.g. `groupId: "g1"`. */
  const argOf = (call: string, name: string): string => {
    const m = new RegExp(`${name}:\\s*"([^"]*)"`).exec(call);
    return m?.[1] ?? "";
  };

  /** A generated builtin's reply, or the seeded refusal. */
  const builtin = (name: string, reply: Record<string, unknown>) =>
    vi.fn(async (_args: Record<string, unknown>) => {
      const seeded = seed.builtins?.[name];
      if (seeded instanceof Error) throw seeded;
      return rowsResult([(seeded ?? reply) as Row]);
    });

  return {
    query: {
      searchUsers: read("searchUsers"),
      pendingUserInvitations: read("pendingUserInvitations"),
      sessionsForSubjectAdmin: read("sessionsForSubjectAdmin"),
      activeRoles: read("activeRoles"),
      activeCapabilities: read("activeCapabilities"),
      clientAccountsAll: read("clientAccountsAll"),
      revokeAuthSession: vi.fn(async () => rowsResult([])),
      // The group reads, through the GENERATED builders the app calls (`make
      // sdk-gen`). They were hand-rendered through `executeNamed` until epic
      // memql#5165's own last task regenerated the SDKs; a harness that
      // answered only the old shape would leave every group surface reading an
      // empty list, which is a completely plausible answer and is what makes
      // that kind of miss survive review.
      groupPeople: vi.fn(async (_args: Record<string, unknown>) => rowsResult(seed.groupPeople ?? [])),
      groupsAll: vi.fn(async (_args: Record<string, unknown>) => rowsResult(seed.groupsAll ?? [])),
      membersOfGroup: vi.fn(async (args: Record<string, unknown>) => {
        const groupId = typeof args["groupId"] === "string" ? args["groupId"] : "";
        return rowsResult(seed.membersOfGroup?.[groupId] ?? []);
      }),
      groupsForUser: vi.fn(async (args: Record<string, unknown>) => {
        const userId = typeof args["userId"] === "string" ? args["userId"] : "";
        return rowsResult(seed.groupsForUser?.[userId] ?? []);
      }),
      groupsForAccount: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      // The five group WRITES, likewise generated. A thrown seed value is the
      // refusal path.
      groupCreate: builtin("groupCreate", { groupId: "g-new" }),
      groupUpdate: builtin("groupUpdate", { ok: "true" }),
      groupArchive: builtin("groupArchive", { ok: "true" }),
      groupMemberAdd: builtin("groupMemberAdd", { ok: "true" }),
      groupMemberRemove: builtin("groupMemberRemove", { ok: "true" }),
      executeNamed: vi.fn(async (name: string, call: string) => {
        if (name === "groupsAll") return rowsResult(seed.groupsAll ?? []);
        if (name === "membersOfGroup") {
          return rowsResult(seed.membersOfGroup?.[argOf(call, "groupId")] ?? []);
        }
        if (name === "groupsForUser") {
          return rowsResult(seed.groupsForUser?.[argOf(call, "userId")] ?? []);
        }
        const builtin = seed.builtins?.[name];
        if (builtin instanceof Error) throw builtin;
        if (builtin !== undefined) return rowsResult([builtin]);
        if (name.startsWith("group") || name.startsWith("role")) return rowsResult([{ ok: "true" } as Row]);
        // `getRowByConceptAndId` composes `concept==<c> && id==<id>`; the
        // harness answers from `byId` so a by-id re-read is a real round trip
        // through the same helper the app calls.
        const match = /id==(\S+)/.exec(call);
        const wanted = match?.[1] ?? "";
        const row = wanted === "" ? undefined : seed.byId?.[wanted];
        return bundleResult(row ? [row] : []);
      }),
    },
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
  };
}

/** A group row with sane defaults. */
export function groupRow(over: Partial<Row> & { id: string }): Row {
  return {
    name: "A group",
    description: "",
    kind: "custom",
    accountId: "",
    status: "active",
    archivedAt: "",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

/** A membership row with sane defaults. */
export function membershipRow(over: Partial<Row> & { id: string }): Row {
  return {
    groupId: "g1",
    userId: "v1:identity:user:someone",
    origin: "added",
    addedBy: "",
    status: "active",
    removedAt: "",
    removedBy: "",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

/** A role row with sane defaults. */
export function roleRow(over: Partial<Row> & { slug: string }): Row {
  return {
    id: `v1:rbac:role:${String(over["slug"])}`,
    name: String(over["slug"]),
    rank: 100,
    description: "",
    predefined: true,
    active: true,
    aliases: [],
    accountId: "",
    ...over,
  };
}

/** A capability row: one role's grant of one verb on one resource. */
export function grantRow(roleSlug: string, verb: string, resourceType: string): Row {
  return {
    id: `v1:rbac:capability:${roleSlug}-${verb}-${resourceType}`,
    roleSlug,
    verb,
    resourceType,
    effect: "allow",
    predefined: true,
    active: true,
  };
}

/** An account row with sane defaults, for the domain match and the chips. */
export function accountRow(over: Partial<Row> & { id: string }): Row {
  return {
    name: "Acme",
    domain: "",
    primaryContactName: "",
    primaryContactEmail: "",
    notes: "",
    status: "active",
    configuredAt: "2026-08-01T00:00:00Z",
    ownerUserId: "",
    domainToken: "",
    domainStatus: "unverified",
    domainFailureReason: "",
    domainFailureDetail: "",
    domainLastCheckedAt: "",
    domainVerifiedAt: "",
    joinOnDomain: false,
    memqlDomain: "",
    memqlReservedAt: "",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

/**
 * A reply the IdentityAdminClient will accept as a success.
 *
 * Built to the wire's own shape rather than to the client's return type, so a
 * test exercises the real unwrapping -- including the rule that a non-zero
 * code is authoritative over `ok`.
 */
export function adminOk(extra: Record<string, unknown> = {}) {
  return {
    identityAdminResult: {
      ok: true,
      errorCode: 0,
      message: "Done.",
      auditEventId: "v1:identity:auditEvent:a1",
      ...extra,
    },
  };
}

/** A refusal, as the engine sends one. 7 is PERMISSION_DENIED. */
export function adminRefusal(message: string, code = 7) {
  return {
    identityAdminResult: {
      ok: false,
      errorCode: code,
      errorMessage: message,
      auditEventId: "v1:identity:auditEvent:denied1",
    },
  };
}

export function withSession(
  children: ReactNode,
  overrides: {
    userId?: string;
    role?: string;
    domain?: string;
    /**
     * Whether the email module reports configured.
     *
     * IT DEFAULTS TO TRUE, and that is the deliberate one. `gateFor` answers
     * "unknown" for an ABSENT readiness feed, and unknown is not ready -- so a
     * harness that left it out would hide the Invite action in every case here
     * and each of them would pass while measuring nothing.
     */
    emailReady?: boolean;
  } = {},
) {
  const config: OsRuntimeConfig = {
    ...UNKNOWN_RUNTIME_CONFIG,
    domain: overrides.domain ?? "memql.example.com",
  };
  const emailState = (overrides.emailReady ?? true) ? "configured" : "unconfigured";
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "v1:identity:user:me",
          primaryEmail: "owner@example.com",
          role: overrides.role ?? "owner",
          everyAccount: ["owner", "admin", "developer"].includes(overrides.role ?? "owner"),
          roleName: "",
          rank: 0,
        },
        config,
        // The shared fixture rather than a shape spelled here: one place says
        // what a Verdict is, and a second copy is one that drifts.
        readiness: readiness(true, [verdict("email", emailState)]),
      }}
    >
      {children}
    </SessionProvider>
  );
}

/** A user row with sane defaults, overridable field by field. */
export function userRow(over: Partial<Row> & { id: string }): Row {
  return {
    displayName: "",
    firstName: "",
    lastName: "",
    primaryEmail: "person@example.com",
    role: "reader",
    signInPolicy: "any",
    sharedMailbox: false,
    active: true,
    suspendedAt: "",
    lastSeenAt: new Date().toISOString(),
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

/** An invitation row with sane defaults. */
export function invitationRow(over: Partial<Row> & { id: string }): Row {
  return {
    kind: "user",
    status: "pending",
    active: true,
    inviteeEmail: "colleague@example.com",
    inviteeName: "",
    inviteeRole: "reader",
    inviterName: "Owner",
    expiresAt: "2099-01-01T00:00:00Z",
    respondedAt: "",
    deliveryState: "sent",
    deliveryError: "",
    groupIds: [],
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}
