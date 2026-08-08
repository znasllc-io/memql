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
  // Opaque keyset continuation cursor (memql#1985 / 5.12). When set, the
  // engine continues the query from the encoded position via a SQL keyset
  // predicate. Obtained from a prior response's ResultMeta.cursor; opaque
  // to clients and bound to the query's sort ordering. Empty/unset for a
  // first page or offset/unpaginated queries. Rides ExecuteQueryMsg.cursor
  // (proto field 5); the JSON field name matches in both camel/snake form,
  // so protojson unmarshals it on the server without a rename.
  cursor?: string;
}

export interface CancelRequestPayload {
  requestId: string;
}

export interface SubscribePayload {
  subscriptionId: string;
  kind: SubscriptionKindWire;
  // filter is the legacy free-text bus pattern, retained for the NON-graph
  // subscription kinds. Graph subscriptions are structured (concept +
  // actions) and the server rejects a filter for them (memql#2460).
  filter?: string;
  // Structured graph subscription fields (kind == GRAPH_EVENTS). The server
  // composes the bus topic from these.
  concept?: string;
  actions?: GraphNodeActionWire[];
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

export interface AiTranscribeStreamStartPayload {
  requestId: string;
  format: string; // "pcm16" | "opus" | "webm" | "wav"
  sampleRate: number;
  channels: number;
  languageHint?: string;
  provider?: string;
}

export interface AiTranscribeStreamChunkPayload {
  requestId: string;
  audio: string; // base64-encoded bytes (protojson encoding of `bytes`)
}

export interface AiTranscribeStreamEndPayload {
  requestId: string;
  cancel?: boolean;
}

// Identity + access envelopes. Guest invites, worker tokens, and
// session revoke. Mirror MemqlClientMessage oneof slots 46..54
// (proto schema: component/grpc/memql.proto).

export interface SendGuestInvitePayload {
  requestId: string;
  spaceId: string;
  spaceName: string;
  inviterName: string;
  email: string;
  guestName?: string;
  joinUrlBase: string;
  expiresInMinutes?: number;
}

export interface ResolveGuestInvitePayload {
  requestId: string;
  token: string;
}

export interface JoinSpaceAsGuestPayload {
  requestId: string;
  participantId: string;
  displayName: string;
}

export interface CancelGuestInvitePayload {
  requestId: string;
  invitationId: string;
}

export interface ResendGuestInviteEmailPayload {
  requestId: string;
  invitationId: string;
  joinUrlBase: string;
}

export interface RevokeCurrentSessionPayload {
  requestId: string;
}

export interface RevokeAllSessionsPayload {
  requestId: string;
}

export interface CreateWorkerTokenPayload {
  requestId: string;
  name: string;
  expiresAt?: string; // ISO8601, empty = no auto-expiry
  ownerUserId?: string;
}

export interface RevokeWorkerTokenPayload {
  requestId: string;
  identityId: string;
}

// Badge registration lifecycle (memql#2513). The GRANT exchange is
// identity-HTTP (POST /auth/badge/grant), not a stream message; these
// register / revoke the badge credential itself.
export interface CreateBadgePayload {
  requestId: string;
  badgeId: string; // plaintext identifier; hashed server-side, never persisted
  label: string;
  ownerUserId?: string; // admin-only override; empty = caller
}

export interface RevokeBadgePayload {
  requestId: string;
  identityId: string;
}

// Deploy console (memql#3311). DeployControlService is a separate
// UNARY gRPC service; a browser cannot dial it, so its RPCs are
// bridged onto MemqlService.Stream behind ONE envelope whose `request`
// oneof carries the service's own request messages verbatim. Exactly
// one request field must be set. Every action is role-gated (view:
// admin+, cut/deploy: developer+, rollback: owner only) and audited
// server-side by the same code the unary path runs -- a denial comes
// back as an ordinary queryError with code "PermissionDenied".
//
// Deployment HISTORY is NOT here: v1:cluster:deployment rows are
// ordinary concept rows, read with a normal query.

export interface GetDeploymentStatusRequestWire {
  env: string; // "staging" | "prod"
}

export interface SuggestNextVersionRequestWire {
  env: string;
}

export interface DeployStagingRequestWire {
  version: string;
}

export interface PromoteRequestWire {
  version: string;
}

export interface RollbackRequestWire {
  env: string;
  commitSha: string;
}

export interface RolloutActionRequestWire {
  env: string;
  rollout: string;
  action: string; // "promote" | "abort"
}

export interface CutVersionRequestWire {
  env: string;
  // bump ∈ {"major","minor","patch"}; ignored when version is set.
  bump?: string;
  // explicit clean-semver override; when set, bump is ignored.
  version?: string;
}

export interface DeployRequestWire {
  deploymentId: string;
}

export interface RollbackDeploymentRequestWire {
  toDeploymentId: string;
}

export type DeployControlRequestWire =
  | { getDeploymentStatus: GetDeploymentStatusRequestWire }
  | { suggestNextVersion: SuggestNextVersionRequestWire }
  | { deployStaging: DeployStagingRequestWire }
  | { promote: PromoteRequestWire }
  | { rollback: RollbackRequestWire }
  | { rolloutAction: RolloutActionRequestWire }
  | { cutVersion: CutVersionRequestWire }
  | { deploy: DeployRequestWire }
  | { rollbackDeployment: RollbackDeploymentRequestWire };

export type DeployControlPayload = { requestId: string } & DeployControlRequestWire;

// Polyphon -- LiveKit room token request. The room name + LiveKit
// URL come back in the reply so the consumer can hand them to the
// LiveKit client SDK without a separate config call.
export interface PolyphonRoomTokenPayload {
  requestId: string;
  scopeId: string;
  participantId: string;
  displayName: string;
}

// MCP-shaped tool RPC + client-execution dispatch envelopes.
// memql#174. ListTools / CallTool are outbound (consumer -> server);
// ClientToolCall is inbound (server -> consumer) for tools marked
// client_execution=true; ClientToolResult is the consumer's reply
// correlated by callId.

export interface ListToolsPayload {
  requestId: string;
  cursor?: string;
}

export interface CallToolPayload {
  requestId: string;
  name: string;
  arguments?: Record<string, unknown>; // google.protobuf.Struct -> plain object
}

export interface ClientToolResultPayload {
  callId: string;
  content: ToolResultContentWire[];
  isError: boolean;
  errorMessage?: string;
}

export interface ToolResultContentWire {
  type: string; // "text" | "image" | "resource"
  text?: string;
  mimeType?: string;
  data?: string; // base64 for "image"
  uri?: string;
}

// One-shot SI envelopes (chat / speech / transcribe / suggest).
// Mirror MemqlClientMessage oneof slots 18..21 (proto schema:
// component/grpc/memql.proto::AiChatMsg .. AiSuggestMsg). Replies
// are correlated to the originating envelope's messageId via
// correlateTo, not via the per-payload requestId.

export interface AiChatMessageWire {
  role: string;
  content: string;
  name?: string;
}

export interface AiChatPayload {
  requestId: string;
  messages: AiChatMessageWire[];
  provider?: string;
  stream?: boolean;
}

export interface AiSpeechPayload {
  requestId: string;
  input: string;
  voice?: string;
  format?: string; // "wav" | "mp3" | "ogg" | ...
  provider?: string;
}

export interface AiTranscribePayload {
  requestId: string;
  audio: string; // base64-encoded bytes
  mimeType?: string;
}

export interface AiSuggestPayload {
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
  | { aiChat: AiChatPayload }
  | { aiSpeech: AiSpeechPayload }
  | { aiTranscribe: AiTranscribePayload }
  | { aiSuggest: AiSuggestPayload }
  | { aiTranscribeStreamStart: AiTranscribeStreamStartPayload }
  | { aiTranscribeStreamChunk: AiTranscribeStreamChunkPayload }
  | { aiTranscribeStreamEnd: AiTranscribeStreamEndPayload }
  | { sendGuestInvite: SendGuestInvitePayload }
  | { resolveGuestInvite: ResolveGuestInvitePayload }
  | { joinSpaceAsGuest: JoinSpaceAsGuestPayload }
  | { cancelGuestInvite: CancelGuestInvitePayload }
  | { resendGuestInviteEmail: ResendGuestInviteEmailPayload }
  | { revokeCurrentSession: RevokeCurrentSessionPayload }
  | { revokeAllSessions: RevokeAllSessionsPayload }
  | { createWorkerToken: CreateWorkerTokenPayload }
  | { revokeWorkerToken: RevokeWorkerTokenPayload }
  | { createBadge: CreateBadgePayload }
  | { revokeBadge: RevokeBadgePayload }
  | { deployControl: DeployControlPayload }
  | { polyphonRoomToken: PolyphonRoomTokenPayload }
  | { listTools: ListToolsPayload }
  | { callTool: CallToolPayload }
  | { clientToolResult: ClientToolResultPayload };

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

export interface AiTranscribeStreamDeltaPayload {
  requestId: string;
  text?: string;
  isFinal?: boolean;
  confidence?: number;
}

export interface AiTranscribeStreamCompletePayload {
  requestId: string;
  text?: string;
  durationMs?: string | number;
  provider?: string;
}

// One-shot SI reply envelopes. AiChatResult mirrors AiChatMsg (the
// streaming-chat path interleaves AiStreamChunk frames for in-progress
// deltas and lands a terminal AiChatResult with the assembled message).
export interface AiChatResultPayload {
  requestId: string;
  message?: AiChatMessageWire;
}

export interface AiSpeechResultPayload {
  requestId: string;
  audio?: string; // base64-encoded bytes (protojson encoding of `bytes`)
  format?: string;
}

export interface AiTranscribeResultPayload {
  requestId: string;
  text?: string;
}

export interface AiSuggestResultPayload {
  requestId: string;
  domain?: string;
  result?: Record<string, unknown>;
}

// AiStreamChunk envelopes interleave during streaming chat. The
// `chunk` oneof unmarshals to exactly one of `textDelta` (string),
// `jsonDelta` (object), or `metadata` (object) per frame.
export interface AiStreamChunkPayload {
  streamId?: string;
  provider?: string;
  requestId: string;
  index?: string | number;
  textDelta?: string;
  jsonDelta?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
  done?: boolean;
}

// Identity + access reply envelopes. errorCode carries a short
// machine-readable tag on partial failures (empty = success); see
// memql.proto for the per-message tag set.

export interface SendGuestInviteResultPayload {
  requestId: string;
  success?: boolean;
  invitationId?: string;
  errorCode?: string;
  errorMessage?: string;
}

export interface ResolveGuestInviteResultPayload {
  requestId: string;
  // "ok" | "invalid" | "expired" | "already_accepted" | "cancelled"
  status?: string;
  invitationId?: string;
  spaceId?: string;
  spaceName?: string;
  inviterName?: string;
  inviteeEmail?: string;
  inviteeName?: string;
  expiresAt?: string; // protojson timestamp -> ISO8601 string
  errorMessage?: string;
}

export interface JoinSpaceAsGuestResultPayload {
  requestId: string;
  success?: boolean;
  participantId?: string;
  spaceId?: string;
  errorCode?: string;
  errorMessage?: string;
}

export interface CancelGuestInviteResultPayload {
  requestId: string;
  success?: boolean;
  invitationId?: string;
  errorCode?: string;
  errorMessage?: string;
}

export interface ResendGuestInviteEmailResultPayload {
  requestId: string;
  success?: boolean;
  invitationId?: string;
  errorCode?: string;
  errorMessage?: string;
}

export interface RevokeCurrentSessionResultPayload {
  requestId: string;
  success?: boolean;
  sessionId?: string;
  errorCode?: string;
  errorMessage?: string;
}

export interface RevokeAllSessionsResultPayload {
  requestId: string;
  success?: boolean;
  revokedCount?: number;
  errorCode?: string;
  errorMessage?: string;
}

export interface CreateWorkerTokenResultPayload {
  requestId: string;
  success?: boolean;
  plainToken?: string; // shown once; never persisted server-side
  identityId?: string;
  ownerUserId?: string;
  errorCode?: string;
  errorMessage?: string;
}

export interface RevokeWorkerTokenResultPayload {
  requestId: string;
  success?: boolean;
  errorCode?: string;
  errorMessage?: string;
}

export interface CreateBadgeResultPayload {
  requestId: string;
  success?: boolean;
  identityId?: string;
  ownerUserId?: string;
  errorCode?: string;
  errorMessage?: string;
}

export interface RevokeBadgeResultPayload {
  requestId: string;
  success?: boolean;
  errorCode?: string;
  errorMessage?: string;
}

// Deploy console reply (memql#3311). The response oneof has three
// arms, not nine, because DeployControlService returns only three
// types -- seven of its RPCs share ActionResult. `rpc` echoes the
// method name so a consumer multiplexing several calls through one
// handler can tell them apart without tracking requestIds itself.
//
// Failures do NOT appear here: they arrive as an ordinary queryError
// carrying the gRPC status code verbatim ("PermissionDenied",
// "InvalidArgument", "Unimplemented" on a node that does not host the
// deploy service).

export interface ComponentDigestWire {
  name?: string;
  digest?: string;
  repo?: string;
}

export interface ArgoStatusWire {
  syncStatus?: string;
  healthStatus?: string;
  lastSyncRevision?: string;
  lastSyncAt?: string;
  outOfSync?: boolean;
}

export interface RolloutStatusWire {
  name?: string;
  kind?: string; // "bluegreen" | "canary"
  phase?: string;
  activeColor?: string;
  previewColor?: string;
  canaryWeight?: number;
  currentStep?: number;
  latestAnalysisResult?: string;
}

export interface GateLegWire {
  name?: string;
  passed?: boolean;
  detail?: string;
}

export interface GateResultWire {
  result?: string; // "pass" | "fail" | "unknown"
  legs?: GateLegWire[];
  ranAt?: string;
}

export interface DeploymentStatusWire {
  env?: string;
  version?: string;
  engineVersion?: string;
  validatedAt?: string;
  gate?: string;
  components?: ComponentDigestWire[];
  argocd?: ArgoStatusWire;
  rollouts?: RolloutStatusWire[];
  gateResult?: GateResultWire;
}

export interface SuggestNextVersionResultWire {
  currentVersion?: string;
  nextMajor?: string;
  nextMinor?: string;
  nextPatch?: string;
  source?: string; // "deployment" | "overlay" | "none"
}

// ActionResult is the uniform return for every write RPC. auditEventId
// is the id of the v1:identity:auditEvent the server wrote for this
// action -- returned on the streamed path exactly as on the unary one.
export interface DeployActionResultWire {
  ok?: boolean;
  message?: string;
  auditEventId?: string;
  correlationId?: string;
  details?: Record<string, string>;
}

export interface DeployControlResultPayload {
  requestId: string;
  rpc?: string;
  deploymentStatus?: DeploymentStatusWire;
  nextVersion?: SuggestNextVersionResultWire;
  action?: DeployActionResultWire;
}

// expiresAt is int64 unix seconds -- protojson encodes int64 as
// either string or number depending on the runtime. We accept both.
export interface PolyphonRoomTokenResultPayload {
  requestId: string;
  token?: string;
  roomName?: string;
  livekitUrl?: string;
  expiresAt?: string | number;
}

// MCP tool reply envelopes + the inbound ClientToolCall the server
// pushes for client-execution tools.

export interface ToolDefinitionWire {
  name?: string;
  description?: string;
  inputSchema?: string; // JSON Schema (string)
  clientExecution?: boolean;
  scopes?: string[];
}

export interface ListToolsResultPayload {
  requestId: string;
  tools?: ToolDefinitionWire[];
  nextCursor?: string;
}

export interface CallToolResultPayload {
  requestId: string;
  content?: ToolResultContentWire[];
  isError?: boolean;
}

// ClientToolCall is INBOUND on the server message envelope. Carries
// a per-call callId the consumer must echo back on its
// ClientToolResult. timeoutMs is the budget the server is willing
// to wait; the consumer should reply with isError=true rather than
// timing out silently so the agent reasoning trace stays coherent.
export interface ClientToolCallPayload {
  callId: string;
  turnId?: string;
  agentId?: string;
  toolName: string;
  argumentsJson?: string;
  timeoutMs?: number;
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

export type GraphNodeActionWire =
  | "GRAPH_NODE_ACTION_UNSPECIFIED"
  | "GRAPH_NODE_ACTION_CREATED"
  | "GRAPH_NODE_ACTION_UPDATED"
  | "GRAPH_NODE_ACTION_DELETED";

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
  | { aiChatResult: AiChatResultPayload }
  | { aiSpeechResult: AiSpeechResultPayload }
  | { aiTranscribeResult: AiTranscribeResultPayload }
  | { aiSuggestResult: AiSuggestResultPayload }
  | { aiChunk: AiStreamChunkPayload }
  | { aiTranscribeStreamDelta: AiTranscribeStreamDeltaPayload }
  | { aiTranscribeStreamComplete: AiTranscribeStreamCompletePayload }
  | { sendGuestInviteResult: SendGuestInviteResultPayload }
  | { resolveGuestInviteResult: ResolveGuestInviteResultPayload }
  | { joinSpaceAsGuestResult: JoinSpaceAsGuestResultPayload }
  | { cancelGuestInviteResult: CancelGuestInviteResultPayload }
  | { resendGuestInviteEmailResult: ResendGuestInviteEmailResultPayload }
  | { revokeCurrentSessionResult: RevokeCurrentSessionResultPayload }
  | { revokeAllSessionsResult: RevokeAllSessionsResultPayload }
  | { createWorkerTokenResult: CreateWorkerTokenResultPayload }
  | { revokeWorkerTokenResult: RevokeWorkerTokenResultPayload }
  | { createBadgeResult: CreateBadgeResultPayload }
  | { revokeBadgeResult: RevokeBadgeResultPayload }
  | { deployControlResult: DeployControlResultPayload }
  | { polyphonRoomTokenResult: PolyphonRoomTokenResultPayload }
  | { listToolsResult: ListToolsResultPayload }
  | { callToolResult: CallToolResultPayload }
  | { clientToolCall: ClientToolCallPayload };

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
  | { kind: "aiChatResult"; value: AiChatResultPayload }
  | { kind: "aiSpeechResult"; value: AiSpeechResultPayload }
  | { kind: "aiTranscribeResult"; value: AiTranscribeResultPayload }
  | { kind: "aiSuggestResult"; value: AiSuggestResultPayload }
  | { kind: "aiChunk"; value: AiStreamChunkPayload }
  | { kind: "aiTranscribeStreamDelta"; value: AiTranscribeStreamDeltaPayload }
  | { kind: "aiTranscribeStreamComplete"; value: AiTranscribeStreamCompletePayload }
  | { kind: "sendGuestInviteResult"; value: SendGuestInviteResultPayload }
  | { kind: "resolveGuestInviteResult"; value: ResolveGuestInviteResultPayload }
  | { kind: "joinSpaceAsGuestResult"; value: JoinSpaceAsGuestResultPayload }
  | { kind: "cancelGuestInviteResult"; value: CancelGuestInviteResultPayload }
  | { kind: "resendGuestInviteEmailResult"; value: ResendGuestInviteEmailResultPayload }
  | { kind: "revokeCurrentSessionResult"; value: RevokeCurrentSessionResultPayload }
  | { kind: "revokeAllSessionsResult"; value: RevokeAllSessionsResultPayload }
  | { kind: "createWorkerTokenResult"; value: CreateWorkerTokenResultPayload }
  | { kind: "revokeWorkerTokenResult"; value: RevokeWorkerTokenResultPayload }
  | { kind: "createBadgeResult"; value: CreateBadgeResultPayload }
  | { kind: "revokeBadgeResult"; value: RevokeBadgeResultPayload }
  | { kind: "deployControlResult"; value: DeployControlResultPayload }
  | { kind: "polyphonRoomTokenResult"; value: PolyphonRoomTokenResultPayload }
  | { kind: "listToolsResult"; value: ListToolsResultPayload }
  | { kind: "callToolResult"; value: CallToolResultPayload }
  | { kind: "clientToolCall"; value: ClientToolCallPayload }
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
  if (m.aiChatResult)
    return { kind: "aiChatResult", value: m.aiChatResult as AiChatResultPayload };
  if (m.aiSpeechResult)
    return { kind: "aiSpeechResult", value: m.aiSpeechResult as AiSpeechResultPayload };
  if (m.aiTranscribeResult)
    return { kind: "aiTranscribeResult", value: m.aiTranscribeResult as AiTranscribeResultPayload };
  if (m.aiSuggestResult)
    return { kind: "aiSuggestResult", value: m.aiSuggestResult as AiSuggestResultPayload };
  if (m.aiChunk)
    return { kind: "aiChunk", value: m.aiChunk as AiStreamChunkPayload };
  if (m.aiTranscribeStreamDelta)
    return {
      kind: "aiTranscribeStreamDelta",
      value: m.aiTranscribeStreamDelta as AiTranscribeStreamDeltaPayload,
    };
  if (m.aiTranscribeStreamComplete)
    return {
      kind: "aiTranscribeStreamComplete",
      value: m.aiTranscribeStreamComplete as AiTranscribeStreamCompletePayload,
    };
  if (m.sendGuestInviteResult)
    return { kind: "sendGuestInviteResult", value: m.sendGuestInviteResult as SendGuestInviteResultPayload };
  if (m.resolveGuestInviteResult)
    return { kind: "resolveGuestInviteResult", value: m.resolveGuestInviteResult as ResolveGuestInviteResultPayload };
  if (m.joinSpaceAsGuestResult)
    return { kind: "joinSpaceAsGuestResult", value: m.joinSpaceAsGuestResult as JoinSpaceAsGuestResultPayload };
  if (m.cancelGuestInviteResult)
    return { kind: "cancelGuestInviteResult", value: m.cancelGuestInviteResult as CancelGuestInviteResultPayload };
  if (m.resendGuestInviteEmailResult)
    return {
      kind: "resendGuestInviteEmailResult",
      value: m.resendGuestInviteEmailResult as ResendGuestInviteEmailResultPayload,
    };
  if (m.revokeCurrentSessionResult)
    return {
      kind: "revokeCurrentSessionResult",
      value: m.revokeCurrentSessionResult as RevokeCurrentSessionResultPayload,
    };
  if (m.revokeAllSessionsResult)
    return { kind: "revokeAllSessionsResult", value: m.revokeAllSessionsResult as RevokeAllSessionsResultPayload };
  if (m.createWorkerTokenResult)
    return { kind: "createWorkerTokenResult", value: m.createWorkerTokenResult as CreateWorkerTokenResultPayload };
  if (m.revokeWorkerTokenResult)
    return { kind: "revokeWorkerTokenResult", value: m.revokeWorkerTokenResult as RevokeWorkerTokenResultPayload };
  if (m.createBadgeResult)
    return { kind: "createBadgeResult", value: m.createBadgeResult as CreateBadgeResultPayload };
  if (m.revokeBadgeResult)
    return { kind: "revokeBadgeResult", value: m.revokeBadgeResult as RevokeBadgeResultPayload };
  if (m.deployControlResult)
    return { kind: "deployControlResult", value: m.deployControlResult as DeployControlResultPayload };
  if (m.polyphonRoomTokenResult)
    return {
      kind: "polyphonRoomTokenResult",
      value: m.polyphonRoomTokenResult as PolyphonRoomTokenResultPayload,
    };
  if (m.listToolsResult)
    return { kind: "listToolsResult", value: m.listToolsResult as ListToolsResultPayload };
  if (m.callToolResult)
    return { kind: "callToolResult", value: m.callToolResult as CallToolResultPayload };
  if (m.clientToolCall)
    return { kind: "clientToolCall", value: m.clientToolCall as ClientToolCallPayload };
  return null;
}

// streamRequestId returns the per-session request_id carried by a
// streaming-protocol server message (mirrors sdk/go's streamRequestId
// in dispatcher.go). Empty when the message does not belong to a
// known streaming family.
//
// AiChatResult appears here so the terminal frame in a streaming chat
// session (aiChatStream, which uses dispatcher.send without
// registering in `pending`) routes to the per-requestId stream
// listener. The non-streaming aiChat path uses sendAndWait, which
// resolves on correlateTo before streamRequestId is consulted.
export function streamRequestId(msg: ServerMessage): string {
  const m = msg as unknown as Record<string, { requestId?: string } | undefined>;
  if (m.aiTranscribeStreamDelta?.requestId) return m.aiTranscribeStreamDelta.requestId;
  if (m.aiTranscribeStreamComplete?.requestId) return m.aiTranscribeStreamComplete.requestId;
  if (m.aiChunk?.requestId) return m.aiChunk.requestId;
  if (m.aiChatResult?.requestId) return m.aiChatResult.requestId;
  return "";
}
