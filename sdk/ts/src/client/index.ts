// Client-agnostic runtime core. The typed query/mutation/logic
// methods are NOT here by design -- each product BFF generates them
// from its DSL and layers them onto QueryClient (via `declare module`
// + prototype augmentation) in the product SDK, e.g.
// the per-product SDK package, which re-exports this core.

export { Connection, type ConnectOptions, type ConnectionAuth } from "./connection.js";
export {
  uploadAttachment,
  type AttachmentUploadSource,
  type UploadAttachmentParams,
  type AttachmentFile,
  type AttachmentRef,
} from "./attachments.js";
export { Dispatcher, type DispatcherOptions } from "./dispatcher.js";
export { QueryClient, type QueryCallOptions, type ConceptRegistryFollow } from "./query.js";
export {
  ModulesClient,
  type Module,
  type ModuleDetail,
  type ModuleEnvVar,
  type ModuleKind,
  type ModulesCallOptions,
  type ModulesInventory,
  type SetPackEnabledOutcome,
} from "./modules.js";
export {
  browseConceptPage,
  getRowByConceptAndId,
  DEFAULT_CONCEPT_BROWSE_PAGE_SIZE,
  type ConceptPage,
  type ConceptBrowseOptions,
} from "./conceptBrowser.js";
export {
  SubscriptionManager,
  type EventHandler,
  type GraphSubscribeOptions,
  type SubscribeOptions,
} from "./subscriptions.js";
export {
  Result,
  rowArray,
  rowBool,
  rowNumber,
  rowObject,
  rowString,
  type AccessSummary,
  type Concept,
  type ConceptRegistryDelta,
  type DomainSubscription,
  type Event,
  type GraphAction,
  type Role,
  type Row,
  type SubscriptionKind,
} from "./types.js";
export { newShortId } from "./id.js";
export { renderMemQLValue } from "./memqlValue.js";
export { deepStripNulls } from "./payload.js";
export { displayDomainIds, isSyntheticDomainId } from "./domainIds.js";

// -----------------------------------------------------------------------------
// The generated typed construct surface (memql#4232).
//
// These modules are emitted by `make sdk-gen` from dsl/**/*.memql and
// committed; the sdk-gen-check drift gate keeps them in lockstep with the
// DSL. Re-exporting them here is load-bearing twice over: the wildcard
// re-export EXECUTES each module, which is what installs the generated
// methods on QueryClient.prototype for every consumer of this barrel, and
// it surfaces the per-construct Args interfaces + call-string builders so
// a client's arguments typecheck against the construct's declared schema
// instead of being hand-rolled around a string name. Construct names are
// globally unique (the generator refuses cross-root collisions), so the
// four wildcards cannot clash.
// -----------------------------------------------------------------------------
export * from "./generated_queries.js";
export * from "./generated_mutations.js";
export * from "./generated_logics.js";
export * from "./generated_builtins.js";
export * from "./generated_concepts.js";
