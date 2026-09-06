// The rows a goal's world is made of, and the node identities drawn from them.
//
// ===========================================================================
// WHY THIS FILE EXISTS AT ALL
// ===========================================================================
// Everything else in scene/ is a pure function over a GoalWorld, which is what
// makes layout, events and time-travel testable without a GPU, without a
// server and without React. That only works if "the world" is a settled data
// shape rather than whatever the wire happened to send -- so this module owns
// the narrowing: SDK Rows in, plain typed records out, every field read
// defensively because the wire is JSON and a missing key is a normal thing,
// not an exception.
//
// NOTHING HERE IMPORTS A RENDERER, and nothing in scene/ may. The map draws
// what these functions return; if a rule about where a node goes needs a
// renderer to express it, the rule is in the wrong place. MemQL OS carries no
// WebGL at all (owner requirement, epic memql#4785) and
// `test/nexus/map.test.tsx` enforces it in both directions.
//
// ===========================================================================
// NODE IDENTITY IS NOT ROW IDENTITY, AND THE DIFFERENCE IS THE RETRY
// ===========================================================================
// A retried step is a NEW step row: `attempt` increments and `key` stays put
// (dsl/work/concepts.memql). The map must re-light the node that already
// exists rather than grow a second mark beside it, so a step's NODE id keys on
// `runId:key` and never on the row id.
//
// That is why a LayoutNode carries `rowId` separately from `id`: the node is
// the thing on screen and in the URL, the row is what the detail panel
// re-reads through the authorized path. For every kind except a retried step
// the two agree; for a retried step `rowId` names the LATEST attempt, which is
// the one somebody opening the node wants to read.
//
// ===========================================================================
// TIMESTAMPS ARE COMPARED AS STRINGS, EVERYWHERE IN THIS LIBRARY
// ===========================================================================
// RFC3339 in a fixed offset sorts lexicographically, MemQL writes UTC `Z`
// stamps, and a string compare cannot silently produce `NaN` the way
// `new Date("")` does -- which is the failure that turns a missing timestamp
// into a node at the origin rather than a node the code declines to place.

