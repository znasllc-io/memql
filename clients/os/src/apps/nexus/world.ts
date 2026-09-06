import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  EMPTY_WORLD,
  readApproval,
  readGoal,
  readRun,
  readStep,
  type GoalWorld,
} from "../../nexus/scene/world";
import { idTail } from "./rows";

// The one place raw rows become a GoalWorld.
//
// ===========================================================================
// WHY THERE ARE TWO NARROWINGS AND NOT ONE
// ===========================================================================
// `rows.ts` beside this file narrows the same wire rows too, and that is
// deliberate rather than an oversight. They answer different questions:
// `rows.ts` produces what the LISTS and the RAIL render -- titles,
// fingerprints, spend figures, the kind band -- and `scene/world.ts` produces
// what the MAP positions, which is a smaller field set plus node identity and
// dependency depth. Neither is a subset of the other, and folding them
// together would put the app's fingerprints inside a library whose whole value
// is that it is pure and testable without React.
//
// What must NOT be duplicated is a DERIVATION, and one was: `kindCalledAModel`
// now lives in the leaf and this app re-exports it, after the map and the rail
// briefly disagreed about whether a `decision` step called a model.
//
// ===========================================================================
// ONE RUN, CHOSEN BY THE CALLER
// ===========================================================================
// A goal's progress toward being done is ONE run's progress; a replay or a
// fork is a different attempt at the same goal rather than further progress on
// it. So the world carries every run of the goal (for the picker and the
// count) and ONE of them as `run` -- and the map draws that one.

/**
 * The run the goal view opens on when nobody has chosen.
 *
 * The run still going, if there is one; otherwise the most recent. NOT simply
 * "the newest": a person arriving at a goal wants the thing that is happening,
 * and a replay opened yesterday is newer than the live run it was taken from.
 */
export function defaultRunId(runRows: readonly Row[], goalId: string): string {
  const mine = runRows
    .map((row) => readRun(row))
    .filter((run): run is NonNullable<typeof run> => run !== null)
    .filter((run) => idTail(run.goalId) === idTail(goalId));
  if (mine.length === 0) return "";
  const live = mine
    .filter((run) => run.status === "running" || run.status === "waiting" || run.status === "compiling")
    .sort((a, b) => (a.startedAt < b.startedAt ? 1 : -1))[0];
  if (live !== undefined) return live.id;
  return mine.sort((a, b) => (a.createdAt < b.createdAt ? 1 : -1))[0]?.id ?? "";
}

export interface BuildWorldInput {
  goalRow: Row | null;
  runRows: readonly Row[];
  stepRows: readonly Row[];
  approvalRows: readonly Row[];
  openRunId: string;
}

export function buildWorld({
  goalRow,
  runRows,
  stepRows,
  approvalRows,
  openRunId,
}: BuildWorldInput): GoalWorld {
  const goal = readGoal(goalRow);
  if (goal === null) return EMPTY_WORLD;

  const runs = runRows
    .map((row) => readRun(row))
    .filter((run): run is NonNullable<typeof run> => run !== null)
    .filter((run) => idTail(run.goalId) === idTail(goal.id))
    // Newest first, which is the order the picker offers them in. A total
    // order on `createdAt` then id, never fold order.
    .sort((a, b) =>
      a.createdAt !== b.createdAt
        ? a.createdAt < b.createdAt
          ? 1
          : -1
        : a.id < b.id
          ? 1
          : -1,
    );

  const run = runs.find((candidate) => idTail(candidate.id) === idTail(openRunId)) ?? null;

  return {
    goal,
    run,
    runs,
    // The step and approval feeds are filtered to the OPEN run rather than
    // trusted to hold only its rows: the approvals feed is the app root's and
    // holds every pending approval this person owns.
    steps:
      run === null
        ? []
        : stepRows.map(readStep).filter((step) => idTail(step.runId) === idTail(run.id)),
    approvals:
      run === null
        ? []
        : approvalRows
            .map(readApproval)
            .filter((approval) => idTail(approval.runId) === idTail(run.id)),
  };
}
