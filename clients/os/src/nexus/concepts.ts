// The concept ids the Nexus surface names.
//
// Same decision artifacts/concepts.ts and sites/concepts.ts make, and the
// same reason: naming a concept id in a FEATURE module is not the
// concept-agnostic BROWSE machinery (src/concepts, src/components,
// src/viewkit) that portal_render_path_test.go holds to zero concept-id
// literals. Nexus is a designed surface about a specific set of populations
// -- a goal, its tasks, the agents it raised, what it authored and what it
// produced -- and it has to say which ones.
//
// They are gathered in ONE module rather than spread across the feed, the
// scene and the pages because the subscription list, the id-only re-read and
// the row-detail route all have to agree on the same seven strings; three
// copies is three chances for one of them to drift and for a live event to
// arrive at a handler that silently does not recognise its concept.

export const PLAN_CONCEPT_ID = "v1:planner:plan";
export const TASK_CONCEPT_ID = "v1:planner:task";
export const AGENT_CONCEPT_ID = "v1:agents:agent";
export const BUNDLE_CONCEPT_ID = "v1:authoring:bundle";
export const CONSTRUCT_CONCEPT_ID = "v1:authoring:construct";
export const DEPENDENCY_EDGE_CONCEPT_ID = "v1:authoring:dependencyEdge";
export const ARTIFACT_CONCEPT_ID = "v1:library:artifact";

// The concepts the feed follows. Order is the order subscriptions are opened
// in, which is not load-bearing -- it is listed newest-fact-first only so a
// reader sees the goal before its debris.
export const NEXUS_CONCEPT_IDS: readonly string[] = [
  PLAN_CONCEPT_ID,
  TASK_CONCEPT_ID,
  AGENT_CONCEPT_ID,
  BUNDLE_CONCEPT_ID,
  CONSTRUCT_CONCEPT_ID,
  DEPENDENCY_EDGE_CONCEPT_ID,
  ARTIFACT_CONCEPT_ID,
];
