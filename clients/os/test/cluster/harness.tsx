import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Cluster app's test harness: a connection-shaped double, and the session
// the app reads its actor and cluster domain from.
//
// CONNECTION-SHAPED rather than a mocked hook per call site, for the reason
// `test/fleet/harness.tsx` states: every read in this app goes through
// `connection.query.<generated method>`, every subscription through
// `connection.subscriptions`, and the module registry through
// `connection.dispatcher` -- so a fake that answers those three exercises the
// real LiveCollection, the real retain/seed path, the real ModulesClient and
// the real projections, which is where the behaviour under test lives.
//
// `subscriptions` is the one bit of the seam a collection uses:
// `subscribeGraph(handler, opts)` returning an unregister, plus `emit` as the
// test's hand on the wire. Without it every test is seed-only, and a
// seed-only test cannot see the FOLD -- which is the half of a live surface
// that runs for the rest of the session, and the half the arrival cue is
// about.

export function rowsResult(rows: Row[], cursor = ""): Result {
  // Result reads its rows off `data` -- the envelope the engine returns for a
  // shape-projected query -- so this is the wire's own shape rather than a
  // convenience the class does not have. `meta.cursor` is the keyset
  // continuation the audit trail walks.
  return new Result({ data: rows, ...(cursor === "" ? {} : { meta: { cursor } }) } as never);
}

export interface FakeQuery {
  inferenceStatus: ReturnType<typeof vi.fn>;
  passkeysForSelf: ReturnType<typeof vi.fn>;
  dataOrigins: ReturnType<typeof vi.fn>;
  syncStatesAll: ReturnType<typeof vi.fn>;
  outboxDeadLetters: ReturnType<typeof vi.fn>;
  datasyncStartBackfill: ReturnType<typeof vi.fn>;
  datasyncSetSyncPaused: ReturnType<typeof vi.fn>;
  datasyncRetryOutboxEntry: ReturnType<typeof vi.fn>;
  datasyncDiscardOutboxEntry: ReturnType<typeof vi.fn>;
  activeAgents: ReturnType<typeof vi.fn>;
  allAgents: ReturnType<typeof vi.fn>;
  agentAuthorizationsForSelf: ReturnType<typeof vi.fn>;
  recentAuditEvents: ReturnType<typeof vi.fn>;
}

/** The reads a seed can preload, by the method that answers them. */
export type QuerySeed = Partial<Record<keyof FakeQuery, Row[]>>;

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

// ---------------------------------------------------------------------------
// The module registry, over the dispatcher
// ---------------------------------------------------------------------------

export interface ModuleInfo {
  kind: string;
  name: string;
  description?: string;
  state?: string;
  stateDetail?: string;
  scope?: string;
}

export interface EnvVar {
  name: string;
  description?: string;
  secret?: boolean;
  scope?: string;
  requiredFor?: string[];
  set?: boolean;
  value?: string;
  defaultValue?: string;
}

export interface ModulesSeed {
  modules?: ModuleInfo[];
  reportingNodeId?: string;
  reportingNodeType?: string;
  /** Env vars keyed `${kind}/${name}`. */
  envVars?: Record<string, EnvVar[]>;
  /** A non-zero code makes `setPackEnabled` throw the way the real client
   *  does -- the error is INSIDE the payload, not a transport failure. */
  flipErrorCode?: number;
  flipErrorMessage?: string;
}

/**
 * The dispatcher seam, faithful to the three calls `ModulesClient` makes.
 *
 * The real client narrows the reply with `readServerPayload`, which keys on
 * the payload's own field name and throws on anything else -- so the double
 * has to answer in the wire's shape rather than in the client's return type.
 * That is deliberate: it means these tests exercise the real client, error
 * semantics included.
 */
export function fakeDispatcher(seed: ModulesSeed = {}) {
  const sendAndWait = vi.fn(async (msg: Record<string, unknown>) => {
    const nodeId = seed.reportingNodeId ?? "bff-abc123";
    const nodeType = seed.reportingNodeType ?? "bff";
    if (msg.modulesList) {
      return {
        modulesListResult: {
          errorCode: 0,
          modules: seed.modules ?? [],
          reportingNodeId: nodeId,
          reportingNodeType: nodeType,
        },
      };
    }
    if (msg.moduleDetail) {
      const ask = msg.moduleDetail as { kind: string; name: string };
      const module =
        (seed.modules ?? []).find((m) => m.kind === ask.kind && m.name === ask.name) ?? null;
      return {
        moduleDetailResult: {
          errorCode: 0,
          module,
          envVars: seed.envVars?.[`${ask.kind}/${ask.name}`] ?? [],
          reportingNodeId: nodeId,
          reportingNodeType: nodeType,
        },
      };
    }
    if (msg.setPackEnabled) {
      const ask = msg.setPackEnabled as { packDomain: string; enabled: boolean };
      return {
        setPackEnabledResult: {
          errorCode: seed.flipErrorCode ?? 0,
          errorMessage: seed.flipErrorMessage ?? "",
          packDomain: ask.packDomain,
          priorEnabled: !ask.enabled,
          enabled: ask.enabled,
          // ALWAYS true in v1, and the double says so rather than defaulting
          // to it: a test that let this be undefined would be asserting the
          // SDK's fallback, not the engine's contract.
          restartRequired: true,
        },
      };
    }
    throw new Error(`unexpected dispatcher message: ${Object.keys(msg).join(", ")}`);
  });
  return { sendAndWait };
}

