// SDK-owned types -- the public surface never returns the wire shapes
// directly (see sdk/ts/README.md "Naming + grammar" and sdk/go/CLAUDE.md
// rule #2). These mirror sdk/go/client/types.go.

import type {
  ConceptInfoWire,
  EventPayload,
  GraphBundleWire,
  GraphNodeActionWire,
  MyAccessResultPayload,
  QueryResultPayload,
  ResultMetaWire,
  SubscriptionKindWire,
  UserRoleWire,
} from "./wire.js";

export type Role = "" | "owner" | "admin" | "writer" | "reader";

export interface AccessSummary {
  requestId: string;
  userId: string;
  primaryEmail: string;
  clusterRole: Role;
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
}

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
}

// GraphAction is a CDC verb a structured graph subscription filters on
// (memql#2460). The server composes the bus topic from the concept +
// actions, so the client never writes a topic string.
export type GraphAction = "created" | "updated" | "deleted";

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

const userRoleFromWire: Record<UserRoleWire, Role> = {
  USER_ROLE_UNSPECIFIED: "",
  USER_ROLE_OWNER: "owner",
  USER_ROLE_ADMIN: "admin",
  USER_ROLE_WRITER: "writer",
  USER_ROLE_READER: "reader",
};

export function roleFromWire(r: UserRoleWire | undefined): Role {
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
    return concept;
  });
}

export function accessSummaryFromWire(p: MyAccessResultPayload | undefined): AccessSummary | null {
  if (!p) return null;
  return {
    requestId: p.requestId ?? "",
    userId: p.userId ?? "",
    primaryEmail: p.primaryEmail ?? "",
    clusterRole: roleFromWire(p.clusterRole),
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
  };
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
