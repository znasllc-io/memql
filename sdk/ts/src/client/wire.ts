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

// One-shot SI envelopes (chat / speech / transcribe / suggest).
// Mirror MemqlClientMessage oneof slots 18..21 (proto schema:
// component/grpc/memql.proto::SIChatMsg .. SISuggestMsg). Replies
// are correlated to the originating envelope's messageId via
// correlateTo, not via the per-payload requestId.

export interface SIChatMessageWire {
  role: string;
  content: string;
  name?: string;
}

export interface SIChatPayload {
  requestId: string;
  messages: SIChatMessageWire[];
  provider?: string;
  stream?: boolean;
}

export interface SISpeechPayload {
  requestId: string;
  input: string;
  voice?: string;
  format?: string; // "wav" | "mp3" | "ogg" | ...
  provider?: string;
}

export interface SITranscribePayload {
  requestId: string;
  audio: string; // base64-encoded bytes
  mimeType?: string;
}

export interface SISuggestPayload {
  requestId: string;
  domain: string;
  payload?: Record<string, unknown>; // google.protobuf.Struct -> plain object
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
  | { siChat: SIChatPayload }
  | { siSpeech: SISpeechPayload }
  | { siTranscribe: SITranscribePayload }
  | { siSuggest: SISuggestPayload }
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

export interface DisplayCardWire {
  primary?: string;
  secondary?: string;
  tertiary?: string;
  status?: string;
}

export interface ConceptInfoWire {
  id?: string;
  version?: string;
  domain?: string;
  entity?: string;
  description?: string;
  type?: string;
  displayCard?: DisplayCardWire;
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

// One-shot SI reply envelopes. SIChatResult mirrors SIChatMsg (the
// streaming-chat path interleaves SIStreamChunk frames for in-progress
// deltas and lands a terminal SIChatResult with the assembled message).
export interface SIChatResultPayload {
  requestId: string;
  message?: SIChatMessageWire;
}

export interface SISpeechResultPayload {
  requestId: string;
  audio?: string; // base64-encoded bytes (protojson encoding of `bytes`)
  format?: string;
}

export interface SITranscribeResultPayload {
  requestId: string;
  text?: string;
}

export interface SISuggestResultPayload {
  requestId: string;
  domain?: string;
  result?: Record<string, unknown>;
}

// SIStreamChunk envelopes interleave during streaming chat. The
// `chunk` oneof unmarshals to exactly one of `textDelta` (string),
// `jsonDelta` (object), or `metadata` (object) per frame.
export interface SIStreamChunkPayload {
  streamId?: string;
  provider?: string;
  requestId: string;
  index?: string | number;
  textDelta?: string;
  jsonDelta?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
  done?: boolean;
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
  | { siChatResult: SIChatResultPayload }
  | { siSpeechResult: SISpeechResultPayload }
  | { siTranscribeResult: SITranscribeResultPayload }
  | { siSuggestResult: SISuggestResultPayload }
  | { siChunk: SIStreamChunkPayload }
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
  | { kind: "siChatResult"; value: SIChatResultPayload }
  | { kind: "siSpeechResult"; value: SISpeechResultPayload }
  | { kind: "siTranscribeResult"; value: SITranscribeResultPayload }
  | { kind: "siSuggestResult"; value: SISuggestResultPayload }
  | { kind: "siChunk"; value: SIStreamChunkPayload }
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
  if (m.siChatResult)
    return { kind: "siChatResult", value: m.siChatResult as SIChatResultPayload };
  if (m.siSpeechResult)
    return { kind: "siSpeechResult", value: m.siSpeechResult as SISpeechResultPayload };
  if (m.siTranscribeResult)
    return { kind: "siTranscribeResult", value: m.siTranscribeResult as SITranscribeResultPayload };
  if (m.siSuggestResult)
    return { kind: "siSuggestResult", value: m.siSuggestResult as SISuggestResultPayload };
  if (m.siChunk)
    return { kind: "siChunk", value: m.siChunk as SIStreamChunkPayload };
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
//
// SIChatResult appears here so the terminal frame in a streaming chat
// session (siChatStream, which uses dispatcher.send without
// registering in `pending`) routes to the per-requestId stream
// listener. The non-streaming siChat path uses sendAndWait, which
// resolves on correlateTo before streamRequestId is consulted.
export function streamRequestId(msg: ServerMessage): string {
  const m = msg as unknown as Record<string, { requestId?: string } | undefined>;
  if (m.siTranscribeStreamDelta?.requestId) return m.siTranscribeStreamDelta.requestId;
  if (m.siTranscribeStreamComplete?.requestId) return m.siTranscribeStreamComplete.requestId;
  if (m.siChunk?.requestId) return m.siChunk.requestId;
  if (m.siChatResult?.requestId) return m.siChatResult.requestId;
  return "";
}