import { rowArray, rowNumber, rowObject, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import {
  APPROVAL_CONCEPT_ID,
  GOAL_CONCEPT_ID,
  RUN_CONCEPT_ID,
  STEP_CONCEPT_ID,
} from "../concepts";

// ---------------------------------------------------------------------------
// The row shapes
// ---------------------------------------------------------------------------

export interface GoalRow {
  id: string;
  statement: string;
  origin: string;
  status: string;
  requestedVia: string;
  closeReason: string;
  closedAt: string;
  createdAt: string;
}

/** `{tokens, cost, modelCalls, retries, wallClockMs, ...}` off `run.spent`. */
export interface RunSpend {
  tokens: number;
  tokensSubscription: number;
  cost: number;
  modelCalls: number;
  retries: number;
  wallClockMs: number;
  /**
   * Whether the row carried `spent` at all. A run that has spent nothing and a
   * run whose engine does not report spend are different facts, and the
   * receipt renders an ABSENT line for the second rather than a zero.
   */
  present: boolean;
}

export interface RunRow {
  id: string;
  goalId: string;
  automationName: string;
  templateConstructId: string;
  templateVersion: string;
  mode: string;
  forkedFromRunId: string;
  forkAtStepKey: string;
  status: string;
  /** `{kind, subject, since}` while status is waiting; empty strings otherwise. */
  waitingKind: string;
  waitingSubject: string;
  waitingSince: string;
  spent: RunSpend;
  stepOrder: string[];
  cancelledBy: string;
  errorMessage: string;
  createdAt: string;
  startedAt: string;
  finishedAt: string;
}

export interface StepBinding {
  provider: string;
  model: string;
  surface: string;
  workerId: string;
  nodeId: string;
  skillIds: string[];
  /** False when the row carries no binding: nothing ran it YET, not "nothing". */
  present: boolean;
}

export interface StepRow {
  id: string;
  runId: string;
  key: string;
  seq: number;
  stepType: string;
  kind: string;
  callName: string;
  dependsOn: string[];
  status: string;
  symptom: string;
  attempt: number;
  binding: StepBinding;
  approvalId: string;
  childRunId: string;
  tokens: number;
  cost: number;
  durationMs: number;
  errorMessage: string;
  createdAt: string;
  startedAt: string;
  finishedAt: string;
}

export interface ApprovalRow {
  id: string;
  runId: string;
  stepKey: string;
  kind: string;
  subject: string;
  question: string;
  /** `evidence.tier` -- the safety tier the classifier assigned. */
  tier: string;
  reason: string;
  ruleId: string;
  decision: string;
  requestedAt: string;
  decidedAt: string;
  createdAt: string;
}

/**
 * One goal, whole.
 *
 * `goal === null` is the honest shape for "not read yet / refused", and every
 * downstream function returns an empty scene for it rather than throwing -- a
 * map with no goal is a legitimate state (the list is empty, the read was
 * refused, the link names a goal that is not yours).
 *
 * `run` is ONE run of that goal, not all of them: a goal's progress toward
 * being done is one run's progress, and a replay or a fork is a different
 * attempt rather than further progress. The goal view's run picker chooses
 * which; `runs` carries the whole set so the picker can be drawn and so the
 * header can say how many there are.
 */
export interface GoalWorld {
  goal: GoalRow | null;
  run: RunRow | null;
  runs: RunRow[];
  steps: StepRow[];
  approvals: ApprovalRow[];
}

export const EMPTY_WORLD: GoalWorld = {
  goal: null,
  run: null,
  runs: [],
  steps: [],
  approvals: [],
};

// ---------------------------------------------------------------------------
// Reading rows
// ---------------------------------------------------------------------------

function stringsOf(row: Row, field: string): string[] {
  const raw = rowArray(row, field) ?? [];
  const out: string[] = [];
  for (const entry of raw) {
    if (typeof entry === "string" && entry !== "") out.push(entry);
  }
  return out;
}

function nested(source: Record<string, unknown> | null, key: string): string {
  if (source === null) return "";
  const value = source[key];
  return typeof value === "string" ? value : "";
}

function nestedNumber(source: Record<string, unknown> | null, key: string): number {
  if (source === null) return 0;
  const value = source[key];
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

export function readGoal(row: Row | null): GoalRow | null {
  if (row === null) return null;
  const id = rowString(row, "id");
  if (id === "") return null;
  return {
    id,
    statement: rowString(row, "statement"),
    origin: rowString(row, "origin"),
    status: rowString(row, "status"),
    requestedVia: rowString(row, "requestedVia"),
    closeReason: rowString(row, "closeReason"),
    closedAt: rowString(row, "closedAt"),
    createdAt: rowString(row, "createdAt"),
  };
}

export function readRun(row: Row | null): RunRow | null {
  if (row === null) return null;
  const id = rowString(row, "id");
  if (id === "") return null;
  const waiting = rowObject(row, "waitingOn");
  const spent = rowObject(row, "spent");
  return {
    id,
    goalId: rowString(row, "goalId"),
    automationName: rowString(row, "automationName"),
    templateConstructId: rowString(row, "templateConstructId"),
    templateVersion: rowString(row, "templateVersion"),
    mode: rowString(row, "mode"),
    forkedFromRunId: rowString(row, "forkedFromRunId"),
    forkAtStepKey: rowString(row, "forkAtStepKey"),
    status: rowString(row, "status"),
    waitingKind: nested(waiting, "kind"),
    waitingSubject: nested(waiting, "subject"),
    waitingSince: nested(waiting, "since"),
    spent: {
      tokens: nestedNumber(spent, "tokens"),
      tokensSubscription: nestedNumber(spent, "tokensSubscription"),
      cost: nestedNumber(spent, "cost"),
      modelCalls: nestedNumber(spent, "modelCalls"),
      retries: nestedNumber(spent, "retries"),
      wallClockMs: nestedNumber(spent, "wallClockMs"),
      present: spent !== null,
    },
    stepOrder: stringsOf(row, "stepOrder"),
    cancelledBy: rowString(row, "cancelledBy"),
    errorMessage: rowString(row, "errorMessage"),
    createdAt: rowString(row, "createdAt"),
    startedAt: rowString(row, "startedAt"),
    finishedAt: rowString(row, "finishedAt"),
  };
}

export function readStep(row: Row): StepRow {
  const binding = rowObject(row, "binding");
  const call = rowObject(row, "call");
  const attempt = rowNumber(row, "attempt");
  const skills = binding === null ? [] : binding["skillIds"];
  return {
    id: rowString(row, "id"),
    runId: rowString(row, "runId"),
    key: rowString(row, "key"),
    seq: rowNumber(row, "seq"),
    stepType: rowString(row, "stepType"),
    kind: rowString(row, "kind"),
    callName: nested(call, "name"),
    dependsOn: stringsOf(row, "dependsOn"),
    status: rowString(row, "status"),
    symptom: rowString(row, "symptom"),
    // A step row that carries no attempt is the FIRST attempt, not a zeroth
    // one: `attempt` is `int!` with `@default("1")`, so an absent value is a
    // projection gap and 1 is the only reading that keeps `latestAttempts`
    // total.
    //
    // WRITTEN AS A COMPARISON RATHER THAN `?? 1`, and the difference is not
    // style: `rowNumber` returns 0 for an absent key, never null, so a
    // null-coalesce here would compile, read correctly, and never fire.
    attempt: attempt > 0 ? attempt : 1,
    binding: {
      provider: nested(binding, "provider"),
      model: nested(binding, "model"),
      surface: nested(binding, "surface"),
      workerId: nested(binding, "workerId"),
      nodeId: nested(binding, "nodeId"),
      skillIds: Array.isArray(skills)
        ? skills.filter((s): s is string => typeof s === "string" && s !== "")
        : [],
      present: binding !== null,
    },
    approvalId: rowString(row, "approvalId"),
    childRunId: rowString(row, "childRunId"),
    tokens: rowNumber(row, "tokens"),
    cost: rowNumber(row, "cost"),
    durationMs: rowNumber(row, "durationMs"),
    errorMessage: rowString(row, "errorMessage"),
    createdAt: rowString(row, "createdAt"),
    startedAt: rowString(row, "startedAt"),
    finishedAt: rowString(row, "finishedAt"),
  };
}

export function readApproval(row: Row): ApprovalRow {
  const evidence = rowObject(row, "evidence");
  return {
    id: rowString(row, "id"),
    runId: rowString(row, "runId"),
    stepKey: rowString(row, "stepKey"),
    kind: rowString(row, "kind"),
    subject: rowString(row, "subject"),
    question: rowString(row, "question"),
    tier: nested(evidence, "tier"),
    reason: nested(evidence, "reason"),
    ruleId: nested(evidence, "ruleId"),
    decision: rowString(row, "decision"),
    requestedAt: rowString(row, "requestedAt"),
    decidedAt: rowString(row, "decidedAt"),
    createdAt: rowString(row, "createdAt"),
  };
}

// ---------------------------------------------------------------------------
// Node identity
// ---------------------------------------------------------------------------

export type NodeKind =
  | "you"
  | "goal"
  | "template"
  | "step"
  | "binding"
  | "approval"
  | "cluster"
  | "fold";

export const YOU_NODE_ID = "you";
export const GOAL_NODE_ID = "goal";

/**
 * The node id for a kind and a key.
 *
 * ONE FUNCTION rather than template literals at the call sites, because the
 * layout writes these ids, the URL carries them and the hover handler compares
 * them -- three places that must agree on a separator.
 */
export function nodeIdFor(kind: NodeKind, key: string): string {
  if (kind === "you") return YOU_NODE_ID;
  if (kind === "goal") return GOAL_NODE_ID;
  return `${kind}:${key}`;
}

/** A step's node id: keyed on `runId:key`, NEVER on the row id (see header). */
export function stepNodeId(step: StepRow): string {
  return nodeIdFor("step", `${step.runId}:${step.key}`);
}

export function bindingNodeId(step: StepRow): string {
  return nodeIdFor("binding", `${step.runId}:${step.key}`);
}

export function approvalNodeId(approval: ApprovalRow): string {
  return nodeIdFor("approval", approval.id);
}

export function templateNodeId(run: RunRow): string {
  return nodeIdFor("template", run.id);
}

export function clusterNodeId(runId: string, depth: number): string {
  return nodeIdFor("cluster", `${runId}:${depth}`);
}

export function foldNodeId(runId: string, from: number, to: number): string {
  return nodeIdFor("fold", `${runId}:${from}-${to}`);
}

/**
 * The concept a node's row lives in, or "" when the node has no row.
 *
 * `you`, `cluster` and `fold` have no row, and that is not a gap: `you` is the
 * viewer, and the other two are drawings that stand in for several rows. A
 * caller that opened one of them would be asking the engine to read a picture.
 */
export function conceptIdForKind(kind: NodeKind): string {
  switch (kind) {
    case "goal":
      return GOAL_CONCEPT_ID;
    case "template":
      return RUN_CONCEPT_ID;
    case "step":
    case "binding":
      return STEP_CONCEPT_ID;
    case "approval":
      return APPROVAL_CONCEPT_ID;
    default:
      return "";
  }
}

// ---------------------------------------------------------------------------
// Derived readings
// ---------------------------------------------------------------------------

/**
 * Whether the machine called a model for a step of this kind.
 *
 * THREE ANSWERS, AND THE THIRD IS NOT `false`. An unclassified step -- kind
 * `""`, which epic A1 leaves true of every `function` step until the A2 loader
 * rule -- is one this build cannot vouch for either way, and counting it as
 * free would put "0 model calls" on a run that made three. That is the single
 * most damaging thing this surface could say, because the product's whole
 * claim is about which steps cost something.
 *
 * IT LIVES HERE, IN THE LEAF, because three surfaces read it and they were
 * briefly allowed to disagree: the rail's ink weight, the road's weight on the
 * map, and the receipt's count. `decision` is the one that looks like thinking
 * and is not -- a switch over a value already in hand reaches no provider.
 */
export function kindCalledAModel(kind: string): boolean | null {
  if (kind === "reasoning" || kind === "loop") return true;
  if (kind === "deterministic" || kind === "decision" || kind === "subrun") return false;
  return null;
}

/** Shorthand for the two surfaces that draw ink: unclassified is NOT ink. */
export function stepThought(step: StepRow): boolean {
  return kindCalledAModel(step.kind) === true;
}

// ---------------------------------------------------------------------------
// Folds over the rows
// ---------------------------------------------------------------------------

/**
 * The latest attempt of every step key in a run.
 *
 * A retry writes a new row with the same `key`, so the map draws ONE node per
 * key and this decides which row it stands for. Ties on `attempt` fall through
 * to `createdAt` and then to the row id, so the answer is total and stable
 * even for two rows written in the same millisecond -- which matters because
 * `layout(sameWorld)` must give the same answer twice for a deep link to frame
 * the node it names.
 */
export function latestAttempts(steps: readonly StepRow[]): Map<string, StepRow> {
  const byKey = new Map<string, StepRow>();
  for (const step of steps) {
    if (step.key === "") continue;
    const held = byKey.get(step.key);
    if (held === undefined || wins(step, held)) byKey.set(step.key, step);
  }
  return byKey;
}

function wins(candidate: StepRow, held: StepRow): boolean {
  if (candidate.attempt !== held.attempt) return candidate.attempt > held.attempt;
  if (candidate.createdAt !== held.createdAt) return candidate.createdAt > held.createdAt;
  return candidate.id > held.id;
}

/** The steps of one run, latest attempt each, in a total and stable order. */
export function stepsOfRun(world: GoalWorld, runId: string): StepRow[] {
  const mine = world.steps.filter((step) => step.runId === runId);
  return [...latestAttempts(mine).values()].sort(compareSteps);
}

/**
 * The order steps are drawn in.
 *
 * `seq` first because it is the template's own execution order, then `key` as
 * the tie-break. NEVER insertion order: the collection folds events in the
 * order the cluster sent them, so a layout that depended on it would reshuffle
 * on an update -- exactly when somebody is watching it.
 */
export function compareSteps(a: StepRow, b: StepRow): number {
  if (a.seq !== b.seq) return a.seq - b.seq;
  return a.key < b.key ? -1 : a.key > b.key ? 1 : 0;
}

/**
 * Dependency depth per step key: 0 for a step that waits on nothing, otherwise
 * one past the deepest thing it waits on.
 *
 * This is the map's x axis, and it is a fact about the rows rather than a
 * rendering convenience: `dependsOn` is a real edge, so two steps in the same
 * column are two steps that can run at the same time.
 *
 * A CYCLE CANNOT HANG THIS. The walk carries its own visiting set and treats a
 * back edge as depth 0 for that arm, so a malformed template renders as a flat
 * map rather than as a browser tab that stops responding. A dependency naming
 * a step that is not in this run is ignored for the same reason -- it is a
 * fact about the template, not about the picture, and refusing to draw the run
 * would hide the forty-six steps that are fine.
 */
export function depths(steps: readonly StepRow[]): Map<string, number> {
  const byKey = new Map<string, StepRow>();
  for (const step of steps) byKey.set(step.key, step);
  const settled = new Map<string, number>();
  const visiting = new Set<string>();

  function walk(key: string): number {
    const already = settled.get(key);
    if (already !== undefined) return already;
    if (visiting.has(key)) return 0;
    const step = byKey.get(key);
    if (step === undefined) return 0;
    visiting.add(key);
    let deepest = -1;
    for (const parent of step.dependsOn) {
      if (!byKey.has(parent)) continue;
      const at = walk(parent);
      if (at > deepest) deepest = at;
    }
    visiting.delete(key);
    const depth = deepest + 1;
    settled.set(key, depth);
    return depth;
  }

  for (const step of steps) walk(step.key);
  return settled;
}

/** The approvals raised by one run, in a stable order. */
export function approvalsOfRun(world: GoalWorld, runId: string): ApprovalRow[] {
  return world.approvals
    .filter((approval) => approval.runId === runId)
    .sort((a, b) =>
      a.requestedAt !== b.requestedAt
        ? a.requestedAt < b.requestedAt
          ? -1
          : 1
        : a.id < b.id
          ? -1
          : a.id > b.id
            ? 1
            : 0,
    );
}
