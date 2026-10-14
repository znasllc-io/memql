import catalog from "../../../../../test/proving/goals/catalog.json";
import type { Result, Row } from "@znasllc-io/memql-sdk-core/client";

export { catalog };
export type Scenario = typeof catalog.scenarios[number];
export type Measurement = { basis: "actual" | "estimated" | "unavailable"; value?: number; source: string };
export type Route = { role: string; level: string; rule: string; doors: { source: string; model: string; configuration: Record<string, unknown> }[] };
export type Evaluation = {
  schemaVersion: number; id: string; scenarioId: string; scenarioVersion: string; scenarioFingerprint: string; fixtureVersion: string;
  outcome: "autonomous" | "assisted" | "failed" | "blocked" | "cancelled"; engineCommit: string; harnessCommit: string;
  setup: { name: string; routingDigest: string; routes: Route[]; paidProvidersAllowed: boolean };
  modelsUsed: { role: string; level: string; source: string; model: string; calls: number; fallback: boolean; evidence: string[]; inputTokens: Measurement; outputTokens: Measurement; costUsd: Measurement }[];
  bindings: { id: string; version: string; digest: string; source: string }[];
  goalId: string; runId: string; startedAt: string; finishedAt: string; artifacts: string[];
  interventions: { at: string; kind: string; description: string; evidence: string }[];
  assertions: { criterionId: string; verdict: string; evidence: string[] }[];
  inputTokens: Measurement; outputTokens: Measurement; costUsd: Measurement;
};

export function measurement(m?: Measurement): string {
  if (!m || !validMeasurement(m) || m.basis === "unavailable") return "Not measured";
  return `${m.value!.toLocaleString()} (${m.basis})`;
}
export function evaluationOutcome(r?: Evaluation): string {
  return r ? ({ autonomous: "Autonomous success", assisted: "Assisted success", failed: "Failed", blocked: "Blocked", cancelled: "Cancelled" })[r.outcome] : "Not run";
}
function object(v: unknown): v is Record<string, unknown> { return !!v && typeof v === "object" && !Array.isArray(v); }
function text(v: unknown): v is string { return typeof v === "string" && v.trim().length > 0; }
function strings(v: unknown): v is string[] { return Array.isArray(v) && v.every(text); }
function list(v: unknown, valid: (v: unknown) => boolean): boolean { return Array.isArray(v) && v.every(valid); }
function validMeasurement(v: unknown): boolean {
  return object(v) && text(v.source) && (v.basis === "unavailable" ? v.value === undefined :
    (v.basis === "actual" || v.basis === "estimated") && typeof v.value === "number" && Number.isFinite(v.value) && v.value >= 0);
}

