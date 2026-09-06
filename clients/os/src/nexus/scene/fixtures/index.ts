// The worlds the scene library is tested against.
//
// Four, and each one exists because a different thing goes wrong without it:
//
//   springCatalogGoal   the prototype's own goal, the shape the owner saw and
//                       corrected. The readable case: three phases, a handful
//                       of specialists, a bundle with constructs, artifacts,
//                       one retried step. Every "does the picture look right"
//                       assertion is written against this.
//   denseGoal(300)      the budget fixture. What a phase over the collapse
//                       threshold does, and the input the frame-time and
//                       separation assertions run on.
//   emptyGoal           a goal with no tasks yet. The state a map is in for
//                       the first seconds of every plan, and the one that
//                       divides "nothing to draw" from "failed to read".
//   failedGoal          a goal that stopped, with a task still running when
//                       it did. Drives the danger tone and the failure half
//                       of the receipt.
//   cancelledGoal       stopped by a person rather than by an error, which
//                       the receipt has to say differently (memql#4376).
//
// The timestamps are fixed literals, never `new Date()`. A fixture built off
// the clock makes a determinism test that passes today and fails at midnight,
// and makes a scrub assertion depend on when it ran.

import type {
  AgentRow,
  ArtifactRow,
  BundleRow,
  ConstructRow,
  DependencyEdgeRow,
  GoalWorld,
  PlanRow,
  TaskRow,
} from "../world";

// A fixed base moment. Every stamp below is an offset from it, written out
// rather than computed so a reader can see the ordering without arithmetic.
const T = (minute: number, second = 0): string => {
  const base = Date.UTC(2026, 7, 20, 9, 0, 0);
  return new Date(base + minute * 60_000 + second * 1000).toISOString().replace(/\.\d{3}Z$/, "Z");
};

function plan(overrides: Partial<PlanRow> = {}): PlanRow {
  return {
    id: "plan-spring",
    goal: "Build a spring catalog from the supplier sheets",
    kind: "userGoal",
    status: "succeeded",
    requestedBy: "user-1",
    ownerAgentId: "agent-planner",
    phases: [
      { name: "gather", startedAt: T(1), completedAt: T(6) },
      { name: "shape", startedAt: T(6), completedAt: T(14) },
      { name: "publish", startedAt: T(14), completedAt: T(21) },
    ],
    tokenSpent: 184_320,
    tokenBudget: 400_000,
    tokenSpentSubscription: 0,
    hasTokenSpentSubscription: false,
    errorMessage: "",
    cancelledBy: "",
    createdAt: T(0),
    startedAt: T(1),
    completedAt: T(21),
    ...overrides,
  };
}

function task(over: Partial<TaskRow> & { id: string; seq: number; phase: string }): TaskRow {
  return {
    planId: "plan-spring",
    category: "semantic",
    kind: over.id,
    logicalStepId: "",
    attemptNumber: 1,
    parentTaskId: "",
    toolName: "",
    status: "succeeded",
    errorMessage: "",
    createdAt: T(1),
    startedAt: T(1),
    completedAt: T(2),
    ...over,
  };
}

function agent(id: string, name: string, createdAt: string, kind = "specialist"): AgentRow {
  return { id, name, kind, role: kind === "system" ? "specialist" : kind, roleSlug: "", createdAt };
}

const PLANNER: AgentRow = agent("agent-planner", "MemQL Planner", T(0), "system");

const SPECIALISTS: AgentRow[] = [
  agent("agent-sheets", "Sheet Reader", T(1)),
  agent("agent-copy", "Catalog Copywriter", T(7)),
  agent("agent-layout", "Layout Specialist", T(15)),
];

const BUNDLE: BundleRow = {
  id: "bundle-spring",
  title: "Spring catalog capture",
  summary: "The reads and the automation this goal needed",
  status: "active",
  sourcePlanId: "plan-spring",
  failureReason: "",
  validationReport: { errors: [], checked: 4 },
  dryRunReport: { rowsTouched: 0, passed: true },
  createdAt: T(16),
  activatedAt: T(20),
  retiredAt: "",
};

const CONSTRUCTS: ConstructRow[] = [
  {
    id: "construct-supplierSheet",
    bundleId: "bundle-spring",
    kind: "concept",
    name: "supplierSheet",
    targetNamespace: "catalog",
    source: "concept supplierSheet {\n  supplier string!\n  season   string!\n}\n",
    status: "active",
    createdAt: T(16),
  },
  {
    id: "construct-sheetsForSeason",
    bundleId: "bundle-spring",
    kind: "query",
    name: "sheetsForSeason",
    targetNamespace: "catalog",
    source:
      "query supplierSheet sheetsForSeason {\n  args {\n    season string!\n  }\n  filter season==args.season\n}\n",
    status: "active",
    createdAt: T(17),
  },
  {
    id: "construct-onSheetLanded",
    bundleId: "bundle-spring",
    kind: "automation",
    name: "onSheetLanded",
    targetNamespace: "catalog",
    source: "automation onSheetLanded { }\n",
    status: "staged",
    createdAt: T(18),
  },
];

const EDGES: DependencyEdgeRow[] = [
  {
    id: "edge-1",
    bundleId: "bundle-spring",
    fromConstruct: "sheetsForSeason",
    fromKind: "query",
    toName: "supplierSheet",
    toKind: "concept",
    toSource: "bundle",
  },
  {
    id: "edge-2",
    bundleId: "bundle-spring",
    fromConstruct: "onSheetLanded",
    fromKind: "automation",
    toName: "sheetsForSeason",
    toKind: "query",
    toSource: "bundle",
  },
];

