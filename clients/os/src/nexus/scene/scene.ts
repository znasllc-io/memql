// The world as it stood at a moment.
//
// `scene(world, at)` is what makes Replay possible without a backend: it takes
// the rows the feed already holds and returns the SAME shape, narrowed to what
// existed at `at` and with the statuses those rows had then. The map renders
// `scene(world, NOW)` -- the whole world, now -- and Replay renders
// `scene(world, scrubPosition)`. One renderer, one layout, one code path.
//
// ===========================================================================
// PRESENCE: A ROW WITH NO createdAt IS ALWAYS PRESENT
// ===========================================================================
// The engine stamps `createdAt` on every row, so this should never bite. When
// it does -- a projection that omitted it, a fixture, an older node -- the row
// is kept at every scrub position rather than filtered out. Dropping it would
// make a node that genuinely exists disappear as soon as somebody touched the
// scrubber, which reads as a rendering bug; keeping it makes the node appear
// earlier than it should, which reads as what it is (an arrival the cluster
// did not date). The wrong-in-the-visible-direction choice is the right one.
//
// ===========================================================================
// STATUS: DERIVED FROM THE ROW'S OWN DATED TRANSITIONS
// ===========================================================================
// The spine dates everything this surface needs, which is a simplification
// over the portal: a step carries `startedAt` and `finishedAt`, a run carries
// both, an approval carries `requestedAt` and `decidedAt`. So there is no
// approximation here of the kind the portal had to make for a construct's
// status, and no status is silently pinned to "whatever it is now" at a point
// in the past.
//
// The one thing to be careful of is the DIRECTION of the fallback. A step
// whose `finishedAt` has not been reached reads `running` if it had started
// and `pending` otherwise -- never its CURRENT status, because a step that is
// `failed` now was not failed an hour before it ran.

import type { ApprovalRow, GoalRow, GoalWorld, RunRow, StepRow } from "./world";
import { latestAttempts } from "./world";

/**
 * "" means NOW -- the whole world, unnarrowed. Used by the map, which is not
 * scrubbing, so it does not pay for a full copy of every collection.
 */
export const NOW = "";

/**
 * The presence rule, in one place. See the header on why an undated row is
 * present.
 *
 * EXPORTED because the goal view's RAIL is drawn from the app's own row
 * projection rather than from this library's, and a page whose map is rewound
 * and whose rail is not is a page showing two moments at once.
 */
export function existedAt(createdAt: string, at: string): boolean {
  const stamp = createdAt.trim();
  if (stamp === "") return true;
  return stamp <= at;
}

/**
 * Had this dated transition occurred by `at`?
 *
 * An empty stamp is NOT a transition that happened -- it is one the row does
 * not record, which is the opposite answer.
 */
function happenedBy(stamp: string, at: string): boolean {
  const moment = stamp.trim();
  return moment !== "" && moment <= at;
}

/**
 * Structural, not `StepRow`, for the reason `existedAt` is exported: the app's
 * rail holds its own projection of the same rows, and both readings of a
 * moment have to come from one function.
 */
export function stepStatusAt(
  step: Pick<StepRow, "status" | "startedAt" | "finishedAt">,
  at: string,
): string {
  if (at === NOW) return step.status;
  if (happenedBy(step.finishedAt, at)) return step.status;
  if (happenedBy(step.startedAt, at)) return "running";
  return "pending";
}

export function runStatusAt(run: RunRow, at: string): string {
  if (at === NOW) return run.status;
  if (happenedBy(run.finishedAt, at)) return run.status;
  if (happenedBy(run.startedAt, at)) return "running";
  return "compiling";
}

export function goalStatusAt(goal: GoalRow, at: string): string {
  if (at === NOW) return goal.status;
  if (happenedBy(goal.closedAt, at)) return goal.status;
  // `open` and `active` are both pre-terminal and the goal row dates neither
  // transition, so a scrub before the close reports `active` when a run had
  // started by then and `open` otherwise. That is decided by the CALLER, which
  // holds the runs -- here the honest floor is `open`.
  return "open";
}

