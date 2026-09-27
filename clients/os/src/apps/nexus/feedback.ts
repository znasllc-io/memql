import type { ObservationRow, ValidationSummary } from "./rows";
import { axesPhrase, type Verdict } from "./words";

// A person's verdicts and the validator's, read out of the journal (epic
// memql#5414, design D21 and D22). Pure, like rows.ts.
//
// ===========================================================================
// BOTH ARE OBSERVATIONS, AND NEITHER IS LIVE
// ===========================================================================
// A verdict is a `feedback` observation and the validator's detail is a
// `decision` observation; `v1:work:observation` does not broadcast. So the run
// page reads them, and a verdict this window writes is added from the write's
// own reply -- which is the server confirming it -- rather than waiting on a
// feed that will never bring it.
//
// A LATER VERDICT IS A NEW ROW and never rewrites an earlier one, so "what did
// I say about this" is the NEWEST row for the target, and the older ones stay
// in the journal as the history they are.

export interface Axes {
  product: boolean;
  process: boolean;
  performance: boolean;
}

export const NO_AXES: Axes = { product: false, process: false, performance: false };

export function anyAxis(axes: Axes): boolean {
  return axes.product || axes.process || axes.performance;
}

/** What a verdict is about: one version of one step, or -- with no step -- the whole run. */
export interface FeedbackTarget {
  stepKey: string;
  version: number | null;
}

export const RUN_TARGET: FeedbackTarget = { stepKey: "", version: null };

/** One key per target, for the maps that hold drafts, busy flags and refusals. */
export function targetKey(target: FeedbackTarget): string {
  return target.stepKey === "" ? "run" : `${target.stepKey}@v${target.version ?? 0}`;
}

export interface FeedbackEntry {
  id: string;
  verdict: Verdict;
  axes: Axes;
  reason: string;
  target: FeedbackTarget;
  /** The server's own answer to "does the validator see it the other way". */
  validatorDisagrees: boolean;
  createdAt: string;
}

export interface ValidatorEntry {
  id: string;
  verdict: "pass" | "flag";
  axes: Axes;
  reason: string;
  target: FeedbackTarget;
  level: string;
  model: string;
  createdAt: string;
}

