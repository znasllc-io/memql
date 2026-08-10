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

import type { StepProgress } from "./addCluster.js";

/**
 * What a failure asks of the operator.
 *
 * `retryable` is not "may the button be shown" -- #3474 offers Retry and
 * Switch-to-Guided on EVERY failure, because the operator may have fixed the
 * cause in another window and we cannot know. It is whether retrying UNCHANGED
 * has any prospect of a different answer, which is what the wording turns on:
 * a bad parameter will fail identically forever, while an operation failure
 * may well have been a transient one.
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
 * The capability-script contract's exit codes.
 *
 * Source: docs/internal/design/capability-script-contract.md -- 0 ok, 2 bad
 * param, 3 refused, 4 prerequisite missing, 5 operation failed.
 */
export function failureGuidance(exitCode: number | null): FailureGuidance {
  switch (exitCode) {
    case 2:
      return {
        headline: "The installer passed this step something it would not accept.",
        advice:
          "This is a fault in memQL rather than in your machine or your answers. " +
          "Retrying unchanged will fail the same way; please report it with the output below.",
        retryable: false,
      };
    case 3:
      return {
        headline: "The step refused to act.",
        advice:
          "A refusal is the script protecting something, not a crash -- most often " +
          "an artifact it will not touch because it did not create it. The output " +
          "below says what it declined to do.",
        retryable: false,
      };
    case 4:
      return {
        headline: "Something this step needs is not on this machine.",
        advice:
          "Install the missing prerequisite named below, then retry. Nothing has " +
          "been left half-done -- a step that cannot start changes nothing.",
        retryable: true,
      };
    case 5:
      return {
        headline: "The step ran and did not succeed.",
        advice:
          "This one can be transient -- a slow pull, a port still in use. Retry is " +
          "worth trying once; if it fails the same way, the output below is what to " +
          "act on.",
        retryable: true,
      };
    case null:
      return {
        headline: "The step did not run to completion.",
        advice:
          "No exit code was recorded, which usually means it was stopped rather " +
          "than that it failed. Retrying is safe.",
        retryable: true,
      };
    default:
      // An unrecognised code is reported AS unrecognised rather than mapped to
      // the nearest known one. Guessing here would put confident wrong advice
      // in front of an operator at the exact moment they are relying on it.
      return {
        headline: `The step exited with code ${exitCode}.`,
        advice:
          "That is not one of the codes the capability-script contract defines, so " +
          "memQL cannot say what it means. The output below is the authority.",
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

    // The exit code rides only on a failure. On any other state it is either
    // absent or stale, and a step that succeeded showing "exit 5" would be
    // alarming and wrong.
    if (step.state === "failed" && step.exitCode !== null) view.exitCode = step.exitCode;

    // Stderr goes through verbatim. It is the one text that says what actually
    // broke, and it is only worth surfacing where something went wrong --
    // attaching the log to every successful step would bury the failure in
    // twelve disclosures that all say "fine".
    if (step.log !== "" && (step.state === "failed" || step.state === "preserved")) {
      view.error = step.log;
    }

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
