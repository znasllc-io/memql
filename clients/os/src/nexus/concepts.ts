// The concept ids the Nexus scene library names.
//
// RE-POINTED FROM THE PLANNER TO THE WORK SPINE (epic memql#4785, sub-project
// B). This module used to name `v1:planner:plan`, `v1:planner:task` and the
// five populations the portal's map drew around them. The spine replaced that
// model: a goal produces RUNS, a run produces STEPS, and a step that has to
// stop and ask raises an APPROVAL. The SHAPE of a goal's world -- a root, its
// work laid out toward the goal, what ran it above and what it had to ask
// below -- is unchanged by the rows underneath it, which is the whole reason
// this library was worth keeping when the portal's pages were deleted.
//
// ===========================================================================
// THE MISSING FIFTH POPULATION IS NO LONGER MISSING
// ===========================================================================
// The portal drew artifacts and authored constructs hanging off the task that
// made them, and this comment used to record that the equivalent join did not
// exist: `v1:library:artifact.producedByPlanId` and
// `v1:authoring:bundle.sourcePlanId` both named `v1:planner:plan`, so NOTHING
// pointed a produced thing at a run, and drawing them would have meant
// inventing a join.
//
// memql#5053 re-pointed both. They are `producedByRunId` and `sourceRunId`
// now, and they name `v1:work:run` -- so an artifact and an authored bundle
// can each be hung off the run that made it, with a real edge rather than an
// invented one.
//
// The ARTIFACT half is drawn (see scene/layout.ts's "made" lane). Its read,
// `artifactsForRun`, was written FOR this map -- its own DSL header says so --
// and had no caller for as long as the join did not exist.
//
// `v1:authoring:bundle` is NOT drawn yet. This note used to say it could not
// be -- that it carried no broadcast routing rule -- and that was wrong:
// component/node/routing.go forwards graph.node.{created,updated,deleted} for
// every `v1:authoring:*` concept (memql#4542), bundle included. A live feed
// over it would move, so drawing it is a client change nobody has made yet,
// not wiring that is absent.
//
// GENERATED CONSTANTS, NEVER COMPOSED IDS (the Logs epic's rule, memql#4895):
// a hand-written "v1:work:step" would silently stop matching the day the
// namespace moves, and the failure is a live event arriving at a handler that
// does not recognise its own concept.

import { Concepts } from "@znasllc-io/memql-sdk-core/client";

export const GOAL_CONCEPT_ID: string = Concepts.WORK_GOAL;
export const RUN_CONCEPT_ID: string = Concepts.WORK_RUN;
export const STEP_CONCEPT_ID: string = Concepts.WORK_STEP;
export const APPROVAL_CONCEPT_ID: string = Concepts.WORK_APPROVAL;
export const ARTIFACT_CONCEPT_ID: string = Concepts.LIBRARY_ARTIFACT;

// The concepts a goal's world is made of. Order is not load-bearing -- it is
// listed root-first only so a reader meets the goal before its work.
export const NEXUS_CONCEPT_IDS: readonly string[] = [
  GOAL_CONCEPT_ID,
  RUN_CONCEPT_ID,
  STEP_CONCEPT_ID,
  APPROVAL_CONCEPT_ID,
  ARTIFACT_CONCEPT_ID,
];
