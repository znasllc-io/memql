// Fixtures for the scene library.
//
// TYPED WORLDS, NOT WIRE ROWS, for everything except the reader tests. The
// readers in world.ts are what turn a Row into a typed record and they have
// their own fixtures below; every other function in this library takes a
// GoalWorld, so building one directly is what keeps a layout test about layout
// rather than about JSON narrowing.
//
// Every builder is DETERMINISTIC and takes no clock: a fixture that read
// `Date.now()` would make `layout(sameWorld)` twice a different question on
// either side of midnight.

import type {
  ApprovalRow,
  GoalRow,
  GoalWorld,
  RunRow,
  StepBinding,
  StepRow,
} from "../world";

const T0 = "2026-09-05T09:00:00Z";

/** A moment `minutes` after the fixture epoch, as the wire would carry it. */
export function moment(minutes: number): string {
  const base = Date.parse(T0);
  return new Date(base + minutes * 60_000).toISOString().replace(/\.\d{3}Z$/, "Z");
}

const NO_BINDING: StepBinding = {
  provider: "",
  model: "",
  surface: "",
  workerId: "",
  nodeId: "",
  skillIds: [],
  present: false,
};

export function goal(over: Partial<GoalRow> = {}): GoalRow {
  return {
    id: "v1:work:goal:g1",
    statement: "Ship the Q4 pricing page",
    origin: "user",
    status: "active",
    requestedVia: "nexus",
    closeReason: "",
    closedAt: "",
    createdAt: moment(0),
    ...over,
  };
}

export function run(over: Partial<RunRow> = {}): RunRow {
  return {
    id: "v1:work:run:r1",
    goalId: "v1:work:goal:g1",
    automationName: "shipPricingPage",
    templateConstructId: "",
    templateVersion: "",
    mode: "live",
    forkedFromRunId: "",
    forkAtStepKey: "",
    status: "running",
    waitingKind: "",
    waitingSubject: "",
    waitingSince: "",
    spent: {
      tokens: 0,
      tokensSubscription: 0,
      cost: 0,
      modelCalls: 0,
      retries: 0,
      wallClockMs: 0,
      present: true,
    },
    stepOrder: [],
    cancelledBy: "",
    errorMessage: "",
    createdAt: moment(0),
    startedAt: moment(1),
    finishedAt: "",
    ...over,
  };
}

export function step(key: string, over: Partial<StepRow> = {}): StepRow {
  return {
    id: `v1:work:step:${key}-1`,
    runId: "v1:work:run:r1",
    key,
    seq: 0,
    stepType: "query",
    kind: "deterministic",
    callName: key,
    dependsOn: [],
    status: "done",
    symptom: "",
    attempt: 1,
    binding: NO_BINDING,
    approvalId: "",
    childRunId: "",
    tokens: 0,
    cost: 0,
    durationMs: 120,
    errorMessage: "",
    createdAt: moment(1),
    startedAt: moment(2),
    finishedAt: moment(3),
    ...over,
  };
}

export function approval(over: Partial<ApprovalRow> = {}): ApprovalRow {
  return {
    id: "v1:work:approval:a1",
    runId: "v1:work:run:r1",
    stepKey: "",
    kind: "sideEffect",
    subject: "send the announcement",
    question: "",
    tier: "B",
    reason: "spend ceiling",
    ruleId: "spend-ceiling",
    decision: "",
    requestedAt: moment(6),
    decidedAt: "",
    createdAt: moment(6),
    ...over,
  };
}

export function world(over: Partial<GoalWorld> = {}): GoalWorld {
  const theRun = over.run ?? run();
  return {
    goal: goal(),
    run: theRun,
    runs: over.runs ?? [theRun],
    steps: [],
    approvals: [],
    ...over,
  };
}

