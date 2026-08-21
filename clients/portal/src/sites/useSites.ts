import { useCallback, useState } from "react";
import { newShortId, type Role } from "@znasllc-io/memql-sdk-core/client";

import { omitBlank } from "../cluster/args";
import { useCluster } from "../cluster/ClusterProvider";
import { useConceptRows, type ConceptRowsState } from "../cluster/useConceptRows";
import { useMyAccess } from "../cluster/useMyAccess";
import { SITE_CONCEPT_ID } from "./concepts";

// The sites LIST screen's state: the live row walk plus the create action.
//
// THE LIST IS LIVE BY CONSTRUCTION, not by an extra wire-up here --
// useConceptRows already combines a paged browse with subscribeGraph
// (cluster/useConceptRows.ts), so reusing it is what makes "create a site,
// see it appear without a refresh" true. createSite deliberately does NOT
// force a reload of the walk on success: the point of this screen is the
// live subscription actually carrying the new row, and a manual refetch
// here would paper over that path working rather than exercise it.
//
// AUTHORIZATION IS A COURTESY HERE, same as every gated screen in the
// portal (see admin/useAdminConsole.ts's header): v1:platform:site is
// clusterOwner-tier (D6) and BOTH declared queries carry
// actor.isClusterOwner==true as an explicit conjunct, so a non-owner's read
// -- named query or the generic browse useConceptRows issues -- comes back
// empty at the engine regardless of what isOwner reads here. createSite is
// refused the same way at the write path. isOwner only decides what this
// screen OFFERS.

export interface CreateSiteInput {
  hostname: string;
  kind: string;
  bundleRef: string;
}

export interface SitesState {
  rows: ConceptRowsState;
  role: Role;
  isOwner: boolean;
  accessResolved: boolean;
  createBusy: boolean;
  createError: string;
  createSite: (input: CreateSiteInput) => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useSites(): SitesState {
  const { query } = useCluster();
  const rows = useConceptRows(SITE_CONCEPT_ID);
  const { access, loading: accessLoading } = useMyAccess();
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState("");

  const createSite = useCallback(
    (input: CreateSiteInput) => {
      if (query === null) return;
      setCreateBusy(true);
      setCreateError("");
      void query
        .createSite({
          siteId: newShortId(),
          hostname: input.hostname,
          kind: omitBlank(input.kind),
          bundleRef: input.bundleRef,
        })
        .catch((err: unknown) => setCreateError(describe(err)))
        .finally(() => setCreateBusy(false));
    },
    [query],
  );

  const role: Role = access?.clusterRole ?? "";
  return {
    rows,
    role,
    isOwner: role === "owner",
    accessResolved: !accessLoading && access !== null,
    createBusy,
    createError,
    createSite,
  };
}
