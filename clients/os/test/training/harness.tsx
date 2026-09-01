import type { ReactNode } from "react";
import { act, fireEvent } from "@testing-library/react";
import { vi } from "vitest";
import { QueryClient, Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { AuthSourceProvider } from "../../src/auth/context";
import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Training app's test harness.
//
// ===========================================================================
// THE FAKE SITS UNDER `executeNamed`, NOT OVER THE GENERATED METHODS
// ===========================================================================
// A double that stubs `query.setChunkValidationStatus` records the ARGUMENTS
// and never renders the call, so the generated builder -- the thing that turns
// those arguments into MemQL text the engine has to parse -- runs in
// production and nowhere else. That is how a feature ships green and fails at
// parse on every call.
//
// So the stub is given `QueryClient.prototype` and answers at `executeNamed`,
// which is what every generated method funnels through. The tests then assert
// the STRING that reached the wire, which is the artefact the engine actually
// sees.
//
// Everything else is connection-shaped for the reason the Fleet's, Users' and
// Deployables' harnesses are: every read goes through `connection.query` and
// every subscription through `connection.subscriptions`, so a fake answering
// those two exercises the real LiveCollection, the real retain / seed path and
// the real projections.

/** The `data` envelope a SHAPE-PROJECTED query returns. */
export function rowsResult(rows: Row[], cursor = ""): Result {
  return new Result({ data: rows, ...(cursor === "" ? {} : { meta: { cursor } }) } as never);
}

/**
 * A BUNDLE envelope -- a different wire shape, and not interchangeable.
 *
 * `getRowByConceptAndId` reads `rawNodes()`, which looks at
 * `payload.bundle.nodes` and nothing else, so a `data` envelope makes a by-id
 * re-read answer null. That failure is silent by design (a null re-read means
 * "keep what you have"), so the wrong envelope would make a re-read look like
 * it was correctly falling back when in fact it never read anything.
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
  /** The test's hand on the wire. */
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

export interface FakeSeed {
  /** Rows `plansForUser` answers with. */
  plans?: Row[];
  /** Rows `allDocumentChunkDomains` answers with (the LITE shape). */
  domainRows?: Row[];
  /**
   * Pages `documentChunksForDomain` answers with, per domain.
   *
   * A list of PAGES rather than a list of rows, so a test can exercise the
   * keyset walk: every page but the last mints a cursor, and the last mints
   * none -- which is exactly what the engine does (a cursor is minted only on
   * a full page).
   */
  chunkPages?: Record<string, Row[][]>;
  /** The user's active space id. Absent = a user row with no pointer. */
  activeSpaceId?: string;
  /** Fails `userActiveSpace` with this server message. */
  activeSpaceError?: string;
  /** Fails `allDocumentChunkDomains` with this server message. */
  domainsError?: string;
  /** Fails the next `setChunkValidationStatus` with this server message. */
  decisionError?: string;
}

export interface FakeConnection {
  query: QueryClient;
  /** Every call string that reached the wire, in order. */
  calls: string[];
  callsNamed: (construct: string) => string[];
  subscriptions: FakeSubscriptions;
  dispatcher: { sendAndWait: ReturnType<typeof vi.fn> };
}

export function fakeConnection(seed: FakeSeed = {}): FakeConnection {
  const calls: string[] = [];
  // Per-domain page cursors, so a repeated read of the same domain walks
  // forward the way the engine's keyset does rather than answering page 1
  // forever.
  const pageIndex = new Map<string, number>();

  const stub = {
    executeNamed: vi.fn(async (_name: string, call: string, opts?: { cursor?: string }) => {
      calls.push(call);

      if (call === "query plansForUser()") return rowsResult(seed.plans ?? []);

      if (call === "query allDocumentChunkDomains()") {
        if (seed.domainsError !== undefined) throw new Error(seed.domainsError);
        return rowsResult(seed.domainRows ?? []);
      }

      if (call.startsWith("query userActiveSpace(")) {
        if (seed.activeSpaceError !== undefined) throw new Error(seed.activeSpaceError);
        return rowsResult([
          { id: "u-me", ...(seed.activeSpaceId ? { activePartitionId: seed.activeSpaceId } : {}) },
        ]);
      }

      if (call.startsWith("query documentChunksForDomain(")) {
        const domainId = /domainId: "([^"]*)"/.exec(call)?.[1] ?? "";
        const pages = seed.chunkPages?.[domainId] ?? [];
        // The cursor a caller passes back is `page:<n>`; its absence is a
        // first page. Reading it from `opts` rather than from a counter is
        // what makes this a test of the walk rather than of the fake.
        const index = opts?.cursor ? Number(opts.cursor.split(":")[1] ?? 0) : 0;
        pageIndex.set(domainId, index);
        const page = pages[index] ?? [];
        const last = index >= pages.length - 1;
        return rowsResult(page, last ? "" : `page:${index + 1}`);
      }

      if (call.startsWith("mutation setChunkValidationStatus(")) {
        if (seed.decisionError !== undefined) throw new Error(seed.decisionError);
        return rowsResult([]);
      }

      // `getRowByConceptAndId` composes `concept==<c> && id==<id>`; nothing in
      // these tests seeds a by-id re-read, so it answers empty rather than
      // throwing -- a null re-read means "keep what you have".
      return bundleResult([]);
    }),
  };

  return {
    query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
  };
}

