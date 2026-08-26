// SDK-owned types -- the public surface never returns the wire shapes
// directly (see sdk/ts/README.md "Naming + grammar" and sdk/go/CLAUDE.md
// rule #2). These mirror sdk/go/client/types.go.

import type {
  ConceptInfoWire,
  ConceptsRegistryDeltaPayload,
  EventPayload,
  GraphBundleWire,
  GraphNodeActionWire,
  MyAccessResultPayload,
  QueryResultPayload,
  ResultMetaWire,
  SubscriptionKindWire,
  UserRoleWire,
} from "./wire.js";

// `developer` is engineering power (authoring + inline DSL + deploy /
// cut-version) WITHOUT user management. It sits in the privileged tier
// alongside admin rather than above or below it -- the two hold different
// powers, so the spectrum is not a strict ordering. Mirrors
// component/auth/rbac.go's AllRoles() and memql.proto's UserRole.
//
// "" is not a role. It is "no role resolved" -- an unauthenticated caller, a
// failed access read, or (until memql#3331) a role the wire union could not
// name. Consumers must treat it as "unknown", never as "least privileged".
export type Role = "" | "owner" | "admin" | "developer" | "writer" | "reader";

export interface AccessSummary {
  requestId: string;
  userId: string;
  primaryEmail: string;
  clusterRole: Role;
  // sessionId names the v1:identity:authSession row backing THIS connection,
  // read by the server off the verified token (memql#4306).
  //
  // It is what lets a sessions list mark "this device" without decoding the
  // JWT -- which clients must not do: a client that parses its own bearer
  // starts making decisions from claims the server never promised it. Empty
  // for a credential with no session behind it (a PAT, an operator key, a
  // service account), which is not an error; there is simply no row to name.
  sessionId: string;
  // displayName is the person's name off their v1:identity:user row
  // (memql#4317), resolved server-side by the same read that produces
  // primaryEmail.
  //
  // It is here rather than decoded from the JWT for the reason sessionId is:
  // a client that parses its own bearer starts making decisions from claims
  // the server never promised it. It is also the fresher of the two -- a
  // claim is what was true when the token was minted, this is what the row
  // says now.
  //
  // Empty when no user row resolved (a first request racing the registration
  // insert, a PAT with no provisioned user). Render the email instead; a
  // caller holds that already.
  displayName: string;
}

// DisplayCard carries the per-concept rendering hints declared via
// the `@displayCard(...)` DSL annotation. Concept-agnostic clients
// project rows through these slot names instead of carrying
// per-concept rendering code. Undefined when the concept didn't
// declare the annotation. See memql#160.
export interface DisplayCard {
  // primary is the payload field that names the row (e.g. "name"
  // for agents, "title" for spaces). Always non-empty when
  // DisplayCard is present; the loader rejects annotations missing
  // the primary slot.
  primary: string;
  // secondary is contextual (role, kind). Optional.
  secondary?: string;
  // tertiary is extra context (owner, parent space). Optional.
  tertiary?: string;
  // status is a boolean or short enum that drives a colored badge.
  // Optional.
  status?: string;
}

export interface Concept {
  id: string;
  version: string;
  domain: string;
  entity: string;
  description: string;
  type: string;
  // displayCard is the per-concept rendering hint set. Undefined
  // when the concept's DSL declaration didn't carry
  // `@displayCard(...)`. See memql#160.
  displayCard?: DisplayCard;
  // dataState is MemQL's relationship to this concept's data (epic
  // memql#4378). Three values, no fourth.
  //
  // "mirror" is the one that changes what a caller may DO: an external
  // system owns the data, and the engine refuses every write that does
  // not come from that system's connector. Rendering an editor over a
  // mirror concept offers an action the server will refuse -- read this
  // before offering one.
  //
  // "" only when talking to a server that predates the field, which is
  // also why the three are OPTIONAL rather than required: the same
  // shape has to describe a descriptor that arrived without them.
  dataState?: DataState | "";
  // dataOrigin names the system where changes to this concept are made.
  // Never "" on a server that carries the field: a concept that declared
  // nothing reports "memql", so no client re-derives the default.
  //
  // NOT a construct's `origin`, which is where its source file lives.
  dataOrigin?: string;
  // dataMirroredTo names the external systems MemQL pushes this
  // concept's changes out to. Empty unless dataState is "origin".
  dataMirroredTo?: string[];
  // The concept's DECLARED SHAPE (epic memql#4661).
  //
  // EMPTY IS A REAL ANSWER AND IT IS NOT "no fields". It means this
  // server does not publish a shape -- either it predates the fields or
  // the concept's definition schema did not parse into properties -- and
  // a client seeing it should fall back to profiling the rows it loaded,
  // which is what every client did before these arrived. Treating empty
  // as "the concept has no fields" renders a real concept as blank.
  fields?: ConceptField[];
  relationships?: ConceptRelationship[];
}

