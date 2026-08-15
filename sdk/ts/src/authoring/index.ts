// The authoring surface (memql#2128 / C1): Gate-1 bundle validation and
// stream-scoped session-define -- the substrate that lets a client run a
// .memql buffer that has never been saved or deployed -- plus the owner-only
// DURABLE pair, promote and demote (memql#3760), which is how a construct
// stops being one caller's buffer and becomes something the whole cluster
// serves.

export {
  AuthoringClient,
  DemoteOutcomeRemoved,
  DemoteOutcomeRetired,
  failedDiagnostics,
  type AuthoringCallOptions,
  type AuthoringConstruct,
  type AuthoringDiagnostic,
  type ConceptSchemaChange,
  type ConceptSchemaDiff,
  type DemoteOutcome,
  type DurableDemoteBundleResult,
  type DurablePromoteBundleOptions,
  type DurablePromoteBundleResult,
  type SessionDefineBundleResult,
  type ValidateBundleResult,
} from "./authoring.js";
