import type { Row } from "@znasllc-io/memql-sdk-core/client";

// The shopifyStoreHealth builtin's REPLY.
//
// The generated SDK types the CALL -- the builtin is @sdk, so a renamed
// argument fails typecheck rather than at runtime -- but not the shape of
// what a builtin answers with. This file is that shape, for the one builtin
// on this surface whose payload a caller reads.

export interface DomainState {
  concept: string;
  phase: string;
  lastAppliedAt: string;
  lastReconciledAt: string;
  driftLast: number;
  driftTotal: number;
  staleWrites: number;
  tombstoned: number;
  lastError: string;
}

export interface CostBucket {
  currentlyAvailable: number;
  maximumAvailable: number;
  restoreRate: number;
}

export interface StoreHealth {
  storeId: string;
  domain: string;
  status: string;
  apiVersion: string;
  mirrorApiVersion: string;
  protectedDataLevel: string;
  scopesGranted: string[];
  scopesNeeded: string[];
  scopesMissing: string[];
  driftLast: number;
  domains: DomainState[];
  costBucket?: CostBucket;
  health: Record<string, unknown>;
}

const MAX_ENVELOPE_DEPTH = 4;

// readStoreHealth digs the report out of the engine's builtin envelope.
//
// A top-level `builtin X(...)` does not come back as a row set: the engine
// marshals the handler's node map into one value keyed by node id, whose
// payload is what the handler wrote. Written as a SEARCH rather than a fixed
// path, because the node id is bare-ified server-side on the way out and a
// hard-coded key would be one rename away from silently reading undefined --
// the same reasoning integrations/statusReply.ts records.
export function readStoreHealth(rows: readonly Row[]): StoreHealth[] {
  for (const row of rows) {
    const found = findStores(row, 0);
    if (found) return found;
  }
  return [];
}

function findStores(value: unknown, depth: number): StoreHealth[] | null {
  if (depth > MAX_ENVELOPE_DEPTH) return null;
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  if (Array.isArray(record["stores"])) {
    return (record["stores"] as unknown[]).map(toStoreHealth);
  }
  for (const key of Object.keys(record)) {
    const found = findStores(record[key], depth + 1);
    if (found) return found;
  }
  return null;
}

function toStoreHealth(value: unknown): StoreHealth {
  const r = (value ?? {}) as Record<string, unknown>;
  return {
    storeId: str(r["storeId"]),
    domain: str(r["domain"]),
    status: str(r["status"]),
    apiVersion: str(r["apiVersion"]),
    mirrorApiVersion: str(r["mirrorApiVersion"]),
    protectedDataLevel: str(r["protectedDataLevel"]),
    scopesGranted: strList(r["scopesGranted"]),
    scopesNeeded: strList(r["scopesNeeded"]),
    scopesMissing: strList(r["scopesMissing"]),
    driftLast: num(r["driftLast"]),
    domains: Array.isArray(r["domains"]) ? (r["domains"] as unknown[]).map(toDomainState) : [],
    costBucket: toCostBucket(r["costBucket"]),
    health: (r["health"] as Record<string, unknown>) ?? {},
  };
}

function toDomainState(value: unknown): DomainState {
  const r = (value ?? {}) as Record<string, unknown>;
  return {
    concept: str(r["concept"]),
    phase: str(r["phase"]),
    lastAppliedAt: str(r["lastAppliedAt"]),
    lastReconciledAt: str(r["lastReconciledAt"]),
    driftLast: num(r["driftLast"]),
    driftTotal: num(r["driftTotal"]),
    staleWrites: num(r["staleWrites"]),
    tombstoned: num(r["tombstoned"]),
    lastError: str(r["lastError"]),
  };
}

function toCostBucket(value: unknown): CostBucket | undefined {
  if (value === null || typeof value !== "object") return undefined;
  const r = value as Record<string, unknown>;
  return {
    currentlyAvailable: num(r["currentlyAvailable"]),
    maximumAvailable: num(r["maximumAvailable"]),
    restoreRate: num(r["restoreRate"]),
  };
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function num(v: unknown): number {
  return typeof v === "number" ? v : 0;
}

function strList(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
}
