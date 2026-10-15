// The machines feed: ONE LiveCollection over the caller's worker
// registrations, shared by the Fleet app (the directory, the detail panel,
// the add-machine growth check) and the desktop's provenance dots (is the
// producing machine online). Worker-registration graph events already carry
// broadcast routing rules to browser subscribers (component/node/routing.go),
// so live here is real, not polled.
//
// ===========================================================================
// WHY THE COLLECTION STAYS IN live/ AND THE PROJECTION LIVES IN THE APP
// ===========================================================================
// Epic #4729 asked for the Fleet exemplar's wiring to move INTO the app. Only
// half of it can: the desktop's provenance dots subscribe to the same
// population, and a second collection for them would open a second
// subscription over the same concept and let two views of one machine drift
// apart. So the COLLECTION -- the subscription, the seed, the presence fold
// -- stays here where both consumers reach it, and what moved into
// apps/fleet is the part that is genuinely the app's: how a row is read
// (rows.ts), what a label means (labels.ts), and every surface over it.
//
// ===========================================================================
// EVENTS ARE FOLDED IN, NOT USED AS A REFETCH TRIGGER
// ===========================================================================
// A heartbeat bumps lastSeenAt every 15 seconds PER MACHINE, and every bump
// is an `updated` event. Re-running the seed on each one turns a ten-machine
// fleet into a read every second and a half, forever, on an idle desktop. The
// event payload updates the row in place instead; a full re-read is reserved
// for an id-only notification, which is what `reread` answers.

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

export const WORKER_REGISTRATION_CONCEPT = "v1:worker:registration";

// The row TYPE travels with the collection -- a consumer holding a
// LiveCollection<MachineRow> needs it -- while the projection and every
// derived reading live in apps/fleet/rows.ts.
export type { MachineRow };

interface MachinesValue {
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

export function useMachines(): MachinesValue {
  return useContext(Ctx);
}

export function MachinesProvider({ children }: { children: ReactNode }) {
  const session = useSessionIfPresent();
  const userId = session?.access?.userId ?? "";

  const { source: collection, snapshot, reseed } = useLiveCollection<Row>(
    userId ? `machines:${userId}` : null,
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

  const value = useMemo(
    () => ({
      collection,
      presence,
      count,
      feedState,
      settled,
      reload: reseed,
    }),
    [collection, presence, count, feedState, settled, reseed],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

