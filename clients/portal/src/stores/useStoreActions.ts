import { useCallback, useState } from "react";

import { useCluster } from "../cluster/ClusterProvider";

// The two operator actions this surface owns.
//
// ONLY TWO, and the shortness is the point. Backfill, reconciliation and
// the per-domain pause switch belong to EVERY connector, so the
// data-origins runtime owns them and the Data origins surface drives them
// -- two pages carrying the same three buttons is the duplication that
// epic's whole design exists to avoid.
//
// What is left here is what is Shopify's alone: re-registering webhook
// subscriptions (nothing generic can know how often a given origin
// forgets its subscribers), and pausing the STORE, which is a different
// switch from pausing a domain -- it stops ingestion for one merchant
// while their deliveries keep being staged.

export interface StoreActionsState {
  busy: string;
  error: string;
  note: string;
  ensureSubscriptions: () => void;
  setStatus: (storeId: string, status: string) => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useStoreActions(onDone: () => void): StoreActionsState {
  const { query } = useCluster();
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [note, setNote] = useState("");

  const run = useCallback(
    (label: string, work: () => Promise<unknown>, done: string) => {
      if (query === null) return;
      setBusy(label);
      setError("");
      setNote("");
      void work()
        .then(() => setNote(done))
        .catch((err: unknown) => setError(describe(err)))
        .finally(() => {
          setBusy("");
          onDone();
        });
    },
    [query, onDone],
  );

  return {
    busy,
    error,
    note,
    ensureSubscriptions: useCallback(() => {
      run("subscriptions", () => query!.shopifyEnsureSubscriptions({}), "Subscriptions reconciled.");
    }, [query, run]),
    setStatus: useCallback(
      (storeId: string, status: string) => {
        run("status", () => query!.setStoreStatus({ storeId, status }), `Store is now ${status}.`);
      },
      [query, run],
    ),
  };
}
