import { useCallback, useMemo, useState } from "react";

import { useReading, type Reading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import { readStoreHealth, type StoreHealth } from "./health";
import { storeFromRow, type StoreRow } from "./rows";

// The reads and the writes a storefront deployable makes about its store.
//
// ===========================================================================
// NOTHING HERE IS A LIVE FEED, AND THAT IS THE HONEST ANSWER
// ===========================================================================
// Carried over from the Stores app, with its reasoning intact. A store's
// health is not on the row: the granted scopes against what the mirror needs,
// the cost bucket, the subscription reconcile and every domain's drift are
// computed from the connector's `v1:platform:syncState` rows and the live
// Admin client, so no row read answers them -- and `v1:shopify:store` carries
// no broadcast routing rule, so a `useLiveCollection` over it would render
// "Loading from the cluster" and then a panel that never moves.
//
// So each read happens once, PRINTS WHEN IT LOOKED, and offers to look again.
// A surface where half the bands move and half do not is worse than one where
// none do, because the reader cannot tell which kind of band they are looking
// at.
//
// ===========================================================================
// THE WRITES RE-READ, FOR THE SAME REASON
// ===========================================================================
// There is no subscription here, so an accepted write that did not re-read
// would leave a paused store reading "Live" until somebody pressed Re-read,
// which looks exactly like a write the engine ignored.
//
// ===========================================================================
// EVERY READ IS SHAPED, SO EVERY READ USES rows()
// ===========================================================================
// `stores`, `storeById` and `developmentStoresFor` all declare `shape
// storeFull`, and a SHAPED query returns no bundle envelope. `rawNodes()`
// reads `payload.bundle.nodes` and nothing else, so it would answer "no
// stores" on a cluster that has them -- with nothing thrown and nothing
// logged.

export interface StoreListReading extends Reading<StoreRow[]> {
  /** The stores, or [] before a read has landed. */
  stores: StoreRow[];
}

/**
 * Every store this caller may read.
 *
 * THE PICKER'S POPULATION IS THE BINDING'S POPULATION, and that is not a
 * coincidence: the engine refuses a binding naming a store the caller cannot
 * read, so a picker built from this read can only ever offer bindings that
 * will be accepted. A picker fed from anywhere else would offer choices the
 * server then refuses, which reads as a broken control rather than as a
 * permission.
 */
export function useStoreList(): StoreListReading {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (connection === null) return null;
    return async (signal: AbortSignal): Promise<StoreRow[]> => {
      const result = await connection.query.stores({}, { signal });
      return result.rows().map(storeFromRow);
    };
  }, [connection]);
  const reading = useReading<StoreRow[]>(connection === null ? "no-connection" : "stores", read);
  return { ...reading, stores: reading.value ?? [] };
}

export interface StoreReading extends Reading<StoreRow | null> {
  /** The bound store, or null when nothing is bound or the read has not landed. */
  store: StoreRow | null;
}

/** One store by id. A blank id reads nothing and stays `unread`. */
export function useStore(storeId: string): StoreReading {
  const connection = useOsConnection();
  const id = storeId.trim();
  const read = useMemo(() => {
    if (connection === null || id === "") return null;
    return async (signal: AbortSignal): Promise<StoreRow | null> => {
      const result = await connection.query.storeById({ storeId: id }, { signal });
      const first = result.rows()[0];
      return first === undefined ? null : storeFromRow(first);
    };
  }, [connection, id]);
  const reading = useReading<StoreRow | null>(connection === null || id === "" ? "no-store" : `store:${id}`, read);
  return { ...reading, store: reading.value ?? null };
}

/** The development stores paired to one live store. */
export function useDevelopmentStores(storeId: string): StoreListReading {
  const connection = useOsConnection();
  const id = storeId.trim();
  const read = useMemo(() => {
    if (connection === null || id === "") return null;
    return async (signal: AbortSignal): Promise<StoreRow[]> => {
      const result = await connection.query.developmentStoresFor({ storeId: id }, { signal });
      return result.rows().map(storeFromRow);
    };
  }, [connection, id]);
  const reading = useReading<StoreRow[]>(
    connection === null || id === "" ? "no-store" : `development:${id}`,
    read,
  );
  return { ...reading, stores: reading.value ?? [] };
}

