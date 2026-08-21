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
// WHAT REMAINS HERE, AND WHY (memql#4232). The TypeScript emitter this
// module's earlier header anticipated has landed: `make sdk-gen` now emits
// typed builders + QueryClient methods for every query and mutation, and the
// call sites that used runQuery/runMutation compose those instead -- so both
// helpers are gone. What stays is the BUILTIN path: the generator emits only
// builtins marked @sdk (most builtins are internal), and the campaign send
// actions -- campaignStartSend / campaignPauseSend / campaignResumeSend /
// campaignScheduleSend -- plus integrationStatus are not yet marked. Until
// they are (a one-line dsl/ annotation each, tracked in the memql#4232
// adoption notes), this file keeps the one hand-built form they need, with
// the same quoting rule as ever: every value goes through the SDK's
// renderMemQLValue, so a name containing a quote or newline cannot break the
// statement around it.

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
  kind: "builtin",
  name: string,
  args: Record<string, unknown> = {},
): string {
  return `${kind} ${name}(${callArgs(args)})`;
}

// runBuiltin is the one verb left on this surface. It landed
// with campaign sending (memql#3348), where the operator actions are builtins
// rather than mutations -- starting a send is a preflight across several rows
// plus two writes, and neither is expressible in a mutation body.
//
// The reply is discarded here on purpose. A builtin answers with one synthetic
// node, and every caller on this surface wants the FAILURE (which arrives as a
// rejected promise carrying the engine's own refusal text -- "template is not
// ready", "no email sender is registered") rather than the payload. A caller
// that needs the payload should read it off executeNamed directly, as
// useIntegrationStatus does.
export async function runBuiltin(
  query: QueryClient,
  name: string,
  args: Record<string, unknown> = {},
): Promise<void> {
  await query.executeNamed(name, buildCall("builtin", name, args));
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
