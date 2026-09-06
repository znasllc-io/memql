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
// FOUR, AND THE MISSING FIFTH IS A FINDING
// ===========================================================================
// The portal drew artifacts and authored constructs hanging off the task that
// made them. There is no equivalent join here:
// `v1:library:artifact.producedByPlanId` still names `v1:planner:plan` and so
// does `v1:authoring:bundle.sourcePlanId` -- the spine retires those concepts
// in its section F, gated on epic A3, and until that lands NOTHING points a
// produced thing at a run. Drawing them would mean inventing a join, which is
// the one thing a surface read as evidence must not do.
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

// The concepts a goal's world is made of. Order is not load-bearing -- it is
// listed root-first only so a reader meets the goal before its work.
export const NEXUS_CONCEPT_IDS: readonly string[] = [
  GOAL_CONCEPT_ID,
  RUN_CONCEPT_ID,
  STEP_CONCEPT_ID,
  APPROVAL_CONCEPT_ID,
];
