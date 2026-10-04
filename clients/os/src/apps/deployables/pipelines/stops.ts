import type { StopState } from "../../../kit/Rail";
import { isFinished, stepsInOrder, type RunRow, type StageStatus, type StepRow } from "./rows";
import { durationWords, stageWord } from "./words";

// A run's stages as STOPS (design record D13: "the stages as stops across the
// top read from the step rows ... in railFor's standing mode"), pure.
//
// READ FROM THE STEP ROWS, NOT THE RUN ROW'S STAGE TABLE. The driver writes
// the table at stage boundaries only, so a stage that is running reads
// "waiting" there until it ends; the step rows move as each step does. The
// table is the fallback for a run whose steps cannot be read -- refused before
// any work, or a feed that has not answered yet -- and the two never mix.
//
// THE STANDING RULES, carried over from Deployables' rail (page/rail.ts):
//   - a run in flight: the stop it is at is `current`, every later stop is
//     `ahead`;
//   - a run that FAILED does not dim the stops after it. A stage an earlier
//     failure kept from running is drawn at full strength, with its own word
//     ("Not run"), because "not reached" would be a different and wrong fact:
//     it was decided, and it was decided not to run;
//   - a stage nothing in this run asked of ("Skipped: no change under bucket
//     os") is a dash, not a failure and not a pass.

export interface RunStop {
  /** The stage's name: unique in a plan, and the stop's id. */
  id: string;
  name: string;
  state: StopState;
  status: StageStatus;
  /** The stage's state in one word, as the check run says it. */
  word: string;
  /** Its time -- its longest finished step's, since its steps run at once -- or "". */
  took: string;
  steps: StepRow[];
}

function stageStatusOf(steps: readonly StepRow[]): StageStatus {
  let passed = 0, failed = 0, cancelled = 0, skipped = 0, blocked = 0, running = 0, waiting = 0;
  for (const s of steps) {
    switch (s.status) {
      case "done":
        passed++;
        break;
      case "failed":
        failed++;
        break;
      case "cancelled":
        cancelled++;
        break;
      case "running":
        running++;
        break;
      case "skipped":
        if (s.errorCode === "pipeline_stage_blocked") blocked++;
        else if (s.errorCode === "pipeline_passed_earlier") passed++;
        else skipped++;
        break;
      default:
        waiting++;
    }
  }
  if (running > 0 || (waiting > 0 && passed + failed + cancelled > 0)) return "running";
  if (failed > 0) return "failed";
  if (cancelled > 0) return "cancelled";
  if (waiting > 0) return "waiting";
  if (blocked > 0 && passed === 0) return "blocked";
  if (passed === 0) return skipped > 0 ? "skipped" : "waiting";
  return "passed";
}

function longest(steps: readonly StepRow[]): number {
  let ms = 0;
  for (const s of steps) {
    if (s.status === "done" || s.status === "failed" || s.status === "cancelled") ms = Math.max(ms, s.durationMs);
  }
  return ms;
}

function stateFor(status: StageStatus, run: RunRow, isCurrent: boolean): StopState {
  switch (status) {
    case "passed":
      return "done";
    case "failed":
      return "stopped";
    case "cancelled":
    case "skipped":
    case "blocked":
      return "skipped";
    case "running":
      return "current";
  }
  // waiting: the stop a run in flight is at is current; anything after it is
  // ahead. A FINISHED run's waiting stage never ran -- it was cut off by a
  // cancel -- and reads Not run at full strength, never dimmed.
  if (!isFinished(run)) return isCurrent ? "current" : "ahead";
  return "skipped";
}

/** The stops of a run, in plan order. */
export function stopsForRun(run: RunRow, steps: readonly StepRow[]): RunStop[] {
  const ordered = stepsInOrder(steps);
  if (ordered.length > 0) {
    const byStage: { name: string; steps: StepRow[] }[] = [];
    const at = new Map<string, StepRow[]>();
    for (const step of ordered) {
      let held = at.get(step.stage);
      if (!held) {
        held = [];
        at.set(step.stage, held);
        byStage.push({ name: step.stage, steps: held });
      }
      held.push(step);
    }
    const statuses = byStage.map((s) => stageStatusOf(s.steps));
    // The stop a run in flight is at: the first running stage, else the first
    // still waiting.
    const current = statuses.findIndex((s) => s === "running") >= 0 ? statuses.findIndex((s) => s === "running") : statuses.findIndex((s) => s === "waiting");
    return byStage.map((stage, i) => {
      let status = statuses[i]!;
      if (status === "waiting" && isFinished(run)) status = "blocked";
      return {
        id: stage.name,
        name: stage.name,
        status,
        state: stateFor(statuses[i]!, run, i === current),
        word: stageWord(status),
        took: durationWords(longest(stage.steps)),
        steps: stage.steps,
      };
    });
  }
  // No step rows: the run row's table, or nothing at all for a run refused
  // before any plan.
  const current = run.stages.findIndex((s) => s.status === "running") >= 0
    ? run.stages.findIndex((s) => s.status === "running")
    : run.stages.findIndex((s) => s.status === "waiting");
  return run.stages.map((stage, i) => ({
    id: stage.name,
    name: stage.name,
    status: stage.status,
    state: stateFor(stage.status, run, i === current),
    word: stageWord(stage.status),
    took: durationWords(stage.durationMs),
    steps: [],
  }));
}

/**
 * The stop that is OPEN when nobody has chosen one -- the one that is the
 * question (Deployables' rule, `openStopFor`): the stop a running run is at;
 * the stop a failed or cancelled run stopped at; for a run that passed, the
 * last stop it ran.
 */
export function openStopFor(stops: readonly RunStop[], run: RunRow): string {
  if (stops.length === 0) return "";
  if (!isFinished(run)) {
    return (stops.find((s) => s.state === "current") ?? stops[0]!).id;
  }
  const stopped = stops.find((s) => s.status === "failed") ?? stops.find((s) => s.status === "cancelled");
  if (stopped) return stopped.id;
  const ran = [...stops].reverse().find((s) => s.status === "passed");
  return (ran ?? stops[stops.length - 1]!).id;
}

/** How many shards a step was split into: the steps of its stage sharing its name. */
export function shardsOf(step: StepRow, stop: RunStop): number {
  return step.shard > 0 ? stop.steps.filter((s) => s.name === step.name && s.shard > 0).length : 0;
}
