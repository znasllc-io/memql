import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import {
  APPROVAL_CONCEPT_ID,
  GOAL_CONCEPT_ID,
  RUN_CONCEPT_ID,
  STEP_CONCEPT_ID,
} from "../../nexus/concepts";

// The concepts the Nexus app is about.
//
// The four the MAP draws come from `src/nexus/concepts`, which the pure scene
// library also reads -- one definition, two consumers, because the layout, the
// subscription list and the id-only re-read all have to agree on the same four
// strings and three copies is three chances for one of them to drift.
//
// GENERATED CONSTANTS, NEVER COMPOSED IDS (the Logs epic's rule, memql#4895):
// the app's Logs section asks the engine for lines tagged `nexus` OR about one
// of these, and a hand-written "v1:work:goal" here would silently stop matching
// the day the namespace moves.
//
// The six are not one list because they do not behave alike. The first four
// BROADCAST (the spine's design record, section D "Live feeds"), so their
// surfaces are live; modelCall and observation deliberately do not, on volume
// grounds -- one row per model request and one per tool result -- so the
// journal is an on-demand read that says when it was taken. Reading a routing
// rule's absence as "nothing to see" is the mistake the Fleet app made once
// and the Training app records; here the split is the design's, stated up
// front. The Automations section reads a SEVENTH concept the same way and for
// the same reason (`v1:authoring:construct`, which carries no routing rule),
// and it is not in this list because it is not a population this app owns --
// it belongs to the authoring catalog, which Nexus reads and does not write.

export const NEXUS_APP_ID = "nexus";

/** Live: goal, run, step and approval carry broadcast routing rules. */
export const NEXUS_LIVE_CONCEPTS = [
  Concepts.WORK_GOAL,
  Concepts.WORK_RUN,
  Concepts.WORK_STEP,
  Concepts.WORK_APPROVAL,
] as const;

/** On demand: the journal. No routing rule, deliberately. */
export const NEXUS_JOURNAL_CONCEPTS = [
  Concepts.WORK_MODEL_CALL,
  Concepts.WORK_OBSERVATION,
] as const;

/** Everything this app owns, for its Logs section's subject scope. */
export const NEXUS_LOG_CONCEPTS = [
  ...NEXUS_LIVE_CONCEPTS,
  ...NEXUS_JOURNAL_CONCEPTS,
] as const;

/** The catalog the Automations section reads. Not owned by this app. */
export const CONSTRUCT_CONCEPT: string = Concepts.AUTHORING_CONSTRUCT;

export const GOAL_CONCEPT: string = GOAL_CONCEPT_ID;
export const RUN_CONCEPT: string = RUN_CONCEPT_ID;
export const STEP_CONCEPT: string = STEP_CONCEPT_ID;
export const APPROVAL_CONCEPT: string = APPROVAL_CONCEPT_ID;
