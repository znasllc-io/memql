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
  renameWorker: ReturnType<typeof vi.fn>;
  setWorkerOperatorLabels: ReturnType<typeof vi.fn>;
  revokeWorker: ReturnType<typeof vi.fn>;
  createRoutingPolicy: ReturnType<typeof vi.fn>;
  updateRoutingPolicy: ReturnType<typeof vi.fn>;
}

export interface FakeConnection {
  query: FakeQuery;
  subscriptions: null;
  dispatcher: { sendAndWait: ReturnType<typeof vi.fn> };
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
      renameWorker: vi.fn(async () => rowsResult([])),
      setWorkerOperatorLabels: vi.fn(async () => rowsResult([])),
      revokeWorker: vi.fn(async () => rowsResult([])),
      createRoutingPolicy: vi.fn(async () => rowsResult([])),
      updateRoutingPolicy: vi.fn(async () => rowsResult([])),
    },
    subscriptions: null,
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
