// The receipt: what a goal cost and what it left behind.
//
// Design D8 -- "the reward is the receipt". No points, no streaks, no badge.
// When a goal reaches a terminal status a card materializes under it with the
// numbers a professional actually wants: how long it took, how much work it
// was, what got raised, produced and authored, and what it spent.
//
// Pure, and separated from the card component on purpose: the card is
// rendered in the SCENE (memql#4376) and again in Replay at the moment of
// success, and both need the same arithmetic. A component that computed it
// would give two answers the first time one of them was changed.
//
// ===========================================================================
// A FAILED GOAL GETS THE SAME CARD
// ===========================================================================
// Not a consolation screen and not silence: the same six readings, plus the
// failure and the last task that was running when it stopped. A goal that
// failed after nine hours and forty tasks cost exactly that, and hiding the
// cost because the outcome was bad is the version of this feature that
// flatters instead of informing.

import { semanticTasks, type GoalWorld } from "./world";

export type Outcome = "succeeded" | "failed" | "cancelled";

export interface Receipt {
  outcome: Outcome;
  // Milliseconds from the goal being set to the goal ending. -1 when either
  // end is missing or unparseable -- distinguishable from 0 (a goal that
  // ended in the same millisecond it started, which a cached plan genuinely
  // can), so the card can decline to render a duration it cannot compute
  // rather than printing "0s".
  elapsedMs: number;
  tasks: number;
  // Retried steps count ONCE. The receipt answers "how much work was this",
  // and a step that failed twice before landing is one piece of work, not
  // three -- the retries are in the timeline, where they belong.
  attempts: number;
  agents: number;
  artifacts: number;
  constructs: number;
  tokensSpent: number;
  // What the subscription covered (epic memql#4358). `null` when the engine
  // serving this plan does not carry the field at all, which is a different
  // fact from "covered nothing" and renders as an absent line rather than a
  // zero.
  subscriptionCovered: number | null;
  // Present only on a failure. The plan's own message, and the name of the
  // task that was still running when it stopped -- the two questions asked
  // in that order every time.
  failure: string;
  lastRunningTask: string;
  // Present only on a cancellation: who stopped it.
  cancelledBy: string;
}

const TERMINAL: Record<string, Outcome> = {
  succeeded: "succeeded",
  failed: "failed",
  cancelled: "cancelled",
};

// elapsed parses the two ends and refuses to guess. Date.parse returns NaN
// for an empty or malformed stamp, and a NaN that reaches arithmetic becomes
// a duration rendered as "NaNs" -- which is the failure this guard exists to
// keep off a card an operator reads as a record.
function elapsed(from: string, to: string): number {
  const start = Date.parse(from);
  const end = Date.parse(to);
  if (Number.isNaN(start) || Number.isNaN(end)) return -1;
  const ms = end - start;
  return ms < 0 ? -1 : ms;
}

// receipt returns null for a goal that has not ended. Callers render nothing
// on null rather than an empty card: the absence of the receipt IS the
// statement that the goal is still going.
export function receipt(world: GoalWorld): Receipt | null {
  const plan = world.plan;
  if (plan === null) return null;
  const outcome = TERMINAL[plan.status];
  if (outcome === undefined) return null;

  const semantic = semanticTasks(world.tasks);
  const steps = new Set<string>();
  for (const task of semantic) {
    steps.add(task.logicalStepId === "" ? task.id : task.logicalStepId);
  }

  // The last task still RUNNING when the goal stopped. Read off the rows'
  // own statuses rather than "the highest seq", because a plan that stopped
  // mid-phase can have queued tasks with a higher seq than the one that was
  // actually in flight.
  const running = semantic
    .filter((task) => task.status === "running")
    .sort((a, b) => (a.startedAt < b.startedAt ? 1 : a.startedAt > b.startedAt ? -1 : 0))[0];

  return {
    outcome,
    elapsedMs: elapsed(plan.createdAt, plan.completedAt),
    tasks: semantic.length,
    attempts: steps.size,
    agents: world.agents.length,
    artifacts: world.artifacts.length,
    constructs: world.constructs.length,
    tokensSpent: plan.tokenSpent,
    subscriptionCovered: plan.hasTokenSpentSubscription ? plan.tokenSpentSubscription : null,
    failure: outcome === "failed" ? plan.errorMessage : "",
    lastRunningTask:
      outcome === "failed" && running !== undefined
        ? running.kind === ""
          ? running.id
          : running.kind
        : "",
    cancelledBy: outcome === "cancelled" ? plan.cancelledBy : "",
  };
}

// formatElapsed renders a duration the way an operator reads one: the two
// largest units, never more. "4h 12m", "12m 30s", "8s". Exported because the
// card and the page header both print it and a second implementation would
// drift.
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
