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
//     scripts/lib/capability.sh -- "Starting services 5 of 9"), spread over
//     what was left of the step when that phase began;
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
// A FAILURE STOPS THE BAR WHERE IT HAPPENED. The steps behind a failed one are
// skipped without running, and letting them leave the total the way a planned
// skip does would carry a run that broke a third of the way in to a full bar.
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
  /**
   * When the step first reported THIS phase (epoch milliseconds). With it, a
   * counted phase fills the part of the step the clock had not yet credited
   * when the phase began, rather than restarting the step from nothing; see
   * `runningFraction`. Absent, the count is the step's whole fraction.
   */
  since?: number;
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
   * several; the next step's label between steps; once nothing is running
   * after a failure, the label of the step that failed; "Finishing" once every
   * step has settled without one.
   */
  status: string;
  /**
   * "Step 6 of 16", counting only steps that will run; "" when none will. After
   * a failure it stays on the failed step, and the steps skipped because of it
   * stay in the count: the run stopped at step 6 of 16, it did not finish.
   */
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

/** The share of its weight a step's elapsed time credits it at `at`, capped. */
function elapsedFraction(step: ProgressStep, at: number): number {
  if (step.startedAt === undefined) return 0;
  const elapsed = Math.max(0, (at - step.startedAt) / 1000);
  return Math.min(ELAPSED_CEILING, elapsed / weightOf(step));
}

/**
 * How much of its own weight a running step has earned.
 *
 * A COUNTED PHASE FILLS WHAT IS LEFT OF THE STEP. Creating the cluster spends
 * minutes on uncounted phases (the clock carries the bar through those) before
 * its long wait starts counting services. Reading "0 of 9" as the whole step's
 * fraction would stop the bar dead for as long as the count takes to overtake
 * the clock. So when the phase says when it began, the count spreads over the
 * part of the step the clock had not yet credited at that moment: 0 of 9 is
 * where the clock left off, 9 of 9 is the whole step. A phase with no start
 * (a caller that does not record one) is read as the whole step's fraction.
 */
