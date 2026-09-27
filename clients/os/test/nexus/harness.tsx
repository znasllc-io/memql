import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Nexus app's test harness: a connection-shaped double.
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

/**
 * A BUILTIN'S reply, in the builtin's WIRE shape: ONE value keyed by node id,
 * each entry a node envelope with the fields under `payload` -- which the
 * SDK's `Result.rows()` unwraps. Answering a builtin with flat rows would hide
 * exactly the unwrap the surface depends on (memory: a builtin reply is one
 * id-keyed map). `idOf` names each node; a builtin answering several rows
 * gives each a DISTINCT id, or the map collapses them into one.
 */
export function builtinReply(name: string, rows: Row[], idOf: (row: Row, index: number) => string = (row, index) => (typeof row["id"] === "string" && row["id"] !== "" ? (row["id"] as string) : `${name}-${index}`)): Result {
  const wrapper: Record<string, unknown> = {};
  rows.forEach((row, index) => {
    const id = idOf(row, index);
    const { id: _own, ...payload } = row as Record<string, unknown>;
    void _own;
    wrapper[id] = {
      id,
      concept: `integration:work:${name}`,
      type: "object",
      // Monotonically DECREASING in slice order, the engine's own stamp for a
      // handler that preserves order (executor_builtin.go, mapToSortedSlice).
      createdAt: `2026-01-01T00:00:00.${String(999_999_999 - index).padStart(9, "0")}Z`,
      payload,
    };
  });
  return new Result({ data: rows.length === 0 ? [] : [wrapper] } as never);
}

/**
 * `workStepVersions`' reply: one entry PER VERSION, keyed `<stepId>@v<version>`
 * -- the distinct id per version the contract names -- with the step row's
 * fields and `current` in the payload.
 */
export function versionsReply(rows: Row[]): Result {
  return builtinReply("stepVersions", rows, (row) => `${String(row["id"])}@v${String(row["version"])}`);
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
  /** What `replayRun` answers with. */
  deriveReply?: Row;
  /** The authoring catalog the Automations section reads. */
  constructs?: Row[] | Error;
  /** The learned procedures, every rung (`learnedProceduresForOwner`). */
  learnedProcedures?: Row[] | Error;
  // --- epic memql#5414: stepping into a run -------------------------------
  /** Every version of every step (`workStepVersions`), as `stepVersionRow`s. EMPTY by default. */
  versions?: Row[] | Error;
  /** What `rerunStep` answers: `{runId, stepKey, version, staleSteps}`. */
  rerunReply?: Row;
  /** What `branchRun` answers: `{runId, forkedFromRunId, forkAtStepKey}`. */
  branchReply?: Row;
  /** What `moveRunHead` answers: `{runId, stepKey, version, staleSteps}`. */
  headReply?: Row;
  /** What `recordFeedback` answers: `{observationId, verdict, validatorDisagrees}`. */
  feedbackReply?: Row;
  /** The app sessions a step opened (`appSessionsForStep`). */
  sessions?: Row[] | Error;
}

