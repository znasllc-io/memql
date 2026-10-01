// Projecting a run's progress for the renderer, and saying what a failure means.
//
// This is the half of #3474 that can be TESTED. `webview/addClusterPanel.ts`
// imports `vscode`, which `cmd/memql-lsp/vscodeimportrule_test.go` keeps out of
// the unit lane by design -- so anything asserted about what an operator is
// shown has to live on this side of that line. The panel calls
// `renderInstallSteps(toStepViews(state.steps))` and adds no judgement of its
// own, exactly as `views/clustersTree.ts` leans on `state/topology.ts`.
//
// TWO JOBS, BOTH DELIBERATELY SMALL:
//
//  1. TRANSLATE, do not decide. `StepProgress` is the wizard's record of a run
//     (state/addCluster.ts, #3470) and `InstallStepView` is what the renderer
//     draws (view-kit's install.ts). The step order, which steps ran, and what
//     each one reported are the graph's and the executor's; this file changes
//     the shape and nothing else.
//
//  2. SAY WHAT AN EXIT CODE MEANS FOR THE OPERATOR. The capability-script
//     contract's codes are stable and each asks for a genuinely different next
//     action -- a refusal is the system working, a missing prerequisite is
//     something to go and install. A run that printed "exit 4" and stopped
//     would be telling the operator the number and withholding the meaning.
//
// Refs: #3474 #3470 #3469 #3463

import type { InstallStepView } from "@znasllc-io/memql-view-kit";

import type { StepProgress, StepState } from "./addCluster.js";
import {
  computeRunProgress,
  stepWeight,
  type ProgressStatus,
  type ProgressStep,
  type RunProgress,
} from "./runProgress.js";

/**
 * What a failure asks of the operator.
 *
 * `retryable` is whether retrying UNCHANGED has any prospect of a different
 * answer: a bad parameter or an unsupported platform will fail identically
 * forever, while an operation failure may well have been a transient one. The
 * run screens offer Retry only when it is true -- an act that cannot succeed
 * is absent, not offered.
 */
export interface FailureGuidance {
  /** The short sentence naming what kind of failure this is. */
  headline: string;
  /** What the operator should do about it. */
  advice: string;
  /** Whether an unchanged retry could plausibly succeed. */
  retryable: boolean;
}

/**
 * The one honest refuse when detect.sh rejects the OS/arch (memql#4294).
 *
 * Generic exit 3 is "the step refused to act" / artifact-protection. Detect's
 * platform refuse is a different sentence: the wizard cannot run here, and
 * retry will not help.
 *
 * THE SUPPORTED SET IS QUOTED, NEVER RESTATED. This used to say "The wizard
 * targets linux/amd64 only. On macOS use make up" -- which stopped being true
 * when darwin/arm64 became a supported, digest-pinned platform, and then told
 * every Mac operator who tripped ANY platform refuse that their machine could
 * not run the installer at all. detect.sh composes the list from
 * scripts/lib/platform.sh's SUPPORTED_PLATFORMS, so its own sentence is the
 * only one here that cannot go stale.
 */
export function refusedPlatformGuidance(detail = ""): FailureGuidance {
  // The supported set as detect.sh composed it ("... targets linux/amd64,
  // darwin/arm64"), never restated here.
  const supported = /targets\s+(.+?)\.?$/i.exec(detail.trim())?.[1]?.trim() ?? "";
  return {
    headline: "This computer can't run a local MemQL cluster.",
    advice: supported === "" ? "Retrying won't change that." : `It runs on ${supported}. Retrying won't change that.`,
    retryable: false,
  };
}

export function isUnsupportedPlatformRefuse(exitCode: number | null, detail = ""): boolean {
  return exitCode === 3 && /unsupported platform/i.test(detail);
}

/** What `failureGuidance` needs beyond the code, when the caller has it. */
export interface GuidanceContext {
  /**
   * How long the step was allowed, in seconds -- its own `timeoutSeconds`
   * from the graph, or the run's default. A timeout names this rather than a
   * figure written here: the cluster step is allowed thirty minutes and the
   * image build forty-five, and "ten minutes" was wrong for exactly the steps
   * most likely to time out.
   */
  timeoutSeconds?: number;
}

/** The run's per-step default ceiling, when a step declares none (see addClusterPanel.ts). */
export const DEFAULT_STEP_TIMEOUT_SECONDS = 600;

function minutes(seconds: number): string {
  const n = Math.max(1, Math.round(seconds / 60));
  return n === 1 ? "1 minute" : `${n} minutes`;
}