export interface StoreHealthReading extends Reading<StoreHealth[]> {
  /** Every configured store's health, or [] before a read has landed. */
  all: StoreHealth[];
}

/**
 * Health for every configured store.
 *
 * ONE CALL FOR ALL OF THEM, deliberately: `shopifyStoreHealth({})` reports
 * every store, and a panel picks its own out of that answer rather than
 * making a second, narrower call that could disagree with it.
 */
export function useStoreHealth(): StoreHealthReading {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (connection === null) return null;
    return async (signal: AbortSignal): Promise<StoreHealth[]> => {
      const result = await connection.query.shopifyStoreHealth({}, { signal });
      return readStoreHealth(result.rows());
    };
  }, [connection]);
  const reading = useReading<StoreHealth[]>(connection === null ? "no-connection" : "store-health", read);
  return { ...reading, all: reading.value ?? [] };
}

/** This store's health out of the whole report, or null when it is not in it. */
export function healthFor(all: StoreHealth[], storeId: string): StoreHealth | null {
  const id = storeId.trim();
  if (id === "") return null;
  return all.find((h) => h.storeId === id) ?? null;
}

/** What `createStore` is given. Every optional field is a string the form holds. */
export interface NewStore {
  storeId: string;
  domain: string;
  name: string;
  appClientId: string;
  /** The NAME of a globalSecret row. Never a token. */
  adminTokenRef: string;
  storefrontTokenRef: string;
  webhookSecretRef: string;
  apiVersion: string;
  protectedDataLevel: string;
  ownerUserId: string;
  /** True when this store is one a storefront is exercised against. */
  isDevelopment: boolean;
  /** The live store it stands in for. Empty unless `isDevelopment`. */
  developmentOfStoreId: string;
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
  isDevelopment: false,
  developmentOfStoreId: "",
};

export interface StoreWrites {
  /** The act in flight, or "". One at a time: nothing is disabled, so the
   *  busy flag is what stops a double click becoming two audit rows. */
  busy: string;
  /** The last refusal, in the server's own words. */
  error: string;
  /** What the last accepted write did, in this surface's voice. */
  note: string;
  clearNote: () => void;
  createStore: (input: NewStore) => Promise<boolean>;
  setStatus: (storeId: string, status: string) => Promise<boolean>;
  ensureSubscriptions: () => Promise<boolean>;
  /** Point this storefront at a store, or clear the binding with "". */
  bindSite: (siteId: string, storeId: string) => Promise<boolean>;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * The writes a storefront's Store panel owns.
 *
 * FOUR, AND THE SHORTNESS IS STILL THE POINT. Backfill, reconciliation and
 * the per-domain pause switch belong to EVERY connector, so the data-origins
 * runtime owns them and Cluster > Data origins drives them. What is here is
 * what is Shopify's alone -- the store-wide pause and the subscription
 * reconcile -- plus the two this epic adds: registering a store, and naming
 * which one this storefront fronts.
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
          // OMITTED, NOT BLANK. `??` in the DSL is blank-coalescing, so a ""
          // a form sends is the same as an absent argument for a defaulted
          // field -- but an enum-validated one is not, and sending "" for
          // `protectedDataLevel` would fail the enum check rather than
          // leaving it unset.
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
            ...(input.isDevelopment ? { isDevelopment: true } : {}),
            ...omitBlank("developmentOfStoreId", input.developmentOfStoreId),
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

  const bindSite = useCallback(
    (siteId: string, storeId: string) =>
      run(
        "bind",
        () => connection!.query.updateSiteStoreBinding({ siteId, storeId }),
        storeId.trim() === "" ? "This storefront is no longer bound to a store." : "This storefront is bound.",
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
    bindSite,
  };
}

function omitBlank(key: string, value: string): Record<string, string> {
  const trimmed = value.trim();
  return trimmed === "" ? {} : { [key]: trimmed };
}
