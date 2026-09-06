import { rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten, stringsOf } from "../../kit/rows";
import { kindCalledAModel, stepKindWord, waitsOnAPerson } from "./words";

// The wire rows the Nexus app renders, and every reading derived from them.
//
// PURE, AND SEPARATE FROM EVERY COMPONENT, for the reason apps/accounts/rows.ts
// is: a projection asserted through render() is asserted through three layers
// that can each fail for unrelated reasons. The interesting decisions in this
// app -- what counts as news, what an absent figure means, which steps thought
// -- are all functions of rows, and they are unit-testable with no browser, no
// cluster and no React.

// ---------------------------------------------------------------------------
// Ids, and joining across a relationship field
// ---------------------------------------------------------------------------

/**
 * The short id at the end of an id, whichever spelling arrived.
 *
 * A ROW'S OWN `id` REACHES A BROWSER BARE -- the engine strips the `{concept}:`
 * prefix at every egress seam, and `component/grpc/wire_bare_ids_test.go` fails
 * the build if a canonical one leaks. A RELATIONSHIP FIELD is a different
 * question: `run.goalId` and `step.runId` are stored canonicalized by the
 * relationship pre-walk, and whether the projection hands one back bare is not
 * something this window can know for certain across every read path.
 *
 * So every join in this app compares TAILS on both sides. The tail of a bare id
 * is itself, and the tail of a canonical one is the short id, so the comparison
 * is right under either spelling and cannot invent a match: two different rows
 * of one concept never share a short id. The alternative -- picking a spelling
 * and hoping -- is the Accounts app's `SELF_ACCOUNT_ID` bug, where one wrong
 * comparison left three surfaces silently and permanently empty.
 */
export function idTail(id: string): string {
  const trimmed = id.trim();
  if (trimmed === "") return "";
  return trimmed.slice(trimmed.lastIndexOf(":") + 1);
}

/** Whether two ids name the same row, under either spelling. */
export function sameRow(a: string, b: string): boolean {
  const left = idTail(a);
  return left !== "" && left === idTail(b);
}

// ---------------------------------------------------------------------------
// Figures: absent is not zero
// ---------------------------------------------------------------------------

/**
 * A number that may be ABSENT, kept absent.
 *
 * The SDK's `rowNumber` answers 0 for a missing key, which is the right
 * default for a count and the wrong one for everything on a run's `spent`
 * object. Epic A1 writes no cost at all, and a run that reported nothing
 * rendering "$0.00 -- 0 model calls" is this window inventing the headline
 * claim the whole product makes. `null` renders as an em dash, which is what
 * every absent value in this shell renders as.
 */
