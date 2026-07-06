// Client-agnostic runtime core. The typed query/mutation/logic
// methods are NOT here by design -- each product BFF generates them
// from its DSL and layers them onto QueryClient (via `declare module`
// + prototype augmentation) in the product SDK, e.g.
// the per-product SDK package, which re-exports this core.

export { Connection, type ConnectOptions, type ConnectionAuth } from "./connection.js";
export { Dispatcher, type DispatcherOptions } from "./dispatcher.js";
export { QueryClient, type QueryCallOptions } from "./query.js";
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
