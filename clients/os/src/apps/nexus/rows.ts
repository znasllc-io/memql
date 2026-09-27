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

function stringIn(from: Record<string, unknown> | null, key: string): string {
  const v = from?.[key];
  return typeof v === "string" ? v : "";
}

// ---------------------------------------------------------------------------
// The head: which version of every step is current (epic memql#5414, D18)
// ---------------------------------------------------------------------------

/** One entry of `run.head`. `runId` is set only when the version lives in ANOTHER run -- a branch's reused prefix. */
export interface HeadEntry {
  version: number;
  runId: string;
}

export type Head = Readonly<Record<string, HeadEntry>>;

/**
 * `run.head` (or a version's `basis`), read tolerantly.
 *
 * AN ABSENT HEAD IS AN EMPTY MAP, NOT AN ERROR. Runs written before the field
 * existed carry none, and the rule the concept states for them is that each
 * step's newest row is current -- which is what every caller falls back to
 * when a key is missing here. An entry without a usable version is dropped
 * rather than read as version 0: there is no version 0, and a guessed one
 * would mark the wrong tick current.
 */
export function parseHead(value: unknown): Head {
  const out: Record<string, HeadEntry> = {};
  if (value === null || typeof value !== "object" || Array.isArray(value)) return out;
  for (const [key, raw] of Object.entries(value as Record<string, unknown>)) {
    if (key === "" || raw === null || typeof raw !== "object" || Array.isArray(raw)) continue;
    const entry = raw as Record<string, unknown>;
    const version = entry["version"];
    if (typeof version !== "number" || !Number.isFinite(version) || version < 1) continue;
    out[key] = {
      version: Math.round(version),
      runId: typeof entry["runId"] === "string" ? entry["runId"] : "",
    };
  }
  return out;
}

/**
 * A re-run the run is carrying out -- `run.rerun`, written by the person's act
 * and cleared to `{}` when the run closes.
 *
 * `{}` AND ABSENT ARE BOTH "NONE", which is why this answers null rather than
 * an object with blank fields: every reader asks one question of it -- is a
 * re-run in flight? -- and a truthy empty object would answer yes.
 */
export interface RerunRequest {
  requestId: string;
  /** `rerun`, `headMove` or `branch`. */
  reason: string;
  stepKey: string;
  requestedAt: string;
}

export function rerunFrom(value: unknown): RerunRequest | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const obj = value as Record<string, unknown>;
  if (Object.keys(obj).length === 0) return null;
  const request = {
    requestId: stringIn(obj, "requestId"),
    reason: stringIn(obj, "reason"),
    stepKey: stringIn(obj, "stepKey"),
    requestedAt: stringIn(obj, "requestedAt"),
  };
  // A map whose every field this build reads is blank is a request nobody can
  // name a step for, and treating it as in flight would hide every step act
  // with nothing on the page able to say why.
  return request.stepKey === "" && request.reason === "" && request.requestId === "" ? null : request;
}

/**
 * The answer validator's summary -- `run.validation` (D22). A PRE-FILTER, never
 * a certification: it is drawn beside the person's own verdict and never in
 * place of one. The per-axis detail and the reason are the `decision`
 * observation it names, which is read on demand.
 */
export interface ValidationSummary {
  verdict: string;
  stepKey: string;
  version: number | null;
  observationId: string;
  level: string;
  at: string;
}

export function validationFrom(value: unknown): ValidationSummary | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const obj = value as Record<string, unknown>;
  const verdict = stringIn(obj, "verdict");
  if (verdict === "") return null;
  return {
    verdict,
    stepKey: stringIn(obj, "stepKey"),
    version: figure(obj, "version"),
    observationId: stringIn(obj, "observationId"),
    level: stringIn(obj, "level"),
    at: stringIn(obj, "at"),
  };
}

// ---------------------------------------------------------------------------
// What a person changed for one version (D20)
// ---------------------------------------------------------------------------

export interface StepOverrideRow {
  level: string;
  model: string;
  effort: string;
  prompt: string;
  inputs: Readonly<Record<string, unknown>>;
  /** The dislike that rode along as guidance, when one did. */
  guidance: { product: boolean; process: boolean; performance: boolean; reason: string } | null;
  requestedBy: string;
}

