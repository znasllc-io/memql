// What a deployment to another tag will actually do, before it does it.
//
// AN UPGRADE IS THE INSTALL GRAPH RE-RUN WITH A DIFFERENT TAG. There is no
// second graph and no second run path: `stackCheckout` sees a tag it is not on
// and moves the checkout, `clusterUp` reconciles the local overlay from it, and
// every other step verifies first and skips because it is already satisfied.
// That is the same property that makes re-running the graph a repair.
//
// Which is exactly why the page needs this. A run that reports fifteen steps
// and does work in two looks, to someone watching it, like a full reinstall of
// their machine -- and the one question they have before pressing the button is
// "what is this going to touch". Saying so up front is the difference between
// an operator who lets it run and one who cancels it.
//
// IT IS A PROJECTION, NOT A DRY RUN. Nothing here executes a verify: the only
// way to know for certain that `toolK3d` will skip is to run its check, which
// is the run itself. What this claims is narrower and honest -- which steps
// CHANGE something when only the tag has changed -- and the page words it that
// way. A step that turns out to have work to do still runs; the projection was
// a forecast, and the run list is the record.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3739 #3733

import type { Graph } from "../install/graph.js";
import { displayVersion } from "./deployments.js";

export type PlannedEffect =
  /** Changes something: this is what the deployment is for. */
  | "runs"
  /** Inspects the machine and writes nothing. */
  | "verifyOnly"
  /** Verifies first and is expected to find itself already satisfied. */
  | "skip";

export interface PlannedStepView {
  id: string;
  description: string;
  effect: PlannedEffect;
  /** The one line the row carries beside the step's name. */
  detail: string;
}

/**
 * The two steps a tag change is FOR.
 *
 * Named rather than derived, because the derivation would have to be "which
 * steps depend on the tag", and that is a fact about the capability scripts
 * rather than about the graph document -- the graph records dependencies and
 * receipts, not which flags a step reads. A list a reviewer can check against
 * `installSessionOptions` beats an inference that looks principled and is not.
 */
const TAG_SENSITIVE_STEPS: Readonly<Record<string, string>> = {
  stackCheckout: "move the checkout",
  clusterUp: "reconcile the local overlay",
};

export interface UpgradePlanInput {
  graph: Graph;
  /** The tag the receipt records. Empty when unknown. */
  from: string;
  /** The tag the operator picked. */
  to: string;
}

/**
 * Every step of the graph, in graph order, with what it will do.
 *
 * GRAPH ORDER, NOT WAVE ORDER -- the same choice `runStarted` makes. Waves are
 * how the executor schedules; a list that reordered itself to match would be a
 * display of the executor rather than of the install.
 */
export function upgradePlan(input: UpgradePlanInput): PlannedStepView[] {
  return input.graph.steps.map((step) => {
    const tagged = TAG_SENSITIVE_STEPS[step.id];
    if (tagged !== undefined) {
      return {
        id: step.id,
        description: step.description ?? "",
        effect: "runs" as const,
        detail:
          step.id === "stackCheckout"
            ? `${displayVersion(input.from)} -> ${displayVersion(input.to)}`
            : tagged,
      };
    }
    if (step.readOnly === true) {
      return {
        id: step.id,
        description: step.description ?? "",
        effect: "verifyOnly" as const,
        detail: "verify only",
      };
    }
    return {
      id: step.id,
      description: step.description ?? "",
      effect: "skip" as const,
      detail: "already satisfied - skip",
    };
  });
}

/**
 * The one-line summary above the list.
 *
 * Counted from the projection rather than written as prose, so the sentence and
 * the list beneath it cannot disagree -- which they would the first time a step
 * is added to the graph.
 */
export function upgradeSummary(plan: readonly PlannedStepView[]): string {
  const runs = plan.filter((s) => s.effect === "runs").length;
  const checks = plan.filter((s) => s.effect === "verifyOnly").length;
  const skips = plan.filter((s) => s.effect === "skip").length;
  return (
    `${runs} ${runs === 1 ? "step changes" : "steps change"} something, ` +
    `${checks} only ${checks === 1 ? "checks" : "check"} the machine, ` +
    `and ${skips} should already be satisfied.`
  );
}

/**
 * Whether moving to `to` from `from` is a change at all.
 *
 * A deployment to the tag the cluster is already on is not refused -- it is a
 * perfectly good way to reconcile a drifted overlay, which is what a repair is
 * -- but the page says so, because an operator who picked the same version by
 * accident should find out before the run rather than from a list of fifteen
 * skips.
 */
export function isSameVersion(from: string, to: string): boolean {
  return from.trim() !== "" && from.trim() === to.trim();
}
