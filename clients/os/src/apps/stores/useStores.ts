import { useCallback, useMemo, useState } from "react";

import { useReading, type Reading } from "../../cluster/reading";
import { useOsConnection } from "../../live/connection";
import { readStoreHealth, type StoreHealth } from "./health";

// The reads and the writes this app makes.
//
// ===========================================================================
// THE HEALTH READ IS ON-DEMAND, AND THAT IS THE HONEST ANSWER
// ===========================================================================
// Nothing here is a live feed, and the temptation to make one is exactly what
// `src/cluster/reading.ts` was written to head off. A store's health is not
// on the row: the granted scopes against what the mirror needs, the cost
// bucket, the subscription reconcile and every domain's drift are computed
// from the connector's `v1:platform:syncState` rows and the live Admin
// client, so no row read answers them -- and `v1:shopify:store` carries no
// broadcast routing rule, so a `useLiveCollection` over it would render
// "Loading from the cluster" and then a list that never moves.
//
// So this reads once, PRINTS WHEN IT LOOKED, and offers to look again. A
// surface where half the bands move and half do not is worse than one where
// none do, because the reader cannot tell which kind of band they are
// looking at.
//
// ===========================================================================
// THE WRITES RE-READ, FOR THE SAME REASON
// ===========================================================================
// Fleet's writes deliberately do NOT refetch -- its subscription carries the
// new value back. There is no subscription here, so an accepted write that
// did not re-read would leave a paused store reading "Live" until somebody
// pressed Re-read, which looks exactly like a write the engine ignored.

export interface StoresReading extends Reading<StoreHealth[]> {
  /** The stores, or [] before a read has landed. Never null at the call
   *  site, because every consumer maps over it. */
  stores: StoreHealth[];
}

export function useStoreHealth(): StoresReading {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (connection === null) return null;
    return async (signal: AbortSignal): Promise<StoreHealth[]> => {
      const result = await connection.query.shopifyStoreHealth({}, { signal });
      return readStoreHealth(result.rows());
    };
  }, [connection]);

  // The key encodes everything `read` closes over. There is exactly one
  // thing -- whether a connection exists -- because the call takes no
  // argument: `shopifyStoreHealth({})` reports EVERY configured store, and
  // the detail page picks its own out of the same answer rather than making
  // a second, narrower call that could disagree with the list beside it.
  const reading = useReading<StoreHealth[]>(connection === null ? "no-connection" : "stores", read);
  return { ...reading, stores: reading.value ?? [] };
}

/** What `createStore` is given. Mirrors the generated `CreateStoreArgs`,
 *  with every optional field present as a string the form can hold. */
export interface NewStore {
  storeId: string;
  domain: string;
  name: string;
  appClientId: string;
  /** The NAME of a globalSecret row. Never a token -- see AddStoreForm. */
  adminTokenRef: string;
  storefrontTokenRef: string;
  webhookSecretRef: string;
  apiVersion: string;
  protectedDataLevel: string;
  ownerUserId: string;
}

export const BLANK_STORE: NewStore = {
  storeId: "",
  domain: "",
  name: "",
  appClientId: "",
  adminTokenRef: "",
  storefrontTokenRef: "",
  webhookSecretRef: "",
  apiVersion: "",
  protectedDataLevel: "",
  ownerUserId: "",
};

export interface StoreWrites {
  /** The act in flight, or "". One at a time: the bar disables nothing, so
   *  the busy flag is what stops a double click becoming two audit rows. */
  busy: string;
  /** The last refusal, in the server's own words. */
  error: string;
  /** What the last accepted write did, in this surface's voice. */
  note: string;
  clearNote: () => void;
  createStore: (input: NewStore) => Promise<boolean>;
  setStatus: (storeId: string, status: string) => Promise<boolean>;
  ensureSubscriptions: () => Promise<boolean>;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * The three writes this surface owns.
 *
 * ONLY THREE, AND THE SHORTNESS IS THE POINT. Backfill, reconciliation and
 * the per-domain pause switch belong to EVERY connector, so the data-origins
 * runtime owns them and the Cluster app's Data origins section drives them --
 * two pages carrying the same three buttons is the duplication that design
 * exists to avoid. What is left here is what is Shopify's alone.
 */
export function useStoreWrites(onWritten: () => void): StoreWrites {
  const connection = useOsConnection();
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [note, setNote] = useState("");

  const run = useCallback(
    async (label: string, work: () => Promise<unknown>, done: string): Promise<boolean> => {
      if (connection === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setBusy(label);
      setError("");
      setNote("");
      try {
        await work();
        setNote(done);
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy("");
        onWritten();
      }
    },
    [connection, onWritten],
  );

  const createStore = useCallback(
    (input: NewStore) =>
      run(
        "create",
        () =>
          // OMITTED, NOT BLANK. `??` in the DSL is blank-coalescing, so a "" a
          // form sends is the same as an absent argument for a defaulted
          // field -- but `@noUnset` fields and enum-validated ones are not,
          // and sending "" for `protectedDataLevel` would fail the enum check
          // rather than leaving it unset.
          connection!.query.createStore({
            storeId: input.storeId.trim(),
            domain: input.domain.trim(),
            ...omitBlank("name", input.name),
            ...omitBlank("appClientId", input.appClientId),
            ...omitBlank("adminTokenRef", input.adminTokenRef),
            ...omitBlank("storefrontTokenRef", input.storefrontTokenRef),
            ...omitBlank("webhookSecretRef", input.webhookSecretRef),
            ...omitBlank("apiVersion", input.apiVersion),
            ...omitBlank("protectedDataLevel", input.protectedDataLevel),
            ...omitBlank("ownerUserId", input.ownerUserId),
          }),
        `${input.domain.trim()} is registered.`,
      ),
    [connection, run],
  );

  const setStatus = useCallback(
    (storeId: string, status: string) =>
      run("status", () => connection!.query.setStoreStatus({ storeId, status }), `Ingestion is now ${status}.`),
    [connection, run],
  );

  const ensureSubscriptions = useCallback(
    () =>
      run(
        "subscriptions",
        () => connection!.query.shopifyEnsureSubscriptions({}),
        "Subscriptions were reconciled for every ingesting store.",
      ),
    [connection, run],
  );

  return {
    busy,
    error,
    note,
    clearNote: useCallback(() => setNote(""), []),
    createStore,
    setStatus,
    ensureSubscriptions,
  };
}

function omitBlank(key: string, value: string): Record<string, string> {
  const trimmed = value.trim();
  return trimmed === "" ? {} : { [key]: trimmed };
}