export const NO_OVERRIDE: StepOverrideRow = {
  level: "",
  model: "",
  effort: "",
  prompt: "",
  inputs: {},
  guidance: null,
  requestedBy: "",
};

export function overrideFrom(value: unknown): StepOverrideRow {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return NO_OVERRIDE;
  const obj = value as Record<string, unknown>;
  const inputs = objectOf(obj["inputs"]) ?? {};
  const rawGuidance = objectOf(obj["guidance"]);
  const axes = objectOf(rawGuidance?.["axes"]);
  const guidance =
    rawGuidance === null
      ? null
      : {
          product: axes?.["product"] === true,
          process: axes?.["process"] === true,
          performance: axes?.["performance"] === true,
          reason: stringIn(rawGuidance, "reason"),
        };
  return {
    level: stringIn(obj, "level"),
    model: stringIn(obj, "model"),
    effort: stringIn(obj, "effort"),
    prompt: stringIn(obj, "prompt"),
    inputs,
    // A guidance block with no axis and no reason carries nothing a person
    // could read back, so it is not reported as one.
    guidance:
      guidance !== null && (guidance.product || guidance.process || guidance.performance || guidance.reason !== "")
        ? guidance
        : null,
    requestedBy: stringIn(obj, "requestedBy"),
  };
}

/** Whether a version was run with anything a person changed. `requestedBy` alone changes nothing. */
export function overridden(o: StepOverrideRow): boolean {
  return (
    o.level !== "" ||
    o.model !== "" ||
    o.effort !== "" ||
    o.prompt !== "" ||
    Object.keys(o.inputs).length > 0 ||
    o.guidance !== null
  );
}

function objectOf(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
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
  /** Which version of every top-level step is current. Empty on runs written before it existed. */
  head: Head;
  /** Steps whose current version was made from an upstream version that is no longer current. */
  staleSteps: string[];
  /** The re-run in flight, or null. */
  rerun: RerunRequest | null;
  /** The answer validator's pre-filter verdict, or null before it has spoken. */
  validation: ValidationSummary | null;
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
    head: parseHead(flat["head"]),
    staleSteps: stringsOf(flat, "staleSteps"),
    rerun: rerunFrom(flat["rerun"]),
    validation: validationFrom(flat["validation"]),
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
  /**
   * Which VERSION this row is (D18). Every version of a step is a version of
   * the SAME row id, so the live feed carries one row per step -- its newest
   * version, which the head re-assertion keeps equal to the current one except
   * while a re-run is writing a newer one.
   */
  version: number;
  /** What a person changed for this version. `NO_OVERRIDE` on a version nobody touched. */
  override: StepOverrideRow;
  /** The person who wrote this version's prompt or inputs; "" when the input is the system's own. */
  authoredBy: string;
  /** Who answered: `{provider, model, surface, ...}` as recorded at dispatch. */
  binding: Record<string, unknown> | null;
  /** The bound input, when it was safe to record. */
  input: Record<string, unknown> | null;
  /** The trimmed result: `{status, result, error, metadata, contentId}`. */
  result: Record<string, unknown> | null;
}