// ConceptField is one declared field, in the DSL's own vocabulary.
export interface ConceptField {
  name: string;
  kind: ConceptFieldKind | "";
  required: boolean;
  // The declared members, for kind === "enum". Empty otherwise.
  enumValues: string[];
  // The field's `///` doc comment. Empty when it has none.
  description: string;
}

// ConceptFieldKind is the authoring vocabulary, not the JSON-Schema one:
// the engine maps `{"type":"string","format":"date-time"}` back to
// "datetime" and `{"type":"string","enum":[...]}` back to "enum" before
// it reaches a client, so this is the word the DSL author actually
// wrote. A client must DEGRADE on an unrecognised value (render it as
// text) rather than drop the field -- the set can grow.
export type ConceptFieldKind =
  | "string"
  | "boolean"
  | "integer"
  | "number"
  | "datetime"
  | "enum"
  | "array"
  | "object";

// ConceptRelationship is one declared edge, carrying BOTH axes
// (memql#3652). They are not interchangeable:
//
//   type -- the closed ENGINE set; what traversal and id canonicalization
//           do with the edge.
//   as   -- the open DOMAIN label; what the edge MEANS to a person.
//
// `as` is empty on every declaration predating the split. A client
// labelling an edge falls back to `field` -- never to `type`, which
// would present "references" to a person as though it were a noun
// somebody chose.
export interface ConceptRelationship {
  type: string;
  as: string;
  // May be a dotted path when the pointer sits in a nested block.
  field: string;
  target: string;
  direction: string;
}

// DataState is the closed set of relationships MemQL can have to a
// concept's data. A client must DEGRADE on an unrecognised value rather
// than reject it -- but there is deliberately no fourth state to add:
// "shared", two systems both authoring one domain, is the option the
// model rejects.
export type DataState = "mirror" | "origin" | "native";

export type SubscriptionKind =
  | "telemetry"
  | "message"
  | "query_spec"
  | "ai_stream"
  | "graph_events"
  | "domain_events"
  | "automation_events"
  | "all";

export interface Event {
  subscriptionId: string;
  kind: string;
  timestamp: Date | null;
  payload: Record<string, unknown> | null;
  // payloadOmitted marks an ID-ONLY notification: `payload` carries the
  // row's identity ({concept, id, createdAt} plus the topic/eventKind that
  // say which action fired) and NOT the row (memql#4309).
  //
  // It happens when the row's concept declares the `granted` row-authz
  // tier, whose predicate is a relationship spec: deciding it needs a join
  // the fan-out cannot perform against one row. RE-READ the row through
  // the normal authorized read path and use what that returns; if the read
  // refuses, you were not entitled to the row and the event should be
  // dropped.
  //
  // Treating an id-only event as a full one yields a row whose fields are
  // all undefined, so a consumer that ignores this flag degrades to
  // rendering blanks rather than to leaking anything. Always false on an
  // ordinary event.
  payloadOmitted: boolean;
  // seq is this notification's position in the CONNECTION's delivery
  // sequence, from 1 (memql#4536). It numbers every notification the server
  // writes on the stream, so a delivery whose seq is not the previous one
  // plus one means something never arrived.
  //
  // 0 against a server that predates the field, which a consumer must read
  // as "this connection carries no sequence" rather than as "the first
  // event" -- LiveCollection treats a zero seq as unnumbered and leans on
  // gapBefore alone.
  //
  // The counter is per STREAM: a reconnect starts a new one at 1, so a
  // consumer treats stream establishment as an implicit gap rather than
  // comparing sequences across connections.
  seq: number;
  // gapBefore is true when one or more deliveries were DROPPED between the
  // previous notification on this stream and this one -- the engine's
  // per-stream event channel overflowed (memql#4536).
  //
  // The correct response is to RE-SEED: re-run the read that produced the
  // current rows and fold subsequent events onto the fresh answer. Ignoring
  // it leaves a folded list permanently diverged from the store with nothing
  // on screen to say so, which is the failure this flag exists to end.
  gapBefore: boolean;
}