export function figure(from: Record<string, unknown> | null, key: string): number | null {
  const v = from?.[key];
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

function objectField(row: Row, key: string): Record<string, unknown> | null {
  const v = row[key];
  if (v && typeof v === "object" && !Array.isArray(v)) return v as Record<string, unknown>;
  return null;
}

// ---------------------------------------------------------------------------
// goal
// ---------------------------------------------------------------------------

export interface GoalRow {
  id: string;
  statement: string;
  origin: string;
  responsibilityId: string;
  accountIds: string[];
  status: string;
  requestedVia: string;
  closedAt: string;
  closeReason: string;
  ceilings: Record<string, unknown> | null;
  createdAt: string;
}

export function goalFromRow(row: Row): GoalRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    statement: rowString(flat, "statement"),
    origin: rowString(flat, "origin"),
    responsibilityId: rowString(flat, "responsibilityId"),
    accountIds: stringsOf(flat, "accountIds"),
    status: rowString(flat, "status"),
    requestedVia: rowString(flat, "requestedVia"),
    closedAt: rowString(flat, "closedAt"),
    closeReason: rowString(flat, "closeReason"),
    ceilings: objectField(flat, "ceilings"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/**
 * What to call a goal with no statement.
 *
 * `statement` is `string!` so this should not happen from a create -- but a
 * folded CDC event carries only what the write touched, so a goal updated by
 * anything that did not name the statement arrives without one until the
 * re-read lands. A blank line in a list is indistinguishable from a row that
 * failed to render.
 */
export function goalTitle(goal: GoalRow): string {
  const trimmed = goal.statement.trim();
  if (trimmed !== "") return trimmed;
  const tail = idTail(goal.id);
  return tail === "" ? "Untitled goal" : `Untitled goal (${tail})`;
}

/** Goal news: a restatement, a status flip, a close. Nothing that ticks. */
export function goalFingerprint(goal: GoalRow): string {
  return [goal.statement, goal.status, goal.closeReason].join("|");
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

export interface RunRow {
  id: string;
  goalId: string;
  automationName: string;
  mode: string;
  replayPolicy: string;
  status: string;
  waitingOnKind: string;
  waitingOnSubject: string;
  waitingSince: string;
  forkedFromRunId: string;
  forkAtStepKey: string;
  spent: Record<string, unknown> | null;
  nodeId: string;
  heartbeatAt: string;
  cancelRequested: boolean;
  errorCode: string;
  errorMessage: string;
  stepOrder: string[];
  triggeredBy: string;
  startedAt: string;
  finishedAt: string;
  createdAt: string;
}

export function runFromRow(row: Row): RunRow {
  const flat = flatten(row);
  const waiting = objectField(flat, "waitingOn");
  return {
    id: rowString(flat, "id"),
    goalId: rowString(flat, "goalId"),
    automationName: rowString(flat, "automationName"),
    mode: rowString(flat, "mode"),
    replayPolicy: rowString(flat, "replayPolicy"),
    status: rowString(flat, "status"),
    waitingOnKind: typeof waiting?.["kind"] === "string" ? (waiting["kind"] as string) : "",
    waitingOnSubject: typeof waiting?.["subject"] === "string" ? (waiting["subject"] as string) : "",
    waitingSince: typeof waiting?.["since"] === "string" ? (waiting["since"] as string) : "",
    forkedFromRunId: rowString(flat, "forkedFromRunId"),
    forkAtStepKey: rowString(flat, "forkAtStepKey"),
    spent: objectField(flat, "spent"),
    nodeId: rowString(flat, "nodeId"),
    heartbeatAt: rowString(flat, "heartbeatAt"),
    cancelRequested: flat["cancelRequested"] === true,
    errorCode: rowString(flat, "errorCode"),
    errorMessage: rowString(flat, "errorMessage"),
    stepOrder: stringsOf(flat, "stepOrder"),
    triggeredBy: rowString(flat, "triggeredBy"),
    startedAt: rowString(flat, "startedAt"),
    finishedAt: rowString(flat, "finishedAt"),
    createdAt: rowString(flat, "createdAt"),
  };
}

export function runTitle(run: RunRow): string {
  const trimmed = run.automationName.trim();
  if (trimmed !== "") return trimmed;
  const tail = idTail(run.id);
  return tail === "" ? "Untitled run" : `Untitled run (${tail})`;
}

export const RUN_TERMINAL = ["succeeded", "failed", "cancelled", "abandoned"];

export function runIsTerminal(run: RunRow): boolean {
  return RUN_TERMINAL.includes(run.status);
}

/** A run parked on a person -- the one urgency this app has. */
export function runWaitsOnYou(run: RunRow): boolean {
  return run.status === "waiting" && waitsOnAPerson(run.waitingOnKind);
}

/**
 * RUN NEWS, AND THE TWO FIELDS DELIBERATELY LEFT OUT OF IT.
 *
 * `heartbeatAt` is written at every step boundary of every running run and
 * broadcasts the whole row each time. Naming it here would ring hardest for
 * the run somebody is already watching move -- the deploy timeline's sharpest
 * case, and this one is sharper, because a run can have hundreds of steps.
 *
 * `spent` is out for the campaigns app's second reason rather than the first:
 * the counters MUST re-render live (that is the whole point of watching a run
 * spend), and re-rendering and ringing are different statements. The
 * fingerprint is the only thing that separates them, so the figures move on
 * screen and the row stays quiet.
 *
 * What IS news: the state changed, it started waiting on something different,
 * somebody asked it to stop, or it ended.
 */
export function runFingerprint(run: RunRow): string {
  return [
    run.status,
    run.waitingOnKind,
    run.mode,
    run.errorCode,
    run.finishedAt,
    run.cancelRequested ? "cancelling" : "",
  ].join("|");
}

// ---------------------------------------------------------------------------
// step
// ---------------------------------------------------------------------------

export interface StepRow {
  id: string;
  runId: string;
  key: string;
  seq: number;
  stepType: string;
  kind: string;
  callName: string;
  callConstruct: string;
  dependsOn: string[];
  status: string;
  symptom: string;
  attempt: number;
  /** `runId:key:attempt` -- the key a side effect runs under, so a resume can
   *  ask the far side whether it already happened. */
  idempotencyKey: string;
  childRunId: string;
  approvalId: string;
  resumeAt: string;
  externalKey: string;
  postconditionKind: string;
  postconditionRef: string;
  /** `true`, `false`, or NULL for "there is no postcondition on this step". */
  postconditionPassed: boolean | null;
  postconditionMessage: string;
  startedAt: string;
  finishedAt: string;
  durationMs: number | null;
  tokens: number | null;
  cost: number | null;
  errorCode: string;
  errorMessage: string;
  createdAt: string;
}

export function stepFromRow(row: Row): StepRow {
  const flat = flatten(row);
  const call = objectField(flat, "call");
  const post = objectField(flat, "postcondition");
  const passed = post?.["passed"];
  return {
    id: rowString(flat, "id"),
    runId: rowString(flat, "runId"),
    key: rowString(flat, "key"),
    seq: rowNumber(flat, "seq"),
    stepType: rowString(flat, "stepType"),
    kind: rowString(flat, "kind"),
    callName: typeof call?.["name"] === "string" ? (call["name"] as string) : "",
    callConstruct: typeof call?.["construct"] === "string" ? (call["construct"] as string) : "",
    dependsOn: stringsOf(flat, "dependsOn"),
    status: rowString(flat, "status"),
    symptom: rowString(flat, "symptom"),
    // `attempt` is `int!` with a default of 1, so a row without one is a fold
    // that did not touch it -- and "attempt 0" is not a thing. 1 is the honest
    // reading of an untouched field here, unlike `spent`, where the whole
    // question is whether anything was measured at all.
    attempt: Math.max(1, rowNumber(flat, "attempt")),
    idempotencyKey: rowString(flat, "idempotencyKey"),
    childRunId: rowString(flat, "childRunId"),
    approvalId: rowString(flat, "approvalId"),
    resumeAt: rowString(flat, "resumeAt"),
    externalKey: rowString(flat, "externalKey"),
    postconditionKind: typeof post?.["kind"] === "string" ? (post["kind"] as string) : "",
    postconditionRef: typeof post?.["ref"] === "string" ? (post["ref"] as string) : "",
    postconditionPassed: typeof passed === "boolean" ? passed : null,
    postconditionMessage: typeof post?.["message"] === "string" ? (post["message"] as string) : "",
    startedAt: rowString(flat, "startedAt"),
    finishedAt: rowString(flat, "finishedAt"),
    durationMs: figure(flat, "durationMs"),
    tokens: figure(flat, "tokens"),
    cost: figure(flat, "cost"),
    errorCode: rowString(flat, "errorCode"),
    errorMessage: rowString(flat, "errorMessage"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/**
 * The run's steps in the order they ran.
 *
 * ORDERED HERE RATHER THAN BY THE READ, AND THAT IS THE QUERY'S DOING.
 * `workStepsForOwnerRun` carries `@unbounded` -- "every step of ONE run,
 * bounded by the run" -- and `@unbounded` excludes `sort`, so the read comes
 * back in whatever order the collection folded it. A timeline drawn in fold
 * order reshuffles itself the moment any step updates, which is exactly when
 * somebody is watching it.
 *
 * `seq` is the template's own 0-based execution order, so it is the key. Ties
 * fall back to the step KEY rather than to a timestamp: a parallel block gives
 * several steps the same instant, and a stable alphabetical tiebreak keeps the
 * list from swapping two rows under the reader on an unrelated update.
 */
export function stepsInOrder(steps: readonly StepRow[]): StepRow[] {
  return [...steps].sort((a, b) => (a.seq === b.seq ? a.key.localeCompare(b.key) : a.seq - b.seq));
}

/** Step news: the state moved, the classifier spoke, or it was retried. */
export function stepFingerprint(step: StepRow): string {
  return [step.status, step.symptom, String(step.attempt), step.kind].join("|");
}

/** What a step DID, in one line. Never blank: an unnamed call is its type. */
export function stepCallLine(step: StepRow): string {
  const name = step.callName.trim();
  if (name !== "") return name;
  const type = step.stepType.trim();
  return type === "" ? "--" : type;
}

/** Whether the timeline draws this step in ink rather than in hairline. */
export function stepThought(step: StepRow): boolean {
  return kindCalledAModel(step.kind) === true;
}

// ---------------------------------------------------------------------------
// The kind band: a run's steps, divided by what each one cost
// ---------------------------------------------------------------------------

export interface KindSegment {
  kind: string;
  /** The enum member, which is what somebody greps for. */
  label: string;
  count: number;
  share: number;
}

export interface KindBreakdown {
  segments: KindSegment[];
  total: number;
  /** How many steps called a model. The headline figure. */
  thought: number;
  /** How many this build cannot classify -- absent from the headline, on purpose. */
  unclassified: number;
  empty: boolean;
}

/**
 * The run's steps, partitioned by kind.
 *
 * A BAND, NOT A ROW OF STAT CARDS -- the campaigns send bar's argument,
 * unchanged: six numbers in six boxes makes a person add them up to learn the
 * one thing they came for, which here is "how much of this run had to think".
 * The band IS that answer, and it makes the two slices nobody goes looking for
 * visible: the human steps that will stop the run, and the unclassified ones
 * this build cannot vouch for.
 *
 * Segments are emitted in a FIXED order rather than by size, so the same run
 * looks the same on every render and two runs can be compared by eye.
 */
export function kindBreakdown(steps: readonly StepRow[]): KindBreakdown {
  const order = ["deterministic", "reasoning", "decision", "human", "loop", "subrun", ""];
  const counts = new Map<string, number>();
  for (const kind of order) counts.set(kind, 0);
  for (const step of steps) {
    const kind = counts.has(step.kind) ? step.kind : "";
    counts.set(kind, (counts.get(kind) ?? 0) + 1);
  }
  const total = steps.length;
  const segments = order.map((kind) => {
    const count = counts.get(kind) ?? 0;
    return {
      kind,
      label: stepKindWord(kind),
      count,
      share: total === 0 ? 0 : count / total,
    };
  });
  return {
    segments,
    total,
    thought: steps.filter(stepThought).length,
    unclassified: counts.get("") ?? 0,
    empty: total === 0,
  };
}

/**
 * The band in words, for a reader who cannot see it.
 *
 * A bar a screen reader cannot read is a bar that excluded somebody, and the
 * picture's whole content is proportion -- which the legend beneath does not
 * convey on its own. Zero slices are omitted HERE and kept in the legend: a
 * spoken sentence listing four zeroes buries the two figures that matter,
 * where a legend column of them reads at a glance.
 */
export function kindBreakdownLabel(breakdown: KindBreakdown): string {
  if (breakdown.empty) return "No steps yet.";
  const parts = breakdown.segments
    .filter((s) => s.count > 0)
    .map((s) => `${s.count} ${s.label.toLowerCase()}`);
  return `${breakdown.total} steps: ${parts.join(", ")}.`;
}

// ---------------------------------------------------------------------------
// What a run spent
// ---------------------------------------------------------------------------

export interface SpendFigure {
  /** The label at a count of one. */
  one: string;
  /** The label at every other count, INCLUDING an absent one. */
  many: string;
  value: number | null;
  /** How to render it -- a count, a token count, or money. */
  as: "count" | "tokens" | "money";
}

/**
 * The label for this figure's own value.
 *
 * "1 retries" is the kind of sloppiness a reader notices and then stops
 * trusting the rest of the panel for. An ABSENT figure takes the plural: it
 * is the label for the quantity in general, not for a count of one.
 */
export function spendLabel(figure: SpendFigure): string {
  return figure.value === 1 ? figure.one : figure.many;
}

/**
 * The run's `spent`, as figures that can be ABSENT.
 *
 * Epic A1 writes none of these; A2 wires the accounting. So every one of them
 * is legitimately absent today, and an absent figure renders as an em dash
 * rather than as a zero -- "0 model calls" on a run that made three is the
 * single most damaging thing this surface could say, because "it reached no
 * model" is the claim the product is making.
 */
export function runSpend(run: RunRow): SpendFigure[] {
  const spent = run.spent;
  return [
    { one: "model call", many: "model calls", value: figure(spent, "modelCalls"), as: "count" },
    { one: "token", many: "tokens", value: figure(spent, "tokens"), as: "tokens" },
    { one: "cost", many: "cost", value: figure(spent, "cost"), as: "money" },
    { one: "retry", many: "retries", value: figure(spent, "retries"), as: "count" },
  ];
}

/** A token count, in the unit a person would say it in. */
export function formatTokens(value: number | null): string {
  if (value === null) return "--";
  if (value < 1000) return String(Math.round(value));
  if (value < 1_000_000) return `${(value / 1000).toFixed(value < 10_000 ? 1 : 0)}k`;
  return `${(value / 1_000_000).toFixed(1)}M`;
}

/**
 * Money.
 *
 * FOUR DECIMALS UNDER A CENT, because a run that cost $0.0032 did cost
 * something, and "$0.00" beside a model call that happened reads as a
 * rendering fault. Exactly zero renders "$0.00": a run that reached no
 * provider genuinely cost nothing, which is a reading somebody wants.
 */
export function formatMoney(value: number | null): string {
  if (value === null) return "--";
  if (value === 0) return "$0.00";
  if (Math.abs(value) < 0.01) return `$${value.toFixed(4)}`;
  return `$${value.toFixed(2)}`;
}

export function formatCount(value: number | null): string {
  return value === null ? "--" : String(Math.round(value));
}

export function formatSpend(figure: SpendFigure): string {
  switch (figure.as) {
    case "tokens":
      return formatTokens(figure.value);
    case "money":
      return formatMoney(figure.value);
    default:
      return formatCount(figure.value);
  }
}

// ---------------------------------------------------------------------------
// approval
// ---------------------------------------------------------------------------

export interface ApprovalOption {
  label: string;
  value: string;
}

export interface ApprovalRow {
  id: string;
  runId: string;
  stepKey: string;
  kind: string;
  subject: Record<string, unknown> | null;
  artifactHash: string;
  question: string;
  options: ApprovalOption[];
  evidenceTier: string;
  evidenceReason: string;
  evidenceRuleId: string;
  evidenceSource: string;
  requestedAt: string;
  decidedBy: string;
  decidedAt: string;
  decision: string;
  expiresAt: string;
  createdAt: string;
}

export function approvalFromRow(row: Row): ApprovalRow {
  const flat = flatten(row);
  const evidence = objectField(flat, "evidence");
  const str = (from: Record<string, unknown> | null, key: string) =>
    typeof from?.[key] === "string" ? (from[key] as string) : "";
  return {
    id: rowString(flat, "id"),
    runId: rowString(flat, "runId"),
    stepKey: rowString(flat, "stepKey"),
    kind: rowString(flat, "kind"),
    subject: objectField(flat, "subject"),
    artifactHash: rowString(flat, "artifactHash"),
    question: rowString(flat, "question"),
    options: approvalOptions(flat["options"]),
    evidenceTier: str(evidence, "tier"),
    evidenceReason: str(evidence, "reason"),
    evidenceRuleId: str(evidence, "ruleId"),
    evidenceSource: str(evidence, "source"),
    requestedAt: rowString(flat, "requestedAt"),
    decidedBy: rowString(flat, "decidedBy"),
    decidedAt: rowString(flat, "decidedAt"),
    decision: rowString(flat, "decision"),
    expiresAt: rowString(flat, "expiresAt"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/**
 * `[{label, value}]`, keeping only the members that are actually choosable.
 *
 * An option with no `value` cannot be sent, so it is DROPPED rather than
 * rendered as a button that produces a refusal. An option with a value and no
 * label falls back to the value: a choice with no name is still a choice, and
 * hiding it would leave somebody with a question they cannot answer.
 */
export function approvalOptions(raw: unknown): ApprovalOption[] {
  if (!Array.isArray(raw)) return [];
  const out: ApprovalOption[] = [];
  for (const member of raw) {
    if (member === null || typeof member !== "object" || Array.isArray(member)) continue;
    const record = member as Record<string, unknown>;
    const value = typeof record["value"] === "string" ? record["value"] : "";
    if (value.trim() === "") continue;
    const label = typeof record["label"] === "string" ? record["label"] : "";
    out.push({ value, label: label.trim() === "" ? value : label });
  }
  return out;
}

export function approvalIsPending(approval: ApprovalRow): boolean {
  return approval.decision === "";
}

/**
 * What is being approved, in one line.
 *
 * The `subject` is a free-form object the classifier filled in, so this reads
 * the keys a subject is LIKELY to carry and falls back to the step it came
 * from. It never renders a JSON blob as a headline: the whole object is on the
 * detail panel in the data voice, where somebody can read it deliberately.
 */
export function approvalSubjectLine(approval: ApprovalRow): string {
  const question = approval.question.trim();
  if (question !== "") return question;
  for (const key of ["summary", "title", "description", "command", "message", "name"]) {
    const value = approval.subject?.[key];
    if (typeof value === "string" && value.trim() !== "") return value.trim();
  }
  const step = approval.stepKey.trim();
  return step === "" ? "Something this run wants to do" : `Step ${step}`;
}

/** Approval news: it arrived, or somebody decided it. */
export function approvalFingerprint(approval: ApprovalRow): string {
  return [approval.decision, approval.decidedBy, approval.artifactHash].join("|");
}

// ---------------------------------------------------------------------------
// The journal (on demand -- these concepts do not broadcast)
// ---------------------------------------------------------------------------

export interface ModelCallRow {
  id: string;
  runId: string;
  stepKey: string;
  provider: string;
  model: string;
  promptRef: string;
  served: string;
  inputTokens: number | null;
  outputTokens: number | null;
  cost: number | null;
  latencyMs: number | null;
  error: string;
  createdAt: string;
}

export function modelCallFromRow(row: Row): ModelCallRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    runId: rowString(flat, "runId"),
    stepKey: rowString(flat, "stepKey"),
    provider: rowString(flat, "provider"),
    model: rowString(flat, "model"),
    promptRef: rowString(flat, "promptRef"),
    served: rowString(flat, "served"),
    inputTokens: figure(flat, "inputTokens"),
    outputTokens: figure(flat, "outputTokens"),
    cost: figure(flat, "cost"),
    latencyMs: figure(flat, "latencyMs"),
    error: rowString(flat, "error"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/** How a model call was answered, in the person's terms. */
export function servedWord(served: string): string {
  switch (served) {
    case "live":
      return "a provider answered";
    case "journal":
      return "served from the journal";
    case "local":
      return "a fleet model answered";
    default:
      return served === "" ? "--" : served;
  }
}

export interface ObservationRow {
  id: string;
  runId: string;
  stepKey: string;
  kind: string;
  content: string;
  createdAt: string;
}

export function observationFromRow(row: Row): ObservationRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    runId: rowString(flat, "runId"),
    stepKey: rowString(flat, "stepKey"),
    kind: rowString(flat, "kind"),
    content: rowString(flat, "content"),
    createdAt: rowString(flat, "createdAt"),
  };
}

export function observationKindWord(kind: string): string {
  switch (kind) {
    case "tool_result":
      return "Tool result";
    case "error":
      return "Error";
    case "note":
      return "Note";
    case "decision":
      return "Decision";
    default:
      return kind === "" ? "--" : kind;
  }
}

// ---------------------------------------------------------------------------
// Folds the surfaces share
// ---------------------------------------------------------------------------

/** The runs of one goal, newest first. Joined on the tail, per `idTail`. */
export function runsOfGoal(runs: readonly RunRow[], goalId: string): RunRow[] {
  const wanted = idTail(goalId);
  if (wanted === "") return [];
  return runs.filter((run) => idTail(run.goalId) === wanted);
}

/** The pending approvals raised by one run. */
export function pendingApprovalsOfRun(
  approvals: readonly ApprovalRow[],
  runId: string,
): ApprovalRow[] {
  const wanted = idTail(runId);
  if (wanted === "") return [];
  return approvals.filter((a) => approvalIsPending(a) && idTail(a.runId) === wanted);
}

/** The steps of one run, in order. */
export function stepsOfRun(steps: readonly StepRow[], runId: string): StepRow[] {
  const wanted = idTail(runId);
  if (wanted === "") return [];
  return stepsInOrder(steps.filter((step) => idTail(step.runId) === wanted));
}

/** Free-text search over a goal, matching what a person would type. */
export function goalMatches(goal: GoalRow, search: string): boolean {
  const needle = search.trim().toLowerCase();
  if (needle === "") return true;
  return [goal.statement, goal.origin, goal.status, idTail(goal.id)]
    .join(" ")
    .toLowerCase()
    .includes(needle);
}

export function runMatches(run: RunRow, search: string): boolean {
  const needle = search.trim().toLowerCase();
  if (needle === "") return true;
  return [run.automationName, run.status, run.mode, run.errorCode, idTail(run.id)]
    .join(" ")
    .toLowerCase()
    .includes(needle);
}
