import type { Row } from "@znasllc-io/memql-sdk-core/client";

// The integrationStatus builtin's REPLY (memql#3323).
//
// Nothing in this module builds a call. The five builtins the integrations
// surface drives -- campaignStartSend / campaignScheduleSend /
// campaignPauseSend / campaignResumeSend and integrationStatus -- are marked
// @sdk (memql#4239), so `make sdk-gen` emits a typed QueryClient method for
// each and the hooks call those: a renamed builtin or a changed argument
// fails typecheck instead of runtime, and quoting lives in the generated
// builders (the SDK's renderMemQLValue), the same place it lives for every
// query and mutation this app calls (memql#4232). This file used to carry the
// hand-built `builtin name(...)` form those five needed before they were
// marked. What remains is the one thing the generator does not type -- the
// SHAPE OF A BUILTIN'S REPLY -- for the only builtin here whose payload a
// caller reads; the four send actions answer with a synthetic node the hooks
// discard, because what they want is the failure, which arrives as the
// rejected promise carrying the engine's own refusal text.

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
