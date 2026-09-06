// The receipt: what a goal cost and what it left behind.
//
// No points, no streaks, no badge. When a run reaches a terminal status a card
// materializes under the map with the numbers a professional actually wants:
// how long it took, how much work it was, how much of it had to think, what it
// had to ask, and what it spent.
//
// Pure, and separated from the card component on purpose: the card is rendered
// in the map and again in Replay at the moment of success, and both need the
// same arithmetic. A component that computed it would give two answers the
// first time one of them was changed.
//
// ===========================================================================
// A FAILED RUN GETS THE SAME CARD
// ===========================================================================
// Not a consolation screen and not silence: the same readings, plus the
// failure and the step that was running when it stopped. A run that failed
// after nine hours and forty steps cost exactly that, and hiding the cost
// because the outcome was bad is the version of this feature that flatters
// instead of informing.

import { latestAttempts, stepThought, type GoalWorld } from "./world";

export type Outcome = "succeeded" | "failed" | "cancelled" | "abandoned";

export interface Receipt {
  outcome: Outcome;
  /**
   * Milliseconds from the run starting to the run ending. -1 when either end
   * is missing or unparseable -- distinguishable from 0 (a run that ended in
   * the same millisecond it started, which a fully-cached replay genuinely
   * can), so the card can decline to render a duration it cannot compute
   * rather than printing "0s".
   */
  elapsedMs: number;
  /** Step KEYS, so a retried step counts once: the receipt answers "how much
   *  work was this", and a step that failed twice before landing is one piece
   *  of work. The retries are in the timeline, where they belong. */
  steps: number;
  /** Rows, which is how many times something actually ran. */
  attempts: number;
  /** Steps whose derived kind says the machine had to think. An
   *  UNCLASSIFIED step is not counted here and not counted as free either --
   *  see `kindCalledAModel`, which answers three ways. */
  thought: number;
  approvals: number;
  tokens: number;
  /**
   * What a subscription covered. `null` when the run row carries no `spent`
   * object at all, which is a different fact from "covered nothing" and
   * renders as an ABSENT line rather than a zero.
   */
  subscriptionTokens: number | null;
  cost: number | null;
  modelCalls: number | null;
  /** Present only on a failure: the run's own message, and the step that was
   *  still running when it stopped -- the two questions asked in that order. */
  failure: string;
  lastRunningStep: string;
  /** Present only on a cancellation: who stopped it. */
  cancelledBy: string;
}

const TERMINAL: Record<string, Outcome> = {
  succeeded: "succeeded",
  failed: "failed",
  cancelled: "cancelled",
  abandoned: "abandoned",
};

/**
 * Parse the two ends and refuse to guess.
 *
 * `Date.parse` returns NaN for an empty or malformed stamp, and a NaN that
 * reaches arithmetic becomes a duration rendered as "NaNs" -- the failure this
 * guard exists to keep off a card somebody reads as a record.
 */
function elapsed(from: string, to: string): number {
  const start = Date.parse(from);
  const end = Date.parse(to);
  if (Number.isNaN(start) || Number.isNaN(end)) return -1;
  const ms = end - start;
  return ms < 0 ? -1 : ms;
}

/**
 * Null for a run that has not ended.
 *
 * Callers render nothing on null rather than an empty card: the ABSENCE of the
 * receipt is the statement that the run is still going.
 */
export function receipt(world: GoalWorld): Receipt | null {
  const run = world.run;
  if (run === null) return null;
  const outcome = TERMINAL[run.status];
  if (outcome === undefined) return null;

  const rows = world.steps.filter((step) => step.runId === run.id);
  const byKey = latestAttempts(rows);
  const steps = [...byKey.values()];

  // The last step still RUNNING when the run stopped. Read off the rows' own
  // statuses rather than "the highest seq", because a run that stopped
  // mid-graph can have pending steps with a higher seq than the one that was
  // actually in flight.
  const running = steps
    .filter((step) => step.status === "running")
    .sort((a, b) => (a.startedAt < b.startedAt ? 1 : a.startedAt > b.startedAt ? -1 : 0))[0];

  const spend = run.spent;
  return {
    outcome,
    elapsedMs: elapsed(run.startedAt, run.finishedAt),
    steps: byKey.size,
    attempts: rows.length,
    thought: steps.filter(stepThought).length,
    approvals: world.approvals.filter((approval) => approval.runId === run.id).length,
    tokens: spend.tokens,
    subscriptionTokens: spend.present ? spend.tokensSubscription : null,
    cost: spend.present ? spend.cost : null,
    modelCalls: spend.present ? spend.modelCalls : null,
    failure: outcome === "failed" || outcome === "abandoned" ? run.errorMessage : "",
    lastRunningStep:
      (outcome === "failed" || outcome === "abandoned") && running !== undefined
        ? running.key === ""
          ? running.id
          : running.key
        : "",
    cancelledBy: outcome === "cancelled" ? run.cancelledBy : "",
  };
}

/**
 * Render a duration the way somebody reads one: the two largest units, never
 * more. "4h 12m", "12m 30s", "8s". Exported because the card and the page
 * header both print it and a second implementation would drift.
 */
export function formatElapsed(ms: number): string {
  if (ms < 0) return "";
  const seconds = Math.floor(ms / 1000);
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = seconds % 60;
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m ${rest}s`;
  return `${rest}s`;
}
