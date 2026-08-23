// The rows a goal's world is made of, and the node identities drawn from
// them.
//
// ===========================================================================
// WHY THIS FILE EXISTS AT ALL
// ===========================================================================
// Everything else in scene/ is a pure function over a GoalWorld, which is
// what makes layout, events and time-travel testable without a GPU, without a
// server and without React. That only works if "the world" is a settled data
// shape rather than whatever the wire happened to send -- so this module owns
// the narrowing: SDK Rows in, plain typed records out, every field read
// defensively because the wire is JSON and a missing key is a normal thing,
// not an exception.
//
// NOTHING HERE IMPORTS three.js, and nothing in scene/ may. The Map renders
// what these functions return; if a rule about where a node goes needs a
// renderer to express it, the rule is in the wrong place.
//
// ===========================================================================
// NODE IDENTITY IS NOT ROW IDENTITY, AND THE DIFFERENCE IS THE RETRY
// ===========================================================================
// A retried step is a NEW task row -- `attemptNumber` increments and
// `logicalStepId` stays put (dsl/planner/concepts.memql). The map must
// re-light the node that already exists rather than grow a second cube beside
// it, so a task's NODE id keys on `logicalStepId` when the row carries one
// and falls back to the row id when it does not.
//
// That is why a SceneNode carries `rowId` separately from `id`: the node is
// the thing on screen and in the URL, the row is what /node/:nodeId re-reads
// through the authorized path. For every kind except a retried task the two
// agree; for a retried task `rowId` names the LATEST attempt, which is the
// one an operator opening the node wants to read.

