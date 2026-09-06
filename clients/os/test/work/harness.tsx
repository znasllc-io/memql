import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Work app's test harness: a connection-shaped double.
//
// CONNECTION-SHAPED rather than a mocked hook per call site, for the reason
// every harness in this suite records: every read goes through
// `connection.query.<generated method>` and every subscription through
// `connection.subscriptions`, so a fake that answers those exercises the real
// LiveCollection, the real retain/seed path, the real projections and the real
// arrival fold -- which is where the behaviour under test actually lives.

export function rowsResult(rows: Row[]): Result {
  return new Result({ data: rows } as never);
}

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

function fakeSubscriptions() {
  const handlers = new Map<string, Set<(event: FakeEvent) => void>>();
  return {
    subscribeGraph(handler: (event: FakeEvent) => void, opts: { concept?: string }) {
      const concept = opts.concept ?? "*";
      const set = handlers.get(concept) ?? new Set();
      set.add(handler);
      handlers.set(concept, set);
      return () => set.delete(handler);
    },
    emit(concept: string, payload: Row, kind = "NODE_UPDATED") {
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
  goals?: Row[];
  runs?: Row[];
  approvals?: Row[];
  accounts?: Row[];
  steps?: Row[] | Error;
  modelCalls?: Row[] | Error;
  observations?: Row[] | Error;
  byId?: Record<string, Row>;
  /** Make a write REFUSE, which is the interesting case while the executors
   *  are still being written: the surface must show the server's sentence. */
  writeError?: Error;
  /** What `createGoal` answers with. The builtin returns {goalId, runId}. */
  createReply?: Row;
  /** What `forkRun` / `replayRun` answer with. */
  deriveReply?: Row;
}

export function fakeConnection(seed: FakeSeed = {}) {
  const read = (rows: Row[] | Error | undefined) =>
    vi.fn(async (_args: Record<string, unknown>) => {
      if (rows instanceof Error) throw rows;
      return rowsResult(rows ?? []);
    });
  const write = (reply?: Row) =>
    vi.fn(async (_args: Record<string, unknown>) => {
      if (seed.writeError) throw seed.writeError;
      return rowsResult(reply ? [reply] : []);
    });
  return {
    query: {
      // TYPED ARGS EVEN ON THE NO-ARGUMENT READS. A `vi.fn(async () => ...)`
      // has an empty parameter list, so `.mock.calls[0][0]` is a tuple of
      // length zero and `tsc -b` -- which covers test/ -- refuses the index.
      // A test that asserts a read was issued UNFILTERED needs that index.
      workGoalsForOwner: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.goals ?? []),
      ),
      workRunsForOwner: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.runs ?? []),
      ),
      workApprovalsForOwner: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.approvals ?? []),
      ),
      // The account roster the goal detail resolves tags through and the
      // create form offers. Seeded EMPTY by default -- which is the state
      // most tests want -- but present, because a read this surface issues
      // and the fake does not answer is one whose absence looks like a
      // feature that works.
      clientAccountsAll: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.accounts ?? []),
      ),
      workStepsForOwnerRun: read(seed.steps),
      workModelCallsForOwnerRun: read(seed.modelCalls),
      workObservationsForOwnerRun: read(seed.observations),
      // TYPED ARGS, so `.mock.calls[0][0]` is a record rather than `never` --
      // a test that asserts WHICH arguments a write received cannot do it
      // through a `vi.fn(async () => ...)` whose parameter list is empty.
      createGoal: write(seed.createReply),
      cancelGoal: write(),
      forkRun: write(seed.deriveReply),
      replayRun: write(seed.deriveReply),
      decideApproval: write(),
      executeNamed: vi.fn(async (_name: string, filter: string) => {
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

export type FakeConnection = ReturnType<typeof fakeConnection>;

export function withSession(children: ReactNode, overrides: { role?: string } = {}) {
  const config: OsRuntimeConfig = { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" };
  return (
    <SessionProvider
      value={{
        access: {
          userId: "v1:identity:user:me",
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

// ---------------------------------------------------------------------------
// Row builders, with defaults chosen so a test has to ASK for the interesting
// state rather than getting it by accident.
// ---------------------------------------------------------------------------

export function goalRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    statement: "Reconcile last month's ledger against the bank export",
    origin: "user",
    responsibilityId: "",
    accountIds: [],
    status: "active",
    requestedVia: "nexus",
    closedAt: "",
    closeReason: "",
    createdAt: "2026-09-01T09:00:00Z",
    ...over,
  };
}

export function runRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    goalId: "",
    automationName: "nightlyReconcile",
    mode: "live",
    replayPolicy: "strict",
    status: "succeeded",
    templateFingerprint: "fp-1",
    // A HEARTBEAT BY DEFAULT, so a test that emits a NEW one is emitting the
    // update that must NOT ring rather than inventing a field.
    heartbeatAt: "2026-09-01T09:05:00Z",
    cancelRequested: false,
    errorCode: "",
    errorMessage: "",
    stepOrder: [],
    startedAt: "2026-09-01T09:00:10Z",
    finishedAt: "2026-09-01T09:05:00Z",
    createdAt: "2026-09-01T09:00:10Z",
    ...over,
  };
}

export function stepRow(over: Partial<Row> & { id: string; key: string; seq: number }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    runId: "run-1",
    stepType: "query",
    kind: "deterministic",
    status: "done",
    attempt: 1,
    symptom: "",
    dependsOn: [],
    errorCode: "",
    errorMessage: "",
    createdAt: "2026-09-01T09:00:11Z",
    ...over,
  };
}

export function approvalRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    runId: "run-1",
    stepKey: "sendInvoice",
    kind: "sideEffect",
    artifactHash: "sha256:abc123",
    question: "",
    options: [],
    decision: "",
    decidedBy: "",
    decidedAt: "",
    expiresAt: "",
    requestedAt: "2026-09-01T09:04:00Z",
    createdAt: "2026-09-01T09:04:00Z",
    ...over,
  };
}
