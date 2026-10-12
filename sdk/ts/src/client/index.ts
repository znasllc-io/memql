// Client-agnostic runtime core. The typed query/mutation/logic
// methods are NOT here by design -- each product BFF generates them
// from its DSL and layers them onto QueryClient (via `declare module`
// + prototype augmentation) in the product SDK, e.g.
// the per-product SDK package, which re-exports this core.

export {
  Connection,
  backoffDelayMs,
  type ConnectOptions,
  type ConnectionAuth,
  type ConnectionStatus,
  type ConnectionStatusEvent,
  type ReconnectOptions,
} from "./connection.js";
export { Dispatcher, type DispatcherOptions } from "./dispatcher.js";
export { QueryClient, refusalSentence, type QueryCallOptions, type ConceptRegistryFollow } from "./query.js";
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
  countConcept,
  getRowByConceptAndId,
  DEFAULT_CONCEPT_BROWSE_PAGE_SIZE,
  type ConceptPage,
  type ConceptBrowseOptions,
} from "./conceptBrowser.js";
export {
  LiveCollection,
  LiveStore,
  LiveValue,
  disposeLiveStoreFor,
  liveStoreFor,
  type LiveCollectionSpec,
  type LiveHandle,
  type LiveSnapshot,
  type LiveState,
  type LiveStoreHost,
  type LiveStoreOptions,
  type LiveValueSnapshot,
  type SeedPage,
} from "./liveCollection.js";
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
  versionOrder,
  type AccessSummary,
  type Concept,
  type ConceptField,
  type ConceptFieldKind,
  type ConceptRegistryDelta,
  type ConceptRelationship,
  type CountAnswer,
  type DomainSubscription,
  type Event,
  type GraphAction,
  type Role,
  type Row,
  type SubscriptionKind,
} from "./types.js";
export { newShortId, bareShortId, sameEntityId } from "./id.js";
// Relationship lookups: resolving a pointer field to the row it names, in
// batches, WITHOUT an engine join (epic memql#4661).
export {
  LookupCache,
  parseRefBinding,
  isRefBinding,
  REF_PREFIX,
  type RefBinding,
} from "./lookups.js";
export { renderMemQLValue } from "./memqlValue.js";
export { deepStripNulls } from "./payload.js";
export { displayDomainIds, isSyntheticDomainId } from "./domainIds.js";
// The one way a client creates a file: start a Materializer composition, follow
// it to its Files entry, and hand a description to the Library's revision
// workflow (docs/superpowers/specs/2026-10-11-materializer-front-door-design.md).
export * from "./materialize.js";
// The rule for what a person may call a new file, which every client checks a
// name with as it is typed.
export { fileNameProblem, fileNameStem } from "./fileName.js";

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