export function approvalDecisionAt(approval: ApprovalRow, at: string): string {
  if (at === NOW) return approval.decision;
  return happenedBy(approval.decidedAt, at) ? approval.decision : "";
}

export function scene(world: GoalWorld, at: string): GoalWorld {
  if (at === NOW) return world;

  const goal =
    world.goal === null || !existedAt(world.goal.createdAt, at)
      ? null
      : { ...world.goal, status: goalStatusAt(world.goal, at) };

  const runs = world.runs
    .filter((run) => existedAt(run.createdAt, at))
    .map((run) => ({ ...run, status: runStatusAt(run, at) }));

  // The OPEN run stays the open run even before it existed at `at`: it is a
  // selection the person made, not a fact about the world, and dropping it
  // would make the first frames of a replay render the goal with no run
  // selected and therefore no road at all.
  const run =
    world.run === null
      ? null
      : (runs.find((candidate) => candidate.id === world.run?.id) ?? {
          ...world.run,
          status: runStatusAt(world.run, at),
        });

  return {
    goal,
    run,
    runs,
    steps: world.steps
      .filter((step) => existedAt(step.createdAt, at))
      .map((step) => ({
        ...step,
        status: stepStatusAt(step, at),
        // A BINDING IS RECORDED AT DISPATCH, so a step that had not started by
        // `at` had no binding then either. Carrying the live one back would
        // draw a machine and a model above a step nothing had yet chosen one
        // for -- the same fabrication as un-deciding an approval guards
        // against, on the other lane of the map.
        binding: happenedBy(step.startedAt, at)
          ? step.binding
          : { ...step.binding, present: false },
      })),
    approvals: world.approvals
      .filter((approval) => existedAt(approval.createdAt, at))
      .map((approval) => ({ ...approval, decision: approvalDecisionAt(approval, at) })),
  };
}

export interface GoalProgress {
  completed: number;
  total: number;
  /**
   * 0..1. Zero when the run has no steps yet -- which is a REAL state and not
   * the same as "no progress on the steps it has", so callers that need to
   * tell them apart read `total`.
   */
  fraction: number;
  /**
   * The beacon is lit when the goal is closed AND its run succeeded, and only
   * then. A run whose steps have all landed but whose goal has not been closed
   * is still dark, because closing is the thing that decides.
   */
  lit: boolean;
  /**
   * True while the run has no step count to be a fraction OF. The beacon
   * renders EMPTY with the word rather than at some fraction of a number
   * nobody has yet -- a compiling run at "0%" reads as a run that has failed
   * to do anything.
   */
  compiling: boolean;
}

/**
 * What fills the beacon: finished steps over all of them, at whatever moment
 * the world represents.
 *
 * COUNTED PER STEP KEY, NOT PER ROW, and that is a correctness matter rather
 * than a preference. A retried step is several rows sharing one `key`, and
 * counting rows puts every failed attempt in the denominator forever -- so a
 * goal that retried one step twice and then succeeded completely would fill to
 * six sevenths and stop, with a beacon that can never light on a run that ever
 * had to try again. The map draws one node for the step; the beacon counts the
 * same one.
 */
export function goalProgress(world: GoalWorld): GoalProgress {
  const run = world.run;
  if (run === null) {
    return { completed: 0, total: 0, fraction: 0, lit: false, compiling: false };
  }
  const mine = world.steps.filter((step) => step.runId === run.id);
  const steps = [...latestAttempts(mine).values()];
  const completed = steps.filter(
    (step) => step.status === "done" || step.status === "skipped",
  ).length;
  const total = steps.length;
  return {
    completed,
    total,
    fraction: total === 0 ? 0 : completed / total,
    lit: world.goal?.status === "closed" && run.status === "succeeded",
    compiling: total === 0 && run.status === "compiling",
  };
}