function runningFraction(step: ProgressStep, now: number): number {
  if (hasCount(step.phase)) {
    const counted = Math.min(1, Math.max(0, step.phase.done / step.phase.total));
    const base = step.phase.since !== undefined ? elapsedFraction(step, step.phase.since) : 0;
    return base + (1 - base) * counted;
  }
  return elapsedFraction(step, now);
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
 * EXCEPT THE SKIPS A FAILURE CAUSES. Once a step has failed, the steps that
 * depended on it are skipped without running, and dropping those would carry
 * the bar to the end of a run that stopped part-way. A skip that settled
 * before the first failure left the total as usual; one that settled at or
 * after it (or whose time is unknown) stays in the total, undone. The failed
 * step itself is credited only the share of its weight its running time had
 * earned, so the bar holds where the run broke.
 *
 * 100 IS RESERVED FOR A RUN THAT FINISHED. While anything is pending or
 * running, or once a step has failed, the percent stops at 99, so a full bar
 * always means the run is over and went through.
 */
export function computeRunProgress(
  steps: readonly ProgressStep[],
  now: number,
  prior?: { highWater: number },
): RunProgress {
  const failures = steps.filter((step) => step.status === "failed");
  const failedAt = earliestFinish(failures);
  const leftTheTotal = (step: ProgressStep): boolean =>
    step.status === "skipped" &&
    (failures.length === 0 ||
      (failedAt !== undefined && step.finishedAt !== undefined && step.finishedAt < failedAt));

  const willRun = steps.filter((step) => !leftTheTotal(step));
  const allSettled = steps.length > 0 && steps.every((step) => isSettled(step.status));

  let expected = 0;
  let behind = 0;
  for (const step of willRun) {
    const weight = weightOf(step);
    expected += weight;
    if (step.status === "ok" || step.status === "preserved") behind += weight;
    else if (step.status === "failed") {
      if (step.finishedAt !== undefined) behind += weight * elapsedFraction(step, step.finishedAt);
    } else if (step.status === "running") behind += weight * runningFraction(step, now);
  }

  let percent: number;
  if (allSettled && failures.length === 0) percent = 100;
  else if (expected === 0) percent = 0;
  else percent = Math.min(99, Math.floor((behind / expected) * 100));

  const floor = prior !== undefined && Number.isFinite(prior.highWater) ? Math.min(100, Math.max(0, prior.highWater)) : 0;
  percent = Math.max(floor, Math.max(0, percent));

  // The step being worked on is the one after every step that went through. A
  // failed step did not go through, so after a failure the count stays on it.
  const through = willRun.filter((step) => step.status === "ok" || step.status === "preserved").length;
  const stepText = willRun.length === 0 ? "" : `Step ${Math.min(through + 1, willRun.length)} of ${willRun.length}`;

  return { percent, status: statusLine(steps, allSettled, failures), stepText, highWater: percent };
}

function earliestFinish(steps: readonly ProgressStep[]): number | undefined {
  let earliest: number | undefined;
  for (const step of steps) {
    if (step.finishedAt === undefined) continue;
    if (earliest === undefined || step.finishedAt < earliest) earliest = step.finishedAt;
  }
  return earliest;
}

function statusLine(
  steps: readonly ProgressStep[],
  allSettled: boolean,
  failures: readonly ProgressStep[],
): string {
  if (steps.length === 0) return "Starting";

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

  // A run a failure stopped is not "Finishing" and has no next step: it is
  // where it broke. The earliest failure, as everywhere else -- the rest may be
  // consequences of it.
  if (failures.length > 0) {
    const failedAt = earliestFinish(failures);
    return labelOf(failures.find((step) => step.finishedAt === failedAt) ?? failures[0]!);
  }

  if (allSettled) return "Finishing";
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

// ---------------------------------------------------------------------------
// the failed step, in the negative
// ---------------------------------------------------------------------------

/**
 * A step label's leading gerund and the base verb that says it failed.
 *
 * EVERY GERUND THE GRAPH DOCUMENTS OPEN WITH. The labels are short present
 * participles ("Creating the cluster"), written for the running bar; a
 * failure names the same step in the negative, "Couldn't create the cluster".
 * test/runProgress.test.ts reads every label in scripts/install/graph/*.json
 * and fails when one starts with a gerund this table does not carry, so a new
 * step cannot quietly fall through to the fallback below.
 *
 * Two words first: "Setting up" is one verb, and matching "Setting" alone
 * would make it "Couldn't set up up".
 */
const FAILED_VERBS: readonly (readonly [string, string])[] = [
  ["Setting up", "set up"],
  ["Checking", "check"],
  ["Installing", "install"],
  ["Adding", "add"],
  ["Downloading", "download"],
  ["Creating", "create"],
  ["Building", "build"],
  ["Preparing", "prepare"],
  ["Removing", "remove"],
  ["Rebuilding", "rebuild"],
];

/**
 * The failure status for a step label: "Couldn't create the cluster".
 *
 * A label that does not open with a known gerund keeps its own words and says
 * so after them ("<label> failed"), which is grammatical whatever the label
 * is; an empty label says only that something failed.
 */
export function failedLabel(label: string): string {
  const trimmed = label.trim();
  if (trimmed === "") return "Something failed";
  for (const [gerund, verb] of FAILED_VERBS) {
    if (trimmed === gerund) return `Couldn't ${verb}`;
    if (trimmed.startsWith(`${gerund} `)) return `Couldn't ${verb} ${trimmed.slice(gerund.length + 1)}`;
  }
  return `${trimmed} failed`;
}

/** The gerunds `failedLabel` understands, for the test that holds the graph documents to it. */
export const FAILED_LABEL_GERUNDS: readonly string[] = FAILED_VERBS.map(([gerund]) => gerund);
