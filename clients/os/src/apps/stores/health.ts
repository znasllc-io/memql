import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { absent, figureFrom, figureOf, type Figure } from "../../cluster/figure";

// The `shopifyStoreHealth` builtin's REPLY, read into the shapes this app
// renders.
//
// ===========================================================================
// WHY THE READ IS SHAPED HERE AND NOT TAKEN AS-IS
// ===========================================================================
// The generated SDK types the CALL -- the builtin is `@sdk`, so a renamed
// argument fails typecheck rather than at runtime -- but not the shape of
// what a builtin ANSWERS with. This file is that shape, for the one builtin
// on this surface whose payload a caller reads.
//
// ===========================================================================
// EVERY COUNT IS A Figure, AND THAT IS THE POINT OF THE FILE
// ===========================================================================
// The portal's version of this reader coerced every number with `?? 0`, and
// then its table carried a comment reading "never run is not ran clean" to
// undo it by hand. `src/cluster/figure.ts` is that distinction as a TYPE, so
// the surfaces cannot lose it: a store nothing has reconciled reports
// `absent("unmeasured")` for drift, and a cost bucket nobody has observed is
// a dash rather than "0 of 0 points, restoring 0/s".
//
// Note where the absence comes from. The Go handler
// (`integrations/shopify/capabilities.go`, `handleStoreHealth`) sums drift
// across the connector's `v1:platform:syncState` rows -- so with NO sync-state
// rows it emits a perfectly measured-looking `driftLast: 0` that means
// "nothing has ever reconciled". `domains` being empty is the evidence for
// that, and it is where the absence is reintroduced.

/*
 * THE REPORT'S KEYS NAME WHAT THEY CARRY, and three of them did not until
 * epic memql#5009. `shopifyStoreHealth` sent `staleWrites` carrying
 * syncState's `lagSeconds`, `tombstoned` carrying its `outboxDepth`, and
 * `driftTotal` carrying the same value as `driftLast` -- so a surface
 * rendering "n / total" always printed "n / n" and claimed a cumulative
 * figure nothing measures.
 *
 * They were repaired in `integrations/shopify/capabilities.go` rather than
 * renamed here, because this surface is the only consumer and a rename at
 * the render leaves the next reader of the builtin to make the same mistake.
 * `driftTotal` was dropped rather than renamed: there was no second number.
 */
export interface DomainState {
  concept: string;
  /** idle / backfilling / paused / error, from the runtime's own syncState. */
  phase: string;
  /** RFC3339, or "" when the live path has applied nothing yet. */
  lastAppliedAt: string;
  lastReconciledAt: string;
  driftLast: Figure;
  /** Seconds of mirror staleness. */
  lagSeconds: Figure;
  /** Pending + failed outbox entries. */
  outboxDepth: Figure;
  lastError: string;
}

/** The Admin API's leaky-bucket state, as of the last call that reported it. */
export interface CostBucket {
  currentlyAvailable: Figure;
  maximumAvailable: Figure;
  restoreRate: Figure;
}

/** What one reconcile pass of the webhook subscriptions did. */
export interface SubscriptionRecord {
  existing: Figure;
  desired: Figure;
  /** RFC3339, or "" when the record carries no stamp. */
  at: string;
  /** Topics the pass could not register. */
  failed: string[];
}

export interface StoreHealth {
  storeId: string;
  domain: string;
  status: string;
  /** The version the store row pins, or "" when it pins none. */
  apiVersion: string;
  /** The version the mirror was GENERATED from. Never blank in practice. */
  mirrorApiVersion: string;
  protectedDataLevel: string;
  scopesGranted: string[];
  scopesNeeded: string[];
  scopesMissing: string[];
  /**
   * Rows the last reconcile wrote that live delivery should have carried.
   *
   * IT IS THE CONNECTOR'S FIGURE, NOT THIS STORE'S. The handler sums drift
   * over every `v1:platform:syncState` row for the shopify connector, and
   * those rows are keyed by (concept, connector) with no store in the key --
   * so in a cluster mirroring two stores both report the same total. See
   * `domains` below, which has the same scope for the same reason.
   */
  driftLast: Figure;
  /**
   * One entry per mirrored concept -- and, like `driftLast`, the CONNECTOR's
   * rows rather than this store's. `syncStatesAll(connector: "shopify")`
   * filters on the connector alone, and the handler hands the same slice to
   * every store in the report.
   */
  domains: DomainState[];
  /** Absent until a call has reported one. */
  costBucket: CostBucket | null;
  /** Absent until a subscription reconcile has been recorded. */
  subscriptions: SubscriptionRecord | null;
}

