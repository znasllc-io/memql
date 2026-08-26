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

// The id names one of the CALLER'S OWN sessions. The server resolves it
// against the caller's own list before writing, so an id from anywhere else
// is refused rather than acted on (memql#4319).
export interface RevokeSessionPayload {
  requestId: string;
  sessionId: string;
}

// No user id: the account written is the caller's own, and the absence of a
// target is the authorization (memql#4319).
export interface SetSignInPolicyPayload {
  requestId: string;
  policy: string;
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

// Account tokens (memql#3322): a credential an operator mints against a
// managed customer account. Same custody rule as worker tokens -- the
// plaintext exists in exactly one place, the mint reply.
export interface CreateAccountTokenPayload {
  requestId: string;
  accountId: string;
  label: string;
  expiresAt?: string; // ISO8601, empty = no auto-expiry
}

export interface RevokeAccountTokenPayload {
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

// Deploy control (memql#3311). DeployControlService is a separate
// UNARY gRPC service on the same listener, which a browser cannot dial
// -- so the whole deploy surface rides ONE bridged envelope on
// MemqlService.Stream instead. Exactly one request field is set; the
// inner shapes are the DeployControlService request messages verbatim
// (component/grpc/deploy_control.proto), reused rather than
// re-declared so the streamed and unary surfaces cannot drift.

// The two reads take no argument at all: every RPC on this surface operates
// on THIS installation since epic memql#3943, and their only argument was the
// environment.
export type DeployControlEmptyPayload = Record<string, never>;

export interface DeployControlRollbackPayload {
  commitSha: string;
}

export interface DeployControlRolloutActionPayload {
  rollout: string;
  // "promote" | "abort" -- the ARGO ROLLOUT verb (advance or cancel an
  // in-flight progressive rollout), unrelated to any environment promote.
  action: string;
}

export interface DeployControlCutVersionPayload {
  bump?: string; // "major" | "minor" | "patch"; empty defaults to patch
  version?: string; // explicit override; when set, bump is ignored
}

export interface DeployControlDeployPayload {
  deploymentId: string;
}

export interface DeployControlRollbackDeploymentPayload {
  toDeploymentId: string;
}

export type DeployControlRequestPayload =
  | { getDeploymentStatus: DeployControlEmptyPayload }
  | { suggestNextVersion: DeployControlEmptyPayload }
  | { rollback: DeployControlRollbackPayload }
  | { rolloutAction: DeployControlRolloutActionPayload }
  | { cutVersion: DeployControlCutVersionPayload }
  | { deploy: DeployControlDeployPayload }
  | { rollbackDeployment: DeployControlRollbackDeploymentPayload }
  // Repair (memql#4209) takes no argument either: it operates on THIS
  // installation and names no version (a repair that installs a different
  // version is an upgrade wearing a repair's name).
  | { repair: DeployControlEmptyPayload };

export type DeployControlPayload = { requestId: string } & DeployControlRequestPayload;

// Identity administration (memql#3324). The writes the server-rendered
// /admin/* console owned, bridged onto the stream as ONE envelope so the
// owner/admin gate and the audit write live in one Go implementation
// (component/identity/adminops) rather than one per operation. Exactly one
// request field is set.
//
// These are WRITES WITH A ROLE FLOOR, and the floor is not here: every one
// is refused server-side for a caller below owner or admin, with an
// `admin_auth_forbidden` audit event whose id comes back on the reply.

export interface IdentityAdminUserProfilePayload {
  userId: string;
  displayName?: string;
  firstName?: string;
  lastName?: string;
  phone?: string;
  primaryRole?: string;
  gender?: string;
  birthdate?: string;
}

export interface IdentityAdminSetRolePayload {
  userId: string;
  role: string; // owner | admin | developer | writer | reader
}

export interface IdentityAdminSetSuspendedPayload {
  userId: string;
  suspended: boolean;
  reason?: string;
}

export interface IdentityAdminRevokeTokenPayload {
  identityId: string;
}

// Mint a single-use enrolment link for another person (memql#3408) -- the
// credential that lets them register their FIRST passkey with no mailbox in
// the loop. The minted link comes back on IdentityAdminResultPayload
// .enrolmentUrl and is shown once.
/**
 * Mint a user-targeted invitation (memql#4270).
 *
 * `role` is the cluster role the recipient lands with; empty takes the
 * cluster's default. An inviter cannot grant above their own role -- the server
 * refuses, it is not a client-side check.
 *
 * `ttlSeconds` of 0 takes the server's 7-day default; a larger value is clamped
 * down to the ceiling rather than refused.
 */
export interface IdentityAdminIssueUserInvitationPayload {
  email: string;
  role?: string;
  ttlSeconds?: number;
}

/** Revoke an unaccepted invitation (memql#4270). A SOFT cancel: the row stays. */
export interface IdentityAdminRevokeUserInvitationPayload {
  invitationId: string;
}

export interface IdentityAdminIssueEnrolmentLinkPayload {
  userId: string;
  // 0 = the server's 15-minute default. Values above the server ceiling are
  // clamped down rather than refused.
  ttlSeconds?: number;
}

// Kill an unused enrolment link before its TTL expires. Distinct from
// consumption all the way down: the /enroll page tells a revoked link apart
// from a spent one, because they call for different next steps.
export interface IdentityAdminRevokeEnrolmentLinkPayload {
  enrolmentTokenId: string;
}

export interface IdentityAdminClusterSettingsPayload {
  brandName?: string;
  brandPrimaryColor?: string;
  brandLogoDataUri?: string;
  brandIconDataUri?: string;
  registrationMode: string;
  registrationDomains?: string;
  internalDomains?: string;
  internalDefaultRole: string;
  registeredClientsJson?: string;
  accessRequestNotifyEmails?: string;
  // Seconds (days for the invitation one). 0 = fall back to the boot default.
  accessTokenTtlSeconds?: number;
  refreshTokenTtlSeconds?: number;
  magicLinkTtlSeconds?: number;
  invitationTtlDays?: number;
  refreshCookieSameSite?: string; // "lax" | "none" | ""
}

export type IdentityAdminRequestPayload =
  | { updateUserProfile: IdentityAdminUserProfilePayload }
  | { setUserRole: IdentityAdminSetRolePayload }
  | { setUserSuspended: IdentityAdminSetSuspendedPayload }
  | { revokeUserToken: IdentityAdminRevokeTokenPayload }
  | { revokeNodeToken: IdentityAdminRevokeTokenPayload }
  | { updateClusterSettings: IdentityAdminClusterSettingsPayload }
  | { issueEnrolmentLink: IdentityAdminIssueEnrolmentLinkPayload }
  | { revokeEnrolmentLink: IdentityAdminRevokeEnrolmentLinkPayload }
  | { rotateRecoveryKey: IdentityAdminRotateRecoveryKeyPayload }
  | { issueUserInvitation: IdentityAdminIssueUserInvitationPayload }
  | { revokeUserInvitation: IdentityAdminRevokeUserInvitationPayload };

/**
 * Rotate the cluster's owner recovery key (memql#3970).
 *
 * `userId` may be omitted when the cluster has exactly one owner; with several
 * it is required, because picking one would hand the caller a credential for
 * an account they did not name.
 */
export interface IdentityAdminRotateRecoveryKeyPayload {
  userId?: string;
}

export type IdentityAdminPayload = { requestId: string } & IdentityAdminRequestPayload;

// -----------------------------------------------------------------------------
// Automation run (memql#3310)
// -----------------------------------------------------------------------------

// RunAutomationMsg -- synthesize a trigger event for a named automation,
// dispatch it through the normal automation path, and stream back a step
// trace. Mirrors MemqlClientMessage oneof slot 102.
export interface RunAutomationPayload {
  requestId: string;
  automation: string;
  // The synthesized trigger event payload (google.protobuf.Struct -> plain
  // object). Omit for a @trigger(schedule=...) automation, which fires now
  // with an empty event.
  payload?: Record<string, unknown>;
  concept?: string;
  topic?: string;
  targetNodeType?: string;
  timeoutMs?: number;
  includeStepOutput?: boolean;
}

// Authoring -- validate + session-define a .memql bundle (memql#2128 / C1,
// consumed by the VS Code run loop in memql#3309).
//
// `sources` is a bundle STRING -- one or more constructs concatenated -- and
// the engine slices it into (kind, name, source) constructs itself. There is
// no per-file structure on the wire, which is why a consumer that assembles a
// bundle from several editor buffers has to keep its own file/line offset
// table to map diagnostics back (see AuthoringDiagnosticWire.line).

export interface AuthoringValidateBundlePayload {
  requestId: string;
  sources: string;
  /** Tree-relative bundle path; supplies the ambient domain (memql#3800). */
  origin?: string;
}

export interface AuthoringSessionDefineBundlePayload {
  requestId: string;
  sources: string;
  /** Tree-relative bundle path; supplies the ambient domain (memql#3800). */
  origin?: string;
}

// The two DURABLE authoring messages (memql#3760). Same `sources` bundle
// string as the pair above and a wildly larger effect: a promote persists the
// constructs, registers them into the SHARED registry and broadcasts them to
// every node; a demote withdraws them the same way. Both are OWNER-only, a
// stricter gate than the owner-or-developer bar validate/session-define use.
//
// The engine refuses a non-owner with a `queryError` carrying the role, so the
// refusal arrives on the same correlateTo as a normal reply would.

export interface DurablePromoteBundlePayload {
  requestId: string;
  sources: string;
  // allowBreaking is the operator's explicit override for a CONCEPT re-promote
  // whose schema change would strand rows already written -- a removed field, a
  // changed field type, a new required field, a narrowed enum (memql#3757).
  // Unset is the normal call and the engine refuses such a change with the
  // classified diff on the reply.
  //
  // OMITTED entirely rather than sent as `false` on a normal promote: proto3
  // reads absent and false identically, so the wire meaning is the same, and
  // keeping the field off the envelope means the dangerous flag appears in a
  // frame only when a caller asked for it.
  allowBreaking?: boolean;
  /** Tree-relative bundle path; supplies the ambient domain (memql#3800). */
  origin?: string;
}

export interface DurableDemoteBundlePayload {
  requestId: string;
  sources: string;
}

// The DURABLE, OWNER-SCOPED middle tier (epic memql#3928). Same `sources`
// bundle string again, and the effect sits precisely between the two above:
// the constructs are persisted and replayed at boot like a promote, and
// registered into an owner-scoped registry instead of the shared one, so they
// are callable by their author and by nobody else.
//
// OWNER-OR-DEVELOPER, matching validate/session-define rather than the
// owner-only durable pair -- staging registers nothing shared and broadcasts
// nothing, so its blast radius is a database row rather than a change to what
// the cluster runs.
//
// THERE IS NO `trainBundle`. Training a staged construct is
// `durablePromoteBundle` over the same source: the engine sees the construct is
// staged for this owner and flips the same persisted row rather than writing a
// second one. A separate message would have forced a caller to know which tier
// a construct was in before it could pick a verb.
//
// No `allowBreaking`, and its absence is structural rather than an omission: a
// bundle declaring a CONCEPT is refused outright, so there is never a schema
// classification here to override.
export interface StageBundlePayload {
  requestId: string;
  sources: string;
  /** Tree-relative bundle path; supplies the ambient domain (memql#3800). */
  origin?: string;
}

// ListConstructs asks a cluster what constructs it has actually LOADED, at
// registry grain (memql#3749). No filters: the surface that consumes it groups
// client-side, and a filter parameter would put the kind vocabulary in a
// second place.
export interface ListConstructsPayload {
  requestId: string;
}

// Pack browser (memql#2127 / B1): read-only enumeration of the embedded,
// plugin-registered and MEMQL_DSL_PATH .memql trees. Three single-reply
// exchanges -- the Go routing ledger (sdk/go/client/dispatcher_stream_routing_test.go)
// already classifies the three results as single-reply, so nothing here joins
// streamRequestId.
export interface ListPackDomainsPayload {
  requestId: string;
}

export interface ListPackFilesPayload {
  requestId: string;
  domain: string;
}

export interface ReadPackFilePayload {
  requestId: string;
  domain: string;
  path: string;
}

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
  | { conceptsSubscribe: ConceptsSubscribePayload }
  | { myAccess: MyAccessPayload }
  | { modulesList: ModulesListPayload }
  | { moduleDetail: ModuleDetailPayload }
  | { setPackEnabled: SetPackEnabledPayload }
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
  | { revokeSession: RevokeSessionPayload }
  | { setSignInPolicy: SetSignInPolicyPayload }
  | { createWorkerToken: CreateWorkerTokenPayload }
  | { createAccountToken: CreateAccountTokenPayload }
  | { revokeAccountToken: RevokeAccountTokenPayload }
  | { revokeWorkerToken: RevokeWorkerTokenPayload }
  | { createBadge: CreateBadgePayload }
  | { revokeBadge: RevokeBadgePayload }
  | { polyphonRoomToken: PolyphonRoomTokenPayload }
  | { deployControl: DeployControlPayload }
  | { identityAdmin: IdentityAdminPayload }
  | { runAutomation: RunAutomationPayload }
  | { authoringValidateBundle: AuthoringValidateBundlePayload }
  | { authoringSessionDefineBundle: AuthoringSessionDefineBundlePayload }
  | { durablePromoteBundle: DurablePromoteBundlePayload }
  | { durableDemoteBundle: DurableDemoteBundlePayload }
  | { stageBundle: StageBundlePayload }
  | { listConstructs: ListConstructsPayload }
  | { listPackDomains: ListPackDomainsPayload }
  | { listPackFiles: ListPackFilesPayload }
  | { readPackFile: ReadPackFilePayload }
  | { listTools: ListToolsPayload }
  | { callTool: CallToolPayload }
  | { clientToolResult: ClientToolResultPayload };

// Server-side payloads. Untyped `any`-shaped fields appear where the
// engine returns `google.protobuf.Struct` -- those decode to plain
// objects in protojson and the SDK exposes them as `unknown` so
// consumers go through typed accessors instead of reaching in blind.

export interface ServerHelloPayload {
  nodeId?: string;
  // The WIRE PROTOCOL version the node speaks ("v1"), not its release.
  version?: string;
  // The release the node's binary was cut from -- e.g. "v0.18.1" -- or
  // "dev+<12 hex>" when it was not cut from a release (memql#3998,
  // memql#4575). Absent when the node predates the field, which says the
  // cluster is older than this contract rather than that it has no version.
  engineVersion?: string;
  // The git revision the node's binary was built from, abbreviated to 12 hex
  // characters and suffixed "-dirty" when built from a modified tree
  // (memql#4575). Absent or empty when it cannot be established -- render it
  // as unknown rather than passing it on.
  engineCommit?: string;
}

export interface QueryResultPayload {
  requestId: string;
  result?: {
    bundle?: GraphBundleWire | null;
    data?: unknown[];
    meta?: ResultMetaWire | null;
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
  // payloadOmitted marks an ID-ONLY notification (memql#4309). The row's
  // concept declares the `granted` row-authz tier, which cannot be decided
  // against a single row in isolation, so the engine sends the identity
  // and nothing else. Absent (rather than false) on every ordinary event,
  // because protojson omits a false bool.
  payloadOmitted?: boolean;
  // seq numbers every notification on THIS stream, from 1 (memql#4536).
  // A uint64 on the wire, so protojson renders it as a STRING; the bridge
  // marshals with EmitUnpopulated, so a current server sends "0" rather
  // than omitting it. Decoded in types.ts.
  seq?: string | number;
  // gapBefore marks that deliveries for this stream were dropped between
  // the previous notification and this one. Absent on an ordinary event.
  gapBefore?: boolean;
}

export interface ConceptsListResultPayload {
  requestId?: string;
  concepts?: ConceptInfoWire[];
  baseTopics?: string[];
  systemTopics?: string[];
}

// ConceptsSubscribeMsg has two modes. follow=false (the default) is the
// one-shot CDC-filter CATALOG: the engine answers ONE reply grouping
// node.created.<concept> filters by domain (component/grpc/concepts_handlers.go),
// which a client uses to discover the per-domain filters it hands to
// SubscriptionManager. follow=true (memql#4238) is the registry-DELTA stream: a
// snapshot then live add/remove deltas, so a client's concept registry stays
// live without a reconnect. See QueryClient.subscribeConceptRegistry.
export interface ConceptsSubscribePayload {
  requestId?: string;
  // Optional: restrict the catalog to these domains (follow=false only).
  domains?: string[];
  // follow=true switches to the registry-delta stream (memql#4238).
  follow?: boolean;
}

export interface DomainSubscriptionWire {
  domain?: string;
  filters?: string[];
}

export interface ConceptsSubscribeResultPayload {
  requestId?: string;
  domains?: DomainSubscriptionWire[];
}

// ConceptsRegistryDelta is one message on a follow-mode ConceptsSubscribeMsg
// stream (memql#4238). The first (reset=true) carries the whole concept set in
// `added`; every later one is incremental. `generation` is a uint64, which
// protojson encodes as a STRING.
export interface ConceptsRegistryDeltaPayload {
  requestId?: string;
  generation?: string;
  added?: ConceptInfoWire[];
  removed?: string[];
  reset?: boolean;
  subscriptionId?: string;
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
  displayCard?: DisplayCardWire | null;
  // The data-origins declaration (epic memql#4378). The `data` prefix
  // separates these from a CONSTRUCT's `origin`, which answers a
  // different question -- where the source file lives.
  dataState?: string;
  dataOrigin?: string;
  dataMirroredTo?: string[];
  // The concept's declared SHAPE (epic memql#4661). Absent on a server
  // that predates the fields -- which is why a client falls back to
  // sampling rows rather than treating absence as "no fields".
  fields?: ConceptFieldWire[];
  relationships?: ConceptRelationshipWire[];
}

export interface ConceptFieldWire {
  name?: string;
  // The AUTHORING kind -- string / boolean / integer / number /
  // datetime / enum / array / object -- not the JSON-Schema type the
  // engine stores it as. The mapping happens server-side, once.
  kind?: string;
  required?: boolean;
  enumValues?: string[];
  description?: string;
}

export interface ConceptRelationshipWire {
  // The closed ENGINE set (parent / owns / references / ...).
  type?: string;
  // The open DOMAIN label (respondsAs, assignedTo). Empty on every
  // declaration that predates memql#3652 -- do not fall back to `type`,
  // which is a different axis; fall back to `field`.
  as?: string;
  field?: string;
  target?: string;
  direction?: string;
}

export interface MyAccessResultPayload {
  requestId?: string;
  userId?: string;
  primaryEmail?: string;
  clusterRole?: UserRoleWire | null;
  sessionId?: string;
  displayName?: string;
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
  message?: AiChatMessageWire | null;
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

export interface RevokeSessionResultPayload {
  requestId: string;
  success?: boolean;
  sessionId?: string;
  wasCurrent?: boolean;
  errorCode?: string;
  errorMessage?: string;
}

export interface SetSignInPolicyResultPayload {
  requestId: string;
  success?: boolean;
  policy?: string;
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

export interface CreateAccountTokenResultPayload {
  requestId?: string;
  success?: boolean;
  // "mql_acct_<43 base64url>". Present only on the mint reply, only once.
  plainToken?: string;
  identityId?: string;
  accountId?: string;
  // The credential's authenticated SUBJECT: the operator user, never the
  // account -- nothing authenticates as an account.
  subjectUserId?: string;
  auditEventId?: string;
  errorCode?: string;
  errorMessage?: string;
}

export interface RevokeAccountTokenResultPayload {
  requestId?: string;
  success?: boolean;
  auditEventId?: string;
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

// Deploy-control reply (memql#3311). A unary DeployControlService call
// fails by returning a gRPC status error; a multiplexed stream has no
// per-message status channel, so the status is carried IN the envelope.
// errorCode is the canonical gRPC code (0 = OK, 7 = PERMISSION_DENIED,
// ...) and protojson OMITS it when zero, so absent means OK.

export interface DeployComponentDigestWire {
  name?: string;
  digest?: string;
  repo?: string;
}

export interface DeployArgoStatusWire {
  syncStatus?: string;
  healthStatus?: string;
  lastSyncRevision?: string;
  lastSyncAt?: string;
  outOfSync?: boolean;
}

export interface DeployRolloutStatusWire {
  name?: string;
  kind?: string; // "bluegreen" | "canary"
  phase?: string;
  activeColor?: string;
  previewColor?: string;
  canaryWeight?: number;
  currentStep?: number;
  latestAnalysisResult?: string;
}

export interface DeployGateLegWire {
  name?: string;
  passed?: boolean;
  detail?: string;
}

export interface DeployGateResultWire {
  result?: string; // "pass" | "fail" | "unknown"
  legs?: DeployGateLegWire[];
  ranAt?: string;
}

export interface DeploymentStatusWire {
  env?: string;
  version?: string;
  engineVersion?: string;
  validatedAt?: string;
  gate?: string;
  components?: DeployComponentDigestWire[];
  argocd?: DeployArgoStatusWire | null;
  rollouts?: DeployRolloutStatusWire[];
  gateResult?: DeployGateResultWire | null;
}

export interface SuggestNextVersionResultWire {
  currentVersion?: string;
  nextMajor?: string;
  nextMinor?: string;
  nextPatch?: string;
  source?: string; // "deployment" | "overlay" | "none"
}

export interface DeployActionResultWire {
  ok?: boolean;
  message?: string;
  auditEventId?: string;
  correlationId?: string;
  details?: Record<string, string>;
}

export interface DeployControlResultPayload {
  requestId?: string;
  // ok is true when the RPC returned without a status error. It is NOT
  // the action's own success -- a write that ran and failed is ok here
  // with action.ok false, exactly as the unary path behaves.
  ok?: boolean;
  errorCode?: number;
  errorMessage?: string;
  // Present on a GATE refusal (PERMISSION_DENIED / UNAUTHENTICATED) --
  // memql#3334. The refusal is itself an audited event and an operator
  // arguing about a denial needs its id, exactly as on IdentityAdminResult
  // below. Absent everywhere else, including on an INVALID_ARGUMENT (which
  // runs before the gate and writes no event) and on a transport failure.
  // The permitted path keeps its id on action.auditEventId.
  auditEventId?: string;
  deploymentStatus?: DeploymentStatusWire | null;
  nextVersion?: SuggestNextVersionResultWire | null;
  action?: DeployActionResultWire | null;
}

// Identity-administration reply (memql#3324). Same envelope-carried status as
// the deploy bridge above, and for the same reason: a multiplexed stream has
// no per-message status channel. protojson omits zero values, so an absent
// errorCode is 0 (OK).
//
// auditEventId is present on a REFUSAL too -- the refusal is itself an audited
// event, and an operator arguing about a denial needs its id.
export interface IdentityAdminResultPayload {
  requestId?: string;
  ok?: boolean;
  errorCode?: number;
  errorMessage?: string;
  auditEventId?: string;
  message?: string;
  // Set by issueEnrolmentLink ONLY (memql#3408) -- the one field on this
  // reply that carries a credential, because the link IS that call's product
  // and no later request can fetch it. Empty on every other operation and on
  // every refusal.
  enrolmentUrl?: string;
  // Set by issueUserInvitation ONLY (memql#4270) -- the THIRD credential-
  // bearing field, for the same reason as enrolmentUrl. Empty on every other
  // operation and on every refusal.
  invitationUrl?: string;
  // What the cluster's registration policy MEANT for that call: one of "open",
  // "domain_restricted", "invite_only", "waitlist". Not a credential -- it is
  // here so a console can say what the link is FOR ("this cluster allows open
  // sign-up, so the link is a convenience") without re-reading cluster
  // settings and racing them.
  registrationMode?: string;
  // Whether the invitation email actually left the server (memql#4584). NOT
  // redundant with the call's own success: `ok` says the invitation was
  // ISSUED -- the row exists and invitationUrl admits somebody -- while this
  // says whether the recipient was TOLD. A delivery fault deliberately does
  // not fail the issue, because the link is what admits and it is returned
  // exactly once.
  invitationEmailSent?: boolean;
  // Why delivery failed; empty when it did not fail (memql#4584).
  //
  // Read the two together. `invitationEmailSent === false` with an EMPTY error
  // means no send was attempted -- the node has no mail wired, a configuration
  // statement. False WITH an error means one was tried and failed -- an
  // incident. A console that collapses them sends an operator to the wrong
  // place.
  invitationEmailError?: string;
  // Set by rotateRecoveryKey ONLY (memql#3970) -- the SECOND field on this
  // reply that carries a credential, for the same reason as enrolmentUrl
  // above. Empty on every other operation.
  //
  // One case where it arrives on a NON-ok reply: the replacement was minted
  // and revealed but retiring a predecessor failed. Withholding it there would
  // tell the caller nothing happened when something did.
  recoveryKey?: string;
}

// AutomationRunEvent -- one frame of a run's streamed trace. Mirrors
// MemqlServerMessage oneof slot 124.
//
// Exactly one of accepted / step / complete is set. Every run opens with
// accepted and closes with exactly one complete, INCLUDING a run that was
// refused outright -- so a caller can always park on complete.
export interface AutomationRunEventPayload {
  requestId?: string;
  runId?: string;
  accepted?: AutomationRunAcceptedWire | null;
  step?: AutomationRunStepWire | null;
  complete?: AutomationRunCompleteWire | null;
}

export interface AutomationRunAcceptedWire {
  automation?: string;
  // Always true. It is a FIELD rather than a comment because the UI has to
  // state that the deployed definition ran, not the caller's buffer:
  // session-define does not cover automations.
  ranDeployedDefinition?: boolean;
  definitionNote?: string;
  triggerKind?: string;
  triggerTopic?: string;
  requestedOnNodeId?: string;
  requestedOnNodeType?: string;
  targetNodeType?: string;
}

export interface AutomationRunStepWire {
  sequence?: number;
  stepId?: string;
  status?: string;
  durationMs?: number | string;
  output?: Record<string, unknown>;
  error?: string;
}

export interface AutomationRunCompleteWire {
  status?: string;
  durationMs?: number | string;
  stepCount?: number;
  error?: string;
  errorCode?: number;
  errorMessage?: string;
  executedOnNodeId?: string;
  executedOnNodeType?: string;
}

// Authoring reply envelopes (memql#2128 / C1).
//
// AuthoringDiagnosticWire's four position fields are 1-based and expressed in
// BUNDLE-FILE coordinates -- the source the caller submitted, before the
// sandbox slices and lowers it. All four are ZERO when the engine could not
// compute a reliable position, and the engine deliberately emits no position
// rather than a wrong one (memql#2375). A consumer MUST therefore read 0 as
// "no position" and never as line 0 / column 0, or every positionless
// diagnostic lands on the first line of the first file in the bundle.
//
// `skipped` constructs (a kind this pass does not compile) report ok=false
// AND skipped=true, and do NOT fail the bundle -- so "is this a failure" is
// `!ok && !skipped`, never `!ok`.
export interface AuthoringDiagnosticWire {
  name?: string;
  kind?: string;
  ok?: boolean;
  skipped?: boolean;
  error?: string;
  line?: number;
  column?: number;
  endLine?: number;
  endColumn?: number;
}

export interface AuthoringConstructWire {
  kind?: string;
  name?: string;
}

export interface AuthoringValidateBundleResultPayload {
  requestId?: string;
  ok?: boolean;
  diagnostics?: AuthoringDiagnosticWire[];
}

export interface AuthoringSessionDefineBundleResultPayload {
  requestId?: string;
  ok?: boolean;
  defined?: AuthoringConstructWire[];
  diagnostics?: AuthoringDiagnosticWire[];
  error?: string;
}

// Durable promote / demote reply envelopes (memql#3760).
//
// Both carry a SECOND repeated field beside the identity list, and in both
// cases that second field is the one a client renders. `promoted` / `demoted`
// only say which constructs the call touched; `conceptDiffs` / `outcomes` say
// what happened to them, which for a concept is not implied by ok=true.

// ConceptSchemaChangeWire is ONE classified difference between the concept
// version the cluster is running and the one being promoted over it
// (memql#3757).
//
// rowsAffected is int64 -- protojson encodes int64 as a STRING on most
// runtimes and a number on others, so both are accepted here and coerced once
// on the way out. It is meaningful ONLY when rowCountKnown is true: a node with
// no database cannot count rows, and reporting its zero as a count would be a
// claim it is not entitled to make.
export interface ConceptSchemaChangeWire {
  concept?: string;
  field?: string;
  kind?: string;
  breaking?: boolean;
  was?: string;
  now?: string;
  rowsAffected?: string | number;
  rowCountKnown?: boolean;
  referencedBy?: string[];
  detail?: string;
}

export interface ConceptSchemaDiffWire {
  concept?: string;
  breaking?: boolean;
  overridden?: boolean;
  changes?: ConceptSchemaChangeWire[];
  summary?: string;
}

export interface DurablePromoteBundleResultPayload {
  requestId?: string;
  ok?: boolean;
  promoted?: AuthoringConstructWire[];
  diagnostics?: AuthoringDiagnosticWire[];
  error?: string;
  // conceptDiffs rides the REFUSAL reply as well as the success one, because a
  // refusal IS a diff. Absent for a first promote and for an unchanged
  // re-promote.
  conceptDiffs?: ConceptSchemaDiffWire[];
}

// DurableDemoteOutcomeWire is what actually happened to one construct.
// `outcome` is "retired" (still registered, name still claimed, existing rows
// readable, new writes refused) or "removed" (gone, name claimable again).
// rowCount is int64 -- same string-or-number encoding as rowsAffected above.
export interface DurableDemoteOutcomeWire {
  kind?: string;
  name?: string;
  conceptId?: string;
  outcome?: string;
  rowCount?: string | number;
}

export interface DurableDemoteBundleResultPayload {
  requestId?: string;
  ok?: boolean;
  demoted?: AuthoringConstructWire[];
  diagnostics?: AuthoringDiagnosticWire[];
  error?: string;
  // outcomes carries the same constructs as `demoted`, in the same order.
  outcomes?: DurableDemoteOutcomeWire[];
}

// The staged-tier reply (epic memql#3928). `staged` names the constructs now
// callable BY THEIR AUTHOR -- deliberately a different field name from
// `promoted`, so a caller reading a reply cannot mistake one tier for the other
// by destructuring the field it expected.
export interface StageBundleResultPayload {
  requestId?: string;
  ok?: boolean;
  staged?: AuthoringConstructWire[];
  diagnostics?: AuthoringDiagnosticWire[];
  error?: string;
}

// ConstructArgWire is the catalog's argument shape. Field for field the
// language server's `RunnableArg` (cmd/memql-lsp/runnable.go), because the
// extension generates ONE argument form from both -- from the LSP when the
// .memql file is open, from the catalog when browsing a cluster where it is
// not. `enumValues` is the one rename, and only because `enum` is a reserved
// word in the proto language. TestRunnableArgMatchesConstructArg pins the two.
export interface ConstructArgWire {
  name?: string;
  /** string | number | boolean | object | array | any -- a closed set. */
  type?: string;
  required?: boolean;
  enumValues?: string[];
  description?: string;
  autoInjected?: boolean;
}

// ConstructTriggerWire is an automation's trigger, field for field the language
// server's `RunnableTrigger` (cmd/memql-lsp/runnable.go) -- no renames, since
// none of the three collides with a proto reserved word.
//
// ABSENT for every kind except automation. An empty object would read as "a
// trigger that fires on nothing" rather than as "not applicable", and would
// ship on every construct in the catalog.
//
// The server DECOMPOSES the composed subscription topic to fill it: an author
// writes `@trigger(event="node.created", concept=cog.participant)` and the
// loader folds the pair into one topic, so `event` here is the structured kind
// and `concept` the id. A topic that does not decompose (a raw application
// topic) arrives whole in `event` with no `concept`.
// TestRunnableTriggerMatchesConstructTrigger pins the two shapes.
export interface ConstructTriggerWire {
  concept?: string;
  event?: string;
  schedule?: string;
}

// ConstructInfoWire is one construct the cluster has loaded.
//
// Three fields carry rules a consumer will get wrong by guessing, all three
// documented on the plain type in ../constructs/constructs.ts: `kind` is the
// kind and NOT the authored keyword (so `runnable` is the answer, not a
// re-derivation from it), `origin` is server-derived, and `source` is present
// only when there is no file to read it from.
export interface ConstructInfoWire {
  name?: string;
  kind?: string;
  namespace?: string;
  origin?: string;
  originPath?: string;
  description?: string;
  runnable?: boolean;
  args?: ConstructArgWire[];
  boundConcept?: string;
  sourceHash?: string;
  source?: string;
  /** Automation only; absent for every other kind. */
  trigger?: ConstructTriggerWire | null;
}

export interface ListConstructsResultPayload {
  requestId?: string;
  constructs?: ConstructInfoWire[];
}

export interface PackDomainWire {
  name?: string;
  origin?: string;
  fileCount?: number;
}

export interface PackFileWire {
  path?: string;
  // int64 on the wire: protojson may encode it as a string.
  size?: string | number;
}

export interface ListPackDomainsResultPayload {
  requestId?: string;
  domains?: PackDomainWire[];
}

export interface ListPackFilesResultPayload {
  requestId?: string;
  domain?: string;
  files?: PackFileWire[];
}

export interface ReadPackFileResultPayload {
  requestId?: string;
  domain?: string;
  path?: string;
  source?: string;
  origin?: string;
  found?: boolean;
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

// Every value `UserRole` declares in component/grpc/memql.proto, in proto
// order. This file is the hand-mirrored TS view of the wire (see the header),
// so an enum value added there does NOT appear here on its own -- and the
// omission is silent: roleFromWire's `?? ""` turns an unlisted role into an
// indeterminate one rather than an error.
//
// USER_ROLE_DEVELOPER was missing for exactly that reason (memql#3331), which
// left the VS Code deploy panel unable to tell a developer from an unknown
// caller and unable to gate cut/deploy. scripts/ci/user_role_wire_parity_test.go
// now fails when the proto declares a role this union does not.
export type UserRoleWire =
  | "USER_ROLE_UNSPECIFIED"
  | "USER_ROLE_OWNER"
  | "USER_ROLE_ADMIN"
  | "USER_ROLE_WRITER"
  | "USER_ROLE_READER"
  | "USER_ROLE_DEVELOPER";

// ---------------------------------------------------------------------------
// Module registry (epic memql#4183). Reads are owner/admin-gated;
// setPackEnabled is owner-only. Every result carries errorCode/errorMessage
// INSIDE the payload (a handler error would tear down the multiplexed
// stream) plus the reporting-node facts, because per-node vs cluster-wide
// honesty is part of the contract. A secret env var carries set/unset and
// NOTHING else -- no value, no mask; there is no reveal call.
// ---------------------------------------------------------------------------

export interface ModulesListPayload {
  requestId?: string;
}

export interface ModuleInfoWire {
  kind?: string;
  name?: string;
  description?: string;
  state?: string;
  stateDetail?: string;
  scope?: string;
  envComponents?: string[];
  fqnPrefixes?: string[];
  codeReference?: string;
}

export interface ModulesListResultPayload {
  requestId?: string;
  modules?: ModuleInfoWire[];
  reportingNodeId?: string;
  reportingNodeType?: string;
  errorCode?: number;
  errorMessage?: string;
}

export interface ModuleDetailPayload {
  requestId?: string;
  kind?: string;
  name?: string;
}

export interface ModuleEnvVarWire {
  name?: string;
  description?: string;
  secret?: boolean;
  scope?: string;
  requiredFor?: string[];
  set?: boolean;
  value?: string;
  defaultValue?: string;
}

export interface ModuleDetailResultPayload {
  requestId?: string;
  module?: ModuleInfoWire | null;
  envVars?: ModuleEnvVarWire[];
  reportingNodeId?: string;
  reportingNodeType?: string;
  errorCode?: number;
  errorMessage?: string;
}

export interface SetPackEnabledPayload {
  requestId?: string;
  packDomain?: string;
  enabled?: boolean;
  reason?: string;
}

export interface SetPackEnabledResultPayload {
  requestId?: string;
  packDomain?: string;
  priorEnabled?: boolean;
  enabled?: boolean;
  restartRequired?: boolean;
  errorCode?: number;
  errorMessage?: string;
}

export type ServerMessage = MessageBase & ServerPayload;

type ServerPayload =
  | { serverHello: ServerHelloPayload }
  | { queryResult: QueryResultPayload }
  | { queryError: QueryErrorPayload }
  | { event: EventPayload }
  | { heartbeat: HeartbeatPayload }
  | { conceptsListResult: ConceptsListResultPayload }
  | { conceptsSubscribeResult: ConceptsSubscribeResultPayload }
  | { conceptsRegistryDelta: ConceptsRegistryDeltaPayload }
  | { myAccessResult: MyAccessResultPayload }
  | { modulesListResult: ModulesListResultPayload }
  | { moduleDetailResult: ModuleDetailResultPayload }
  | { setPackEnabledResult: SetPackEnabledResultPayload }
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
  | { revokeSessionResult: RevokeSessionResultPayload }
  | { setSignInPolicyResult: SetSignInPolicyResultPayload }
  | { createWorkerTokenResult: CreateWorkerTokenResultPayload }
  | { createAccountTokenResult: CreateAccountTokenResultPayload }
  | { revokeAccountTokenResult: RevokeAccountTokenResultPayload }
  | { revokeWorkerTokenResult: RevokeWorkerTokenResultPayload }
  | { createBadgeResult: CreateBadgeResultPayload }
  | { revokeBadgeResult: RevokeBadgeResultPayload }
  | { polyphonRoomTokenResult: PolyphonRoomTokenResultPayload }
  | { deployControlResult: DeployControlResultPayload }
  | { identityAdminResult: IdentityAdminResultPayload }
  | { automationRunEvent: AutomationRunEventPayload }
  | { authoringValidateBundleResult: AuthoringValidateBundleResultPayload }
  | { authoringSessionDefineBundleResult: AuthoringSessionDefineBundleResultPayload }
  | { durablePromoteBundleResult: DurablePromoteBundleResultPayload }
  | { durableDemoteBundleResult: DurableDemoteBundleResultPayload }
  | { stageBundleResult: StageBundleResultPayload }
  | { listConstructsResult: ListConstructsResultPayload }
  | { listPackDomainsResult: ListPackDomainsResultPayload }
  | { listPackFilesResult: ListPackFilesResultPayload }
  | { readPackFileResult: ReadPackFileResultPayload }
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
  | { kind: "conceptsSubscribeResult"; value: ConceptsSubscribeResultPayload }
  | { kind: "conceptsRegistryDelta"; value: ConceptsRegistryDeltaPayload }
  | { kind: "myAccessResult"; value: MyAccessResultPayload }
  | { kind: "modulesListResult"; value: ModulesListResultPayload }
  | { kind: "moduleDetailResult"; value: ModuleDetailResultPayload }
  | { kind: "setPackEnabledResult"; value: SetPackEnabledResultPayload }
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
  | { kind: "revokeSessionResult"; value: RevokeSessionResultPayload }
  | { kind: "setSignInPolicyResult"; value: SetSignInPolicyResultPayload }
  | { kind: "createWorkerTokenResult"; value: CreateWorkerTokenResultPayload }
  | { kind: "createAccountTokenResult"; value: CreateAccountTokenResultPayload }
  | { kind: "revokeAccountTokenResult"; value: RevokeAccountTokenResultPayload }
  | { kind: "revokeWorkerTokenResult"; value: RevokeWorkerTokenResultPayload }
  | { kind: "createBadgeResult"; value: CreateBadgeResultPayload }
  | { kind: "revokeBadgeResult"; value: RevokeBadgeResultPayload }
  | { kind: "polyphonRoomTokenResult"; value: PolyphonRoomTokenResultPayload }
  | { kind: "deployControlResult"; value: DeployControlResultPayload }
  | { kind: "identityAdminResult"; value: IdentityAdminResultPayload }
  | { kind: "automationRunEvent"; value: AutomationRunEventPayload }
  | { kind: "authoringValidateBundleResult"; value: AuthoringValidateBundleResultPayload }
  | { kind: "authoringSessionDefineBundleResult"; value: AuthoringSessionDefineBundleResultPayload }
  | { kind: "durablePromoteBundleResult"; value: DurablePromoteBundleResultPayload }
  | { kind: "durableDemoteBundleResult"; value: DurableDemoteBundleResultPayload }
  | { kind: "stageBundleResult"; value: StageBundleResultPayload }
  | { kind: "listConstructsResult"; value: ListConstructsResultPayload }
  | { kind: "listPackDomainsResult"; value: ListPackDomainsResultPayload }
  | { kind: "listPackFilesResult"; value: ListPackFilesResultPayload }
  | { kind: "readPackFileResult"; value: ReadPackFileResultPayload }
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
  if (m.conceptsSubscribeResult)
    return {
      kind: "conceptsSubscribeResult",
      value: m.conceptsSubscribeResult as ConceptsSubscribeResultPayload,
    };
  if (m.conceptsRegistryDelta)
    return {
      kind: "conceptsRegistryDelta",
      value: m.conceptsRegistryDelta as ConceptsRegistryDeltaPayload,
    };
  if (m.myAccessResult)
    return { kind: "myAccessResult", value: m.myAccessResult as MyAccessResultPayload };
  if (m.modulesListResult)
    return { kind: "modulesListResult", value: m.modulesListResult as ModulesListResultPayload };
  if (m.moduleDetailResult)
    return { kind: "moduleDetailResult", value: m.moduleDetailResult as ModuleDetailResultPayload };
  if (m.setPackEnabledResult)
    return { kind: "setPackEnabledResult", value: m.setPackEnabledResult as SetPackEnabledResultPayload };
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
  if (m.revokeSessionResult)
    return { kind: "revokeSessionResult", value: m.revokeSessionResult as RevokeSessionResultPayload };
  if (m.setSignInPolicyResult)
    return { kind: "setSignInPolicyResult", value: m.setSignInPolicyResult as SetSignInPolicyResultPayload };
  if (m.createWorkerTokenResult)
    return { kind: "createWorkerTokenResult", value: m.createWorkerTokenResult as CreateWorkerTokenResultPayload };
  if (m.createAccountTokenResult)
    return {
      kind: "createAccountTokenResult",
      value: m.createAccountTokenResult as CreateAccountTokenResultPayload,
    };
  if (m.revokeAccountTokenResult)
    return {
      kind: "revokeAccountTokenResult",
      value: m.revokeAccountTokenResult as RevokeAccountTokenResultPayload,
    };
  if (m.revokeWorkerTokenResult)
    return { kind: "revokeWorkerTokenResult", value: m.revokeWorkerTokenResult as RevokeWorkerTokenResultPayload };
  if (m.createBadgeResult)
    return { kind: "createBadgeResult", value: m.createBadgeResult as CreateBadgeResultPayload };
  if (m.revokeBadgeResult)
    return { kind: "revokeBadgeResult", value: m.revokeBadgeResult as RevokeBadgeResultPayload };
  if (m.polyphonRoomTokenResult)
    return {
      kind: "polyphonRoomTokenResult",
      value: m.polyphonRoomTokenResult as PolyphonRoomTokenResultPayload,
    };
  if (m.deployControlResult)
    return { kind: "deployControlResult", value: m.deployControlResult as DeployControlResultPayload };
  if (m.identityAdminResult)
    return { kind: "identityAdminResult", value: m.identityAdminResult as IdentityAdminResultPayload };
  if (m.automationRunEvent)
    return { kind: "automationRunEvent", value: m.automationRunEvent as AutomationRunEventPayload };
  if (m.authoringValidateBundleResult)
    return {
      kind: "authoringValidateBundleResult",
      value: m.authoringValidateBundleResult as AuthoringValidateBundleResultPayload,
    };
  if (m.authoringSessionDefineBundleResult)
    return {
      kind: "authoringSessionDefineBundleResult",
      value: m.authoringSessionDefineBundleResult as AuthoringSessionDefineBundleResultPayload,
    };
  if (m.durablePromoteBundleResult)
    return {
      kind: "durablePromoteBundleResult",
      value: m.durablePromoteBundleResult as DurablePromoteBundleResultPayload,
    };
  if (m.durableDemoteBundleResult)
    return {
      kind: "durableDemoteBundleResult",
      value: m.durableDemoteBundleResult as DurableDemoteBundleResultPayload,
    };
  if (m.stageBundleResult)
    return {
      kind: "stageBundleResult",
      value: m.stageBundleResult as StageBundleResultPayload,
    };
  if (m.listConstructsResult)
    return {
      kind: "listConstructsResult",
      value: m.listConstructsResult as ListConstructsResultPayload,
    };
  if (m.listPackDomainsResult)
    return {
      kind: "listPackDomainsResult",
      value: m.listPackDomainsResult as ListPackDomainsResultPayload,
    };
  if (m.listPackFilesResult)
    return {
      kind: "listPackFilesResult",
      value: m.listPackFilesResult as ListPackFilesResultPayload,
    };
  if (m.readPackFileResult)
    return {
      kind: "readPackFileResult",
      value: m.readPackFileResult as ReadPackFileResultPayload,
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
// request-scoped server message, or "" when the message is not part of an
// exchange a registerStream listener can be parked on.
//
// THE COVERAGE RULE, shared verbatim with sdk/go/client/dispatcher.go's
// streamRequestId (memql#3429). This table lists exactly the families whose
// frames a caller can be waiting for on registerStream -- every family
// participating in an exchange the correlateTo tier structurally CANNOT
// serve, because that tier resolves one reply and unregisters:
//
//   - families the engine emits MORE THAN ONCE for a single request: the
//     deltas and the terminal that closes them; and
//   - queryError, which can end any of those exchanges in place of its
//     normal terminal.
//
// NOT "every payload that carries a requestId". Nearly every result message
// does, and almost all of them are single replies that sendAndWait resolves
// on correlateTo before this function is consulted at all.
//
// The two SDKs must list the same families. They have diverged twice, and
// both directions were silent: the Go table was missing the streaming-chat
// pair this one has always had, and this table was missing queryError -- so
// the `payload?.kind === "queryError"` branches inside the registerStream
// listeners in automationRun.ts and ai/chat.ts, written precisely so a
// refused request would not leave the caller parked forever, could never
// actually fire. Both are fixed here.
//
// Go's dispatcher_stream_routing_test.go enumerates the proto's
// MemqlServerMessage payload oneof and fails on any family classified
// neither routed nor deliberately unrouted; that gate is the backstop for
// this file too, since the families are the same on both sides.
export function streamRequestId(msg: ServerMessage): string {
  const m = msg as unknown as Record<string, { requestId?: string } | undefined>;
  // Streaming transcription: delta repeats, complete terminates.
  if (m.aiTranscribeStreamDelta?.requestId) return m.aiTranscribeStreamDelta.requestId;
  if (m.aiTranscribeStreamComplete?.requestId) return m.aiTranscribeStreamComplete.requestId;
  // Streaming chat: N token deltas, then the terminal carrying the assembled
  // text. aiChatStream uses dispatcher.send without registering in `pending`,
  // so requestId routing is the only way these reach it; the non-streaming
  // aiChat path uses sendAndWait and resolves on correlateTo first.
  if (m.aiChunk?.requestId) return m.aiChunk.requestId;
  if (m.aiChatResult?.requestId) return m.aiChatResult.requestId;
  // Automation run (memql#3310). A run is many frames correlated by
  // request_id -- accepted, then one per step, then exactly one complete --
  // so it routes here rather than through the single-reply `pending` map.
  if (m.automationRunEvent?.requestId) return m.automationRunEvent.requestId;
  // Agent / voice-agent turn streaming: deltas then one complete. No browser
  // consumer today; listed so the tables stay identical and the next consumer
  // does not rediscover memql#3414 in this language too.
  if (m.agentGenerateTurnDelta?.requestId) return m.agentGenerateTurnDelta.requestId;
  if (m.agentGenerateTurnComplete?.requestId) return m.agentGenerateTurnComplete.requestId;
  if (m.voiceAgentTurnDelta?.requestId) return m.voiceAgentTurnDelta.requestId;
  if (m.voiceAgentTurnComplete?.requestId) return m.voiceAgentTurnComplete.requestId;
  // QueryResultChunk carries `done`: a chunked contract even though the engine
  // emits exactly one frame today. Costless for current callers, which use
  // sendAndWait and are served by correlateTo.
  if (m.queryResult?.requestId) return m.queryResult.requestId;
  // Concept-registry follow stream (memql#4238). One ConceptsSubscribeMsg with
  // follow=true yields a snapshot frame then one per registry change, all
  // carrying that request id. The follow=false catalog reply is a single reply
  // served by correlateTo in the tier above and is unaffected.
  if (m.conceptsRegistryDelta?.requestId) return m.conceptsRegistryDelta.requestId;
  // The error terminal for any of the above. See the coverage rule.
  if (m.queryError?.requestId) return m.queryError.requestId;
  return "";
}