// GraphAction is a CDC verb a structured graph subscription filters on
// (memql#2460). The server composes the bus topic from the concept +
// actions, so the client never writes a topic string.
export type GraphAction = "created" | "updated" | "deleted";

// One domain's CDC subscription filters, from the engine's catalog
// (ConceptsSubscribeMsg). Filters are the node.<action>.<concept> form the
// backend prefixes per subscription kind -- hand one to
// SubscriptionManager rather than composing topic strings by hand.
export interface DomainSubscription {
  domain: string;
  filters: string[];
}

// ConceptRegistryDelta is one registry-change notification on a follow-mode
// concept subscription (memql#4238). `added` carries whole descriptors (upsert
// by id); `removed` carries ids that left; `reset` marks the initial snapshot
// (replace the whole registry with `added`); `generation` is the monotonic
// registry version after this delta -- a client that sees a non-contiguous
// generation has missed one and re-subscribes.
export interface ConceptRegistryDelta {
  generation: number;
  added: Concept[];
  removed: string[];
  reset: boolean;
}

// Row is the shape-flattened form every query / mutation returns. Keys
// depend on the construct's bound shape; field-access helpers below
// pluck typed values so consumers don't have to type-switch the map.
export type Row = Record<string, unknown>;

export function rowString(row: Row | null, key: string): string {
  const v = row?.[key];
  return typeof v === "string" ? v : "";
}

export function rowBool(row: Row | null, key: string): boolean {
  const v = row?.[key];
  return typeof v === "boolean" ? v : false;
}

export function rowNumber(row: Row | null, key: string): number {
  const v = row?.[key];
  return typeof v === "number" ? v : 0;
}

export function rowObject(row: Row | null, key: string): Record<string, unknown> | null {
  const v = row?.[key];
  if (v && typeof v === "object" && !Array.isArray(v)) return v as Record<string, unknown>;
  return null;
}

export function rowArray(row: Row | null, key: string): unknown[] | null {
  const v = row?.[key];
  return Array.isArray(v) ? v : null;
}

// Result wraps a typed engine response. Generated query / mutation /
// logic methods return Result. Consumers iterate via .rows() rather
// than reaching into the raw envelope. Mirrors sdk/go's Result.
export class Result {
  constructor(private readonly payload: QueryResultPayload["result"] | null) {}

  rows(): Row[] {
    const p = this.payload;
    if (!p) return [];
    if (Array.isArray(p.data)) {
      const out: Row[] = [];
      for (const item of p.data) {
        if (item && typeof item === "object" && !Array.isArray(item)) {
          out.push(item as Row);
        }
      }
      return out;
    }
    const bundle = p.bundle;
    if (bundle?.nodes) {
      return bundle.nodes.map(flattenNode);
    }
    return [];
  }

  single(): Row | null {
    const rs = this.rows();
    return rs.length > 0 ? (rs[0] ?? null) : null;
  }

  // rawNodes returns the bundle's nodes with their full nested shape
  // preserved (payload / metadata / intrinsics distinct). Use this in
  // admin / concept-browser surfaces where flattening would drop the
  // type / schema / createdBy intrinsics. Empty when the result is
  // not a bundle envelope.
  rawNodes(): Row[] {
    const nodes = this.payload?.bundle?.nodes ?? [];
    return nodes.map((n) => ({ ...n }));
  }