/** A goal whose run is a straight chain: each step waits on the one before. */
export function chainWorld(length: number, status = "done"): GoalWorld {
  const steps: StepRow[] = [];
  for (let i = 0; i < length; i += 1) {
    steps.push(
      step(`s${i}`, {
        seq: i,
        status,
        dependsOn: i === 0 ? [] : [`s${i - 1}`],
        createdAt: moment(1 + i),
        startedAt: status === "pending" ? "" : moment(2 + i),
        finishedAt: status === "done" ? moment(3 + i) : "",
      }),
    );
  }
  return world({ steps });
}

/** A run that fans out: one root, `width` steps depending on it, one join. */
export function fanOutWorld(width: number, status = "running"): GoalWorld {
  const steps: StepRow[] = [step("root", { seq: 0, status: "done" })];
  const middle: string[] = [];
  for (let i = 0; i < width; i += 1) {
    const key = `p${i}`;
    middle.push(key);
    steps.push(
      step(key, {
        seq: 1 + i,
        status,
        dependsOn: ["root"],
        finishedAt: status === "done" ? moment(5) : "",
      }),
    );
  }
  steps.push(
    step("join", { seq: 1 + width, status: "pending", dependsOn: middle, startedAt: "", finishedAt: "" }),
  );
  return world({ steps });
}

/** The same step, three times, as a retry writes it. */
export function retryWorld(): GoalWorld {
  return world({
    steps: [
      { ...step("flaky", { seq: 0, status: "failed", attempt: 1 }), id: "v1:work:step:flaky-1" },
      { ...step("flaky", { seq: 0, status: "failed", attempt: 2 }), id: "v1:work:step:flaky-2" },
      { ...step("flaky", { seq: 0, status: "done", attempt: 3 }), id: "v1:work:step:flaky-3" },
    ],
  });
}

/**
 * Rows exactly as the wire carries them, for the reader tests.
 *
 * DELIBERATELY MISSING KEYS on the second entry of each: the readers exist to
 * survive a projection gap, and a fixture where every key is present tests
 * nothing they are for.
 */
export const wireRows = {
  goal: {
    id: "v1:work:goal:g1",
    statement: "Ship the Q4 pricing page",
    origin: "user",
    status: "active",
    requestedVia: "nexus",
    createdAt: moment(0),
  } as Record<string, unknown>,
  sparseGoal: { id: "v1:work:goal:g2" } as Record<string, unknown>,
  run: {
    id: "v1:work:run:r1",
    goalId: "v1:work:goal:g1",
    automationName: "shipPricingPage",
    status: "running",
    mode: "live",
    spent: { tokens: 1200, cost: 0.41, modelCalls: 3 },
    waitingOn: { kind: "approval", subject: "v1:work:approval:a1", since: moment(6) },
    stepOrder: ["s0", "s1"],
    createdAt: moment(0),
    startedAt: moment(1),
  } as Record<string, unknown>,
  sparseRun: { id: "v1:work:run:r2" } as Record<string, unknown>,
  step: {
    id: "v1:work:step:s0-2",
    runId: "v1:work:run:r1",
    key: "s0",
    seq: 0,
    stepType: "function",
    kind: "reasoning",
    call: { construct: "logic", name: "decide" },
    dependsOn: ["root"],
    status: "done",
    attempt: 2,
    binding: { provider: "anthropic", model: "claude-opus-5", skillIds: ["s:a", "s:b"] },
    tokens: 900,
    cost: 0.12,
    durationMs: 1200,
    createdAt: moment(1),
    startedAt: moment(2),
    finishedAt: moment(3),
  } as Record<string, unknown>,
  sparseStep: { id: "v1:work:step:x", runId: "v1:work:run:r1", key: "x" } as Record<
    string,
    unknown
  >,
  approval: {
    id: "v1:work:approval:a1",
    runId: "v1:work:run:r1",
    stepKey: "s1",
    kind: "sideEffect",
    subject: "send the announcement",
    evidence: { tier: "B", reason: "spend ceiling", ruleId: "spend-ceiling" },
    requestedAt: moment(6),
    createdAt: moment(6),
  } as Record<string, unknown>,
};
