import { Check, Minus, X } from "lucide-react";

import type { DeploymentRow } from "./rows";

// THE STAGE RAIL -- this surface's one signature element.
//
// ===========================================================================
// WHY A SEQUENCE IS THE RIGHT SHAPE HERE, AND ALMOST NOWHERE ELSE
// ===========================================================================
// Numbered, ordered devices are the most over-used structure in software
// design, and they are right exactly when the order carries information the
// reader needs. A package deploy is that case: `stage -> roll -> publish` is a
// LAW (design D6), not a rendering choice, and it is reversed on rollback so
// that the app and the schema never disagree in either direction. A failure
// before `publish` leaves every site serving what it was serving -- which is
// only legible if you can see where the run stopped.
//
// So one component does three jobs that would otherwise be three widgets: it
// is the progress of a running deploy, the record of a finished one, and the
// explanation of what a run DID NOT do.
//
// ===========================================================================
// THE SKIPPED STAGES ARE THE POINT
// ===========================================================================
// A progress bar shows what happened. This shows what did not, and says why:
//
//     build  ->  stage DSL  ->  roll  ->  publish
//                 skipped       skipped
//                 this package ships no DSL
//
// That is the single most useful sentence this surface can show, because it is
// exactly why an SPA-only redeploy lands in seconds and restarts nothing --
// and a person who cannot see it has no way to know whether their deploy was
// fast or broken. Rendering a skipped stage as absent would leave them
// counting steps to find out.
//
// The rail also runs BACKWARDS for a rollback, because that is what actually
// happens, and a picture that ran forwards would quietly teach the wrong model
// of the only ordering rule in the product.

/** The D6 order, and its labels. Mirrors component/packages/pipeline.go. */
const STAGES: readonly { id: string; label: string; blurb: string }[] = [
  { id: "analyzing", label: "Analyze", blurb: "Read the tree and run the gates a node runs at boot" },
  { id: "awaiting_confirm", label: "Confirm", blurb: "Show what deploying would do, and wait" },
  { id: "building", label: "Build", blurb: "Turn each app's source into the files that get served" },
  { id: "staging_dsl", label: "Stage DSL", blurb: "Write the package's MemQL into storage, by content" },
  { id: "rolling", label: "Roll", blurb: "Restart the cluster onto the staged MemQL" },
  { id: "publishing", label: "Publish", blurb: "Point each site at its new files" },
];

const TERMINAL: Record<string, "done" | "refused" | "failed"> = {
  succeeded: "done",
  refused: "refused",
  failed: "failed",
};

type StageState = "done" | "current" | "skipped" | "stopped" | "ahead";

export interface RailStage {
  id: string;
  label: string;
  blurb: string;
  state: StageState;
  /** Why a stage was skipped, in the words a person can act on. */
  reason: string;
}

/**
 * railFor turns one deployment row into the stages to draw.
 *
 * Pure, and exported, because what a rail SAYS is the assertion worth making
 * in a test -- "an SPA-only run marks stage and roll skipped, with the reason"
 * is a statement about this function, not about a DOM.
 */
