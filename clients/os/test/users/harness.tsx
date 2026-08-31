import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
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
  /** What `getRowByConceptAndId` goes through -- the detail panel's re-read
   *  and the collection's id-only event path both land here. */
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
  /** Rows the by-id re-read answers with, keyed by row id. */
  byId?: Record<string, Row>;
}

export function fakeConnection(seed: FakeSeed = {}): FakeConnection {
  const read = (key: "searchUsers" | "pendingUserInvitations" | "sessionsForSubjectAdmin") =>
    vi.fn(async () => rowsResult(seed[key] ?? []));
  return {
    query: {
      searchUsers: read("searchUsers"),
      pendingUserInvitations: read("pendingUserInvitations"),
      sessionsForSubjectAdmin: read("sessionsForSubjectAdmin"),
      executeNamed: vi.fn(async (_name: string, filter: string) => {
        // `getRowByConceptAndId` composes `concept==<c> && id==<id>`; the
        // harness answers from `byId` so a by-id re-read is a real round trip
        // through the same helper the app calls.
        const match = /id==(\S+)/.exec(filter);
        const wanted = match?.[1] ?? "";
        const row = wanted === "" ? undefined : seed.byId?.[wanted];
        return bundleResult(row ? [row] : []);
      }),
    },
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
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
  overrides: { userId?: string; role?: string; domain?: string } = {},
) {
  const config: OsRuntimeConfig = {
    ...UNKNOWN_RUNTIME_CONFIG,
    domain: overrides.domain ?? "memql.example.com",
  };
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "v1:identity:user:me",
          primaryEmail: "owner@example.com",
          clusterRole: overrides.role ?? "owner",
        },
        config,
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
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}
