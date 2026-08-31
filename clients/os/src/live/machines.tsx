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

import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import {
  getRowByConceptAndId,
  LiveCollection,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import type { MachinePresence } from "../items/provenance";
import { isWorkerOnline } from "../apps/fleet/online";
import { machineFromRow, machineName, type MachineRow } from "../apps/fleet/rows";
import { useOsConnection } from "./connection";

export const WORKER_REGISTRATION_CONCEPT = "v1:worker:registration";

// The row TYPE travels with the collection -- a consumer holding a
// LiveCollection<MachineRow> needs it -- while the projection and every
// derived reading live in apps/fleet/rows.ts.
export type { MachineRow };

interface MachinesValue {
  collection: LiveCollection<MachineRow> | null;
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
  /** Re-run the seed. The one caller is an explicit operator refresh; the
   *  feed keeps itself current without it. */
  reload: () => void;
}

const Ctx = createContext<MachinesValue>({
  collection: null,
  presence: () => null,
  count: 0,
  reload: () => {},
});

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
        // machines, status included, scoped by the engine. Revoked rows come
        // back too -- myWorkersWithStatus filters on ownership and nothing
        // else -- which is what lets the show-revoked setting be a filter
        // rather than a second read.
        seed: async (_cursor, signal) => {
          const result = await query.myWorkersWithStatus({}, { signal });
          return { rows: result.rows().map(machineFromRow), nextCursor: "" };
        },
        reread: async (rowId, signal) => {
          const row = await getRowByConceptAndId(query, WORKER_REGISTRATION_CONCEPT, rowId, {
            signal,
          });
          return row ? machineFromRow(row as Row) : null;
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

  // RETAIN, AND RETAIN INSIDE THE EFFECT.
  //
  // A LiveCollection does nothing until it is retained: `start()` -- which
  // opens the subscription and runs the seed -- is reachable from `retain()`
  // and from nowhere else, and `subscribe()` only registers a change
  // listener. A provider that subscribed without retaining held a collection
  // that stayed in `seeding` with no rows forever, which renders as the
  // LiveList's "Loading from the cluster" caption and never resolves. That
  // was the foundation's exemplar (memql#4710), and it is the failure mode
  // this whole substrate is hardest to see: nothing errors, nothing is
  // logged, and an empty fleet is a completely plausible answer.
  //
  // Inside the effect rather than during render, for the reason the portal's
  // useLive spells out: React 19's StrictMode renders twice before
  // committing, so a retain taken during render is never balanced by this
  // cleanup and the collection outlives every consumer. The double MOUNT is
  // harmless -- release lingers, so the second retain reuses the same
  // collection and issues no new read.
  useEffect(() => {
    if (!collection) return;
    collection.retain();
    const off = collection.subscribe(() => setVersion((v) => v + 1));
    return () => {
      off();
      // The store's onExpire deletes the key from a registry this provider
      // does not have -- the collection is constructed here rather than
      // through a LiveStore -- so there is nothing to clean up beyond the
      // close release() already performs.
      collection.release(() => {});
    };
  }, [collection]);

  const { presence, count } = useMemo(() => {
    void version;
    const byId = new Map<string, MachinePresence>();
    const rows = collection ? collection.snapshot.rows : [];
    for (const row of rows) {
      const bare = row.id.includes(":") ? row.id.slice(row.id.lastIndexOf(":") + 1) : row.id;
      const entry: MachinePresence = { name: machineName(row), online: isWorkerOnline(row) };
      byId.set(row.id, entry);
      byId.set(bare, entry);
    }
    return {
      presence: (workerId: string) => byId.get(workerId) ?? null,
      count: rows.length,
    };
  }, [collection, version]);

  const value = useMemo(
    () => ({
      collection,
      presence,
      count,
      reload: () => collection?.reseed(),
    }),
    [collection, presence, count],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}