/**
 * What a failed step's exit code means for the operator.
 *
 * Source: docs/internal/design/capability-script-contract.md -- 2 bad param,
 * 3 refused, 4 prerequisite missing, 5 operation failed -- PLUS the two codes
 * that reach a failed outcome without being contract classifications:
 *
 *   0  the script succeeded and its VERIFY did not hold. executor.ts treats a
 *      zero exit as a precondition and nothing more, so this is the shape most
 *      real failures take here rather than an edge case.
 *   1  the catch-all. `cap_fail` clamps any out-of-range code to 1, and
 *      capability.sh's EXIT trap emits a failure envelope for any non-zero
 *      abort, so a `set -e` death lands here too.
 *
 * -- PLUS every code MemQL SYNTHESISES for itself, named in
 * `runner.SYNTHESISED_EXIT_CODES`: 124 (the step outran its ceiling), 127 (the
 * script could not be spawned) and 128 (the child died on a signal nobody here
 * sent). A number MemQL assigned itself and then cannot explain is the worst
 * version of this failure, and `installProgress.test.ts` derives its reachable
 * set from that object rather than from a list written out beside it.
 *
 * Both were once falling through to the default branch, which told the
 * operator MemQL "cannot say what it means" about the two cases it understands
 * best. That is the confident-wrong-advice failure this function exists to
 * prevent, inverted into confidently disclaiming knowledge the system has.
 *
 * WRITTEN FOR THE PERSON WATCHING THE INSTALL, not for the contract. The page
 * leads with what the step's own script said (`StepProgress.message`); this is
 * what stands beside it, or alone when the script said nothing. So it names
 * the kind of failure and the next move, and leaves the exit code, the verify
 * detail and the output to the log.
 */
export function failureGuidance(
  exitCode: number | null,
  remedy = "",
  detail = "",
  context: GuidanceContext = {},
): FailureGuidance {
  if (isUnsupportedPlatformRefuse(exitCode, detail)) {
    return refusedPlatformGuidance(detail);
  }
  // A REMEDY OUTRANKS THE CODE, on the one code that can carry it: a step that
  // needs root classifies as a missing prerequisite, and the wizard is holding
  // the exact command (memql#3560). The password was asked once, up front;
  // this is the case where it was declined or not enough, so the fix is the
  // command, in the person's own terminal.
  if (exitCode === 4 && remedy !== "") {
    return {
      headline: "This step needs administrator access.",
      advice: "Run the command in a terminal, then retry.",
      retryable: true,
    };
  }
  switch (exitCode) {
    case 0:
      // The script finished and its own check did not hold -- the commonest
      // real failure here, usually something still starting.
      return {
        headline: "The step finished, but its check didn't pass.",
        advice: "Something may still be starting. Retry in a moment.",
        retryable: true,
      };
    case 1:
      return {
        headline: "The step failed.",
        advice: "The log has the details.",
        retryable: true,
      };
    case 124: {
      // SYNTHESISED BY US: runner.ts kills a step that outruns its ceiling.
      const allowed = context.timeoutSeconds ?? DEFAULT_STEP_TIMEOUT_SECONDS;
      return {
        headline: `The step timed out after ${minutes(allowed)}.`,
        advice: "Retry is safe. Steps that already finished are skipped.",
        retryable: true,
      };
    }
    case 127:
      // Also ours: the script behind the step could not be launched. A broken
      // package, not a broken machine.
      return {
        headline: "This step couldn't start.",
        advice: "Reinstall the MemQL extension, or report the problem.",
        retryable: false,
      };
    case 128:
      // Also ours: the child died on a signal nobody here sent.
      return {
        headline: "The step was interrupted.",
        advice: "Retry is safe. Steps that already finished are skipped.",
        retryable: true,
      };
    case 2:
      return {
        headline: "The installer passed this step something it won't accept.",
        advice: "That's a fault in MemQL, not your computer. Please report it with the log.",
        retryable: false,
      };
    case 3:
      return {
        headline: "The step stopped to protect something already on this computer.",
        advice: "The log says what it kept.",
        retryable: false,
      };
    case 4:
      return {
        headline: "Something this step needs is missing.",
        advice: "Install what the log names, then retry.",
        retryable: true,
      };
    case 5:
      return {
        headline: "The step failed.",
        advice: "Retrying often helps. If it fails again, the log has the details.",
        retryable: true,
      };
    case null:
      return {
        headline: "The step didn't finish.",
        advice: "Retry is safe.",
        retryable: true,
      };
    default:
      // A code nobody defined is reported as such rather than mapped to the
      // nearest known one.
      return {
        headline: `The step failed with code ${exitCode}.`,
        advice: "MemQL doesn't know what that code means. The log has the details.",
        retryable: true,
      };
  }
}

/**
 * Projects the wizard's step records into what the renderer draws.
 *
 * ORDER IS PRESERVED. `AddClusterState` appends in the order the executor
 * reported steps, which is the graph's wave order; re-sorting here would draw a
 * sequence that is a property of this function rather than of the dependency
 * graph that was actually walked.
 */