function canonical(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  if (object(v)) return `{${Object.keys(v).sort().map((k) => `${JSON.stringify(k)}:${canonical(v[k])}`).join(",")}}`;
  return JSON.stringify(v);
}
export async function fingerprint(v: unknown): Promise<string> {
  // Match Go's canonical JSON, including its HTML-safe string escapes.
  const json = canonical(v).replace(/[<>&\u2028\u2029]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, "0")}`);
  const bytes = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(json));
  return Array.from(new Uint8Array(bytes), (n) => n.toString(16).padStart(2, "0")).join("");
}

// A malformed receipt must never render as green, or crash the inspection page.
export async function evaluation(value: unknown): Promise<Evaluation | null> {
  if (!object(value)) return null;
  const v: Record<string, unknown> = { ...value, bindings: value.bindings ?? [], modelsUsed: value.modelsUsed ?? [], artifacts: value.artifacts ?? [], interventions: value.interventions ?? [], assertions: value.assertions ?? [] };
  if (!object(v) || v.schemaVersion !== 1 || !["id", "scenarioId", "scenarioVersion", "scenarioFingerprint", "fixtureVersion", "engineCommit", "harnessCommit", "goalId", "runId", "startedAt", "finishedAt"].every((k) => text(v[k]))) return null;
  if (!["autonomous", "assisted", "failed", "blocked", "cancelled"].includes(String(v.outcome)) || !strings(v.artifacts)) return null;
  if (![v.engineCommit, v.harnessCommit].every((sha) => typeof sha === "string" && /^[a-f0-9]{40}$/.test(sha))) return null;
  if (!Number.isFinite(Date.parse(String(v.startedAt))) || !Number.isFinite(Date.parse(String(v.finishedAt))) || Date.parse(String(v.finishedAt)) < Date.parse(String(v.startedAt))) return null;
  if (![v.inputTokens, v.outputTokens, v.costUsd].every(validMeasurement)) return null;
  if (!object(v.setup) || !text(v.setup.name) || typeof v.setup.paidProvidersAllowed !== "boolean" || !Array.isArray(v.setup.routes) || !v.setup.routes.length) return null;
  for (const route of v.setup.routes) {
    if (!object(route) || ![route.role, route.level, route.rule].every(text) || !Array.isArray(route.doors) || !route.doors.length) return null;
    const paidAllowed = v.setup.paidProvidersAllowed;
    if (!route.doors.every((d) => object(d) && ["fleet", "app", "session", "federation"].includes(String(d.source)) && text(d.model) && object(d.configuration) && (paidAllowed || d.source !== "federation"))) return null;
  }
  if (v.setup.routingDigest !== await fingerprint({ routes: v.setup.routes, paidProvidersAllowed: v.setup.paidProvidersAllowed })) return null;
  if (!list(v.modelsUsed, (u) => object(u) && [u.role,u.level,u.source,u.model].every(text) && Number.isSafeInteger(u.calls) && Number(u.calls) > 0 && typeof u.fallback === "boolean" && strings(u.evidence) && u.evidence.length > 0 && [u.inputTokens,u.outputTokens,u.costUsd].every(validMeasurement))) return null;
  if (!list(v.bindings, (b) => object(b) && [b.id,b.version,b.digest,b.source].every(text))) return null;
  if (!list(v.interventions, (i) => object(i) && [i.at,i.kind,i.description,i.evidence].every(text))) return null;
  if (!list(v.assertions, (a) => object(a) && text(a.criterionId) && ["pass","fail","unverified"].includes(String(a.verdict)) && strings(a.evidence) && (a.verdict === "unverified" || a.evidence.length > 0))) return null;
  const r = v as Evaluation;
  if (r.interventions.some((i) => !Number.isFinite(Date.parse(i.at)) || Date.parse(i.at) < Date.parse(r.startedAt) || Date.parse(i.at) > Date.parse(r.finishedAt))) return null;
  if (r.bindings.some((b) => !/^[a-f0-9]{64}$/.test(b.digest))) return null;
  if (["autonomous", "assisted"].includes(r.outcome) && !r.setup.paidProvidersAllowed && r.modelsUsed.some((use) => use.source === "federation")) return null;
  if (["failed", "blocked", "cancelled"].includes(r.outcome) && !r.artifacts.length) return null;
  const scenario = catalog.scenarios.find((s) => s.id === r.scenarioId);
  if (scenario && r.scenarioVersion === scenario.version && r.scenarioFingerprint === await fingerprint(scenario)) {
    if (r.fixtureVersion !== scenario.fixture.version || new Set(r.assertions.map((a) => a.criterionId)).size !== r.assertions.length || r.assertions.some((a) => !scenario.criteria.some((c) => c.id === a.criterionId))) return null;
    if (["autonomous","assisted"].includes(r.outcome) && (!r.modelsUsed.length || scenario.criteria.some((c) => c.required && !r.assertions.some((a) => a.criterionId === c.id && a.verdict === "pass")) || ((r.outcome === "autonomous") !== (r.interventions.length === 0)))) return null;
  }
  return r;
}

// Explicit cursor paging: one page is not the owner's complete run history.
export async function readHistory(read: (cursor?: string) => Promise<Result>): Promise<Row[]> {
  const rows: Row[] = []; const seen = new Set<string>(); let cursor: string | undefined;
  for (let page = 0; page < 100; page++) {
    const result = await read(cursor); rows.push(...result.rows());
    if (!result.meta()?.hasMore) return rows;
    cursor = result.meta()?.cursor;
    if (!cursor || seen.has(cursor)) throw new Error("History returned an incomplete page.");
    seen.add(cursor);
  }
  throw new Error("History exceeds the inspection window.");
}
