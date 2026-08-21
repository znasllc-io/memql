import { useCallback, useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

// One site's detail + the four row-level actions the brief scopes: publish
// / roll back (both are updateSiteBundle -- ruling 1: "publish" IS the row
// flip, there is no upload path here), enable/disable (updateSiteStatus),
// and delete (deleteSite, blocked server-side for a systemOwned row --
// ruling 3 -- so a refusal here is the real gate working, not a UI bug).
//
// Read through the NAMED siteById query rather than the generic
// getRowByConceptAndId useRowDetail uses: siteById is the seam this issue
// adds specifically so the rollback picker (useSiteHistory) can wrap it in
// asOf(); reading the CURRENT row through the same query keeps "the current
// state" and "version one of the history" answers from two different code
// paths that could disagree.

export interface SiteDetailState {
  site: Row | null;
  loading: boolean;
  error: string;
  actionMessage: string;
  actionError: string;
  busy: boolean;
  // True once deleteSite has succeeded. The page navigates away on this --
  // there is nothing left at this address to show.
  deleted: boolean;
  reload: () => void;
  publish: (bundleRef: string) => void;
  setStatus: (status: string) => void;
  remove: () => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useSiteDetail(siteId: string): SiteDetailState {
  const { query } = useCluster();
  const [site, setSite] = useState<Row | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [actionMessage, setActionMessage] = useState("");
  const [actionError, setActionError] = useState("");
  const [busy, setBusy] = useState(false);
  const [deleted, setDeleted] = useState(false);
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null || siteId === "") {
      setSite(null);
      setLoading(false);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void query
      .siteById({ siteId })
      .then((result) => {
        if (live) setSite(result.rows()[0] ?? null);
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, siteId, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  // run funnels the two non-terminal actions (publish, setStatus) through
  // one place so busy/message handling and the follow-up re-read cannot be
  // forgotten on the second one. remove() is deliberately NOT run through
  // this -- a successful delete has no row left to reload.
  const run = useCallback((what: Promise<unknown> | null, done: string) => {
    if (what === null) return;
    setBusy(true);
    setActionMessage("");
    setActionError("");
    void what
      .then(() => setActionMessage(done))
      .catch((err: unknown) => setActionError(describe(err)))
      .finally(() => {
        setBusy(false);
        setEpoch((n) => n + 1);
      });
  }, []);

  const publish = useCallback(
    (bundleRef: string) =>
      run(
        query ? query.updateSiteBundle({ siteId, bundleRef }) : null,
        "Published. The edge resolves the new bundle on its next cache miss for this hostname.",
      ),
    [query, run, siteId],
  );

  const setStatus = useCallback(
    (status: string) =>
      run(
        query ? query.updateSiteStatus({ siteId, status }) : null,
        `Status set to ${status}.`,
      ),
    [query, run, siteId],
  );

  const remove = useCallback(() => {
    if (query === null) return;
    setBusy(true);
    setActionMessage("");
    setActionError("");
    void query
      .deleteSite({ siteId })
      .then(() => {
        setActionMessage("Site deleted.");
        setDeleted(true);
      })
      .catch((err: unknown) => setActionError(describe(err)))
      .finally(() => setBusy(false));
  }, [query, siteId]);

  return {
    site,
    loading,
    error,
    actionMessage,
    actionError,
    busy,
    deleted,
    reload,
    publish,
    setStatus,
    remove,
  };
}
