// JSON envelope shapes for `/memql/ws` traffic.
//
// The bridge (component/server/memqlws/handler.go) accepts both binary
// (proto) and text (protojson) WebSocket frames. The TS SDK speaks
// protojson exclusively -- bundling a proto runtime in the browser is
// not worth the bytes for the message families we care about. Field
// naming matches protojson defaults (camelCase).
//
// Only the subset of `MemqlClientMessage` / `MemqlServerMessage` the
// SDK actually emits or consumes is typed here. Adding a new wire
// message means extending these envelopes plus the dispatcher's switch
// in connection.ts. The proto remains the source of truth -- this file
// is the hand-mirrored TS view of the subset.

export interface ClientHelloPayload {
  clientId: string;
  sdkName: string;
  sdkVersion: string;
}

export interface ExecuteQueryPayload {
  requestId: string;
  query: string;
  clientId?: string;
}

export interface CancelRequestPayload {
  requestId: string;
}

export interface SubscribePayload {
  subscriptionId: string;
  kind: SubscriptionKindWire;
  filter?: string;
}

export interface UnsubscribePayload {
  subscriptionId: string;
}

export interface RotateAuthPayload {
  accessToken: string;
}

export interface ConceptsListPayload {
  requestId?: string;
}

export interface MyAccessPayload {
  requestId?: string;
}

export interface SITranscribeStreamStartPayload {
  requestId: string;
  format: string; // "pcm16" | "opus" | "webm" | "wav"
  sampleRate: number;
  channels: number;
  languageHint?: string;
  provider?: string;
}

export interface SITranscribeStreamChunkPayload {
  requestId: string;
  audio: string; // base64-encoded bytes (protojson encoding of `bytes`)
}

export interface SITranscribeStreamEndPayload {
  requestId: string;
  cancel?: boolean;
}

// MemqlClientMessage oneof. Exactly one payload field must be set.
export type ClientMessage = MessageBase & ClientPayload;

interface MessageBase {
  messageId?: string;
  correlateTo?: string;
  metadata?: Record<string, string>;
}

type ClientPayload =
  | { clientHello: ClientHelloPayload }
  | { executeQuery: ExecuteQueryPayload }
  | { cancelRequest: CancelRequestPayload }
  | { subscribe: SubscribePayload }
  | { unsubscribe: UnsubscribePayload }
  | { rotateAuth: RotateAuthPayload }
  | { conceptsList: ConceptsListPayload }
  | { myAccess: MyAccessPayload }
  | { siTranscribeStreamStart: SITranscribeStreamStartPayload }
  | { siTranscribeStreamChunk: SITranscribeStreamChunkPayload }
  | { siTranscribeStreamEnd: SITranscribeStreamEndPayload };

// Server-side payloads. Untyped `any`-shaped fields appear where the
// engine returns `google.protobuf.Struct` -- those decode to plain
// objects in protojson and the SDK exposes them as `unknown` so
// consumers go through typed accessors instead of reaching in blind.

export interface ServerHelloPayload {
  nodeId?: string;
  version?: string;
}

export interface QueryResultPayload {
  requestId: string;
  result?: {
    bundle?: GraphBundleWire;
    data?: unknown[];
    meta?: ResultMetaWire;
  };
  done?: boolean;
}

export interface QueryErrorPayload {
  requestId: string;
  error?: { code?: string; message?: string };
}

export interface EventPayload {
  subscriptionId: string;
  kind?: string; // EventKind enum string e.g. "EVENT_KIND_NODE_CREATED"
  ts?: string;
  payload?: unknown;
}

export interface ConceptsListResultPayload {
  requestId?: string;
  concepts?: ConceptInfoWire[];
  baseTopics?: string[];
  systemTopics?: string[];
}

export interface ConceptInfoWire {
  id?: string;
  version?: string;
  domain?: string;
  entity?: string;
  description?: string;
  type?: string;
}

export interface MyAccessResultPayload {
  requestId?: string;
  userId?: string;
  primaryEmail?: string;
  clusterRole?: UserRoleWire;
}

export interface RotateAuthResultPayload {
  ok?: boolean;
  error?: string;
  errorDescription?: string;
}

export interface HeartbeatPayload {
  // Engine ping. The SDK ignores its contents.
  [key: string]: unknown;
}

export interface SITranscribeStreamDeltaPayload {
  requestId: string;
  text?: string;
  isFinal?: boolean;
  confidence?: number;
}

export interface SITranscribeStreamCompletePayload {
  requestId: string;
  text?: string;
  durationMs?: string | number;
  provider?: string;
}

export interface GraphBundleWire {
  nodes?: MemoryNodeWire[];
  edges?: unknown[];
  rootIds?: string[];
}

