import { renderMemQLValue, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

// The wire vocabulary of the integrations surface (memql#3323).
//
// Everything this module builds is a NAMED CALL -- `query campaigns(...)`,
// `mutation createCampaign(...)`, `builtin integrationStatus(...)` -- dispatched
// through QueryClient.executeNamed, which is the same seam the concept browser
// and the generated SDKs ride. No new gRPC message was added for this surface
// and none is needed: the engine already routes a named call over
// MemqlService.Stream.
//
// WHY THE CALL STRINGS ARE HAND-BUILT HERE. `make sdk-gen` emits typed builders
// for Go only -- the Makefile passes an empty `--ts-out` -- so there is no
// generated TypeScript counterpart to import. The alternative to this module is
// call strings scattered through six components, which is worse in the one way
// that matters: argument quoting. Every value goes through the SDK's
// renderMemQLValue, which mirrors the engine's own literal grammar (and, on the
// Go side, the lexer-exact QuoteString), so a campaign name containing a quote
// or a newline cannot break the statement around it. That escaping decision
// lives in ONE file, here.
//
// If a TypeScript emitter lands later, this module is what it replaces.

// callArgs renders `k: v, k: v` from a record, dropping undefined and empty
// entries so an optional argument is OMITTED rather than sent as "".
//
// Omission is not a tidiness choice. A `when(args.x) { ... }` guard in a filter
// is dropped when its argument is ABSENT, and an empty string is present -- so
// sending `status: ""` to a query that guards on status filters for rows whose
// status is the empty string, which is nothing at all.
function callArgs(args: Record<string, unknown>): string {
  const parts: string[] = [];
  for (const key of Object.keys(args)) {
    const value = args[key];
    if (value === undefined || value === null) continue;
    if (typeof value === "string" && value === "") continue;
    parts.push(`${key}: ${renderMemQLValue(value)}`);
  }
  return parts.join(", ");
}

export function buildCall(
  kind: "query" | "mutation" | "builtin",
  name: string,
  args: Record<string, unknown> = {},
): string {
  return `${kind} ${name}(${callArgs(args)})`;
}

// runQuery / runMutation / runBuiltin are the three verbs this surface uses.
// Split by kind rather than folded into one because the kind keyword is part
// of the wire form the engine's parser dispatches on, and a caller that had to
// remember which keyword a construct needs would eventually get it wrong.

export async function runQuery(
  query: QueryClient,
  name: string,
  args: Record<string, unknown> = {},
): Promise<Row[]> {
  const result = await query.executeNamed(name, buildCall("query", name, args));
  return result.rows();
}

export async function runMutation(
  query: QueryClient,
  name: string,
  args: Record<string, unknown> = {},
): Promise<void> {
  await query.executeNamed(name, buildCall("mutation", name, args));
}

// ---------------------------------------------------------------------------
// The integration-status builtin's reply
// ---------------------------------------------------------------------------

export interface IntegrationSetting {
  name: string;
  value: string;
  source: string;
  envVar: string;
  purpose: string;
  editable: boolean;
}

export interface IntegrationCredential {
  name: string;
  present: boolean;
  source: string;
  envVar: string;
  purpose: string;
  rotate: string;
}

export interface IntegrationReport {
  name: string;
  registered: boolean;
  capabilities: string[];
  // "yes" | "no" | "unknown". Deliberately a tri-state: most integrations
  // publish no self-report and "we do not know" must not render as "no".
  configured: string;
  // "healthy" | "unhealthy" | "degraded" | "unknown".
  health: string;
  detail: string;
  mode: string;
  settings: IntegrationSetting[];
  credentials: IntegrationCredential[];
  probed: boolean;
}

export interface IntegrationStatus {
  checkedAt: string;
  probed: boolean;
  integrations: IntegrationReport[];
}

// readStatusPayload digs the report out of the engine's builtin envelope.
//
// A top-level `builtin X(...)` does NOT come back as a row set. The engine
// marshals the handler's returned node map into a single value, so the reply is
// one object keyed by node id, whose value is the node envelope, whose
// `payload` is what the handler actually wrote. Written as a SEARCH rather than
// as a fixed path (`rows[0].integrationStatus.payload`) because the node id is
// bare-ified server-side on the way out and a key this client hard-coded would
// be one rename away from silently reading undefined.
export function readStatusPayload(rows: readonly Row[]): IntegrationStatus | null {
  for (const row of rows) {
    const found = findStatus(row, 0);
    if (found) return found;
  }
  return null;
}

const MAX_ENVELOPE_DEPTH = 4;

function findStatus(value: unknown, depth: number): IntegrationStatus | null {
  if (depth > MAX_ENVELOPE_DEPTH) return null;
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  if (Array.isArray(record["integrations"])) {
    return {
      checkedAt: typeof record["checkedAt"] === "string" ? record["checkedAt"] : "",
      probed: record["probed"] === true,
      integrations: (record["integrations"] as unknown[]).map(toReport),
    };
  }
  for (const key of Object.keys(record)) {
    const found = findStatus(record[key], depth + 1);
    if (found) return found;
  }
  return null;
}

function toReport(raw: unknown): IntegrationReport {
  const r = (raw ?? {}) as Record<string, unknown>;
  return {
    name: str(r["name"]),
    registered: r["registered"] === true,
    capabilities: strList(r["capabilities"]),
    configured: str(r["configured"]) || "unknown",
    health: str(r["health"]) || "unknown",
    detail: str(r["detail"]),
    mode: str(r["mode"]),
    settings: list(r["settings"]).map((s) => ({
      name: str(s["name"]),
      value: str(s["value"]),
      source: str(s["source"]),
      envVar: str(s["envVar"]),
      purpose: str(s["purpose"]),
      editable: s["editable"] === true,
    })),
    credentials: list(r["credentials"]).map((c) => ({
      name: str(c["name"]),
      present: c["present"] === true,
      source: str(c["source"]),
      envVar: str(c["envVar"]),
      purpose: str(c["purpose"]),
      rotate: str(c["rotate"]),
    })),
    probed: r["probed"] === true,
  };
}

function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function strList(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((v): v is string => typeof v === "string");
}

function list(value: unknown): Record<string, unknown>[] {
  if (!Array.isArray(value)) return [];
  const out: Record<string, unknown>[] = [];
  for (const item of value) {
    if (item && typeof item === "object" && !Array.isArray(item)) {
      out.push(item as Record<string, unknown>);
    }
  }
  return out;
}
