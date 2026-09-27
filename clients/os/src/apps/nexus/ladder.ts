import { rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";
import type { ApprovalRow } from "./rows";

// A LEARNED PROCEDURE, AND THE LADDER IT CLIMBS (epic memql#5408, #5412).
//
// ===========================================================================
// FOUR RUNGS, AND RETIRED IS NOT A FIFTH
// ===========================================================================
// candidate -> shadow -> canary -> trusted is a real sequence a procedure
// climbs on its own evidence (component/work/ladder.go), so the page draws it
// as one. Retired is off the side: a procedure unused past the window stops
// being served on whatever rung it stood, and a changed one comes back as a
// NEW candidate rather than climbing back up. Drawing retired as a fifth rung
// would say a retired procedure had got further than a trusted one.
//
// ===========================================================================
// THE LADDER IS A WORD, AND ITS EVIDENCE IS A COUNT
// ===========================================================================
// Every figure here is a count a person can check -- "3 of 5 matches beside
// the app" -- and never a percentage: the climb is gated on consecutive
// matches and on distinct bindings, and "60%" would describe neither. The
// thresholds are the cluster's ROWS (v1:authoring:ladderPolicy:primary), so
// they are read, never assumed: a cluster that has not published the row gets
// counts with no "of", and a sentence saying why, rather than the Go
// defaults dressed up as a policy nobody wrote.
//
// PURE, AND SEPARATE FROM EVERY COMPONENT, for the reason rows.ts is: what a
// row says -- which rung, what the next one needs, which parameter is short --
// is a function of the row, and it is tested without a browser.

/** The four rungs a procedure climbs, in order. Retired is off the ladder. */
export const LADDER_RUNGS = ["candidate", "shadow", "canary", "trusted"] as const;
export type LadderRung = (typeof LADDER_RUNGS)[number];

/** The rung a stored value names, or null when it is not one of the four. */
export function climbingRung(ladder: string): LadderRung | null {
  return (LADDER_RUNGS as readonly string[]).includes(ladder) ? (ladder as LadderRung) : null;
}

/** The rung's name, as the page and the list say it. */
export function ladderWord(ladder: string): string {
  switch (ladder) {
    case "candidate":
      return "Candidate";
    case "shadow":
      return "Shadow";
    case "canary":
      return "Canary";
    case "trusted":
      return "Trusted";
    case "retired":
      return "Retired";
    case "":
      // ABSENT IS NOT A RUNG. A procedure lifted before the ladder existed
      // carries none, and the engine never serves it (component/work.ParseRung
      // reads it as not servable) -- so it is named for what it is rather
      // than guessed onto the bottom rung.
      return "Off the ladder";
    default:
      // A value this build has no word for is shown as itself. Guessing the
      // nearest rung would put a confident claim on a row nothing here reads.
      return ladder;
  }
}

/** What each rung MEANS, in one line -- the rail's second line. */
export function rungMeaning(rung: LadderRung): string {
  switch (rung) {
    case "candidate":
      return "Learned from your runs";
    case "shadow":
      return "Replays beside the app, which still answers";
    case "canary":
      return "Runs for real with the app standing by";
    case "trusted":
      return "Runs without a model";
  }
}

// ---------------------------------------------------------------------------
// The policy: v1:authoring:ladderPolicy:primary, as values that can be absent
// ---------------------------------------------------------------------------

export interface LadderPolicy {
  /** m: consecutive shadow matches before promotion is proposed. */
  shadowMatches: number | null;
  /** k: distinct bindings every free parameter needs within that streak. */
  distinctBindings: number | null;
  /** Consecutive clean canary replays that make a procedure trusted. */
  canaryMatches: number | null;
  /** Consecutive failed replays that demote it to shadow. */
  failuresToDemote: number | null;
  /** Replays whose preconditions proved insufficient that demote it. */
  insufficientToDemote: number | null;
  /** Days without a replay after which it retires. */
  retireAfterDays: number | null;
}

/**
 * One threshold, or null.
 *
 * NOT `rowNumber`, which answers 0 for a missing key -- and 0 is the one value
 * a threshold can never honestly be: the concept declares every field
 * `@minimum(1)`, and the Go side reads a non-positive value as "not
 * configured". A zero or a missing field here is therefore "this row does not
 * say", which renders as the absence it is.
 */
function threshold(row: Row, key: string): number | null {
  const v = row[key];
  return typeof v === "number" && Number.isFinite(v) && v >= 1 ? Math.floor(v) : null;
}

/** The policy row, or null when the cluster has not published one. */
export function ladderPolicyFromRow(row: Row | null | undefined): LadderPolicy | null {
  if (row === null || row === undefined) return null;
  const flat = flatten(row);
  return {
    shadowMatches: threshold(flat, "shadowMatches"),
    distinctBindings: threshold(flat, "distinctBindings"),
    canaryMatches: threshold(flat, "canaryMatches"),
    failuresToDemote: threshold(flat, "failuresToDemote"),
    insufficientToDemote: threshold(flat, "insufficientToDemote"),
    retireAfterDays: threshold(flat, "retireAfterDays"),
  };
}

// ---------------------------------------------------------------------------
// The procedure a replay executes, as the construct stores it
// ---------------------------------------------------------------------------

/**
 * One node of a step's argument tree (component/procedure Node JSON).
 *
 * `form` is how the recording's string was READ -- a command line, a path, a
 * JSON document -- and it is the only thing that says how to write the tree
 * back as one line a person recognises.
 */
export interface ArgNode {
  kind: "object" | "array" | "lit" | "hole";
  keys: string[];
  kids: ArgNode[];
  form: string;
  lit: string;
  litType: string;
  holeId: string;
  holeType: string;
}

export interface ProcedureStep {
  tool: string;
  args: ArgNode | null;
}

export interface ProcedureHole {
  id: string;
  stepIndex: number;
  path: string[];
  /** free | constant | dataflow | unexplained; "" when nothing classified it. */
  cls: string;
  /** The value, for a constant hole. */
  constValue: string;
  /** Where a data-flow hole's value comes from. */
  ref: { stepIndex: number; path: string[] } | null;
}

export interface RecordedFrom {
  app: string;
  model: string;
  effort: string;
  sessionIds: string[];
  runIds: string[];
}

export interface Footprint {
  files: boolean;
  machine: boolean;
  external: boolean;
  spend: boolean;
  concepts: string[];
}

export interface Preconditions {
  os: string;
  arch: string;
  /** name -> version, in name order. */
  tools: Array<[string, string]>;
  /** name -> "unset" | a digest, in name order. */
  variables: Array<[string, string]>;
  /** null when the recordings did not agree, which is not the same as false. */
  emptyWorkspace: boolean | null;
}

export interface ProcedureRow {
  id: string;
  name: string;
  status: string;
  /** The stored rung, verbatim. "" on a procedure lifted before the ladder. */
  ladder: string;
  shadowMatches: number;
  canaryMatches: number;
  /** free parameter hole id -> the distinct binding digests of the streak. */
  distinctBindings: Record<string, string[]>;
  failures: number;
  insufficient: number;
  promotionApprovalId: string;
  lastReplayAt: string;
  ladderReason: string;
  ladderChangedAt: string;
  /** 0..1, or null when never written -- a word is drawn only when present. */
  reliability: number | null;
  reinforceCount: number;
  goalSignature: string;
  procedureHash: string;
  createdAt: string;
  // The procedure payload.
  title: string;
  steps: ProcedureStep[];
  holes: ProcedureHole[];
  /** free hole id -> the goal input key that binds it. */
  inputMap: Record<string, string>;
  freeParameters: string[];
  footprint: Footprint;
  target: string;
  recordedFrom: RecordedFrom;
  preconditions: Preconditions;
}

function objectOf(v: unknown): Record<string, unknown> | null {
  if (typeof v === "string") {
    // An object field reaches a browser as an object. A string is the same
    // document spelled once more, and parsing it is cheaper than a page that
    // reads blank because a writer quoted it.
    try {
      return objectOf(JSON.parse(v));
    } catch {
      return null;
    }
  }
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
}

function str(from: Record<string, unknown> | null, key: string): string {
  const v = from?.[key];
  return typeof v === "string" ? v : "";
}

function num(from: Record<string, unknown> | null, key: string): number {
  const v = from?.[key];
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((m): m is string => typeof m === "string") : [];
}

function stringMap(v: unknown): Record<string, string> {
  const obj = objectOf(v);
  const out: Record<string, string> = {};
  if (obj === null) return out;
  for (const [k, val] of Object.entries(obj)) if (typeof val === "string") out[k] = val;
  return out;
}

/** A Node, read defensively: an unknown kind is dropped rather than guessed. */
export function argNodeOf(v: unknown): ArgNode | null {
  const obj = objectOf(v);
  if (obj === null) return null;
  const kind = str(obj, "kind");
  if (kind !== "object" && kind !== "array" && kind !== "lit" && kind !== "hole") return null;
  const kids = Array.isArray(obj["kids"])
    ? (obj["kids"] as unknown[]).map(argNodeOf).filter((k): k is ArgNode => k !== null)
    : [];
  return {
    kind,
    keys: strings(obj["keys"]),
    kids,
    form: str(obj, "form"),
    lit: str(obj, "lit"),
    litType: str(obj, "litType"),
    holeId: str(obj, "holeId"),
    holeType: str(obj, "holeType"),
  };
}

function holeOf(v: unknown): ProcedureHole | null {
  const obj = objectOf(v);
  if (obj === null) return null;
  const id = str(obj, "id");
  if (id === "") return null;
  const ref = objectOf(obj["ref"]);
  return {
    id,
    stepIndex: num(obj, "stepIndex"),
    path: strings(obj["path"]),
    cls: str(obj, "class"),
    constValue: str(obj, "const"),
    ref: ref === null ? null : { stepIndex: num(ref, "stepIndex"), path: strings(ref["path"]) },
  };
}

function recordedFromOf(v: unknown): RecordedFrom {
  const obj = objectOf(v);
  return {
    app: str(obj, "app"),
    model: str(obj, "model"),
    effort: str(obj, "effort"),
    sessionIds: strings(obj?.["sessionIds"]),
    runIds: strings(obj?.["runIds"]),
  };
}

function preconditionsOf(v: unknown): Preconditions {
  const obj = objectOf(v);
  const platform = objectOf(obj?.["platform"]);
  const byName = (m: Record<string, string>) =>
    Object.entries(m).sort(([a], [b]) => a.localeCompare(b));
  const empty = obj?.["emptyWorkspace"];
  return {
    os: str(platform, "os"),
    arch: str(platform, "arch"),
    tools: byName(stringMap(obj?.["tools"])),
    variables: byName(stringMap(obj?.["variables"])),
    emptyWorkspace: typeof empty === "boolean" ? empty : null,
  };
}

function bindingsOf(v: unknown): Record<string, string[]> {
  const obj = objectOf(v);
  const out: Record<string, string[]> = {};
  if (obj === null) return out;
  for (const [k, val] of Object.entries(obj)) out[k] = strings(val);
  return out;
}

export function procedureFromRow(row: Row): ProcedureRow {
  const flat = flatten(row);
  const procedure = objectOf(flat["procedure"]);
  const footprint = objectOf(procedure?.["footprint"]);
  const rawSteps = Array.isArray(procedure?.["steps"]) ? (procedure?.["steps"] as unknown[]) : [];
  const rawHoles = Array.isArray(procedure?.["holes"]) ? (procedure?.["holes"] as unknown[]) : [];
  const reliability = flat["reliability"];
  return {
    id: rowString(flat, "id"),
    name: rowString(flat, "name"),
    status: rowString(flat, "status"),
    ladder: rowString(flat, "ladder"),
    // Absent counters read as 0, which the concept declares: a streak nobody
    // has started is a streak of none.
    shadowMatches: rowNumber(flat, "shadowMatches"),
    canaryMatches: rowNumber(flat, "canaryMatches"),
    distinctBindings: bindingsOf(flat["distinctBindings"]),
    failures: rowNumber(flat, "failures"),
    insufficient: rowNumber(flat, "insufficient"),
    promotionApprovalId: rowString(flat, "promotionApprovalId"),
    lastReplayAt: rowString(flat, "lastReplayAt"),
    ladderReason: rowString(flat, "ladderReason"),
    ladderChangedAt: rowString(flat, "ladderChangedAt"),
    reliability: typeof reliability === "number" && Number.isFinite(reliability) ? reliability : null,
    reinforceCount: rowNumber(flat, "reinforceCount"),
    goalSignature: rowString(flat, "goalSignature"),
    procedureHash: rowString(flat, "procedureHash"),
    createdAt: rowString(flat, "createdAt"),
    title: str(procedure, "title"),
    steps: rawSteps
      .map((s) => objectOf(s))
      .filter((s): s is Record<string, unknown> => s !== null)
      .map((s) => ({ tool: str(s, "tool"), args: argNodeOf(s["args"]) })),
    holes: rawHoles.map(holeOf).filter((h): h is ProcedureHole => h !== null),
    inputMap: stringMap(procedure?.["inputMap"]),
    freeParameters: strings(procedure?.["freeParameters"]),
    footprint: {
      files: footprint?.["files"] === true,
      machine: footprint?.["machine"] === true,
      external: footprint?.["external"] === true,
      spend: footprint?.["spend"] === true,
      concepts: strings(footprint?.["concepts"]),
    },
    target: str(procedure, "target"),
    recordedFrom: recordedFromOf(procedure?.["recordedFrom"]),
    preconditions: preconditionsOf(flat["preconditions"]),
  };
}

/**
 * What to call a procedure.
 *
 * The goal statement it serves, because that is the thing a person asked for
 * and will recognise. The construct NAME is a machine name the lift minted;
 * it is on the page, in the data voice, for matching against the approval
 * that names it -- it is not a title.
 */
export function procedureTitle(p: ProcedureRow): string {
  const title = p.title.trim();
  if (title !== "") return title;
  const runs = p.recordedFrom.runIds.length;
  if (runs > 0) return `Procedure learned from ${runs} ${runs === 1 ? "run" : "runs"}`;
  return "A learned procedure";
}

/**
 * What a free parameter is called: the goal input that binds it, when the lift
 * learned one. A hole id is the procedure's own name for the position, which
 * is what the approval's evidence keys by -- so it is the honest fallback,
 * shown in the data voice.
 */
export function parameterName(inputMap: Record<string, string>, holeId: string): string {
  const key = inputMap[holeId];
  return typeof key === "string" && key.trim() !== "" ? key : holeId;
}

/** Whether the parameter is named by a goal input, rather than by its hole id. */
export function parameterIsNamed(inputMap: Record<string, string>, holeId: string): boolean {
  const key = inputMap[holeId];
  return typeof key === "string" && key.trim() !== "";
}

/**
 * The procedure's free parameters, in a stable order.
 *
 * `freeParameters` is the set the ladder counts bindings over. A parameter the
 * streak has bindings for but the payload does not list is still shown: the
 * evidence is on the row, and dropping it would hide the half of the rule a
 * person is being asked to trust.
 */
export function freeParametersOf(p: ProcedureRow): string[] {
  const out = [...p.freeParameters];
  for (const id of Object.keys(p.distinctBindings)) if (!out.includes(id)) out.push(id);
  return out;
}

// ---------------------------------------------------------------------------
// Where it stands
// ---------------------------------------------------------------------------

export type RungState = "passed" | "current" | "ahead";

/** Each climbing rung's state, for a procedure standing on one of them. */
export function rungStates(ladder: string): Array<{ rung: LadderRung; state: RungState }> {
  const at = LADDER_RUNGS.indexOf(climbingRung(ladder) ?? ("" as LadderRung));
  return LADDER_RUNGS.map((rung, i) => ({
    rung,
    state: at < 0 ? "ahead" : i < at ? "passed" : i === at ? "current" : "ahead",
  }));
}

/**
 * What a count of zero says, given where the procedure stands against the
 * rung the count is earned on.
 *
 * BELOW that rung, or on it, nothing has been earned YET. Off the ladder
 * nothing more will be, so it is plain "none". PAST it, a zero is a counter
 * reset on the way up -- the rung WAS passed, so "none" would be false -- and
 * the answer is null: the fact is left out rather than said wrong.
 */
export function zeroCountWord(ladder: string, earnedOn: LadderRung): string | null {
  const at = climbingRung(ladder);
  if (at === null) return "none";
  return LADDER_RUNGS.indexOf(at) > LADDER_RUNGS.indexOf(earnedOn) ? null : "none yet";
}

/** A rung's place, for ordering: trusted highest, off the ladder below every rung. */
export function ladderRank(ladder: string): number {
  const at = climbingRung(ladder);
  return at === null ? -1 : LADDER_RUNGS.indexOf(at);
}

/**
 * What a person would call a change to a learned procedure: it moved to another
 * rung, or a promotion arrived or was answered.
 *
 * NEVER ITS EVIDENCE. The counts, the last replay and the reason the ladder
 * last wrote move on every shadow comparison and every replay, and a row that
 * rang on them would ring on every evidence tick -- the strobe the OS README's
 * arrival-cue rule exists to prevent ("a heartbeat is not news"). The marks
 * and the freshness already show those continuously.
 */
export function procedureFingerprint(p: ProcedureRow): string {
  return `${p.ladder}|${p.promotionApprovalId}`;
}

/**
 * Whether the next-rung sentence needs the policy's numbers.
 *
 * A candidate's gate is code and a waiting promotion is a fact about the row,
 * so both can be said before the policy row has arrived -- or when it could not
 * be read. Every other rung's sentence is counted against the policy.
 */
export function sentenceNeedsPolicy(p: ProcedureRow): boolean {
  return p.ladder !== "candidate" && !promotionWaiting(p) && climbingRung(p.ladder) !== null;
}

/** Whether a promotion is open on this procedure -- the one thing it waits on you for. */
export function promotionWaiting(p: ProcedureRow): boolean {
  return p.ladder === "shadow" && p.promotionApprovalId.trim() !== "";
}

/**
 * The current rung's state, in words. The rail carries it under the rung's
 * name, so the one accented place on the page says what is happening there
 * and never relies on the accent to say it.
 */
export function currentStateWords(p: ProcedureRow): string {
  switch (p.ladder) {
    case "candidate":
      return "Waiting to be compared";
    case "shadow":
      return promotionWaiting(p) ? "Proposed for canary" : "Running beside the app";
    case "canary":
      return "Running for real";
    case "trusted":
      return "Running on its own";
    default:
      return "";
  }
}

/** One countable piece of evidence: how many, of how many, and what of. */
export interface EvidenceCount {
  /** What is being counted, in words -- or a parameter's name. */
  label: string;
  /** True when the label is a parameter NAME, drawn in the data voice. */
  labelIsData: boolean;
  count: number;
  /** The threshold, or null when the cluster has not published one. */
  need: number | null;
  /** The unit after the numbers: "matches", "bindings", "replays". */
  unit: string;
  /** A count that moves a procedure DOWN wears the warning ink, and only then. */
  tone: "climb" | "fall";
}

/**
 * The counts that stand between a procedure and its next rung, in the order a
 * person reads them.
 *
 * ONLY THE CURRENT RUNG'S. A canary's shadow streak is history -- it earned
 * the proposal that is already decided -- and drawing it beside the replays
 * that do count would ask the reader to work out which numbers still matter.
 * The whole record is on the Evidence panel, in words.
 *
 * A demotion counter is drawn only once it has moved. Zero failed replays on a
 * trusted procedure is the ordinary state, and a row of empty warning marks
 * would make a healthy procedure look like it was on its way down.
 */
export function evidenceCounts(p: ProcedureRow, policy: LadderPolicy | null): EvidenceCount[] {
  const out: EvidenceCount[] = [];
  switch (p.ladder) {
    case "shadow": {
      out.push({
        label: "Matches beside the app",
        labelIsData: false,
        count: p.shadowMatches,
        need: policy?.shadowMatches ?? null,
        unit: "matches",
        tone: "climb",
      });
      for (const id of freeParametersOf(p)) {
        out.push({
          label: parameterName(p.inputMap, id),
          labelIsData: true,
          count: (p.distinctBindings[id] ?? []).length,
          need: policy?.distinctBindings ?? null,
          unit: "bindings",
          tone: "climb",
        });
      }
      break;
    }
    case "canary":
      out.push({
        label: "Clean replays in a row",
        labelIsData: false,
        count: p.canaryMatches,
        need: policy?.canaryMatches ?? null,
        unit: "replays",
        tone: "climb",
      });
      break;
    default:
      break;
  }
  if (p.ladder === "canary" || p.ladder === "trusted") {
    if (p.failures > 0) {
      out.push({
        label: "Failed replays in a row",
        labelIsData: false,
        count: p.failures,
        need: policy?.failuresToDemote ?? null,
        unit: "failures",
        tone: "fall",
      });
    }
    if (p.insufficient > 0) {
      out.push({
        label: "Failed after passing its checks",
        labelIsData: false,
        count: p.insufficient,
        need: policy?.insufficientToDemote ?? null,
        unit: "replays",
        tone: "fall",
      });
    }
  }
  return out;
}

/**
 * The count beside its marks: "3 of 5", or "3 so far" when there is no
 * threshold to set it against. A row labelled by a parameter NAME carries its
 * unit, because "month  2 of 2" does not say what is being counted; a row
 * labelled in words already has.
 */
export function countFigure(c: EvidenceCount): string {
  const unit = c.labelIsData ? ` ${c.count === 1 && c.need === null ? singular(c.unit) : c.unit}` : "";
  if (c.need === null) return `${c.count}${unit} so far`;
  if (c.count > c.need) return `${c.count}${unit}, ${c.need} needed`;
  return `${c.count} of ${c.need}${unit}`;
}

function singular(unit: string): string {
  return unit.endsWith("s") ? unit.slice(0, -1) : unit;
}

/** The count, said whole, for the marks' accessible name. */
export function countSentence(c: EvidenceCount): string {
  const of = c.need === null ? "" : ` of the ${c.need} ${c.tone === "fall" ? "that demote it" : "needed"}`;
  return `${c.label}: ${c.count}${of}`;
}

function plural(n: number, one: string, many: string): string {
  return n === 1 ? one : many;
}

const ORDINALS = ["", "first", "second", "third", "fourth", "fifth", "sixth", "seventh", "eighth", "ninth", "tenth"];

function nth(n: number): string {
  return ORDINALS[n] ?? `${n}th`;
}

/**
 * What the next rung needs, in one sentence, from the POLICY ROW.
 *
 * A cluster that has not published the row gets no numbers here at all:
 * guessing "5" because the Go default is 5 would be this window inventing a
 * policy -- and the one sentence a person would plan around.
 */
export function nextRungSentence(p: ProcedureRow, policy: LadderPolicy | null): string {
  switch (p.ladder) {
    case "candidate":
      // The candidate gate is code, not a value (component/work.CandidateGate),
      // so this sentence is the gate itself. The row's own reason names which
      // part of it is holding this one.
      return "It starts replaying beside the app once it has been used at least twice, every value in it is explained, and no step you disliked stands.";
    case "shadow": {
      if (promotionWaiting(p)) {
        return "Promotion to canary is waiting for your decision. Promoted, it runs for real with the app standing by to take over.";
      }
      const m = policy?.shadowMatches ?? null;
      const k = policy?.distinctBindings ?? null;
      if (m === null || k === null) {
        return "How far it has to go is not shown: this cluster has not published its ladder values.";
      }
      const parts: string[] = [];
      const short = m - p.shadowMatches;
      if (short > 0) parts.push(`${short} more ${plural(short, "match", "matches")} in a row`);
      for (const id of freeParametersOf(p)) {
        const have = (p.distinctBindings[id] ?? []).length;
        if (have >= k) continue;
        const name = parameterName(p.inputMap, id);
        parts.push(
          k - have === 1 ? `a ${nth(have + 1)} binding for ${name}` : `${k - have} more bindings for ${name}`,
        );
      }
      if (parts.length === 0) {
        // Advance proposes on the comparison that meets the threshold, so a
        // row here with no open proposal is one whose proposal has not been
        // written yet -- the next matching comparison raises it.
        return "It has matched enough to be proposed; the proposal is raised with its next match.";
      }
      return `It needs ${joinAnd(parts)}; then promotion to canary is put to you.`;
    }
    case "canary": {
      const n = policy?.canaryMatches ?? null;
      if (n === null) {
        return "How far it has to go is not shown: this cluster has not published its ladder values.";
      }
      const short = Math.max(0, n - p.canaryMatches);
      if (short === 0) return "It has replayed cleanly enough to be trusted; the next clean replay moves it up.";
      return `${short} more clean ${plural(short, "replay", "replays")} in a row and it is trusted to run without a model. Nobody is asked again.`;
    }
    case "trusted": {
      const f = policy?.failuresToDemote ?? null;
      const i = policy?.insufficientToDemote ?? null;
      if (f === null || i === null) {
        return "It stays here while its replays succeed; this cluster has not published how many failures demote it.";
      }
      return `It stays here while its replays succeed. ${f} failed ${plural(f, "replay", "replays")} in a row, or ${i} that ${plural(i, "fails after passing its checks", "fail after passing their checks")}, return it to shadow.`;
    }
    default:
      return "";
  }
}

function joinAnd(parts: string[]): string {
  if (parts.length <= 1) return parts[0] ?? "";
  if (parts.length === 2) return `${parts[0]} and ${parts[1]}`;
  return `${parts.slice(0, -1).join(", ")} and ${parts[parts.length - 1]}`;
}

/** Sentence case for a reason the Go side wrote as a clause. */
export function sentence(text: string): string {
  const t = text.trim();
  if (t === "") return "";
  const first = t.charAt(0).toUpperCase() + t.slice(1);
  return /[.!?]$/.test(first) ? first : `${first}.`;
}

// ---------------------------------------------------------------------------
// The one line a list row says about where a procedure stands
// ---------------------------------------------------------------------------

export interface EvidenceLine {
  text: string;
  /** `waiting` is a real event -- a person is needed -- and wears the accent. */
  tone: "waiting" | "fall" | "quiet";
}

export function evidenceLine(p: ProcedureRow, policy: LadderPolicy | null): EvidenceLine {
  switch (p.ladder) {
    case "candidate":
      return { text: "Not compared yet", tone: "quiet" };
    case "shadow": {
      if (promotionWaiting(p)) return { text: "Promotion waiting for you", tone: "waiting" };
      const m = policy?.shadowMatches ?? null;
      const k = policy?.distinctBindings ?? null;
      if (m !== null && p.shadowMatches < m) {
        return { text: `${p.shadowMatches} of ${m} matches beside the app`, tone: "quiet" };
      }
      if (m !== null && k !== null) {
        let fewest = -1;
        for (const id of freeParametersOf(p)) {
          const have = (p.distinctBindings[id] ?? []).length;
          if (fewest < 0 || have < fewest) fewest = have;
        }
        if (fewest >= 0 && fewest < k) {
          return {
            text: k - fewest === 1 ? `Needs a ${nth(fewest + 1)} binding` : `Needs ${k - fewest} more bindings`,
            tone: "quiet",
          };
        }
      }
      return {
        text: `${p.shadowMatches} ${plural(p.shadowMatches, "match", "matches")} beside the app`,
        tone: "quiet",
      };
    }
    case "canary": {
      if (p.failures > 0) {
        return { text: `${p.failures} failed ${plural(p.failures, "replay", "replays")} in a row`, tone: "fall" };
      }
      const n = policy?.canaryMatches ?? null;
      return {
        text:
          n === null
            ? `${p.canaryMatches} clean ${plural(p.canaryMatches, "replay", "replays")}`
            : `${p.canaryMatches} clean ${plural(p.canaryMatches, "replay", "replays")} of ${n}`,
        tone: "quiet",
      };
    }
    case "trusted":
      if (p.failures > 0) {
        return { text: `${p.failures} failed ${plural(p.failures, "replay", "replays")} in a row`, tone: "fall" };
      }
      return { text: "Replays without a model", tone: "quiet" };
    case "retired": {
      const d = policy?.retireAfterDays ?? null;
      return {
        text: d === null ? "Unused past the retirement window" : `Unused for more than ${d} days`,
        tone: "quiet",
      };
    }
    default:
      return { text: "Never served", tone: "quiet" };
  }
}

/**
 * Where a procedure sits in the merged list, in bands a person can explain:
 * the one waiting on you, then the ones that serve, then the ones still
 * earning it, then the ones that serve nothing. Lower sorts first.
 */
export function procedureBand(p: ProcedureRow): number {
  if (promotionWaiting(p)) return 0;
  switch (p.ladder) {
    case "trusted":
    case "canary":
      return 1;
    case "shadow":
    case "candidate":
      return 2;
    default:
      return 3;
  }
}

// ---------------------------------------------------------------------------
// The steps, as a person reads them
// ---------------------------------------------------------------------------

/** A step's tool, as a verb. */
export function toolWord(tool: string): string {
  switch (tool) {
    case "exec":
      return "Run";
    case "fs_write":
      return "Write";
    case "fs_read":
      return "Read";
    case "fetch":
      return "Fetch";
    case "mcp":
      return "Call";
    case "app_answer":
      return "Answer";
    default:
      return tool === "" ? "--" : tool;
  }
}

/** What each tool does, for the verb's title. */
export function toolMeaning(tool: string): string {
  switch (tool) {
    case "exec":
      return "Runs a command";
    case "fs_write":
      return "Writes a file";
    case "fs_read":
      return "Reads a file";
    case "fetch":
      return "Fetches a URL";
    case "mcp":
      return "Calls a MemQL tool";
    case "app_answer":
      return "Answers the goal";
    default:
      return "";
  }
}

/** One piece of an argument, written out: text, or a value that varies. */
export type ArgToken = { kind: "text"; text: string } | { kind: "hole"; holeId: string };

/** The argument that says what a step DOES, drawn on its own line. */
const PRIMARY_KEYS = ["command", "cmd", "argv", "path", "file", "filePath", "targetPath", "url", "uri", "name", "tool"];

export interface StepArgs {
  /** The argument a person identifies the step by -- the command, the path. */
  primary: ArgToken[];
  /** Every other argument, by name. */
  rest: Array<{ key: string; tokens: ArgToken[] }>;
}

export function stepArgs(node: ArgNode | null): StepArgs {
  if (node === null) return { primary: [], rest: [] };
  if (node.kind !== "object") return { primary: tokensOf(node), rest: [] };
  const keys = node.keys;
  const primaryKey = PRIMARY_KEYS.find((k) => keys.includes(k)) ?? "";
  const rest: StepArgs["rest"] = [];
  let primary: ArgToken[] = [];
  keys.forEach((key, i) => {
    const kid = node.kids[i];
    if (kid === undefined) return;
    if (key === primaryKey) primary = tokensOf(kid);
    else rest.push({ key, tokens: tokensOf(kid) });
  });
  return { primary, rest };
}

/**
 * A node written back as one line, the way the recording read it.
 *
 * A command line is its words; a path is its segments joined; a JSON document
 * is compact JSON. A position where the recordings disagreed is a HOLE token,
 * which the page draws as a named chip -- never as a placeholder string that
 * could be mistaken for a literal somebody typed.
 */
export function tokensOf(node: ArgNode): ArgToken[] {
  const out: ArgToken[] = [];
  const text = (t: string) => {
    const last = out[out.length - 1];
    if (last !== undefined && last.kind === "text") last.text += t;
    else out.push({ kind: "text", text: t });
  };
  // HOW A LITERAL IS SPELLED DEPENDS ON WHAT IT SITS IN: a word on a command
  // line is quoted only when it has to be, a path segment is itself -- an
  // empty one included, since "a//b" is not "a/b" -- and a JSON value is JSON.
  type Voice = "plain" | "argv" | "path" | "json";
  const walk = (n: ArgNode, voice: Voice) => {
    switch (n.kind) {
      case "hole":
        out.push({ kind: "hole", holeId: n.holeId });
        return;
      case "lit":
        text(voice === "json" ? jsonLit(n) : voice === "argv" ? shellWord(n.lit) : n.lit);
        return;
      case "array": {
        if (n.form === "argv") {
          n.kids.forEach((k, i) => {
            if (i > 0) text(" ");
            walk(k, "argv");
          });
          return;
        }
        if (n.form === "path" || n.form === "rootedPath") {
          if (n.form === "rootedPath") text("/");
          n.kids.forEach((k, i) => {
            if (i > 0) text("/");
            walk(k, "path");
          });
          return;
        }
        text("[");
        n.kids.forEach((k, i) => {
          if (i > 0) text(", ");
          walk(k, "json");
        });
        text("]");
        return;
      }
      case "object":
        text("{");
        n.keys.forEach((key, i) => {
          if (i > 0) text(", ");
          text(`${key}: `);
          const kid = n.kids[i];
          if (kid !== undefined) walk(kid, "json");
        });
        text("}");
        return;
    }
  };
  walk(node, "plain");
  return out;
}

function jsonLit(n: ArgNode): string {
  if (n.litType === "number" || n.litType === "bool") return n.lit;
  if (n.litType === "null") return "null";
  return JSON.stringify(n.lit);
}

/** A literal as it would sit on a command line: quoted only when it has to be. */
function shellWord(s: string): string {
  if (s === "") return "''";
  return /\s/.test(s) ? `'${s.replace(/'/g, "'\\''")}'` : s;
}

/** A hole, found by its id. */
export function holeById(p: ProcedureRow, holeId: string): ProcedureHole | null {
  return p.holes.find((h) => h.id === holeId) ?? null;
}

// ---------------------------------------------------------------------------
// The promotion approval (component/work.ProcedurePromotionApproval)
// ---------------------------------------------------------------------------

export const PROCEDURE_PROMOTION = "procedurePromotion";

export interface PromotionSubject {
  constructId: string;
  constructName: string;
  procedureHash: string;
  title: string;
  /** null when the subject did not carry it. */
  shadowMatches: number | null;
  /** free parameter hole id -> distinct bindings the streak saw, in id order. */
  distinctBindings: Array<[string, number]>;
  recordedFrom: RecordedFrom;
}

/** The subject of a promotion approval, or null for every other kind. */
export function promotionSubject(approval: ApprovalRow): PromotionSubject | null {
  if (approval.kind !== PROCEDURE_PROMOTION) return null;
  const s = approval.subject;
  const matches = s?.["shadowMatches"];
  const bindings = objectOf(s?.["distinctBindings"]);
  return {
    constructId: str(s, "constructId"),
    constructName: str(s, "constructName"),
    procedureHash: str(s, "procedureHash"),
    title: str(s, "title"),
    shadowMatches: typeof matches === "number" && Number.isFinite(matches) ? matches : null,
    distinctBindings: Object.entries(bindings ?? {})
      .filter((e): e is [string, number] => typeof e[1] === "number")
      .sort(([a], [b]) => a.localeCompare(b)),
    recordedFrom: recordedFromOf(s?.["recordedFrom"]),
  };
}

/** The app a recording came from, as its maker names it. */
export function appWord(app: string): string {
  switch (app) {
    case "claude-code":
      return "Claude Code";
    case "codex":
      return "Codex";
    default:
      return app;
  }
}
