// How rows become a world, and stay one while the goal is worked.
//
// Pure, and separate from the hook that wires it (useGoalWorld.ts), for the
// same reason src/concepts/liveBand.ts is separate from useConceptRows: the
// invariants worth asserting here -- a duplicate `created` leaves one node, an
// out-of-order `updated` does not roll a row backwards, a seed that lands
// after a follow event does not overwrite it -- are assertions about a fold,
// and asserting them through render() and waitFor() puts three layers between
// the claim and the thing claimed.
//
// ===========================================================================
// THE THREE LANDMINES IN THE FEED, AND WHAT EACH ONE COSTS
// ===========================================================================
// From the design's section 1, measured against the engine rather than
// assumed:
//
//   1. `graph.node.created` fires on every write INCLUDING updates
//      (component/memql/executor_mutation.go). Keyed by row id, so a second
//      `created` for a row already held is an update, not a second node. This
//      is why the state is a Map and never an array.
//
//   2. Delivery order is not guaranteed. Two events for one row can settle in
//      either order, and each is resolved through its own read -- so the LAST
//      read to settle is not necessarily the FRESHEST row. Every slot
//      therefore carries a watermark read off the row's own timestamps, and a
//      row whose watermark is older than the one already held is dropped.
//
//   3. CDC has no replay. The world is seeded from queries and then followed,
//      and the seed can settle after the first follow events have already
//      landed. The watermark rule covers that too: a seed row older than a
//      followed one loses, which is the correct answer and is why the race
//      does not need a barrier.
//
// ===========================================================================
// EVERY ROW IS FILTERED BY ITS PLAN POINTER
// ===========================================================================
// `plan` and `task` are undeclared today (memql#4366), so a subscription to
// `plan` delivers rows from other people's goals. `agent` and `bundle` are in
// the same long tail. The client filters every arrival by the pointer that
// ties it to THIS goal -- and that filter is here, in one function, rather
// than sprinkled through seven subscription handlers where the eighth would
// forget it.
//
// This is a client-side filter and it is recorded as one. It is not a
// substitute for the declaration; it is what the design's section 7 asks for
// while the declaration is pending ("the client filters by their plan
// pointers, and the residual is recorded in the undeclared gate, not hidden
// here").

