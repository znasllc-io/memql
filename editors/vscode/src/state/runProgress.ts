// How far along a long run is: one number that only moves forward, and one
// short line saying what is happening now.
//
// Install, repair, uninstall, rebuild and update are all graph runs, and they
// are all long in the same uneven way: sixteen steps where one of them
// (creating the cluster) is most of the wait. Counting steps settled out of
// steps total makes the bar sprint through the quick checks and then sit at
// two thirds for eight minutes, which reads as stuck. So each step carries a
// WEIGHT -- the seconds it is expected to take -- and the bar is the share of
// the run's expected time that is behind it.
//
// THREE SOURCES OF TRUTH, IN ORDER OF PREFERENCE, for how far a running step
// has got:
//
//  1. its own phase count, when the script reports one (`cap_progress` in
//     scripts/lib/capability.sh -- "Starting services 5 of 9");
//  2. otherwise its elapsed time against its weight, capped at 90% so a step
//     that overruns its estimate slows to a crawl instead of claiming it is
//     finished;
//  3. its weight itself comes from the most recent successful run of that step
//     on this machine when one exists (`historicalWeights`), and from the
//     defaults below when not.
//
// THE BAR NEVER MOVES BACKWARDS WITHIN AN ATTEMPT. A phase count can restart
// lower than the elapsed-time estimate it replaces, and a step can finish
// sooner than expected; the caller keeps the high-water mark and passes it back
// in, so the number shown is the most that has ever been true. A new attempt
// (a Retry) starts a new high-water mark: it genuinely has more left to do.
//
// PURE except for `historicalWeights`, which reads the run records it is
// pointed at. No `vscode` import (cmd/memql-lsp/vscodeimportrule_test.go), so
// every claim the bar makes is asserted under bare `node --test`.

import type { Run, RunKind } from "./deployments.js";
import { listRuns, sortRunsNewestFirst } from "./runLog.js";

/** A step's state as far as progress is concerned: the executor's four, plus not-yet and now. */
export type ProgressStatus = "pending" | "running" | "ok" | "failed" | "skipped" | "preserved";

/** What a running step last said about itself (a `stepPhase` event). */
export interface ProgressPhase {
  label: string;
  /** Present together with `total` or not at all. */
  done?: number;
  total?: number;
}

export interface ProgressStep {
  id: string;
  /** The graph's short label ("Creating the cluster"). */
  label: string;
  /** Expected seconds. */
  weight: number;
  status: ProgressStatus;
  /** Epoch milliseconds. */
  startedAt?: number;
  /** Epoch milliseconds. */
  finishedAt?: number;
  phase?: ProgressPhase;
}

export interface RunProgress {
  /** 0-100, a whole number, never lower than the high-water mark passed in. */
  percent: number;
  /**
   * The one status line: the running step's current phase ("Starting services
   * 5 of 9") or its label, the heaviest running step's when a wave runs
   * several; the next step's label between steps; "Finishing" once every step
   * has settled.
   */
  status: string;
  /** "Step 6 of 16", counting only steps that will run; "" when none will. */
  stepText: string;
  /** Pass back in as `prior.highWater` on the next call within this attempt. */
  highWater: number;
}

/**
 * Expected seconds per step when the run history has no measurement.
 *
 * The design record's table, plus the two steps it does not list that run the
 * same work as one it does: a rebuild runs the image build `buildImages` runs,
 * and fetching updates costs about what the first download did.
 */
export const DEFAULT_STEP_WEIGHTS: Readonly<Record<string, number>> = {
  detect: 5,
  dockerAccess: 3,
  providerFederation: 1,
  toolK3d: 15,
  toolKubectl: 15,
  toolMkcert: 15,
  hostsBlock: 5,
  browserTrust: 30,
  stackCheckout: 30,
  localCA: 10,
  clusterUp: 480,
  buildImages: 900,
  seedBootstrap: 90,
  frontDoor: 20,
  magicLink: 5,
  enrolmentLink: 5,
  recoveryKey: 5,
  removeCluster: 30,
  removeCheckout: 3,
  removeHostsBlock: 3,
  removeLocalCA: 3,
  removeToolK3d: 3,
  removeToolKubectl: 3,
  removeToolMkcert: 3,
  rebuildFromCheckout: 900,
  updateCheckout: 30,
};

/** The weight of a step nobody has measured or estimated. */
export const UNKNOWN_STEP_WEIGHT = 10;

/** How much of its weight a step may claim on elapsed time alone. */
const ELAPSED_CEILING = 0.9;

export function defaultStepWeight(id: string): number {
  return DEFAULT_STEP_WEIGHTS[id] ?? UNKNOWN_STEP_WEIGHT;
}

/** A step's weight: measured on this machine when it has been, the default otherwise. */
export function stepWeight(id: string, historical?: Readonly<Record<string, number>>): number {
  const measured = historical?.[id];
  return measured !== undefined && Number.isFinite(measured) && measured > 0 ? measured : defaultStepWeight(id);
}

function isSettled(status: ProgressStatus): boolean {
  return status === "ok" || status === "failed" || status === "preserved" || status === "skipped";
}

function weightOf(step: ProgressStep): number {
  return Number.isFinite(step.weight) && step.weight > 0 ? step.weight : UNKNOWN_STEP_WEIGHT;
}

function hasCount(phase: ProgressPhase | undefined): phase is ProgressPhase & { done: number; total: number } {
  return (
    phase !== undefined &&
    typeof phase.done === "number" &&
    typeof phase.total === "number" &&
    Number.isFinite(phase.done) &&
    Number.isFinite(phase.total) &&
    phase.total > 0
  );
}