import { rowArray, rowNumber, rowObject, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import {
  AGENT_CONCEPT_ID,
  ARTIFACT_CONCEPT_ID,
  BUNDLE_CONCEPT_ID,
  CONSTRUCT_CONCEPT_ID,
  DEPENDENCY_EDGE_CONCEPT_ID,
  PLAN_CONCEPT_ID,
  TASK_CONCEPT_ID,
} from "../concepts";

// ---------------------------------------------------------------------------
// The row shapes
// ---------------------------------------------------------------------------

// A phase as the plan records it. The planner writes `phases[]` as an opaque
// object list; only these three keys are read, and any of them may be absent.
export interface PlanPhase {
  name: string;
  startedAt: string;
  completedAt: string;
}

export interface PlanRow {
  id: string;
  goal: string;
  kind: string;
  status: string;
  requestedBy: string;
  ownerAgentId: string;
  phases: PlanPhase[];
  tokenSpent: number;
  tokenBudget: number;
  // The subscription-covered figure lands on the plan once epic memql#4358
  // ships. Absent today; read as 0 and RENDERED ONLY when the field is
  // actually present, which is why presence is carried separately (see
  // scene/receipt.ts) rather than inferred from a zero.
  tokenSpentSubscription: number;
  hasTokenSpentSubscription: boolean;
  errorMessage: string;
  cancelledBy: string;
  createdAt: string;
  startedAt: string;
  completedAt: string;
}

export interface TaskRow {
  id: string;
  planId: string;
  // "semantic" | "toolInvocation". Only semantic tasks become nodes (design
  // D2); a toolInvocation ticks its parent's counter.
  category: string;
  kind: string;
  logicalStepId: string;
  attemptNumber: number;
  parentTaskId: string;
  toolName: string;
  status: string;
  seq: number;
  phase: string;
  errorMessage: string;
  createdAt: string;
  startedAt: string;
  completedAt: string;
}

export interface AgentRow {
  id: string;
  name: string;
  // "assistant" | "specialist" | "system". The planner is a system agent.
  kind: string;
  role: string;
  roleSlug: string;
  createdAt: string;
}

export interface BundleRow {
  id: string;
  title: string;
  summary: string;
  status: string;
  sourcePlanId: string;
  failureReason: string;
  validationReport: Record<string, unknown> | null;
  dryRunReport: Record<string, unknown> | null;
  createdAt: string;
  activatedAt: string;
  retiredAt: string;
}

export interface ConstructRow {
  id: string;
  bundleId: string;
  kind: string;
  name: string;
  targetNamespace: string;
  source: string;
  status: string;
  createdAt: string;
}

export interface DependencyEdgeRow {
  id: string;
  bundleId: string;
  fromConstruct: string;
  fromKind: string;
  toName: string;
  toKind: string;
  toSource: string;
}

export interface ArtifactRow {
  id: string;
  title: string;
  summary: string;
  kind: string;
  format: string;
  producedByPlanId: string;
  createdAt: string;
}

// GoalWorld is one goal, whole. `plan === null` is the honest shape for "not
// read yet / refused", and every downstream function returns an empty scene
// for it rather than throwing -- a map with no goal is a legitimate state
// (the picker is empty, the read was refused, the link names a plan that is
// not yours).
export interface GoalWorld {
  plan: PlanRow | null;
  // The plan's ownerAgentId, resolved separately from the raised specialists
  // because it is a pointer on the PLAN rather than a lineage stamp on the
  // agent (dsl/agents/queries.memql: agentsForPlan's own header).
  planner: AgentRow | null;
  agents: AgentRow[];
  tasks: TaskRow[];
  bundle: BundleRow | null;
  constructs: ConstructRow[];
  edges: DependencyEdgeRow[];
  artifacts: ArtifactRow[];
}

export const EMPTY_WORLD: GoalWorld = {
  plan: null,
  planner: null,
  agents: [],
  tasks: [],
  bundle: null,
  constructs: [],
  edges: [],
  artifacts: [],
};

// ---------------------------------------------------------------------------
// Reading rows
// ---------------------------------------------------------------------------

// phasesOf narrows plan.phases[] -- an []object on the concept, so every
// entry is an arbitrary map and the three keys read here may each be absent.
// A non-object entry is DROPPED rather than rendered as a nameless phase.
function phasesOf(row: Row): PlanPhase[] {
  const raw = rowArray(row, "phases") ?? [];
  const out: PlanPhase[] = [];
  for (const entry of raw) {
    if (!entry || typeof entry !== "object" || Array.isArray(entry)) continue;
    const phase = entry as Record<string, unknown>;
    out.push({
      name: typeof phase["name"] === "string" ? phase["name"] : "",
      startedAt: typeof phase["startedAt"] === "string" ? phase["startedAt"] : "",
      completedAt: typeof phase["completedAt"] === "string" ? phase["completedAt"] : "",
    });
  }
  return out;
}

export function readPlan(row: Row | null): PlanRow | null {
  if (row === null) return null;
  const id = rowString(row, "id");
  if (id === "") return null;
  return {
    id,
    goal: rowString(row, "goal"),
    kind: rowString(row, "kind"),
    status: rowString(row, "status"),
    requestedBy: rowString(row, "requestedBy"),
    ownerAgentId: rowString(row, "ownerAgentId"),
    phases: phasesOf(row),
    tokenSpent: rowNumber(row, "tokenSpent"),
    tokenBudget: rowNumber(row, "tokenBudget"),
    tokenSpentSubscription: rowNumber(row, "tokenSpentSubscription"),
    // PRESENCE, not truthiness. The field arrives with epic memql#4358; a
    // plan that spent nothing on the subscription and a plan whose engine
    // does not carry the field at all are different facts, and the receipt
    // renders only the first.
    hasTokenSpentSubscription: typeof row["tokenSpentSubscription"] === "number",
    errorMessage: rowString(row, "errorMessage"),
    cancelledBy: rowString(row, "cancelledBy"),
    createdAt: rowString(row, "createdAt"),
    startedAt: rowString(row, "startedAt"),
    completedAt: rowString(row, "completedAt"),
  };
}

export function readTask(row: Row): TaskRow {
  return {
    id: rowString(row, "id"),
    planId: rowString(row, "planId"),
    category: rowString(row, "category") || "semantic",
    kind: rowString(row, "kind"),
    logicalStepId: rowString(row, "logicalStepId"),
    // Defaults to 1 rather than 0: the concept's own @default is "1", and a
    // zero would make the first attempt read as "no attempt yet".
    attemptNumber: rowNumber(row, "attemptNumber") || 1,
    parentTaskId: rowString(row, "parentTaskId"),
    toolName: rowString(row, "toolName"),
    status: rowString(row, "status") || "queued",
    seq: rowNumber(row, "seq"),
    phase: rowString(row, "phase"),
    errorMessage: rowString(row, "errorMessage"),
    createdAt: rowString(row, "createdAt"),
    startedAt: rowString(row, "startedAt"),
    completedAt: rowString(row, "completedAt"),
  };
}

export function readAgent(row: Row): AgentRow {
  return {
    id: rowString(row, "id"),
    name: rowString(row, "name"),
    kind: rowString(row, "kind"),
    role: rowString(row, "role"),
    roleSlug: rowString(row, "roleSlug"),
    createdAt: rowString(row, "createdAt"),
  };
}

export function readBundle(row: Row | null): BundleRow | null {
  if (row === null) return null;
  const id = rowString(row, "id");
  if (id === "") return null;
  return {
    id,
    title: rowString(row, "title"),
    summary: rowString(row, "summary"),
    status: rowString(row, "status") || "draft",
    sourcePlanId: rowString(row, "sourcePlanId"),
    failureReason: rowString(row, "failureReason"),
    validationReport: rowObject(row, "validationReport"),
    dryRunReport: rowObject(row, "dryRunReport"),
    createdAt: rowString(row, "createdAt"),
    activatedAt: rowString(row, "activatedAt"),
    retiredAt: rowString(row, "retiredAt"),
  };
}

export function readConstruct(row: Row): ConstructRow {
  return {
    id: rowString(row, "id"),
    bundleId: rowString(row, "bundleId"),
    kind: rowString(row, "kind"),
    name: rowString(row, "name"),
    targetNamespace: rowString(row, "targetNamespace"),
    source: rowString(row, "source"),
    status: rowString(row, "status") || "draft",
    createdAt: rowString(row, "createdAt"),
  };
}

export function readDependencyEdge(row: Row): DependencyEdgeRow {
  return {
    id: rowString(row, "id"),
    bundleId: rowString(row, "bundleId"),
    fromConstruct: rowString(row, "fromConstruct"),
    fromKind: rowString(row, "fromKind"),
    toName: rowString(row, "toName"),
    toKind: rowString(row, "toKind"),
    toSource: rowString(row, "toSource"),
  };
}

export function readArtifact(row: Row): ArtifactRow {
  return {
    id: rowString(row, "id"),
    title: rowString(row, "title"),
    summary: rowString(row, "summary"),
    kind: rowString(row, "kind"),
    format: rowString(row, "format"),
    producedByPlanId: rowString(row, "producedByPlanId"),
    createdAt: rowString(row, "createdAt"),
  };
}

// ---------------------------------------------------------------------------
// Node identity
// ---------------------------------------------------------------------------

// The kinds a scene node can be. `you` and `goal` are singletons; `cluster`
// is the collapsed stand-in for a phase over the density threshold (design
// 4.2) and is the only kind with no row behind it.
export type NodeKind =
  | "you"
  | "goal"
  | "planner"
  | "specialist"
  | "task"
  | "cluster"
  | "artifact"
  | "construct"
  | "bundle";

// The two singleton node ids. Written as constants because the URL, the
// layout and the event stream all name them and a typo in one of the three
// is a node that never lights.
export const YOU_NODE_ID = "you";
export const GOAL_NODE_ID = "goal";

// nodeIdFor composes a node id from a kind and a row key. The separator is a
// tilde rather than a colon so a node id never looks like a concept id --
// which matters because these end up in a URL segment beside `/node/`, and
// a reader (or a future route matcher) should not have to work out which of
// the two grammars is in play.
export function nodeIdFor(kind: NodeKind, key: string): string {
  return `${kind}~${key}`;
}

// taskNodeId keys on logicalStepId when the row carries one, so every
// attempt of a retried step lands on ONE node. See this file's header.
export function taskNodeId(task: TaskRow): string {
  return nodeIdFor("task", task.logicalStepId === "" ? task.id : task.logicalStepId);
}

export function clusterNodeId(phase: string): string {
  return nodeIdFor("cluster", phase);
}

// conceptIdForKind maps a node back to the concept its row lives in, which
// is what /nexus/:planId/node/:nodeId needs to perform the authorized re-read
// through getRowByConceptAndId. `you` and `cluster` have no row: `you` is the
// caller (there is a whole /me surface for that) and a cluster is a drawing
// device, so both return "" and the detail route renders nothing for them.
export function conceptIdForKind(kind: NodeKind): string {
  switch (kind) {
    case "goal":
      return PLAN_CONCEPT_ID;
    case "task":
      return TASK_CONCEPT_ID;
    case "planner":
    case "specialist":
      return AGENT_CONCEPT_ID;
    case "bundle":
      return BUNDLE_CONCEPT_ID;
    case "construct":
      return CONSTRUCT_CONCEPT_ID;
    case "artifact":
      return ARTIFACT_CONCEPT_ID;
    case "you":
    case "cluster":
      return "";
  }
}

// Exported so the feed can name the edge concept without importing the
// concepts module a second time; dependency edges are rows the map draws
// BETWEEN construct nodes rather than nodes of their own, so they have no
// NodeKind.
export const EDGE_CONCEPT_ID = DEPENDENCY_EDGE_CONCEPT_ID;

// semanticTasks is the D2 filter, in one place: only semantic tasks become
// nodes. Everything else in the scene library calls this rather than testing
// `category` inline, because "which tasks are nodes" is a product decision
// and it should be possible to read it once.
export function semanticTasks(tasks: readonly TaskRow[]): TaskRow[] {
  return tasks.filter((task) => task.category !== "toolInvocation");
}

// toolInvocationsByParent counts the tool calls hanging off each semantic
// task, which is the counter the map ticks on a running node (design D2).
// Keyed by the PARENT's node id so a retry's invocations accumulate on the
// same node the step already owns.
export function toolInvocationsByParent(tasks: readonly TaskRow[]): Map<string, number> {
  const byRowId = new Map<string, TaskRow>();
  for (const task of tasks) byRowId.set(task.id, task);

  const counts = new Map<string, number>();
  for (const task of tasks) {
    if (task.category !== "toolInvocation") continue;
    const parent = byRowId.get(task.parentTaskId);
    // An invocation whose parent is not in this world is DROPPED rather than
    // counted against a synthetic node. It means the parent was filtered out
    // (a different plan, or a scrub that predates it), and inventing a node
    // to hang the count on would draw a task that does not exist.
    if (parent === undefined) continue;
    const key = taskNodeId(parent);
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  return counts;
}

// latestAttempts collapses a retried step to its highest attempt, which is
// the row a node's status and detail read come from. Ties (two rows with the
// same attemptNumber, which the planner should not write but the wire cannot
// forbid) break on createdAt and then on id, so the answer is deterministic.
export function latestAttempts(tasks: readonly TaskRow[]): Map<string, TaskRow> {
  const best = new Map<string, TaskRow>();
  for (const task of tasks) {
    const key = taskNodeId(task);
    const current = best.get(key);
    if (current === undefined || outranks(task, current)) best.set(key, task);
  }
  return best;
}

function outranks(candidate: TaskRow, incumbent: TaskRow): boolean {
  if (candidate.attemptNumber !== incumbent.attemptNumber) {
    return candidate.attemptNumber > incumbent.attemptNumber;
  }
  if (candidate.createdAt !== incumbent.createdAt) {
    return candidate.createdAt > incumbent.createdAt;
  }
  return candidate.id > incumbent.id;
}