export function railFor(deployment: DeploymentRow): { stages: RailStage[]; reversed: boolean } {
  const terminal = TERMINAL[deployment.status];
  const reachedIndex = STAGES.findIndex((s) => s.id === deployment.status);

  // A run with no DSL in its report never enters stage or roll, and that is
  // the D6 fast path rather than an omission. Read from the REPORT rather than
  // from dslVersion, because a run that was refused before it staged has no
  // dslVersion either and those are different stories.
  const domains = deployment.report?.dslDomains ?? [];
  const carriesDsl = domains.length > 0;
  const dslSkipped = !carriesDsl || (terminal === "done" && deployment.dslVersion === "");

  // THE TWO SKIPPED STAGES SAY DIFFERENT THINGS, and stacking one sentence
  // twice read as a stutter rather than as two facts. They are a CHAIN: the
  // first says why there was nothing to stage, the second says what follows
  // from that -- which is also the more useful half, because "nothing had to
  // restart" is the answer to the question somebody actually has.
  const skipReasonFor = (id: string): string => {
    if (id === "staging_dsl") {
      return carriesDsl
        ? "this package's MemQL is already the version this cluster is running"
        : "this package ships no MemQL, so there is nothing to stage";
    }
    return "nothing was staged, so nothing had to restart";
  };

  const stages = STAGES.map((stage, index): RailStage => {
    const isDslStage = stage.id === "staging_dsl" || stage.id === "rolling";

    if (isDslStage && dslSkipped) {
      return { ...stage, state: "skipped", reason: skipReasonFor(stage.id) };
    }
    if (terminal === "done") {
      return { ...stage, state: "done", reason: "" };
    }
    if (terminal !== undefined) {
      // A refused or failed run stopped somewhere. The row records the last
      // stage it reached, so everything before it ran and everything after it
      // never started -- which is the guarantee that nothing was published.
      const stoppedAt = lastReachedIndex(deployment);
      if (index < stoppedAt) return { ...stage, state: "done", reason: "" };
      if (index === stoppedAt) return { ...stage, state: "stopped", reason: "" };
      return { ...stage, state: "ahead", reason: "" };
    }
    if (reachedIndex < 0) return { ...stage, state: "ahead", reason: "" };
    if (index < reachedIndex) return { ...stage, state: "done", reason: "" };
    if (index === reachedIndex) return { ...stage, state: "current", reason: "" };
    return { ...stage, state: "ahead", reason: "" };
  });

  return { stages, reversed: false };
}

// lastReachedIndex is where a terminal run got to.
//
// A refusal that happened during analysis leaves no later stage on the row, so
// the honest answer for a run whose status is now `refused` is the FURTHEST
// stage its evidence supports: a report means analysis ran, deployables mean
// publishing did.
function lastReachedIndex(d: DeploymentRow): number {
  if (d.deployables.length > 0) return indexOf("publishing");
  if (d.dslVersion !== "") return indexOf("rolling");
  if (d.buildLogTail !== "") return indexOf("building");
  if (d.report !== null) return indexOf("analyzing");
  return 0;
}

function indexOf(id: string): number {
  return STAGES.findIndex((s) => s.id === id);
}

export function StageRail({ deployment, reversed = false }: { deployment: DeploymentRow; reversed?: boolean }) {
  const { stages } = railFor(deployment);
  const ordered = reversed ? [...stages].reverse() : stages;

  return (
    <ol className="os-rail" data-reversed={reversed ? "true" : "false"} aria-label="Deploy stages">
      {ordered.map((stage) => (
        <li key={stage.id} className="os-rail-stage" data-state={stage.state}>
          <span className="os-rail-mark" aria-hidden>
            <StageGlyph state={stage.state} />
          </span>
          <span className="os-rail-body">
            <span className="os-rail-label">{stage.label}</span>
            <span className="os-rail-note">{stage.reason === "" ? stage.blurb : stage.reason}</span>
          </span>
          <span className="os-visually-hidden">{stateSentence(stage.state)}</span>
        </li>
      ))}
    </ol>
  );
}

function StageGlyph({ state }: { state: StageState }) {
  switch (state) {
    case "done":
      return <Check size={11} />;
    case "skipped":
      return <Minus size={11} />;
    case "stopped":
      return <X size={11} />;
    case "current":
      // The running stage gets no glyph at all: it is the one thing on the
      // rail that is moving, and the animated ring around it says so more
      // clearly than a symbol inside it would.
      return null;
    default:
      // NOR DOES AN UNREACHED ONE. A crossed circle reads as "forbidden",
      // which is a different statement from "has not happened yet" -- and on a
      // rail six stages long it put five refusal symbols on a healthy deploy.
      // An empty ring says exactly as much and says nothing wrong.
      return null;
  }
}

function stateSentence(state: StageState): string {
  switch (state) {
    case "done":
      return "finished";
    case "current":
      return "running now";
    case "skipped":
      return "skipped";
    case "stopped":
      return "stopped here";
    default:
      return "not reached";
  }
}
