// Full machine inventory is demanded by routing, setup and equipment pickers.
// The Fleet directory owns its ten-row feed and by-id details. Desktop origin
// dots demand a separate lightweight identity/presence projection, so opening
// the desktop does not download every machine's apps, hardware and sharing.
// Heartbeats fold into retained rows; they never trigger whole-feed refreshes.

import { createContext, useContext, useMemo, type ReactNode } from "react";
import {
  getRowByConceptAndId,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import type { MachinePresence } from "../items/provenance";
import { isWorkerOnline } from "../apps/fleet/online";
import { machineFromRow, machineName, type MachineRow } from "../apps/fleet/rows";
import { useSessionIfPresent } from "../chrome/access";
import type { LiveListSource } from "./LiveList";
import { useLiveCollection } from "./useLiveCollection";
import { useDemandedRead, useReadDemand } from "../kit/readDemand";
import { LiveReadVisibility } from "./useGraphRevision";

export const WORKER_REGISTRATION_CONCEPT = "v1:worker:registration";

// The row TYPE travels with the collection -- a consumer holding a
// LiveCollection<MachineRow> needs it -- while the projection and every
// derived reading live in apps/fleet/rows.ts.
export type { MachineRow };

interface MachinesValue {
  retain?: () => () => void;
  /**
   * RAW rows, not projected ones.
   *
   * The fold upserts an event's payload AS the row type with no projection
   * hook (`liveCollection.ts`), so a collection typed with MachineRow would
   * be correct until the first heartbeat and hold a raw wire row after it --
   * and the machines view's own `isRevoked` predicate throws on the absent
   * field rather than degrading. Consumers project through
   * `apps/fleet/rows.machineFromRow`, on the read side, where it runs over
   * whatever the collection currently holds. Same rule the portal follows.
   */
  collection: LiveListSource<Row> | null;
  /** Presence by BARE registration id, for the provenance dots. */
  presence: (workerId: string) => MachinePresence | null;
  /**
   * The current row count. The add-machine panel reports success when this
   * GROWS past the value it captured at mint time -- counting rather than
   * matching by name, because the token's label is what the operator typed
   * and the registration's name is the cockpit's hostname, so the two are
   * routinely different and a name match would report failure on a success.
   */
  count: number;
  /**
   * Feed readiness for setup / Fleet empty copy. `seeding` and a null
   * collection are NOT "no machines" -- they are "we have not finished asking".
   * Only `live` (or degraded-with-rows) may drive an empty-state sentence.
   */
  feedState: "absent" | "seeding" | "live" | "degraded" | "disconnected";
  /** True once the seed has completed for the signed-in user. */
  settled: boolean;
  /** Re-run the seed. The one caller is an explicit operator refresh; the
   *  feed keeps itself current without it. */
  reload: () => void;
}

const Ctx = createContext<MachinesValue>({
  collection: null,
  presence: () => null,
  count: 0,
  feedState: "absent",
  settled: false,
  reload: () => {},
});

export function useMachines(enabled = true): MachinesValue {
  const value = useContext(Ctx);
  const visible = useContext(LiveReadVisibility);
  useDemandedRead(enabled && visible ? value.retain : undefined);
  return value;
}

const PresenceContext = createContext<{ presence: MachinesValue["presence"]; retain?: () => () => void }>({ presence: () => null });

/** Desktop provenance needs identity and liveness, never a machine's equipment,
 * installed apps, permissions or sharing detail. Its own small projection also
 * keeps the directory from silently retaining the full-fleet inventory. */
export function useMachinePresence() {
  const value = useContext(PresenceContext);
  const visible = useContext(LiveReadVisibility);
  useDemandedRead(visible ? value.retain : undefined);
  return value;
}

export function MachinesProvider({ children }: { children: ReactNode }) {
  const session = useSessionIfPresent();
  const userId = session?.access?.userId ?? "";
  const demand = useReadDemand();
  const presenceDemand = useReadDemand();

  const { source: collection, snapshot, reseed } = useLiveCollection<Row>(
    userId && demand.requested ? `machines:${userId}` : null,
    connection => ({
      concept: WORKER_REGISTRATION_CONCEPT,
      seed: async (_cursor, signal) => ({ rows: (await connection.query.myWorkersWithStatus({}, { signal })).rows(), nextCursor: "" }),
      reread: async (rowId, signal) => (await getRowByConceptAndId(connection.query, WORKER_REGISTRATION_CONCEPT, rowId, { signal })) as Row | null,
      // Cluster owners can receive other owners' events. Match the caller-
      // scoped seed even when subscription admission is broader.
      inScope: row => machineFromRow(row).ownerUserId === userId,
      paged: false,
    }),
  );

  const { presence, count, feedState, settled } = useMemo(() => {
    const byId = new Map<string, MachinePresence>();
    const rows = snapshot ? snapshot.rows.map(machineFromRow) : [];
    for (const row of rows) {
      if (row.id === "") continue;
      const bare = row.id.includes(":") ? row.id.slice(row.id.lastIndexOf(":") + 1) : row.id;
      const entry: MachinePresence = { name: machineName(row), online: isWorkerOnline(row) };
      byId.set(row.id, entry);
      byId.set(bare, entry);
    }
    const state = !collection ? "absent" : snapshot.error ? "degraded" : snapshot.state;
    return {
      presence: (workerId: string) => byId.get(workerId) ?? null,
      count: rows.length,
      feedState: state as MachinesValue["feedState"],
      settled: state === "live" || state === "degraded",
    };
  }, [collection, snapshot]);

  const summaries = useLiveCollection<Row>(
    userId && presenceDemand.requested ? `machine-presence:${userId}` : null,
    connection => ({
      concept: WORKER_REGISTRATION_CONCEPT,
      seed: async (cursor, signal) => {
        const result = await connection.query.myMachinePresence({}, { signal, ...(cursor ? { cursor } : {}) });
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (rowId, signal) => await getRowByConceptAndId(connection.query, WORKER_REGISTRATION_CONCEPT, rowId, { signal }) as Row | null,
      inScope: row => machineFromRow(row).ownerUserId === userId,
    }),
  );
  const presenceOnly = useMemo(() => {
    const byId = new Map(summaries.snapshot.rows.map(raw => {
      const row = machineFromRow(raw);
      return [row.id, { name: machineName(row), online: isWorkerOnline(row) }] as const;
    }));
    return (workerId: string): MachinePresence | null => byId.get(workerId) ?? null;
  }, [summaries.snapshot]);
  const value = useMemo(
    () => ({
      collection,
      presence,
      count,
      feedState,
      settled,
      reload: reseed,
      retain: demand.retain,
    }),
    [collection, presence, count, feedState, settled, reseed, demand.retain],
  );
  return <PresenceContext.Provider value={{ presence: presenceOnly, retain: presenceDemand.retain }}><Ctx.Provider value={value}>{children}</Ctx.Provider></PresenceContext.Provider>;
}

