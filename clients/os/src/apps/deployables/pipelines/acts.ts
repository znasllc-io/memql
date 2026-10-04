import type { ActionBarTone } from "../../../kit/ActionBar";
import type { PartsHeld } from "../parts";
import { isFinished, type RunRow, type StepRow } from "./rows";
import { runOutcome } from "./words";

// The run page's ONE bar (DESIGN.md rule 12; design record D13: "one
// ActionBar carrying the state and the legal acts, Open on GitHub, Re-run
// failed, Re-run"), as a pure reading so the table is what gets asserted.
//
// AN ACT THAT IS NOT LEGAL IS ABSENT, NEVER DISABLED, and "legal" is the
// engine's own rule, restated nowhere else:
//   - Re-run is refused while the key's newest attempt is unfinished
//     (ErrRunInProgress), for a fork (pipeline_fork_refused) and on a
//     disconnected pipeline (pipeline_disconnected);
//   - Re-run failed is refused unless the run failed or was cancelled with a
//     step that failed or was cancelled (pipeline_nothing_to_rerun);
//   - Cancel stops a queued or running run, once.
// And a part the session does not hold takes its act away (`rerun`,
// `cancel`). Open on GitHub is navigation, not an act on the run, so it is
// always there when GitHub has a page to open.
//
// AT MOST THREE, ONE BUTTON, PRIMARY LAST: the forward act is the button and
// whatever stands beside it is text.

export type RunActName = "Open on GitHub" | "Re-run" | "Re-run failed" | "Cancel";

export interface RunActSpec {
  name: RunActName;
  /** The one button; the rest are text acts. */
  primary: boolean;
  danger?: boolean;
}

export interface RunBar {
  state: string;
  detail: string;
  tone: ActionBarTone;
  acts: RunActSpec[];
}

export interface RunBarInput {
  run: RunRow;
  steps: readonly StepRow[];
  /** The newest attempt of this run's key, when it is newer than this one. */
  newer: RunRow | null;
  /** Whether the run's pipeline still opens runs. Unknown counts as active: the engine refuses anyway. */
  pipelineActive: boolean;
  can: Pick<PartsHeld, "rerun" | "cancel">;
  /** Whether GitHub has a page for this run (`runGithubUrl`). */
  onGithub: boolean;
}

/** Whether a step of the run failed or was cancelled: what Re-run failed would run again. */
export function hasFailedSteps(steps: readonly StepRow[]): boolean {
  return steps.some((s) => s.status === "failed" || s.status === "cancelled");
}

export function runBarFor(input: RunBarInput): RunBar {
  const { run, steps, newer, pipelineActive, can, onGithub } = input;
  const outcome = runOutcome(run);
  const acts: RunActSpec[] = [];
  if (onGithub) acts.push({ name: "Open on GitHub", primary: false });

  let detail = outcome.detail;
  if (!isFinished(run)) {
    if (can.cancel && !run.cancelRequested) acts.push({ name: "Cancel", primary: true, danger: true });
    return { state: outcome.word, detail, tone: "busy", acts };
  }

  const newerRunning = newer !== null && !isFinished(newer);
  if (newerRunning) {
    // The engine refuses a re-run while the key's newest attempt is going, so
    // the act is absent and the line that explains the state says why.
    detail = [detail, `attempt ${newer!.attempt} is running`].filter((p) => p !== "").join(", ");
  }
  const rerunnable = can.rerun && !newerRunning && pipelineActive && run.refusalCode !== "pipeline_fork_refused";
  if (rerunnable) {
    const failedOnly = (run.conclusion === "failure" || run.conclusion === "cancelled") && run.workRunId !== "" && hasFailedSteps(steps);
    if (failedOnly) {
      acts.push({ name: "Re-run", primary: false }, { name: "Re-run failed", primary: true });
    } else {
      acts.push({ name: "Re-run", primary: true });
    }
  }
  return { state: outcome.word, detail, tone: run.conclusion === "success" ? "live" : "none", acts };
}
