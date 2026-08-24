import { useCallback, useEffect, useState } from "react";
import type { Role } from "@znasllc-io/memql-sdk-core/client";

import { omitBlank } from "../cluster/args";
import { useCluster } from "../cluster/ClusterProvider";
import { useConceptRows, type ConceptRowsState } from "../cluster/useConceptRows";
import { useMyAccess } from "../cluster/useMyAccess";
import { readStoreHealth, type StoreHealth } from "./health";
import { STORE_CONCEPT_ID } from "./concepts";

// The Stores LIST screen's state: the live row walk, the health report, and
// the add-a-store action.
//
// TWO SOURCES, deliberately. The row walk is the live subscription over
// v1:shopify:store -- add a store and it appears without a refresh, the same
// mechanism every concept surface uses. The HEALTH is a builtin call, because
// what an operator needs to see is not on the row: the granted scopes against
// what the mirror needs, the cost bucket, and every domain's drift. Those are
// computed from the sync-state rows and the live client, so a row read cannot
// answer them.
//
// AUTHORIZATION IS A COURTESY HERE, as on every gated screen: v1:shopify:store
// is clusterOwner-tier and both declared reads carry actor.isClusterOwner as
// an explicit conjunct, so a non-owner's read comes back empty at the engine
// regardless of what isOwner says. isOwner only decides what this screen
// OFFERS.

export interface CreateStoreInput {
  storeId: string;
  domain: string;
  name: string;
  appClientId: string;
  adminTokenRef: string;
  storefrontTokenRef: string;
  webhookSecretRef: string;
  apiVersion: string;
  protectedDataLevel: string;
  ownerUserId: string;
}

export interface StoresState {
  rows: ConceptRowsState;
  health: StoreHealth[];
  healthLoading: boolean;
  healthError: string;
  refreshHealth: () => void;
  role: Role;
  isOwner: boolean;
  accessResolved: boolean;
  createBusy: boolean;
  createError: string;
  createStore: (input: CreateStoreInput) => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useStores(): StoresState {
  const { query } = useCluster();
  const rows = useConceptRows(STORE_CONCEPT_ID);
  const { access, loading: accessLoading } = useMyAccess();
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState("");
  const [health, setHealth] = useState<StoreHealth[]>([]);
  const [healthLoading, setHealthLoading] = useState(false);
  const [healthError, setHealthError] = useState("");

  const refreshHealth = useCallback(() => {
    if (query === null) return;
    setHealthLoading(true);
    setHealthError("");
    void query
      .shopifyStoreHealth({})
      .then((result) => setHealth(readStoreHealth(result.rows())))
      .catch((err: unknown) => setHealthError(describe(err)))
      .finally(() => setHealthLoading(false));
  }, [query]);

  useEffect(refreshHealth, [refreshHealth]);

  const createStore = useCallback(
    (input: CreateStoreInput) => {
      if (query === null) return;
      setCreateBusy(true);
      setCreateError("");
      void query
        .createStore({
          storeId: input.storeId,
          domain: input.domain,
          name: omitBlank(input.name),
          appClientId: omitBlank(input.appClientId),
          adminTokenRef: omitBlank(input.adminTokenRef),
          storefrontTokenRef: omitBlank(input.storefrontTokenRef),
          webhookSecretRef: omitBlank(input.webhookSecretRef),
          apiVersion: omitBlank(input.apiVersion),
          protectedDataLevel: omitBlank(input.protectedDataLevel),
          ownerUserId: omitBlank(input.ownerUserId),
        })
        .then(() => refreshHealth())
        .catch((err: unknown) => setCreateError(describe(err)))
        .finally(() => setCreateBusy(false));
    },
    [query, refreshHealth],
  );

  const role: Role = access?.clusterRole ?? "";
  return {
    rows,
    health,
    healthLoading,
    healthError,
    refreshHealth,
    role,
    isOwner: role === "owner",
    accessResolved: !accessLoading && access !== null,
    createBusy,
    createError,
    createStore,
  };
}
