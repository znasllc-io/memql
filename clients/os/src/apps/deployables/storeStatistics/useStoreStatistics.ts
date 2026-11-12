import type { Period } from "../../../kit/statistics/period";
import { useStatisticsRead, type StatisticsRead } from "../statistics/useStatisticsRead";
import { fetchStoreStatistics, type StoreStatistics } from "./read";

// One store's statistics for one period, through the app's one statistics
// read (`statistics/useStatisticsRead.ts`, which states its rules). The
// identity is the site, the store and the period: a change of destination or
// of period drops what was held and aborts the read still out.
//
// THE CLIENT ASKS; IT DOES NOT DECIDE. Whether the person may see this store's
// sales is the engine's to answer: a refusal comes back as its own sentence,
// shown verbatim, and is never replaced by a guess.

export type StoreStatisticsReading = StatisticsRead<StoreStatistics>;

export function useStoreStatistics({ siteId, storeId, period, enabled }: {
  siteId: string;
  /** The store of one of the site's destinations, as the site holds it. */
  storeId: string;
  period: Period;
  /** False reads nothing. */
  enabled: boolean;
}): StoreStatisticsReading {
  return useStatisticsRead({
    identity: [siteId, storeId, period],
    enabled: enabled && siteId !== "" && storeId !== "",
    fetch: (query, signal) => fetchStoreStatistics(query, { siteId, storeId, period }, signal),
  });
}