export interface MemoryNodeWire {
  id?: string;
  concept?: string;
  type?: string;
  createdBy?: string;
  createdAt?: string;
  payload?: Record<string, unknown>;
  schema?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
}

export interface ResultMetaWire {
  count?: string | number;
  hasMore?: boolean;
  cursor?: string;
  tookMs?: string | number;
  clientId?: string;
  serverId?: string;
  version?: string | number;
}

export type SubscriptionKindWire =
  | "SUBSCRIPTION_KIND_UNSPECIFIED"
  | "SUBSCRIPTION_KIND_TELEMETRY"
  | "SUBSCRIPTION_KIND_MESSAGE"
  | "SUBSCRIPTION_KIND_QUERY_SPEC"
  | "SUBSCRIPTION_KIND_AI_STREAM"
  | "SUBSCRIPTION_KIND_GRAPH_EVENTS"
  | "SUBSCRIPTION_KIND_DOMAIN_EVENTS"
  | "SUBSCRIPTION_KIND_AUTOMATION_EVENTS"
  | "SUBSCRIPTION_KIND_ALL";

export type UserRoleWire =
  | "USER_ROLE_UNSPECIFIED"
  | "USER_ROLE_OWNER"
  | "USER_ROLE_ADMIN"
  | "USER_ROLE_WRITER"
  | "USER_ROLE_READER";

export type ServerMessage = MessageBase & ServerPayload;

type ServerPayload =
  | { serverHello: ServerHelloPayload }
  | { queryResult: QueryResultPayload }
  | { queryError: QueryErrorPayload }
  | { event: EventPayload }
  | { heartbeat: HeartbeatPayload }
  | { conceptsListResult: ConceptsListResultPayload }
  | { myAccessResult: MyAccessResultPayload }
  | { rotateAuthResult: RotateAuthResultPayload }
  | { siTranscribeStreamDelta: SITranscribeStreamDeltaPayload }
  | { siTranscribeStreamComplete: SITranscribeStreamCompletePayload };

// Narrow a ServerMessage to its single payload entry. Returns the
// first present payload key + its value, or null when the envelope
// carries nothing the SDK recognises.
export function readServerPayload(msg: ServerMessage):
  | { kind: "serverHello"; value: ServerHelloPayload }
  | { kind: "queryResult"; value: QueryResultPayload }
  | { kind: "queryError"; value: QueryErrorPayload }
  | { kind: "event"; value: EventPayload }
  | { kind: "heartbeat"; value: HeartbeatPayload }
  | { kind: "conceptsListResult"; value: ConceptsListResultPayload }
  | { kind: "myAccessResult"; value: MyAccessResultPayload }
  | { kind: "rotateAuthResult"; value: RotateAuthResultPayload }
  | { kind: "siTranscribeStreamDelta"; value: SITranscribeStreamDeltaPayload }
  | { kind: "siTranscribeStreamComplete"; value: SITranscribeStreamCompletePayload }
  | null {
  const m = msg as unknown as Record<string, unknown>;
  if (m.serverHello) return { kind: "serverHello", value: m.serverHello as ServerHelloPayload };
  if (m.queryResult) return { kind: "queryResult", value: m.queryResult as QueryResultPayload };
  if (m.queryError) return { kind: "queryError", value: m.queryError as QueryErrorPayload };
  if (m.event) return { kind: "event", value: m.event as EventPayload };
  if (m.heartbeat) return { kind: "heartbeat", value: m.heartbeat as HeartbeatPayload };
  if (m.conceptsListResult)
    return { kind: "conceptsListResult", value: m.conceptsListResult as ConceptsListResultPayload };
  if (m.myAccessResult)
    return { kind: "myAccessResult", value: m.myAccessResult as MyAccessResultPayload };
  if (m.rotateAuthResult)
    return { kind: "rotateAuthResult", value: m.rotateAuthResult as RotateAuthResultPayload };
  if (m.siTranscribeStreamDelta)
    return {
      kind: "siTranscribeStreamDelta",
      value: m.siTranscribeStreamDelta as SITranscribeStreamDeltaPayload,
    };
  if (m.siTranscribeStreamComplete)
    return {
      kind: "siTranscribeStreamComplete",
      value: m.siTranscribeStreamComplete as SITranscribeStreamCompletePayload,
    };
  return null;
}

// streamRequestId returns the per-session request_id carried by a
// streaming-protocol server message (mirrors sdk/go's streamRequestId
// in dispatcher.go). Empty when the message does not belong to a
// known streaming family.
export function streamRequestId(msg: ServerMessage): string {
  const m = msg as unknown as Record<string, { requestId?: string } | undefined>;
  if (m.siTranscribeStreamDelta?.requestId) return m.siTranscribeStreamDelta.requestId;
  if (m.siTranscribeStreamComplete?.requestId) return m.siTranscribeStreamComplete.requestId;
  return "";
}
