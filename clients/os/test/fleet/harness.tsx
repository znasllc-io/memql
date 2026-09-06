import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Fleet's test harness: a connection-shaped double, and the session the
// app reads its actor and cluster domain from.
//
// The double is deliberately CONNECTION-SHAPED rather than a mocked hook per
// call site. Every read in this app goes through `connection.query.<generated
// method>` and every subscription through `connection.subscriptions`, so a
// fake that answers those two is exercising the real LiveCollection, the real
// retain/seed path and the real projections -- which is where the behaviour
// under test actually lives. `subscriptions: null` is a supported shape (the
// collection guards for it and simply has no liveness), which is what lets a
// jsdom test seed a live list without a server.

export function rowsResult(rows: Row[]): Result {
  // Result reads its rows off `data` -- the envelope the engine returns for a
  // shape-projected query -- so this is the wire's own shape rather than a
  // convenience the class does not have.
  return new Result({ data: rows } as never);
}

export interface FakeQuery {
  myWorkersWithStatus: ReturnType<typeof vi.fn>;
  myRoutingPolicies: ReturnType<typeof vi.fn>;
  myWorkspaces: ReturnType<typeof vi.fn>;
  clusterNodes: ReturnType<typeof vi.fn>;
  invocationsForWorker: ReturnType<typeof vi.fn>;
  // The Apps section's reads (epic memql#5009). Neither concept broadcasts,
  // so both are on-demand reads rather than seeds behind a subscription.
  delegationPolicyForUser: ReturnType<typeof vi.fn>;
  appSessionsForUser: ReturnType<typeof vi.fn>;
  appSessionById: ReturnType<typeof vi.fn>;
  renameWorker: ReturnType<typeof vi.fn>;
  setWorkerOperatorLabels: ReturnType<typeof vi.fn>;
  revokeWorker: ReturnType<typeof vi.fn>;
  createRoutingPolicy: ReturnType<typeof vi.fn>;
  updateRoutingPolicy: ReturnType<typeof vi.fn>;
  setDelegationPolicy: ReturnType<typeof vi.fn>;
}

// The subscription seam, faithful to the one bit of it a collection uses:
// `subscribeGraph(handler, opts)` returning an unregister. `emit` is the
// test's hand on the wire.
//
// WITHOUT THIS EVERY TEST IS SEED-ONLY, and a seed-only test cannot see the
// fold -- which is the half of a live surface that runs for the rest of the
// session. That gap hid a real defect: the collection upserts a folded
// event's payload AS THE ROW TYPE, with no projection, so a collection typed
// with a projected row holds raw wire rows the moment anything updates.
export interface FakeSubscriptions {
  subscribeGraph: (handler: (event: FakeEvent) => void, opts: { concept?: string }) => () => void;
  /** Push an event to every handler subscribed to `concept`. */
  emit: (concept: string, payload: Row, kind?: string) => void;
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

export interface FakeConnection {
  query: FakeQuery;
  subscriptions: FakeSubscriptions;
  dispatcher: { sendAndWait: ReturnType<typeof vi.fn> };
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

export function fakeConnection(seed: Partial<Record<keyof FakeQuery, Row[]>> = {}): FakeConnection {
  const read = (key: keyof FakeQuery) => vi.fn(async () => rowsResult(seed[key] ?? []));
  return {
    query: {
      myWorkersWithStatus: read("myWorkersWithStatus"),
      myRoutingPolicies: read("myRoutingPolicies"),
      myWorkspaces: read("myWorkspaces"),
      clusterNodes: read("clusterNodes"),
      invocationsForWorker: read("invocationsForWorker"),
      delegationPolicyForUser: read("delegationPolicyForUser"),
      appSessionsForUser: read("appSessionsForUser"),
      appSessionById: read("appSessionById"),
      renameWorker: vi.fn(async () => rowsResult([])),
      setWorkerOperatorLabels: vi.fn(async () => rowsResult([])),
      revokeWorker: vi.fn(async () => rowsResult([])),
      createRoutingPolicy: vi.fn(async () => rowsResult([])),
      updateRoutingPolicy: vi.fn(async () => rowsResult([])),
      setDelegationPolicy: vi.fn(async () => rowsResult([])),
    },
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
  };
}

export function withSession(
  children: ReactNode,
  overrides: { userId?: string; domain?: string } = {},
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
          primaryEmail: "me@example.com",
          clusterRole: "owner",
        },
        config,
      }}
    >
      {children}
    </SessionProvider>
  );
}

/** A registration row with sane defaults, overridable field by field. */
export function machineRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "host.local",
    displayName: "",
    platformInfo: { os: "darwin", arch: "arm64", hostname: "host.local" },
    labels: {},
    operatorLabels: {},
    concurrency: { HEADLESS: 4 },
    activeCount: 0,
    registeredAt: "2026-08-01T00:00:00Z",
    lastSeenAt: new Date().toISOString(),
    ...over,
  };
}

/**
 * A delegated app-session row (epic memql#5009).
 *
 * `usage` IS ABSENT BY DEFAULT, which is the state most easily got wrong: an
 * app that reported nothing did not report zero, and the reading has to keep
 * those apart all the way to the pixel. Pass one explicitly to model an app
 * that DID report.
 */
export function appSessionRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    workerId: "v1:worker:registration:laptop",
    app: "claude-code",
    kind: "run",
    planId: "",
    taskId: "",
    status: "ended",
    billing: "subscription",
    startedAt: "2026-09-01T09:00:00Z",
    endedAt: "2026-09-01T09:04:00Z",
    ...over,
  };
}

/** A delegation policy row. ABSENT is the common case -- pass no row at all
 *  for it, rather than one of these with the switch off: they are different
 *  facts about a person. */
export function delegationPolicyRow(over: Partial<Row> = {}): Row {
  return {
    id: "v1:worker:delegationPolicy:v1-identity-user-me",
    ownerUserId: "v1:identity:user:me",
    preferSubscriptionApps: true,
    eligibleKinds: ["runCommand"],
    appOrder: ["claude-code"],
    maxConcurrentSessions: 2,
    workspaceRoot: "/Users/ana/memql-workspaces",
    credentialLifetimeSeconds: 14400,
    updatedAt: "2026-09-01T08:00:00Z",
    ...over,
  };
}