function objectOf(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function axesOf(value: unknown): Axes {
  const obj = objectOf(value);
  return {
    product: obj?.["product"] === true,
    process: obj?.["process"] === true,
    performance: obj?.["performance"] === true,
  };
}

function targetOf(value: unknown): FeedbackTarget {
  const obj = objectOf(value);
  const stepKey = typeof obj?.["stepKey"] === "string" ? (obj["stepKey"] as string) : "";
  const version = obj?.["version"];
  return {
    stepKey,
    version: stepKey !== "" && typeof version === "number" && Number.isFinite(version) ? Math.round(version) : null,
  };
}

/** A person's verdict, or null for any observation that is not one. */
export function feedbackFromObservation(observation: ObservationRow): FeedbackEntry | null {
  if (observation.kind !== "feedback") return null;
  const data = observation.data;
  const verdict = data?.["verdict"];
  if (verdict !== "like" && verdict !== "dislike" && verdict !== "neutral") return null;
  return {
    id: observation.id,
    verdict,
    axes: axesOf(data?.["axes"]),
    reason: typeof data?.["reason"] === "string" ? (data["reason"] as string) : "",
    target: targetOf(data?.["target"]),
    validatorDisagrees: data?.["validatorDisagrees"] === true,
    createdAt: observation.createdAt,
  };
}

/**
 * The validator's verdict, or null.
 *
 * ONLY A `decision` CARRYING A `validator` BLOCK. `decision` is also every
 * other decision a run records, and reading one of those as a verdict on the
 * answer would put the validator's name on something it never said.
 */
export function validatorFromObservation(observation: ObservationRow): ValidatorEntry | null {
  if (observation.kind !== "decision") return null;
  const data = observation.data;
  const block = objectOf(data?.["validator"]);
  if (block === null) return null;
  const axes = axesOf(block["axes"]);
  const stated = block["verdict"];
  // A block that names no verdict is read from its axes: an axis set to true
  // is a problem found on it, which is what "flag" means.
  const verdict = stated === "pass" || stated === "flag" ? stated : anyAxis(axes) ? "flag" : "pass";
  return {
    id: observation.id,
    verdict,
    axes,
    reason: typeof block["reason"] === "string" ? (block["reason"] as string) : "",
    target: targetOf(data?.["target"]),
    level: typeof data?.["level"] === "string" ? (data["level"] as string) : "",
    model: typeof data?.["model"] === "string" ? (data["model"] as string) : "",
    createdAt: observation.createdAt,
  };
}

function sameTarget(a: FeedbackTarget, b: FeedbackTarget): boolean {
  if (a.stepKey !== b.stepKey) return false;
  return a.stepKey === "" || a.version === b.version;
}

/**
 * The newest of a list, by `createdAt`, with LATER IN THE LIST winning a tie --
 * a verdict this window just wrote is appended with this browser's clock, and
 * two rows in one second are the ordinary case for a person changing their
 * mind.
 */
function newest<T extends { createdAt: string }>(entries: readonly T[]): T | null {
  let best: T | null = null;
  for (const entry of entries) {
    if (best === null || entry.createdAt >= best.createdAt) best = entry;
  }
  return best;
}

/** The newest verdict a person gave this target, or null. */
export function newestVerdict(entries: readonly FeedbackEntry[], target: FeedbackTarget): FeedbackEntry | null {
  return newest(entries.filter((entry) => sameTarget(entry.target, target)));
}

/**
 * The validator's verdict the run names.
 *
 * THE RUN'S OWN POINTER FIRST: `run.validation.observationId` says which
 * decision is the current one. Without it the newest validator decision
 * stands.
 */
export function validatorFor(entries: readonly ValidatorEntry[], summary: ValidationSummary | null): ValidatorEntry | null {
  if (summary !== null && summary.observationId !== "") {
    const named = entries.find((entry) => entry.id === summary.observationId);
    if (named !== undefined) return named;
  }
  return newest(entries);
}

/**
 * Whether the validator sees it the other way.
 *
 * A like on a flagged answer, or a dislike on a passed one. NEUTRAL NEVER
 * DISAGREES: it is a person declining to judge, and a disagreement with
 * somebody who said nothing is not one.
 */
export function disagrees(verdict: Verdict | null, validator: "pass" | "flag" | string | null): boolean {
  if (verdict === null || validator === null) return false;
  if (verdict === "like") return validator === "flag";
  if (verdict === "dislike") return validator === "pass";
  return false;
}

/**
 * The validator's line, in words.
 *
 * "Checked before you saw it" and never "Validated": this is a pre-filter a
 * model ran, it never certifies anything and it never moves a procedure's
 * ladder, so it is not given a word that sounds like a stamp. A flag whose
 * detail has not been read yet says "flagged a problem" rather than guessing
 * which.
 */
export function validatorSentence(verdict: string, entry: ValidatorEntry | null, stepKey = ""): string {
  const where = stepKey === "" ? "" : ` in ${stepKey}`;
  if (verdict === "pass") return `Checked before you saw it: no problems found${where}.`;
  const phrase = entry === null ? "" : axesPhrase(entry.axes);
  const what = phrase === "" ? "a problem" : phrase;
  const reason = entry?.reason.trim() ?? "";
  return reason === ""
    ? `Checked before you saw it: flagged ${what}${where}.`
    : `Checked before you saw it: flagged ${what}${where} -- ${reason}`;
}

/** What the saved verdict said, for the quiet line under the pills. "" for anything but a dislike. */
export function dislikeSummary(entry: FeedbackEntry | null): string {
  if (entry === null || entry.verdict !== "dislike") return "";
  const phrase = axesPhrase(entry.axes);
  const head = phrase === "" ? "You disliked it" : `You disliked ${phrase}`;
  const reason = entry.reason.trim();
  return reason === "" ? `${head}.` : `${head}: ${reason}`;
}
