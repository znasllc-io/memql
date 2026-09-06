import { Concepts } from "@znasllc-io/memql-sdk-core/client";

// The concepts the Work app is about.
//
// GENERATED CONSTANTS, NEVER COMPOSED IDS (the Logs epic's rule, memql#4895):
// the app's Logs section asks the engine for lines tagged `work` OR about one
// of these, and a hand-written "v1:work:goal" here would silently stop
// matching the day the namespace moves.
//
// The six are not one list because they do not behave alike. The first four
// BROADCAST (design record section D, "Live feeds"), so their surfaces are
// live; modelCall and observation deliberately do not, on volume grounds --
// one row per model request and one per tool result -- so the journal is an
// on-demand read that says when it was taken. Reading a routing rule's
// absence as "nothing to see" is the mistake the Fleet app made once and the
// Training app records; here the split is the design's, stated up front.

export const WORK_APP_ID = "work";

/** Live: goal, run, step and approval carry broadcast routing rules. */
export const WORK_LIVE_CONCEPTS = [
  Concepts.WORK_GOAL,
  Concepts.WORK_RUN,
  Concepts.WORK_STEP,
  Concepts.WORK_APPROVAL,
] as const;

/** On demand: the journal. No routing rule, deliberately. */
export const WORK_JOURNAL_CONCEPTS = [Concepts.WORK_MODEL_CALL, Concepts.WORK_OBSERVATION] as const;

/** Everything this app owns, for its Logs section's subject scope. */
export const WORK_LOG_CONCEPTS = [...WORK_LIVE_CONCEPTS, ...WORK_JOURNAL_CONCEPTS] as const;

export const GOAL_CONCEPT: string = Concepts.WORK_GOAL;
export const RUN_CONCEPT: string = Concepts.WORK_RUN;
export const STEP_CONCEPT: string = Concepts.WORK_STEP;
export const APPROVAL_CONCEPT: string = Concepts.WORK_APPROVAL;