  meta(): ResultMetaWire | null {
    return this.payload?.meta ?? null;
  }

  // raw exposes the protojson-decoded payload for debugging / adapters.
  // Prefer rows() / single() in normal use.
  raw(): QueryResultPayload["result"] | null {
    return this.payload;
  }
}

function flattenNode(node: import("./wire.js").MemoryNodeWire): Row {
  const out: Row = {};
  if (node.id != null) out.id = node.id;
  if (node.concept != null) out.concept = node.concept;
  if (node.createdAt != null) out.createdAt = node.createdAt;
  if (node.payload) {
    for (const [k, v] of Object.entries(node.payload)) out[k] = v;
  }
  return out;
}

// Internal converters -- not exported through the package barrel.

const subscriptionKindToWire: Record<SubscriptionKind, SubscriptionKindWire> = {
  telemetry: "SUBSCRIPTION_KIND_TELEMETRY",
  message: "SUBSCRIPTION_KIND_MESSAGE",
  query_spec: "SUBSCRIPTION_KIND_QUERY_SPEC",
  ai_stream: "SUBSCRIPTION_KIND_AI_STREAM",
  graph_events: "SUBSCRIPTION_KIND_GRAPH_EVENTS",
  domain_events: "SUBSCRIPTION_KIND_DOMAIN_EVENTS",
  automation_events: "SUBSCRIPTION_KIND_AUTOMATION_EVENTS",
  all: "SUBSCRIPTION_KIND_ALL",
};

export function subscriptionKindWire(k: SubscriptionKind): SubscriptionKindWire {
  return subscriptionKindToWire[k] ?? "SUBSCRIPTION_KIND_UNSPECIFIED";
}

const graphActionToWire: Record<GraphAction, GraphNodeActionWire> = {
  created: "GRAPH_NODE_ACTION_CREATED",
  updated: "GRAPH_NODE_ACTION_UPDATED",
  deleted: "GRAPH_NODE_ACTION_DELETED",
};

export function graphActionWire(a: GraphAction): GraphNodeActionWire {
  return graphActionToWire[a] ?? "GRAPH_NODE_ACTION_UNSPECIFIED";
}

// Typed `Record<UserRoleWire, Role>` deliberately: the compiler then REQUIRES
// an entry for every member of the union, so widening UserRoleWire without
// mapping the new value is a build error rather than another silent "".
// That half of memql#3331 is structural; the half that is not -- the proto
// declaring a role the union never listed -- is covered by
// scripts/ci/user_role_wire_parity_test.go.
const userRoleFromWire: Record<UserRoleWire, Role> = {
  USER_ROLE_UNSPECIFIED: "",
  USER_ROLE_OWNER: "owner",
  USER_ROLE_ADMIN: "admin",
  USER_ROLE_WRITER: "writer",
  USER_ROLE_READER: "reader",
  USER_ROLE_DEVELOPER: "developer",
};

export function roleFromWire(r: UserRoleWire | null | undefined): Role {
  if (!r) return "";
  return userRoleFromWire[r] ?? "";
}