export interface FakeConnection {
  query: FakeQuery;
  subscriptions: FakeSubscriptions;
  dispatcher: ReturnType<typeof fakeDispatcher>;
}

export function fakeConnection(seed: QuerySeed = {}, modules: ModulesSeed = {}): FakeConnection {
  const read = (key: keyof FakeQuery) => vi.fn(async () => rowsResult(seed[key] ?? []));
  return {
    query: {
      inferenceStatus: read("inferenceStatus"),
      passkeysForSelf: read("passkeysForSelf"),
      dataOrigins: read("dataOrigins"),
      syncStatesAll: read("syncStatesAll"),
      outboxDeadLetters: read("outboxDeadLetters"),
      datasyncStartBackfill: vi.fn(async () => rowsResult([])),
      datasyncSetSyncPaused: vi.fn(async () => rowsResult([])),
      datasyncRetryOutboxEntry: vi.fn(async () => rowsResult([])),
      datasyncDiscardOutboxEntry: vi.fn(async () => rowsResult([])),
      activeAgents: read("activeAgents"),
      allAgents: read("allAgents"),
      agentAuthorizationsForSelf: read("agentAuthorizationsForSelf"),
      recentAuditEvents: read("recentAuditEvents"),
    },
    subscriptions: fakeSubscriptions(),
    dispatcher: fakeDispatcher(modules),
  };
}

export function withSession(
  children: ReactNode,
  overrides: { userId?: string; domain?: string; clusterRole?: string; identityUrl?: string } = {},
) {
  const config: OsRuntimeConfig = {
    ...UNKNOWN_RUNTIME_CONFIG,
    domain: overrides.domain ?? "memql.example.com",
    identityUrl: overrides.identityUrl ?? "https://identity.memql.example.com",
  };
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "v1:identity:user:me",
          primaryEmail: "me@example.com",
          // Owner by default: most of this app is owner-floored, and a
          // harness whose default actor could not reach the surface under
          // test would make every assertion about the refusal instead.
          clusterRole: overrides.clusterRole ?? "owner",
        },
        config,
        ladderLoaded: true,
      }}
    >
      {children}
    </SessionProvider>
  );
}

// ---------------------------------------------------------------------------
// Row fixtures
// ---------------------------------------------------------------------------

/** A `v1:agents:agent` row with sane defaults, overridable field by field. */
export function agentRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "Assistant",
    description: "The general assistant.",
    kind: "assistant",
    role: "General Assistant",
    roleSlug: "general-assistant",
    capabilities: { skillIds: [], tools: [] },
    groupIds: [],
    active: true,
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

/** A `v1:platform:dataOrigin` projection row. */
export function dataOriginRow(over: Partial<Row> & { conceptId: string }): Row {
  return {
    dataState: "native",
    origin: "memql",
    mirroredTo: [],
    connectors: [],
    ...over,
  };
}

/** A `v1:platform:syncState` row. Every health FIGURE is deliberately absent
 *  unless a caller names it -- which is the state the em-dash test is about,
 *  and the state a `?? 0` in the projection would erase. */
export function syncStateRow(
  over: Partial<Row> & { conceptId: string; connector: string },
): Row {
  return {
    id: `v1:platform:syncState:${over.conceptId}-${over.connector}`,
    direction: "inbound",
    ...over,
  };
}

/** A `v1:platform:outboxEntry` dead letter. */
export function outboxEntryRow(over: Partial<Row> & { id: string }): Row {
  return {
    conceptId: "v1:shopify:product",
    rowRef: "v1:shopify:product:abc",
    action: "upsert",
    target: "shopify",
    status: "dead",
    attempts: 5,
    lastError: "429 from the vendor",
    createdAt: "2026-09-01T00:00:00Z",
    ...over,
  };
}

/** A `v1:identity:auditEvent` row. */
export function auditEventRow(over: Partial<Row> & { id: string }): Row {
  return {
    occurredAt: "2026-09-05T10:00:00Z",
    category: "authentication",
    action: "sign_in",
    actorUserId: "v1:identity:user:me",
    actorEmail: "me@example.com",
    actorRole: "owner",
    targetType: "user",
    targetId: "v1:identity:user:me",
    outcome: "success",
    createdAt: "2026-09-05T10:00:00Z",
    ...over,
  };
}

/** A `v1:identity:identity` passkey summary row. */
export function passkeyRow(over: Partial<Row> & { id: string }): Row {
  return {
    userId: "v1:identity:user:me",
    label: "Phone",
    active: true,
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}