/** How much of its own weight a running step has earned. */
function runningFraction(step: ProgressStep, now: number): number {
  if (hasCount(step.phase)) {
    return Math.min(1, Math.max(0, step.phase.done / step.phase.total));
  }
  if (step.startedAt === undefined) return 0;
  const elapsed = Math.max(0, (now - step.startedAt) / 1000);
  return Math.min(ELAPSED_CEILING, elapsed / weightOf(step));
}

function labelOf(step: ProgressStep): string {
  return step.label !== "" ? step.label : step.id;
}

/**
 * The run's progress at `now` (epoch milliseconds).
 *
 * SKIPPED STEPS LEAVE THE TOTAL the moment they are known to be skipped. A
 * repair skips most of an install, and counting a skipped cluster step as eight
 * minutes done would make the bar leap on a step that did nothing; dropping it
 * instead spreads the remaining steps over the whole bar. A skip can only ever
 * shrink what is left, so this never moves the bar backwards.
 *
 * 100 IS RESERVED FOR A SETTLED RUN. While anything is pending or running the
 * percent stops at 99, so a full bar always means the run is over.
 */
export function computeRunProgress(
  steps: readonly ProgressStep[],
  now: number,
  prior?: { highWater: number },
): RunProgress {
  const willRun = steps.filter((step) => step.status !== "skipped");
  const allSettled = steps.length > 0 && steps.every((step) => isSettled(step.status));

  let expected = 0;
  let behind = 0;
  for (const step of willRun) {
    const weight = weightOf(step);
    expected += weight;
    if (isSettled(step.status)) behind += weight;
    else if (step.status === "running") behind += weight * runningFraction(step, now);
  }

  let percent: number;
  if (allSettled) percent = 100;
  else if (expected === 0) percent = 0;
  else percent = Math.min(99, Math.floor((behind / expected) * 100));

  const floor = prior !== undefined && Number.isFinite(prior.highWater) ? Math.min(100, Math.max(0, prior.highWater)) : 0;
  percent = Math.max(floor, Math.max(0, percent));

  const settledCount = willRun.filter((step) => isSettled(step.status)).length;
  const stepText =
    willRun.length === 0 ? "" : `Step ${Math.min(settledCount + 1, willRun.length)} of ${willRun.length}`;

  return { percent, status: statusLine(steps, allSettled), stepText, highWater: percent };
}

function statusLine(steps: readonly ProgressStep[], allSettled: boolean): string {
  if (steps.length === 0) return "Starting";
  if (allSettled) return "Finishing";

  // THE HEAVIEST RUNNING STEP SPEAKS. A wave runs its steps together, and the
  // line has room for one: the one the operator is actually waiting on is the
  // longest, not whichever the list happens to reach first. Ties go to graph
  // order, so the choice is stable from one render to the next.
  let lead: ProgressStep | undefined;
  for (const step of steps) {
    if (step.status !== "running") continue;
    if (lead === undefined || weightOf(step) > weightOf(lead)) lead = step;
  }
  if (lead !== undefined) {
    const phase = lead.phase;
    if (phase !== undefined && phase.label.trim() !== "") {
      return hasCount(phase) ? `${phase.label} ${phase.done} of ${phase.total}` : phase.label;
    }
    return labelOf(lead);
  }

  // Between waves, or before the first step starts: what comes next.
  const next = steps.find((step) => step.status === "pending");
  return next === undefined ? "Finishing" : labelOf(next);
}

// ---------------------------------------------------------------------------
// weights measured on this machine
// ---------------------------------------------------------------------------

export interface HistoricalWeightOptions {
  /** Only runs of these kinds. Absent means every kind. */
  kinds?: readonly RunKind[];
  /** How many of the most recent successful runs to read. Default 20. */
  limit?: number;
}

/**
 * Per-step durations, in whole seconds, from the most recent successful runs.
 *
 * A STEP COUNTS ONLY WHERE IT DID ITS WORK: an item recorded `ok`, with both a
 * start and a finish. A skipped step took no time and would teach the bar that
 * creating a cluster is instant; a failed run's timings are an account of the
 * failure. The newest measurement of a step wins, so a machine that got faster
 * is believed straight away.
 *
 * Records written before the start time was kept carry only a finish time, and
 * contribute nothing rather than a guess.
 */
export function weightsFromRuns(
  runs: readonly Run[],
  options: HistoricalWeightOptions = {},
): Record<string, number> {
  const limit = options.limit ?? 20;
  const out: Record<string, number> = {};
  const successful = sortRunsNewestFirst(runs)
    .filter((run) => run.status === "succeeded")
    .filter((run) => options.kinds === undefined || options.kinds.includes(run.kind))
    .slice(0, Math.max(0, limit));
  for (const run of successful) {
    for (const item of run.items) {
      if (item.status !== "ok" || item.startedAt === undefined || item.at === undefined) continue;
      if (out[item.label] !== undefined) continue;
      const seconds = (Date.parse(item.at) - Date.parse(item.startedAt)) / 1000;
      if (!Number.isFinite(seconds) || seconds <= 0) continue;
      out[item.label] = Math.max(1, Math.round(seconds));
    }
  }
  return out;
}

/**
 * `weightsFromRuns` over the records in `dir` (runLog.defaultRunsDir() in
 * production; a temporary directory in tests).
 *
 * NEVER THROWS. A history that cannot be read is a history with no
 * measurements, and the defaults are a perfectly good bar.
 */
export async function historicalWeights(
  dir: string,
  options: HistoricalWeightOptions = {},
): Promise<Record<string, number>> {
  try {
    return weightsFromRuns(await listRuns(dir), options);
  } catch {
    return {};
  }
}
