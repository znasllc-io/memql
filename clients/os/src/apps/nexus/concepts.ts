import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import {
  APPROVAL_CONCEPT_ID,
  ARTIFACT_CONCEPT_ID,
  GOAL_CONCEPT_ID,
  RUN_CONCEPT_ID,
  STEP_CONCEPT_ID,
} from "../../nexus/concepts";

// The concepts the Nexus app is about.
//
// The five the MAP draws come from `src/nexus/concepts`, which the pure scene
// library also reads -- one definition, two consumers, because the layout, the
// subscription list and the id-only re-read all have to agree on the same five
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
// front. The Automations section follows TWO more concepts, and they are live
// too: `v1:authoring:construct` and the ladder's policy row both broadcast,
// through the `v1:authoring:*` rules in component/node/routing.go (memql#4542).
// They are not in this list because they are not populations this app owns --
// they belong to the authoring catalog, which Nexus reads and does not write.

export const NEXUS_APP_ID = "nexus";

/**
 * Live: goal, run, step, approval and artifact carry broadcast routing rules.
 *
 * The artifact is the one this app does NOT own -- it belongs to the Library,
 * and Nexus reads it to draw what a run produced. It is in this list rather
 * than beside `v1:authoring:construct` because it is genuinely live
 * (routing.go broadcasts all three verbs), so the map fills in as files
 * appear rather than only on load.
 */
export const NEXUS_LIVE_CONCEPTS = [
  Concepts.WORK_GOAL,
  Concepts.WORK_RUN,
  Concepts.WORK_STEP,
  Concepts.WORK_APPROVAL,
  Concepts.LIBRARY_ARTIFACT,
] as const;

/** On demand: the journal. No routing rule, deliberately. */
export const NEXUS_JOURNAL_CONCEPTS = [
  Concepts.WORK_MODEL_CALL,
  Concepts.WORK_OBSERVATION,
] as const;

/**
 * Everything this app OWNS, for its Logs section's subject scope.
 *
 * The artifact is deliberately absent: Nexus reads it and the Library writes
 * it, and a Logs scope that included it would answer "what happened in Nexus"
 * with every upload, promotion and archive in the system.
 */
export const NEXUS_LOG_CONCEPTS = [
  Concepts.WORK_GOAL,
  Concepts.WORK_RUN,
  Concepts.WORK_STEP,
  Concepts.WORK_APPROVAL,
  ...NEXUS_JOURNAL_CONCEPTS,
] as const;

/** The catalog the Automations section follows. Not owned by this app. */
export const CONSTRUCT_CONCEPT: string = Concepts.AUTHORING_CONSTRUCT;

/** The ladder's values: one seeded row, rewritten on every boot. Not owned by this app. */
export const LADDER_POLICY_CONCEPT: string = Concepts.AUTHORING_LADDER_POLICY;

export const GOAL_CONCEPT: string = GOAL_CONCEPT_ID;
export const RUN_CONCEPT: string = RUN_CONCEPT_ID;
export const STEP_CONCEPT: string = STEP_CONCEPT_ID;
export const APPROVAL_CONCEPT: string = APPROVAL_CONCEPT_ID;
export const ARTIFACT_CONCEPT: string = ARTIFACT_CONCEPT_ID;
