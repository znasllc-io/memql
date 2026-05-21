// Side-effect imports: the generated_*.ts modules augment
// QueryClient via `declare module` + `QueryClient.prototype.<name> =
// ...`. Importing them here guarantees the prototype assignments run
// once consumers import anything from `@znasllc-io/memql-sdk/client`.
import "./generated_queries.js";
import "./generated_mutations.js";
import "./generated_logics.js";

export { Connection, type ConnectOptions, type ConnectionAuth } from "./connection.js";
export { Dispatcher, type DispatcherOptions } from "./dispatcher.js";
export { QueryClient, type QueryCallOptions } from "./query.js";
export { SubscriptionManager, type EventHandler, type SubscribeOptions } from "./subscriptions.js";
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
  type Role,
  type Row,
  type SubscriptionKind,
} from "./types.js";
export { newShortId } from "./id.js";
export { renderMemQLValue } from "./memqlValue.js";
