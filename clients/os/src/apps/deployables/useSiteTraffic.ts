import { useContext, useEffect, useMemo } from "react";
import { useLiveCollection } from "../../live/useLiveCollection";
import { LiveReadVisibility } from "../../live/useGraphRevision";
import { fetchTrafficSummaries, windowSpec, type TrafficSummary, type TrafficWindow } from "./traffic";

interface SummarySnapshot { id: string; figures: Array<[string, TrafficSummary]>; readAt: string }

/** One bounded request for the visible ids. Shared snapshots survive reopening;
 * hidden panes stop their cadence, and obsolete requests cannot fill a new key. */
export function useSiteTraffic(siteIds: readonly string[], window: TrafficWindow) {
  const visible = useContext(LiveReadVisibility);
  const ids = [...new Set(siteIds)].sort();
  const key = JSON.stringify(ids);
  const feed = useLiveCollection<SummarySnapshot>(ids.length ? `deployables:traffic:${window}:${key}` : null, connection => ({
    concept: "v1:observability:siteTraffic", actions: [], paged: false,
    seed: async (_cursor, signal) => {
      const now = new Date();
      const figures = await fetchTrafficSummaries(connection.query, ids, window, now, signal);
      return { rows: [{ id: "summary", figures: [...figures], readAt: now.toISOString() }], nextCursor: "" };
    },
  }));
  useEffect(() => {
    if (!visible || !ids.length) return;
    const timer = setInterval(feed.reseed, windowSpec(window).refreshMs);
    return () => clearInterval(timer);
  }, [visible, key, window, feed.reseed]);
  const row = feed.snapshot.rows[0];
  const figures = useMemo(() => new Map(row?.figures ?? []), [row]);
  return { figures, readAt: row?.readAt ?? "" };
}
