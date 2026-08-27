// The machines feed: ONE LiveCollection over the caller's worker
// registrations, shared by the Fleet exemplar (the list) and the desktop's
// provenance dots (is the producing machine online). Worker-registration
// graph events already carry broadcast routing rules to browser
// subscribers (component/node/routing.go), so live here is real, not
// polled.

import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import {
  getRowByConceptAndId,
  LiveCollection,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import type { MachinePresence } from "../items/provenance";
import { isWorkerOnline } from "../apps/fleet/online";
import { useOsConnection } from "./connection";

export const WORKER_REGISTRATION_CONCEPT = "v1:worker:registration";

export interface MachineRow {
  id: string;
  displayName?: string;
  hostname?: string;
  platform?: string;
  lastSeenAt?: string;
  revokedAt?: string;
  labels?: Record<string, string>;
  operatorLabels?: Record<string, string>;
}

function asMachineRow(row: Row): MachineRow {
  const rec = row as Record<string, unknown>;
  const payload =
    rec["payload"] && typeof rec["payload"] === "object" && !Array.isArray(rec["payload"])
      ? (rec["payload"] as Record<string, unknown>)
      : rec;
  const str = (key: string): string | undefined =>
    typeof payload[key] === "string" ? (payload[key] as string) : undefined;
  const labels = (key: string): Record<string, string> | undefined => {
    const value = payload[key];
    if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      if (typeof v === "string") out[k] = v;
    }
    return out;
  };
  return {
    id: typeof rec["id"] === "string" ? (rec["id"] as string) : (str("id") ?? ""),
    displayName: str("displayName"),
    hostname: str("hostname"),
    platform: str("platform"),
    lastSeenAt: str("lastSeenAt"),
    revokedAt: str("revokedAt"),
    labels: labels("labels"),
    operatorLabels: labels("operatorLabels"),
  };
}

export function machineName(m: MachineRow): string {
  return m.displayName || m.hostname || m.id;
}

interface MachinesValue {
  collection: LiveCollection<MachineRow> | null;
  /** Presence by BARE registration id, for the provenance dots. */
  presence: (workerId: string) => MachinePresence | null;
}

const Ctx = createContext<MachinesValue>({ collection: null, presence: () => null });

export function useMachines(): MachinesValue {
  return useContext(Ctx);
}

export function MachinesProvider({ children }: { children: ReactNode }) {
  const connection = useOsConnection();

  const collection = useMemo(() => {
    if (!connection) return null;
    const query = connection.query;
    return new LiveCollection<MachineRow>(
      {
        concept: WORKER_REGISTRATION_CONCEPT,
        // The same read the portal's machines list uses: the caller's own
        // machines, status included, scoped by the engine.
        seed: async (_cursor, signal) => {
          const result = await query.executeNamed(
            "myWorkersWithStatus",
            "query myWorkersWithStatus()",
            { signal },
          );
          return { rows: result.rows().map(asMachineRow), nextCursor: "" };
        },
        reread: async (rowId, signal) => {
          const row = await getRowByConceptAndId(query, WORKER_REGISTRATION_CONCEPT, rowId, {
            signal,
          });
          return row ? asMachineRow(row) : null;
        },
        paged: false,
      },
      connection.subscriptions ?? null,
      // Linger briefly so a window close/reopen does not tear the feed down.
      5_000,
    );
  }, [connection]);

  // The presence map folds the same snapshot the list renders.
  const [version, setVersion] = useState(0);
  useEffect(() => {
    if (!collection) return;
    return collection.subscribe(() => setVersion((v) => v + 1));
  }, [collection]);

  const presence = useMemo(() => {
    void version;
    const byId = new Map<string, MachinePresence>();
    if (collection) {
      for (const row of collection.snapshot.rows) {
        const bare = row.id.includes(":") ? row.id.slice(row.id.lastIndexOf(":") + 1) : row.id;
        const entry: MachinePresence = { name: machineName(row), online: isWorkerOnline(row) };
        byId.set(row.id, entry);
        byId.set(bare, entry);
      }
    }
    return (workerId: string) => byId.get(workerId) ?? null;
  }, [collection, version]);

  const value = useMemo(() => ({ collection, presence }), [collection, presence]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}