export function withSession(
  children: ReactNode,
  overrides: { userId?: string; role?: string; domain?: string; bearer?: string | null } = {},
) {
  const config: OsRuntimeConfig = {
    ...UNKNOWN_RUNTIME_CONFIG,
    domain: overrides.domain ?? "memql.example.com",
  };
  const bearer = overrides.bearer === undefined ? "tok-test" : overrides.bearer;
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "u-me",
          primaryEmail: "owner@example.com",
          clusterRole: overrides.role ?? "owner",
        },
        config,
      }}
    >
      <AuthSourceProvider source={{ bearer: async () => bearer, refresh: async () => bearer }}>
        {children}
      </AuthSourceProvider>
    </SessionProvider>
  );
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

export function planRow(over: Partial<Row> & { id: string }): Row {
  return {
    partitionId: "space-1",
    kind: "analyzeFile",
    status: "queued",
    goal: "Analyze notes.pdf",
    requestedBy: "u-me",
    errorMessage: "",
    completedAt: "",
    createdAt: "2026-08-30T10:00:00Z",
    ...over,
  };
}

/** The caller's own, mid-flight. */
export const RUNNING_PLAN = planRow({
  id: "plan-running",
  status: "running",
  goal: "Analyze contract.pdf",
});

/** The caller's own, finished. */
export const DONE_PLAN = planRow({
  id: "plan-done",
  status: "succeeded",
  goal: "Analyze handbook.docx",
  completedAt: "2026-08-30T10:05:00Z",
});

/** The caller's own, failed, carrying the engine's reason. */
export const FAILED_PLAN = planRow({
  id: "plan-failed",
  status: "failed",
  goal: "Analyze broken.pdf",
  errorMessage: "extract: unsupported pdf encoding",
  completedAt: "2026-08-30T10:06:00Z",
});

/** SOMEBODY ELSE'S. It reaches the browser because `v1:planner:plan` declares
 *  no tier, so the subscription admits every subscriber. */
export const OTHER_USERS_PLAN = planRow({
  id: "plan-theirs",
  requestedBy: "u-someone-else",
  status: "running",
  goal: "Analyze theirs.pdf",
});

/** The caller's own, but a different KIND of work. */
export const GOAL_PLAN = planRow({
  id: "plan-goal",
  kind: "userGoal",
  status: "running",
  goal: "Ship the quarterly report",
});

export function chunkRow(over: Partial<Row> & { id: string }): Row {
  return {
    domainId: "domain-sales",
    text: "Net 30 terms apply to every wholesale order.",
    sourceRef: "doc:terms.pdf#p2",
    seq: 0,
    tokenCount: 12,
    documentId: "",
    source: "fileUpload",
    sourceTopic: "",
    validationStatus: "unvalidated",
    superseded: false,
    supersededReason: "",
    createdAt: "2026-08-30T09:00:00Z",
    ...over,
  };
}

/** A lite-shape row: what `allDocumentChunkDomains` returns per chunk. */
export function domainLiteRow(domainId: string, validationStatus: string): Row {
  return { domainId, validationStatus };
}

// ---------------------------------------------------------------------------
// Interaction helpers
// ---------------------------------------------------------------------------
//
// Clicks and emitted events go through act(): a state update outside it is not
// flushed before the next assertion, which reads exactly like a control that
// did nothing.

export async function click(el: Element | null | undefined): Promise<void> {
  if (!el) throw new Error("click() was handed nothing to click");
  await act(async () => {
    fireEvent.click(el);
  });
}

/** Push a graph event onto the fake wire and let React settle. */
export async function emit(
  connection: FakeConnection,
  concept: string,
  payload: Row,
  kind = "NODE_UPDATED",
): Promise<void> {
  await act(async () => {
    connection.subscriptions.emit(concept, payload, kind);
  });
}

/** Let every pending microtask land. The queue walk is a loop of awaited
 *  reads, so one flush is not always enough. */
export async function settle(times = 3): Promise<void> {
  for (let i = 0; i < times; i += 1) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}
