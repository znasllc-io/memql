import { useCallback, useEffect, useMemo, useState } from "react";

import { useCluster } from "../cluster/ClusterProvider";

// Data wiring for the Data origins surface (epic memql#4378).
//
// TWO READS, and they are different KINDS of thing rather than two halves
// of one:
//
//   dataOrigins    the INVENTORY -- every concept's declaration, produced at
//                  query time from the live registry and never persisted. It
//                  changes when the DSL changes, which is a deploy.
//   syncStatesAll  the HEALTH -- per-(concept, connector) operational state
//                  that accumulates: cursors, lag, drift, queue depth.
//
// Both are request/reply with an explicit Refresh rather than a
// subscription, and that is a deliberate downgrade from the issue's "live
// through the subscription". A live feed here would be honest only if BOTH
// halves were live, and the inventory has no stream -- it is a projection of
// the registry, not rows anyone writes. A half-live page is worse than a
// dated one with a visible Refresh: it invites a reader to trust numbers
// that stopped moving for a reason the page does not show.

export interface DataOriginRow {
  conceptId: string;
  dataState: string;
  origin: string;
  mirroredTo: string[];
  connectors: string[];
}

export interface SyncStateRow {
  id: string;
  conceptId: string;
  connector: string;
  direction: string;
  backfillCursor: string;
  backfillStatus: string;
  lastInboundAt: string;
  lagSeconds: number;
  lastReconcileAt: string;
  driftCount: number;
  outboxDepth: number;
  deadLetterCount: number;
  paused: boolean;
  lastError: string;
}

export interface DeadLetterRow {
  id: string;
  conceptId: string;
  rowRef: string;
  action: string;
  version: string;
  target: string;
  attempts: number;
  lastError: string;
}

export interface DataOriginsState {
  origins: DataOriginRow[];
  health: SyncStateRow[];
  loading: boolean;
  error: string;
  reload: () => void;
}

// str / num / bool absorb what a materialized row actually carries: values
// arrive as whatever JSON produced, and a page that type-switched at every
// field would say nothing about data origins.
function str(row: Record<string, unknown>, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

function num(row: Record<string, unknown>, key: string): number {
  const v = row[key];
  if (typeof v === "number") return v;
  if (typeof v === "string") {
    const n = Number.parseInt(v, 10);
    return Number.isNaN(n) ? 0 : n;
  }
  return 0;
}

function bool(row: Record<string, unknown>, key: string): boolean {
  const v = row[key];
  if (typeof v === "boolean") return v;
  return typeof v === "string" && v.toLowerCase() === "true";
}

function strList(row: Record<string, unknown>, key: string): string[] {
  const v = row[key];
  if (!Array.isArray(v)) return [];
  return v.filter((item): item is string => typeof item === "string");
}

export function toOriginRows(rows: readonly Record<string, unknown>[]): DataOriginRow[] {
  return rows.map((row) => ({
    conceptId: str(row, "conceptId"),
    dataState: str(row, "dataState"),
    origin: str(row, "origin"),
    mirroredTo: strList(row, "mirroredTo"),
    connectors: strList(row, "connectors"),
  }));
}

export function toSyncStateRows(rows: readonly Record<string, unknown>[]): SyncStateRow[] {
  return rows.map((row) => ({
    id: str(row, "id"),
    conceptId: str(row, "conceptId"),
    connector: str(row, "connector"),
    direction: str(row, "direction"),
    backfillCursor: str(row, "backfillCursor"),
    backfillStatus: str(row, "backfillStatus"),
    lastInboundAt: str(row, "lastInboundAt"),
    lagSeconds: num(row, "lagSeconds"),
    lastReconcileAt: str(row, "lastReconcileAt"),
    driftCount: num(row, "driftCount"),
    outboxDepth: num(row, "outboxDepth"),
    deadLetterCount: num(row, "deadLetterCount"),
    paused: bool(row, "paused"),
    lastError: str(row, "lastError"),
  }));
}

export function toDeadLetterRows(rows: readonly Record<string, unknown>[]): DeadLetterRow[] {
  return rows.map((row) => ({
    id: str(row, "id"),
    conceptId: str(row, "conceptId"),
    rowRef: str(row, "rowRef"),
    action: str(row, "action"),
    version: str(row, "version"),
    target: str(row, "target"),
    attempts: num(row, "attempts"),
    lastError: str(row, "lastError"),
  }));
}

// healthFor indexes health by (conceptId, connector), which is how the table
// joins it onto the inventory. A domain with no health row has simply never
// been worked -- reported as absent rather than as zeros, because "never run"
// and "ran and found nothing" are different answers.
export function healthFor(
  health: readonly SyncStateRow[],
): (conceptId: string, connector: string) => SyncStateRow | null {
  const index = new Map<string, SyncStateRow>();
  for (const row of health) {
    index.set(`${row.conceptId}|${row.connector}`, row);
  }
  return (conceptId, connector) => index.get(`${conceptId}|${connector}`) ?? null;
}

type RowBag = Record<string, unknown>;

function materialize(result: unknown): RowBag[] {
  // The SDK's Result exposes rows(); a plain array is what the tests hand
  // in. Both are accepted so a page test does not have to construct an SDK
  // Result to say what the server returned.
  if (Array.isArray(result)) return result as RowBag[];
  const bag = result as { rows?: () => unknown } | null;
  if (bag && typeof bag.rows === "function") {
    const rows = bag.rows();
    if (Array.isArray(rows)) return rows as RowBag[];
  }
  return [];
}

export function useDataOrigins(enabled: boolean): DataOriginsState {
  const { query, status } = useCluster();
  const [origins, setOrigins] = useState<DataOriginRow[]>([]);
  const [health, setHealth] = useState<SyncStateRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (!enabled || !query || status !== "connected") return;
    let stale = false;
    setLoading(true);
    setError("");
    Promise.all([query.dataOrigins({}), query.syncStatesAll({})])
      .then(([inventory, states]) => {
        if (stale) return;
        setOrigins(toOriginRows(materialize(inventory)));
        setHealth(toSyncStateRows(materialize(states)));
      })
      .catch((err: unknown) => {
        if (!stale) setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
    };
  }, [enabled, query, status, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return useMemo(
    () => ({ origins, health, loading, error, reload }),
    [origins, health, loading, error, reload],
  );
}