export function stepFromRow(row: Row): StepRow {
  const flat = flatten(row);
  const call = objectField(flat, "call");
  const post = objectField(flat, "postcondition");
  const passed = post?.["passed"];
  // `attempt` is `int!` defaulting to 1; `version` equals it on every row the
  // journal writes and is ABSENT on rows written before it existed -- where the
  // attempt number is exactly what the version would have said.
  const attempt = Math.max(1, rowNumber(flat, "attempt"));
  const version = figure(flat, "version");
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
    attempt,
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
    version: version !== null && version >= 1 ? Math.round(version) : attempt,
    override: overrideFrom(flat["override"]),
    authoredBy: rowString(flat, "authoredBy"),
    binding: objectField(flat, "binding"),
    input: objectField(flat, "input"),
    result: objectField(flat, "result"),
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

/** Step news: the state moved, the classifier spoke, it was retried, or a new version arrived. */
export function stepFingerprint(step: StepRow): string {
  return [step.status, step.symptom, String(step.attempt), step.kind, String(step.version)].join("|");
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

// ---------------------------------------------------------------------------
// Which door answered: one decision per step
// ---------------------------------------------------------------------------

/**
 * What one step's model calls came to, folded into a single reading.
 *
 * ONE READING PER STEP, NOT ONE PER CALL. The step row is a ROW, and the spine
 * works by weight -- the eye finds where the machine thought before a word is
 * read, which it can only do while every row is the same height. A step that
 * made four calls growing to four lines would take that away from every other
 * row on the page. The per-call detail is already in the journal panel below,
 * where somebody can read it deliberately.
 *
 * `cost` and `tokens` are summed over the calls that REPORTED one, and stay
 * null when none did -- `figure`'s rule, for `figure`'s reason. A step whose
 * calls reported nothing must not render "$0.00" beside a provider that
 * answered.
 */
export interface StepDecision {
  /** The door, in the wire's own word: `local`, `live` or `journal`. */
  served: string;
  /** The provider as the router resolved it. Blank unless one answered. */
  provider: string;
  /** The model as the router resolved it. */
  model: string;
  cost: number | null;
  tokens: number | null;
  /** How many calls this step made. Never less than 1. */
  calls: number;
}

/**
 * Which door a step is reported as having gone through, when its calls
 * disagree.
 *
 * THE MOST EXPENSIVE ONE WINS, and that is the whole rule. A step that made
 * two fleet calls and one vendor call DID reach a vendor and was billed for
 * it, so reporting it as "your fleet" would put a money figure under a line
 * that says nothing was billed -- the row contradicting itself, in the one
 * place this epic exists to make trustworthy. Under-reporting the door is the
 * only error here that a reader cannot catch.
 *
 * WHICH IS ALSO WHERE A DOOR THIS BUILD HAS NO WORD FOR SITS: above the two
 * readings that claim nothing was billed, below the one that says something
 * was. A newer engine's door hidden behind "your fleet" is the confident
 * claim; naming the value it sent asserts less, and is the reading that
 * cannot mislead somebody about their bill.
 */
const DOOR_RANK: Record<string, number> = { live: 4, local: 2, journal: 1 };

function doorRank(served: string): number {
  // A call that named no door at all never outranks one that did.
  if (served.trim() === "") return 0;
  return DOOR_RANK[served] ?? 3;
}

/** A sum over the values that were REPORTED. Null when none were. */
function sumReported(values: readonly (number | null)[]): number | null {
  let total = 0;
  let reported = false;
  for (const value of values) {
    if (value === null) continue;
    total += value;
    reported = true;
  }
  return reported ? total : null;
}

/** One call's tokens. Null only when NEITHER half was reported. */
function callTokens(call: ModelCallRow): number | null {
  if (call.inputTokens === null && call.outputTokens === null) return null;
  return (call.inputTokens ?? 0) + (call.outputTokens ?? 0);
}

/**
 * The journal's model calls, keyed by the step that made them.
 *
 * ORDERED HERE RATHER THAN BY THE READ, for `stepsInOrder`'s reason:
 * `workModelCallsForOwnerRun` carries `@unbounded`, `@unbounded` excludes
 * `sort`, so the rows arrive in whatever order the read folded them. This
 * fold names "the last call" as the one whose model and provider the line
 * carries, and "last" has to mean last in TIME or the line names a different
 * model on every read of the same journal.
 *
 * A call with no `stepKey` is DROPPED rather than attributed to a step: the
 * journal panel below still lists it, and guessing which step it belonged to
 * would put a cost on a row that did not incur it.
 */
export function decisionsByStep(calls: readonly ModelCallRow[]): Map<string, StepDecision> {
  const grouped = new Map<string, ModelCallRow[]>();
  for (const call of calls) {
    const key = call.stepKey.trim();
    if (key === "") continue;
    const held = grouped.get(key);
    if (held === undefined) grouped.set(key, [call]);
    else held.push(call);
  }

  const out = new Map<string, StepDecision>();
  for (const [key, group] of grouped) {
    const decision = foldDecision(group);
    if (decision !== null) out.set(key, decision);
  }
  return out;
}

function foldDecision(group: readonly ModelCallRow[]): StepDecision | null {
  if (group.length === 0) return null;

  // Decorated sort with the arrival index as the tiebreak: two calls of one
  // step can share a timestamp, and a fold that reshuffles equal rows names a
  // different model each time the panel is re-read.
  const ordered = group
    .map((call, index) => ({ call, index }))
    .sort((a, b) =>
      a.call.createdAt === b.call.createdAt
        ? a.index - b.index
        : a.call.createdAt.localeCompare(b.call.createdAt),
    )
    .map((entry) => entry.call);

  let served = "";
  for (const call of ordered) {
    if (doorRank(call.served) > doorRank(served)) served = call.served;
  }
  if (served === "") served = ordered[ordered.length - 1]?.served ?? "";

  // THE MODEL AND THE PROVIDER COME FROM A CALL THAT WENT THROUGH THE DOOR
  // BEING NAMED. Taking the last call of the group regardless would pair a
  // vendor door with a model that answered on somebody's laptop, and print a
  // blank provider beside it -- a line that is wrong in a way nothing on the
  // page could correct.
  const throughTheDoor = ordered.filter((call) => call.served === served);
  const last = throughTheDoor[throughTheDoor.length - 1] ?? ordered[ordered.length - 1] ?? null;

  return {
    served,
    provider: last?.provider ?? "",
    model: last?.model ?? "",
    cost: sumReported(ordered.map((call) => call.cost)),
    tokens: sumReported(ordered.map(callTokens)),
    calls: ordered.length,
  };
}

/**
 * THE DECISION, IN ONE LINE, NAMING THE DOOR IN THE PRODUCT'S OWN WORDS.
 *
 * This is the sentence the whole epic is for: a person reading a run should be
 * able to see, per step, whether their own hardware answered, whether a vendor
 * was billed, or whether nothing was called at all. So the door leads the
 * line, before the model and before any figure.
 *
 * ONE FUNCTION, TWO CONSUMERS. `StepSpine` renders this string and puts the
 * same string in the row's accessible name, because the file's contract is
 * that the name says everything the drawing says -- and two spellings of one
 * sentence is two things to keep in step.
 *
 * A live call whose cost was not reported says so IN WORDS rather than
 * rendering an em dash: the visible difference between a fleet line and a
 * vendor line is the money, so a vendor line with the money silently missing
 * is a billed call dressed as a free one.
 */
export function decisionLine(decision: StepDecision): string {
  const model = decision.model.trim() === "" ? "an unnamed model" : decision.model.trim();
  const parts: string[] = [];

  switch (decision.served) {
    case "local":
      // No money, deliberately. Nothing was billed, and a figure here would
      // invite the reader to look for the charge.
      parts.push(`your fleet · ${model}`);
      break;
    case "live": {
      const provider = decision.provider.trim();
      parts.push(`${provider === "" ? "a provider" : provider} · ${model}`);
      parts.push(decision.cost === null ? "cost not reported" : formatMoney(decision.cost));
      break;
    }
    case "journal":
      parts.push(`replayed · ${model}, no call${decision.calls === 1 ? "" : "s"} made`);
      break;
    default:
      // A door this build has no word for. `servedWord` answers with the raw
      // value, which is what somebody greps for -- guessing the commonest door
      // would put a confident claim about money on a row nothing here
      // understands. A call that named NO door says only what it can: a model
      // answered. "-- · claude-sonnet-4" reads as a rendering fault.
      parts.push(
        `${decision.served.trim() === "" ? "a model answered" : servedWord(decision.served)} · ${model}`,
      );
  }

  // The count only appears when there is more than one call to fold, for the
  // reason the cost readout only appears on the steps that cost something: a
  // "1 call" on every row is a row of noise. A replayed step counts ANSWERS,
  // because "no calls made · 3 calls" is a line arguing with itself.
  if (decision.calls > 1) {
    parts.push(`${decision.calls} ${decision.served === "journal" ? "answers" : "calls"}`);
  }

  return parts.join(" · ");
}

export interface ObservationRow {
  id: string;
  runId: string;
  stepKey: string;
  kind: string;
  content: string;
  /** The structured half: a verdict, the validator's axes, a tool's result. */
  data: Record<string, unknown> | null;
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
    data: objectField(flat, "data"),
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
    case "feedback":
      return "Feedback";
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
