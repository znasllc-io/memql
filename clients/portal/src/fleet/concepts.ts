// The concept ids the Fleet surface is about.
//
// Same decision sites/concepts.ts and artifacts/concepts.ts make: naming a
// concept id in a FEATURE module is not the concept-agnostic BROWSE machinery
// (src/concepts, src/components, src/viewkit) that portal_render_path_test.go
// holds to zero concept-id literals. A designed surface is about a specific
// population and has to say which one.
//
// They are collected in ONE module rather than written at the four call sites
// that subscribe with them, because a subscription filter and the read it
// keeps fresh have to name the same concept -- and a typo in one of them is a
// list that silently stops updating rather than an error.

// The machine record: one row per memql-cockpit instance in worker mode.
export const WORKER_REGISTRATION_CONCEPT_ID = "v1:worker:registration";

// The owner's routing policy. One ACTIVE row per user; superseded rows are
// kept because an invocation's routing.policyId names whichever row chose.
export const WORKER_ROUTING_POLICY_CONCEPT_ID = "v1:worker:routingPolicy";

// Per-call telemetry. The per-machine activity list reads these.
export const WORKER_INVOCATION_CONCEPT_ID = "v1:worker:invocation";

// The per-Plan sandboxed working directory on a workbench replica.
export const WORKBENCH_WORKSPACE_CONCEPT_ID = "v1:workbench:workspace";

// The mesh's own node rows. The workbench NODES section reads these and
// narrows to nodeType=workbench client-side -- see useWorkbenches.ts for why
// that narrowing is not a query argument.
export const CLUSTER_NODE_CONCEPT_ID = "v1:cluster:node";

// The nodeType value that makes a v1:cluster:node row a workbench replica.
export const WORKBENCH_NODE_TYPE = "workbench";