const MAX_ENVELOPE_DEPTH = 4;

/**
 * Dig the report out of the engine's builtin envelope.
 *
 * A top-level `builtin X(...)` does not come back as a row set: the engine
 * marshals the handler's node map into one value keyed by node id, whose
 * payload is what the handler wrote. `Result.rows()` unwraps that map and
 * flattens each node's payload, so `stores` lands as a top-level key of the
 * row -- but this stays written as a SEARCH rather than a fixed path, because
 * the node id is bare-ified server-side on the way out and a hard-coded key
 * would be one rename away from silently reading undefined.
 */
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
  const domains = Array.isArray(r["domains"]) ? (r["domains"] as unknown[]).map(toDomainState) : [];
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
    // NO SYNC STATE, NO DRIFT MEASUREMENT. The handler sums drift over the
    // connector's syncState rows, so with none of them the sum is a
    // measured-looking zero for a store that has never reconciled at all --
    // which is the one reading this whole module exists to refuse.
    driftLast: domains.length === 0 ? absent("unmeasured") : figureFrom(r, "driftLast"),
    domains,
    costBucket: toCostBucket(r["costBucket"]),
    subscriptions: readSubscriptions(r["health"]),
  };
}

function toDomainState(value: unknown): DomainState {
  const r = (value ?? {}) as Record<string, unknown>;
  return {
    concept: str(r["concept"]),
    phase: str(r["phase"]),
    lastAppliedAt: str(r["lastAppliedAt"]),
    lastReconciledAt: str(r["lastReconciledAt"]),
    driftLast: figureFrom(r, "driftLast"),
    // The wire keys are the mislabelled ones -- see DomainState's header.
    lagSeconds: figureFrom(r, "lagSeconds"),
    outboxDepth: figureFrom(r, "outboxDepth"),
    lastError: str(r["lastError"]),
  };
}

function toCostBucket(value: unknown): CostBucket | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const r = value as Record<string, unknown>;
  return {
    currentlyAvailable: figureFrom(r, "currentlyAvailable"),
    maximumAvailable: figureFrom(r, "maximumAvailable"),
    restoreRate: figureFrom(r, "restoreRate"),
  };
}

/**
 * The subscription reconcile record off the store's free-form health.
 *
 * NULL IS A DIFFERENT ANSWER FROM ZERO REGISTERED. Shopify deletes a
 * subscription after eight consecutive delivery failures, so a store that has
 * gone quiet and a store nobody has ever checked look identical in the data
 * until a reconcile has run -- and only the second one is answered by
 * pressing Reconcile now.
 */
function readSubscriptions(health: unknown): SubscriptionRecord | null {
  if (health === null || typeof health !== "object" || Array.isArray(health)) return null;
  const subs = (health as Record<string, unknown>)["subscriptions"];
  if (subs === null || subs === undefined || typeof subs !== "object" || Array.isArray(subs)) return null;
  const r = subs as Record<string, unknown>;
  return {
    existing: figureFrom(r, "existing"),
    desired: figureFrom(r, "desired"),
    at: str(r["at"]),
    failed: strList(r["failed"]),
  };
}

/** How many concepts of the mirror this store has sync state for. */
export function mirroredDomainCount(store: StoreHealth): Figure {
  return figureOf(store.domains.length);
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function strList(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
}
