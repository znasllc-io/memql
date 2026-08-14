import { useCallback, useEffect, useState } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import { fetchSiteVersionHistory, type SiteVersion } from "./history";

export interface SiteHistoryState {
  versions: SiteVersion[];
  loading: boolean;
  error: string;
  reload: () => void;
}

// The rollback picker's data: fetchSiteVersionHistory's asOf walk, wired to
// the connection and re-run whenever the caller bumps reload() (after a
// publish, so the newly published version enters the history the next time
// the picker is opened).
export function useSiteHistory(siteId: string): SiteHistoryState {
  const { query } = useCluster();
  const [versions, setVersions] = useState<SiteVersion[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null || siteId === "") {
      setVersions([]);
      setLoading(false);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void fetchSiteVersionHistory(query, siteId)
      .then((next) => {
        if (live) setVersions(next);
      })
      .catch((err: unknown) => {
        if (live) setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, siteId, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { versions, loading, error, reload };
}