export function fakeConnection(seed: FakeSeed = {}) {
  const receipts: Row[] = [];
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
  // A BUILTIN write, answered in the builtin wire shape. Refuses with
  // `writeError` like every other write here.
  const builtinWrite = (name: string, reply: (args: Record<string, unknown>) => Row) =>
    vi.fn(async (args: Record<string, unknown>, _opts?: unknown) => {
      if (seed.writeError) throw seed.writeError;
      return builtinReply(name, [reply(args)]);
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
      // The authoring catalog. NOT a feed -- `v1:authoring:construct` carries
      // no broadcast routing rule -- so the section reads it and dates itself.
      cataloguedConstructsForOwner: read(seed.constructs),
      learnedProceduresForOwner: read(seed.learnedProcedures),
      setConstructStatus: write(),
      // Every version of every step: a BUILTIN, so it answers in the wire
      // shape -- one map keyed `<stepId>@v<version>`.
      workStepVersions: vi.fn(async (_args: Record<string, unknown>, _opts?: unknown) => {
        if (seed.versions instanceof Error) throw seed.versions;
        return versionsReply(seed.versions ?? []);
      }),
      appSessionsForStep: read(seed.sessions),
      rerunStep: builtinWrite("rerunStep", (args) =>
        seed.rerunReply ?? { id: "rerun", runId: args["runId"], stepKey: args["stepKey"], version: 2, staleSteps: [] },
      ),
      branchRun: builtinWrite("branchRun", (args) =>
        seed.branchReply ?? {
          id: "branch",
          runId: "run-branch",
          forkedFromRunId: args["runId"],
          forkAtStepKey: args["stepKey"],
        },
      ),
      moveRunHead: builtinWrite("moveRunHead", (args) =>
        seed.headReply ?? { id: "head", runId: args["runId"], stepKey: args["stepKey"], version: args["version"], staleSteps: [] },
      ),
      recordFeedback: builtinWrite("recordFeedback", (args) =>
        seed.feedbackReply ?? { id: "obs-new", observationId: "obs-new", verdict: args["verdict"], validatorDisagrees: false },
      ),
      setConstructReuse: builtinWrite("setConstructReuse", (args) => ({
        id: String(args["constructId"]),
        constructId: args["constructId"],
        reuse: args["label"],
      })),
      feedbackPolicyCurrent: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult([{ id: "v1:work:feedbackPolicy:primary", validateAnswers: true, reusableAfterSignatures: 2 }]),
      ),
      workModelCallsForOwnerRun: read(seed.modelCalls),
      workObservationsForOwnerRun: read(seed.observations),
      // TYPED ARGS, so `.mock.calls[0][0]` is a record rather than `never` --
      // a test that asserts WHICH arguments a write received cannot do it
      // through a `vi.fn(async () => ...)` whose parameter list is empty.
      createGoal: write(seed.createReply),
      cancelGoal: write(),
      // `forkRun` is RETIRED (epic memql#5414) and deliberately still answered
      // here, so a test can assert NOTHING calls it -- a method the fake did
      // not have would fail the call loudly and prove less.
      forkRun: write(seed.deriveReply),
      replayRun: write(seed.deriveReply),
      decideApproval: write(),
      // The shared attention service's two calls (src/attention), so a suite
      // can mount the real AttentionProvider over this connection.
      myAttentionReceipts: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        builtinReply("myAttentionReceipts", receipts),
      ),
      acknowledgeAttention: vi.fn(async (args: Record<string, unknown>) => {
        receipts.push({ id: `${String(args["changeId"])}:${String(args["revision"])}`, ...args });
        return builtinReply("acknowledgeAttention", []);
      }),
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
          role: overrides.role ?? "owner",
          roleName: "",
          rank: 0,
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

export function constructRow(over: Partial<Row> & { id: string; name: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    bundleId: "v1:authoring:bundle:b1",
    // AUTOMATION BY DEFAULT, because that is the kind this app draws -- a test
    // that wants the other kinds filtered out has to ASK for one.
    kind: "automation",
    targetNamespace: "owner.me",
    source: "automation nightlyReconcile { }",
    status: "active",
    catalogued: true,
    goalSignature: "sha256:goal-1",
    reliability: 0.95,
    reinforceCount: 12,
    lastReinforced: "2026-09-04T09:00:00Z",
    catalogedAt: "2026-08-20T09:00:00Z",
    catalogedFromBundleId: "v1:authoring:bundle:b1",
    createdAt: "2026-08-20T09:00:00Z",
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

// ---------------------------------------------------------------------------
// Stepping into a run (epic memql#5414)
// ---------------------------------------------------------------------------

/**
 * One VERSION of one step, as `workStepVersions` answers it: the step row's
 * fields plus `version` and `current`. `id` is the STEP's row id -- the reply
 * keys each version `<id>@v<version>` itself (see `versionsReply`). Version 1,
 * current and untouched by default, so a test has to ASK for an override, an
 * author or an earlier head.
 */
export function stepVersionRow(
  over: Partial<Row> & { id: string; key: string; seq: number; version: number },
): Row {
  return {
    ...stepRow({ id: over.id, key: over.key, seq: over.seq }),
    attempt: over.version,
    override: {},
    authoredBy: "",
    current: true,
    ...over,
  };
}

/** A person's verdict as the journal holds it: a `feedback` observation in D's shape. */
export function feedbackObservation(
  over: Partial<Row> & {
    id: string;
    verdict: "like" | "dislike" | "neutral";
    stepKey?: string;
    version?: number;
    axes?: { product?: boolean; process?: boolean; performance?: boolean };
    reason?: string;
    validatorDisagrees?: boolean;
  },
): Row {
  const { verdict, stepKey, version, axes, reason, validatorDisagrees, ...rest } = over;
  return {
    ownerUserId: "v1:identity:user:me",
    runId: "run-1",
    stepKey: stepKey ?? "",
    kind: "feedback",
    content: `${verdict} ${stepKey ?? "the run"}`,
    data: {
      verdict,
      axes: { product: false, process: false, performance: false, ...axes },
      reason: reason ?? "",
      target: stepKey === undefined ? {} : { stepKey, version: version ?? 1 },
      ...(validatorDisagrees === undefined ? {} : { validatorDisagrees }),
    },
    createdAt: "2026-09-01T10:00:00Z",
    ...rest,
  };
}

/** The validator's `decision` observation: an axis set to true is a problem found on it. */
export function validatorObservation(
  over: Partial<Row> & {
    id: string;
    verdict: "pass" | "flag";
    stepKey: string;
    version: number;
    axes?: { product?: boolean; process?: boolean; performance?: boolean };
    reason?: string;
  },
): Row {
  const { verdict, stepKey, version, axes, reason, ...rest } = over;
  return {
    ownerUserId: "v1:identity:user:me",
    runId: "run-1",
    stepKey,
    kind: "decision",
    content: `validator ${verdict}`,
    data: {
      validator: {
        verdict,
        axes: { product: false, process: false, performance: false, ...axes },
        reason: reason ?? "",
      },
      target: { stepKey, version },
      level: "strong",
      model: "claude-sonnet-4-5",
    },
    createdAt: "2026-09-01T09:06:00Z",
    ...rest,
  };
}

/** One app session a step opened, as `appSessionsForStep` answers it. */
export function sessionRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    workerId: "v1:worker:registration:w1",
    app: "claude-code",
    kind: "run",
    runId: "run-1",
    stepId: "run-1-draft",
    status: "ended",
    prompt: "Draft the weekly report from the fetched figures.",
    sessionRunId: "run-session-1",
    startedAt: "2026-09-01T09:01:00Z",
    createdAt: "2026-09-01T09:01:00Z",
    ...over,
  };
}