const ARTIFACTS: ArtifactRow[] = [
  {
    id: "artifact-catalog",
    title: "Spring 2026 catalog",
    summary: "48 pages, 212 products",
    kind: "generated_output",
    format: "document",
    producedByPlanId: "plan-spring",
    createdAt: T(19),
  },
  {
    id: "artifact-pricing",
    title: "Pricing sheet",
    summary: "",
    kind: "generated_output",
    format: "spreadsheet",
    producedByPlanId: "plan-spring",
    createdAt: T(20),
  },
];

// The readable goal. One retried step (`shape-normalise`, two attempts on one
// logicalStepId) and two tool invocations hanging off it, which is what makes
// the counter and the re-light testable at all.
export function springCatalogGoal(): GoalWorld {
  const tasks: TaskRow[] = [
    task({ id: "gather-fetch", seq: 1, phase: "gather", createdAt: T(1), startedAt: T(1), completedAt: T(3) }),
    task({ id: "gather-verify", seq: 2, phase: "gather", createdAt: T(1), startedAt: T(3), completedAt: T(6) }),
    task({
      id: "shape-normalise-a1",
      kind: "shape-normalise",
      seq: 3,
      phase: "shape",
      logicalStepId: "step-normalise",
      attemptNumber: 1,
      status: "failed",
      errorMessage: "supplier column missing",
      createdAt: T(6),
      startedAt: T(6),
      completedAt: T(8),
    }),
    task({
      id: "shape-normalise-a2",
      kind: "shape-normalise",
      seq: 3,
      phase: "shape",
      logicalStepId: "step-normalise",
      attemptNumber: 2,
      status: "succeeded",
      createdAt: T(8),
      startedAt: T(8),
      completedAt: T(11),
    }),
    task({ id: "shape-price", seq: 4, phase: "shape", createdAt: T(8), startedAt: T(11), completedAt: T(14) }),
    task({ id: "publish-render", seq: 5, phase: "publish", createdAt: T(14), startedAt: T(14), completedAt: T(19) }),
    task({ id: "publish-index", seq: 6, phase: "publish", createdAt: T(14), startedAt: T(19), completedAt: T(21) }),
    // Tool invocations: not nodes, they tick their parent's counter (D2).
    task({
      id: "tool-read-1",
      seq: 7,
      phase: "shape",
      category: "toolInvocation",
      parentTaskId: "shape-normalise-a2",
      toolName: "workbenchHost",
      createdAt: T(9),
      startedAt: T(9),
      completedAt: T(9, 30),
    }),
    task({
      id: "tool-read-2",
      seq: 8,
      phase: "shape",
      category: "toolInvocation",
      parentTaskId: "shape-normalise-a2",
      toolName: "workbenchHost",
      createdAt: T(10),
      startedAt: T(10),
      completedAt: T(10, 20),
    }),
  ];

  return {
    plan: plan(),
    planner: PLANNER,
    agents: [PLANNER, ...SPECIALISTS],
    tasks,
    bundle: BUNDLE,
    constructs: CONSTRUCTS,
    edges: EDGES,
    artifacts: ARTIFACTS,
  };
}

// denseGoal builds a goal whose single phase carries `count` semantic tasks.
// Used for the collapse threshold and for the budget assertions; `count` is a
// parameter rather than a constant so a test can sit either side of the
// threshold without a second fixture.
export function denseGoal(count = 300): GoalWorld {
  const tasks: TaskRow[] = [];
  for (let i = 0; i < count; i += 1) {
    tasks.push(
      task({
        id: `bulk-${String(i).padStart(4, "0")}`,
        seq: i + 1,
        phase: "sweep",
        createdAt: T(1),
        startedAt: T(1 + (i % 30)),
        completedAt: T(2 + (i % 30)),
      }),
    );
  }
  return {
    plan: plan({
      id: "plan-dense",
      goal: "Reconcile every supplier row",
      phases: [{ name: "sweep", startedAt: T(1), completedAt: T(40) }],
    }),
    planner: PLANNER,
    agents: [PLANNER, ...SPECIALISTS],
    tasks: tasks.map((t) => ({ ...t, planId: "plan-dense" })),
    bundle: null,
    constructs: [],
    edges: [],
    artifacts: [],
  };
}

// A goal that has been set and nothing else. The first seconds of every plan.
export function emptyGoal(): GoalWorld {
  return {
    plan: plan({
      id: "plan-empty",
      goal: "Summarise last quarter",
      status: "queued",
      phases: [],
      tokenSpent: 0,
      startedAt: "",
      completedAt: "",
    }),
    planner: PLANNER,
    agents: [PLANNER],
    tasks: [],
    bundle: null,
    constructs: [],
    edges: [],
    artifacts: [],
  };
}

export function failedGoal(): GoalWorld {
  const world = springCatalogGoal();
  return {
    ...world,
    plan: plan({
      id: "plan-failed",
      status: "failed",
      errorMessage: "the supplier feed stopped responding",
      completedAt: T(12),
    }),
    tasks: world.tasks
      .filter((t) => t.seq <= 5)
      .map((t) =>
        t.id === "shape-price"
          ? { ...t, status: "running", completedAt: "", startedAt: T(11) }
          : t,
      ),
    bundle: null,
    constructs: [],
    edges: [],
    artifacts: [],
  };
}

export function cancelledGoal(): GoalWorld {
  const world = springCatalogGoal();
  return {
    ...world,
    plan: plan({
      id: "plan-cancelled",
      status: "cancelled",
      cancelledBy: "user-1",
      completedAt: T(9),
    }),
    tasks: world.tasks.filter((t) => t.seq <= 3),
    bundle: null,
    constructs: [],
    edges: [],
    artifacts: [],
  };
}

// Exported so tests can name the fixture's own moments instead of retyping
// stamps that have to agree with the ones above.
export const MOMENT = T;