import { rowObject, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import {
  readAgent,
  readArtifact,
  readBundle,
  readConstruct,
  readDependencyEdge,
  readPlan,
  readTask,
  type GoalWorld,
} from "../scene/world";

// One slot per concept the feed follows. A string union rather than the
// concept ids themselves so a switch over it is exhaustive at compile time --
// the ids are strings and a typo in one is a handler that silently never
// fires.
export type FeedSlot =
  | "plan"
  | "task"
  | "agent"
  | "bundle"
  | "construct"
  | "edge"
  | "artifact";

export interface FeedState {
  plan: Row | null;
  // The plan's ownerAgentId, read separately (dsl/agents/queries.memql:
  // agentsForPlan's header explains why it is not part of that read).
  planner: Row | null;
  tasks: Map<string, Row>;
  agents: Map<string, Row>;
  bundle: Row | null;
  constructs: Map<string, Row>;
  edges: Map<string, Row>;
  artifacts: Map<string, Row>;
}

export const EMPTY_FEED: FeedState = {
  plan: null,
  planner: null,
  tasks: new Map(),
  agents: new Map(),
  bundle: null,
  constructs: new Map(),
  edges: new Map(),
  artifacts: new Map(),
};

// watermark is "how current is this copy of the row".
//
// Each concept's own latest dated transition, because that is the only
// monotonic thing on the wire: MemQL is append-only and none of these
// concepts carries a generic `updatedAt` except `artifact`. Where a concept
// records nothing but its creation, the watermark is the creation -- which
// makes two copies of it indistinguishable, and that is correct: they are the
// same row and neither is fresher.
export function watermark(slot: FeedSlot, row: Row): string {
  switch (slot) {
    case "plan":
    case "task":
      return (
        rowString(row, "completedAt") ||
        rowString(row, "startedAt") ||
        rowString(row, "createdAt")
      );
    case "bundle":
      return (
        rowString(row, "retiredAt") ||
        rowString(row, "activatedAt") ||
        rowString(row, "createdAt")
      );
    case "artifact":
      return rowString(row, "updatedAt") || rowString(row, "createdAt");
    case "agent":
    case "construct":
    case "edge":
      return rowString(row, "createdAt");
  }
}

// supersedes decides whether an arriving copy replaces the one held.
//
// `>=` rather than `>`: an equal watermark means the two copies describe the
// same state, and taking the arriving one costs nothing while refusing it
// would pin a row to whatever the seed happened to project. A row with NO
// watermark at all (a projection that dropped the timestamps) always wins,
// because the alternative is a row that can never be updated again.
export function supersedes(slot: FeedSlot, incoming: Row, held: Row | undefined): boolean {
  if (held === undefined) return true;
  const a = watermark(slot, incoming);
  const b = watermark(slot, held);
  if (a === "" || b === "") return true;
  return a >= b;
}

// belongsToPlan is the client-side narrowing. See this file's header.
//
// `bundleId` is threaded in because a construct and a dependency edge point
// at their BUNDLE rather than at the plan, so "does this belong to my goal"
// is only answerable once the goal's bundle is known. Before then a construct
// event is refused -- which is right: a construct that arrives before its
// bundle cannot be placed, and the bundle's own arrival re-seeds them.
export function belongsToPlan(
  slot: FeedSlot,
  row: Row,
  planId: string,
  bundleId: string,
): boolean {
  switch (slot) {
    case "plan":
      return rowString(row, "id") === planId;
    case "task":
      return rowString(row, "planId") === planId;
    case "agent": {
      // Nested under `lineage` on the wire, and the flattened CDC envelope
      // keeps the nested object rather than hoisting its leaves -- so this
      // reads the object and then the leaf, never a top-level
      // "lineage.originatingPlanId" key that does not exist.
      const lineage = rowObject(row, "lineage");
      const origin = typeof lineage?.["originatingPlanId"] === "string"
        ? (lineage["originatingPlanId"] as string)
        : "";
      return origin === planId;
    }
    case "bundle":
      return rowString(row, "sourcePlanId") === planId;
    case "construct":
    case "edge":
      return bundleId !== "" && rowString(row, "bundleId") === bundleId;
    case "artifact":
      return rowString(row, "producedByPlanId") === planId;
  }
}

// foldRow applies one arrival. Returns the SAME state object when nothing
// changed, so a React setState on the result is a no-op re-render rather than
// a fresh identity every time an unrelated event arrives.
export function foldRow(state: FeedState, slot: FeedSlot, row: Row): FeedState {
  const id = rowString(row, "id");
  if (id === "") return state;

  switch (slot) {
    case "plan":
      if (!supersedes("plan", row, state.plan ?? undefined)) return state;
      return { ...state, plan: row };
    case "bundle":
      if (!supersedes("bundle", row, state.bundle ?? undefined)) return state;
      return { ...state, bundle: row };
    default: {
      const key = collectionFor(slot);
      const current = state[key];
      if (!supersedes(slot, row, current.get(id))) return state;
      const next = new Map(current);
      next.set(id, row);
      return { ...state, [key]: next };
    }
  }
}

// dropRow removes a row the cluster says is gone. Rare -- these concepts are
// append-only -- but a `deleted` event whose re-read finds nothing is the one
// case where the honest answer is to take the node off the map.
export function dropRow(state: FeedState, slot: FeedSlot, rowId: string): FeedState {
  switch (slot) {
    case "plan":
      return state.plan === null ? state : { ...state, plan: null };
    case "bundle":
      return state.bundle === null ? state : { ...state, bundle: null };
    default: {
      const key = collectionFor(slot);
      if (!state[key].has(rowId)) return state;
      const next = new Map(state[key]);
      next.delete(rowId);
      return { ...state, [key]: next };
    }
  }
}

type CollectionSlot = Exclude<FeedSlot, "plan" | "bundle">;
type CollectionKey = "tasks" | "agents" | "constructs" | "edges" | "artifacts";

function collectionFor(slot: CollectionSlot): CollectionKey {
  switch (slot) {
    case "task":
      return "tasks";
    case "agent":
      return "agents";
    case "construct":
      return "constructs";
    case "edge":
      return "edges";
    case "artifact":
      return "artifacts";
  }
}

// setPlanner is separate from foldRow because the planner is not a member of
// any followed collection: it is one agent resolved by the plan's
// ownerAgentId, and it also appears in `agents` when its lineage happens to
// name the plan. Held in its own slot so the layout can put it where it
// belongs (between you and the work) rather than in the specialist grid.
export function setPlanner(state: FeedState, row: Row | null): FeedState {
  return { ...state, planner: row };
}

// worldFromFeed narrows the held rows into the shape the scene library
// consumes. Sorting is NOT done here -- layout() and events() each impose
// their own total order, and a second one would be a second answer.
export function worldFromFeed(state: FeedState): GoalWorld {
  const planner = readPlanner(state);
  const agents = [...state.agents.values()].map(readAgent);
  // The planner is included in `agents` so the receipt's "agents raised"
  // count and the event stream both see it, and DE-DUPLICATED because it is
  // legitimately in both slots when its lineage names this plan.
  if (planner !== null && !agents.some((agent) => agent.id === planner.id)) {
    agents.unshift(planner);
  }
  return {
    plan: readPlan(state.plan),
    planner,
    agents,
    tasks: [...state.tasks.values()].map(readTask),
    bundle: readBundle(state.bundle),
    constructs: [...state.constructs.values()].map(readConstruct),
    edges: [...state.edges.values()].map(readDependencyEdge),
    artifacts: [...state.artifacts.values()].map(readArtifact),
  };
}

function readPlanner(state: FeedState): ReturnType<typeof readAgent> | null {
  if (state.planner === null) return null;
  const agent = readAgent(state.planner);
  return agent.id === "" ? null : agent;
}