export function conceptsFromWire(in_: ConceptInfoWire[] | undefined): Concept[] {
  if (!in_) return [];
  return in_.map((c) => {
    const concept: Concept = {
      id: c.id ?? "",
      version: c.version ?? "",
      domain: c.domain ?? "",
      entity: c.entity ?? "",
      description: c.description ?? "",
      type: c.type ?? "",
      dataState: (c.dataState ?? "") as DataState | "",
      dataOrigin: c.dataOrigin ?? "",
      dataMirroredTo: c.dataMirroredTo ?? [],
    };
    // memql#160: surface the per-concept rendering hints when the
    // concept declared @displayCard(...).
    if (c.displayCard) {
      concept.displayCard = {
        primary: c.displayCard.primary ?? "",
        secondary: c.displayCard.secondary,
        tertiary: c.displayCard.tertiary,
        status: c.displayCard.status,
      };
    }
    // The declared shape (epic memql#4661). Set only when the server
    // sent something: `undefined` and `[]` mean different things to a
    // client deciding whether to fall back to row sampling, and
    // defaulting to `[]` here would erase the distinction at the one
    // place it is still visible.
    if (c.fields && c.fields.length > 0) {
      concept.fields = c.fields.map((f) => ({
        name: f.name ?? "",
        kind: (f.kind ?? "") as ConceptFieldKind | "",
        required: f.required === true,
        enumValues: f.enumValues ?? [],
        description: f.description ?? "",
      }));
    }
    if (c.relationships && c.relationships.length > 0) {
      concept.relationships = c.relationships.map((r) => ({
        type: r.type ?? "",
        as: r.as ?? "",
        field: r.field ?? "",
        target: r.target ?? "",
        direction: r.direction ?? "",
      }));
    }
    return concept;
  });
}

// conceptRegistryDeltaFromWire decodes a ConceptsRegistryDelta into the
// SDK-owned shape (memql#4238). `generation` is a uint64 -> protojson string, so
// it is parsed with Number(); a missing/NaN value coalesces to 0.
export function conceptRegistryDeltaFromWire(
  p: ConceptsRegistryDeltaPayload,
): ConceptRegistryDelta {
  const generation = Number(p.generation ?? 0);
  return {
    generation: Number.isFinite(generation) ? generation : 0,
    added: conceptsFromWire(p.added),
    removed: (p.removed ?? []).filter((r): r is string => typeof r === "string"),
    reset: p.reset === true,
  };
}

export function accessSummaryFromWire(p: MyAccessResultPayload | undefined): AccessSummary | null {
  if (!p) return null;
  return {
    requestId: p.requestId ?? "",
    userId: p.userId ?? "",
    primaryEmail: p.primaryEmail ?? "",
    clusterRole: roleFromWire(p.clusterRole),
    sessionId: p.sessionId ?? "",
    displayName: p.displayName ?? "",
  };
}

export function eventFromWire(ev: EventPayload): Event {
  let timestamp: Date | null = null;
  if (ev.ts) {
    const parsed = new Date(ev.ts);
    if (!Number.isNaN(parsed.getTime())) timestamp = parsed;
  }
  return {
    subscriptionId: ev.subscriptionId,
    kind: stripEventKindPrefix(ev.kind),
    timestamp,
    payload:
      ev.payload && typeof ev.payload === "object" && !Array.isArray(ev.payload)
        ? (ev.payload as Record<string, unknown>)
        : null,
    // Normalised to a real boolean: protojson omits a false bool, so the
    // wire field is absent on every ordinary event and a consumer reading
    // `ev.payloadOmitted` directly would be reading undefined.
    payloadOmitted: ev.payloadOmitted === true,
    // uint64 -> protojson string, exactly like ConceptRegistryDelta's
    // generation. Number() over a decimal string is exact well past any
    // plausible per-connection event count.
    seq: decodeSeq(ev.seq),
    gapBefore: ev.gapBefore === true,
  };
}

// decodeSeq normalises the wire's uint64 -- a protojson string, a number from
// a bridge that emits one, or absent from a server predating the field -- to a
// positive integer, or 0 for "unnumbered". Collapsing every unusable form onto
// the SAME value an older server produces leaves a consumer one case to handle
// rather than three.
function decodeSeq(raw: string | number | undefined): number {
  if (raw == null) return 0;
  const n = typeof raw === "number" ? raw : Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
}

function stripEventKindPrefix(kind: string | undefined): string {
  if (!kind) return "";
  const prefix = "EVENT_KIND_";
  return kind.startsWith(prefix) ? kind.slice(prefix.length) : kind;
}

// Internal: used by Result's constructor + dispatcher to lift the
// QueryResult payload into a typed wrapper.
export function resultFromQueryPayload(p: QueryResultPayload | null): Result {
  return new Result(p?.result ?? null);
}

// re-exports for the generated_*.ts files
export type { GraphBundleWire, ResultMetaWire };