export function toStepViews(steps: readonly StepProgress[]): InstallStepView[] {
  return steps.map((step) => {
    const view: InstallStepView = {
      id: step.id,
      // The description is what the step DOES, in the graph's own words. Falling
      // back to the id keeps a step visible when the description has not
      // arrived yet -- a blank row would read as a bug in the wizard rather
      // than as a step whose first event has not landed.
      label: step.description === "" ? step.id : step.description,
      state: step.state,
    };

    const detail = detailFor(step);
    if (detail !== "") view.detail = detail;

    // NEITHER THE EXIT CODE NOR A LOG LINE RIDES ON A CHECKLIST ROW ANY MORE
    // (memql#4456 over memql#4194).
    //
    // memql#4194 put a short redacted last-line here because the full log had
    // nowhere on the page to be -- it went to the MemQL Install output channel,
    // and the inline line ended by SAYING SO. That is no longer true: the run
    // screens carry a log pane (memql#4455), so the sentence pointed somewhere
    // else while the thing it described was one click below it, and the same
    // stderr rendered in two places.
    //
    // D4's rule is that verbatim output, exit codes and envelope fields have
    // exactly one home, and it is the pane. The checklist keeps what a
    // checklist is for: which step, what state, and the reason sentence the
    // capability wrote for a human (`detailFor`).
    return view;
  });
}

/**
 * The one line under a step's label.
 *
 * A guided step says so even when it has a reason, because "you are running
 * this one by hand" changes what the operator is looking at more than the
 * reason does.
 */
function detailFor(step: StepProgress): string {
  const parts: string[] = [];
  if (step.guided) parts.push("guided -- you run this one");
  if (step.reason !== "") parts.push(step.reason);
  return parts.join(" -- ");
}

/**
 * Whether a run has anything left to do.
 *
 * Used to decide whether the run screen offers Cancel. A run with no pending or
 * running steps is over in every sense except the report, and offering to
 * cancel it would promise something there is nothing left to stop.
 *
 * AN EMPTY LIST IS NOT SETTLED. No steps reported yet is the START of a run --
 * the first `stepStarted` has not arrived -- and that is precisely when an
 * operator is most likely to want out. Reading "nothing pending" off an empty
 * list would withdraw Cancel for the whole opening stretch of the longest
 * operation the wizard performs.
 */
export function runIsSettled(steps: readonly StepProgress[]): boolean {
  if (steps.length === 0) return false;
  return !steps.some((s) => s.state === "pending" || s.state === "running");
}

// ---------------------------------------------------------------------------
// the run's progress, as a number
// ---------------------------------------------------------------------------

/** The executor's words for a step's state, from the wizard's. */
const STATE_TO_STATUS: Readonly<Record<StepState, ProgressStatus>> = {
  pending: "pending",
  running: "running",
  done: "ok",
  skipped: "skipped",
  preserved: "preserved",
  failed: "failed",
};

/**
 * The wizard's step records as the progress model reads them.
 *
 * THE LABEL FALLS BACK TO THE DESCRIPTION, then the id, only so a row whose
 * first event has not landed is never blank; every graph step carries a label
 * and it arrives with `runStarted`.
 */
export function progressStepsOf(
  steps: readonly StepProgress[],
  weights?: Readonly<Record<string, number>>,
): ProgressStep[] {
  return steps.map((step) => {
    const view: ProgressStep = {
      id: step.id,
      label: step.label !== "" ? step.label : step.description !== "" ? step.description : step.id,
      weight: stepWeight(step.id, weights),
      status: STATE_TO_STATUS[step.state] ?? "pending",
    };
    if (step.startedAt !== undefined) view.startedAt = step.startedAt;
    if (step.finishedAt !== undefined) view.finishedAt = step.finishedAt;
    if (step.phase !== undefined) view.phase = { ...step.phase };
    return view;
  });
}

/**
 * The run's progress, from the step list the executor seeded.
 *
 * PURE, AND HERE RATHER THAN IN THE RENDERER, for the reason the rest of this
 * module exists: the panels that draw the bar import `vscode`, so a percentage
 * computed inside a template literal is one no test can reach.
 *
 * A state machine calls this through its own `progress(now)`, which carries the
 * high-water mark from one render to the next; a caller with only a step list
 * (a gallery page, a test) gets the same answer without one.
 */
export function runProgressOf(
  steps: readonly StepProgress[],
  now: number,
  prior?: { highWater: number },
  weights?: Readonly<Record<string, number>>,
): RunProgress {
  return computeRunProgress(progressStepsOf(steps, weights), now, prior);
}
