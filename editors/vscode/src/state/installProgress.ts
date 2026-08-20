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
 */
export function failureGuidance(exitCode: number | null, remedy = ""): FailureGuidance {
  // A REMEDY OUTRANKS THE CODE, on the one code that can carry it.
  //
  // "prerequisite missing" is the honest classification for a step that needs
  // root: nothing is half-done and the thing it needs is not available to it.
  // But the generic exit-4 advice -- "install the missing prerequisite named
  // below" -- sends the operator looking for a package when what the step
  // actually wants is a password, and the wizard is already showing them the
  // exact command. Naming that changes nothing about the classification and
  // everything about what they do next (memql#3560).
  if (exitCode === 4 && remedy !== "") {
    return {
      headline: "This step needs to run with more privilege than the installer has.",
      advice:
        "It cannot ask for your password: an editor runs it as a background " +
        "process with no terminal attached. Use the button below to open a " +
        "terminal with the exact command, run it there, then retry -- the step " +
        "will find its work already done. Nothing has been left half-finished.",
      retryable: true,
    };
  }
  switch (exitCode) {
    case 0:
      // THE NORMAL FAILURE SHAPE FOR THIS INSTALLER, and the one this function
      // originally had no case for. `executor.ts` records a FAILED outcome
      // carrying exit 0 whenever a script exits cleanly and its verify
      // predicate does not hold -- "an exit code of 0 is a precondition and
      // nothing more". All 13 install steps verify a `result.*` field, so this
      // is the path most real failures take. `verify-frontdoor.sh
      // --report-only` is the worked example: it warns, calls cap_ok, and
      // returns exit 0 with `allPassed=false`.
      return {
        headline: "The step ran without error, but the machine is not in the state it checks for.",
        advice:
          "The script itself succeeded -- what it was supposed to achieve did not " +
          "hold when it checked. That is usually something taking longer than the " +
          "step waits for, or a change that did not take effect. The output below " +
          "says which check failed.",
        retryable: true,
      };
    case 1:
      // `cap_fail` clamps any out-of-range code to 1, and capability.sh's EXIT
      // trap emits a failure envelope for any non-zero abort -- so 1 is where
      // both an unclassified failure and a `set -e` abort land. It is a real
      // code with a real meaning, not an unknown one.
      return {
        headline: "The step stopped without classifying what went wrong.",
        advice:
          "This is the catch-all: either the script failed in a way it does not " +
          "have a specific code for, or it aborted partway. The output below is " +
          "the only account of what happened, so read it before retrying.",
        retryable: true,
      };
    case 124:
      // SYNTHESISED BY US, not by the script. runner.ts kills a step that
      // outruns its timeout and reports 124 with SIGKILL. Leaving it to the
      // default branch had MemQL say it "cannot say what it means" about a
      // code MemQL assigned itself -- and to an operator whose install just
      // stopped after ten minutes, that is the least useful moment to be
      // vague.
      return {
        headline: "The step ran out of time and was stopped.",
        advice:
          "It exceeded the ten minutes any one step is allowed and was killed, " +
          "so it did not finish what it was doing. A slow network is the usual " +
          "cause on the download and clone steps; a wedged Docker daemon is the " +
          "usual cause on the cluster step. Retry is safe -- every step checks " +
          "first and skips what is already done.",
        retryable: true,
      };
    case 127:
      // Also ours: runner.ts reports 127 when the script could not be started
      // or run at all. That is an installer-side problem -- a missing or
      // unexecutable capability script -- not something the operator's machine
      // did wrong, and it is the signature of a broken package rather than a
      // broken install.
      return {
        headline: "The installer could not start this step at all.",
        advice:
          "The script behind this step could not be launched -- missing, or not " +
          "executable. That is a fault in this MemQL build rather than in your " +
          "machine, and retrying will not change it. Please report it with the " +
          "output below.",
        retryable: false,
      };
    case 128:
      // Also ours, and the one that stayed unexplained longest: runner.ts
      // reports 128 when the child dies on a signal and reports no code of its
      // own. It is NOT the timeout -- that path reports 124 and says so -- which
      // is precisely what makes it worth its own sentence rather than a
      // near-enough mapping onto one.
      return {
        headline: "Something outside MemQL stopped this step.",
        advice:
          "The step was killed by a signal MemQL did not send -- a Ctrl-C in the " +
          "window behind this one, a system running out of memory, or a process " +
          "supervisor. Nothing about the step itself failed. Retry is safe: every " +
          "step checks first and skips what is already done.",
        retryable: true,
      };
    case 2:
      return {
        headline: "The installer passed this step something it would not accept.",
        advice:
          "This is a fault in MemQL rather than in your machine or your answers. " +
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
          "MemQL cannot say what it means. The output below is the authority.",
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
