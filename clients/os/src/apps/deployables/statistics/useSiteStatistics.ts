import type { Period } from "../../../kit/statistics/period";
import { fetchSiteStatistics, type SiteStatistics } from "./read";
import { useStatisticsRead, type StatisticsRead } from "./useStatisticsRead";

// One period of website statistics, read on demand and kept current, through
// the app's one statistics read (`useStatisticsRead`, which states its rules:
// not a live feed, what a read belongs to, a failure standing until a read
// answers). Regression: test/deployables/statistics.test.tsx, "keeps a
// refusal standing while its retry is out".

export type StatisticsReading = StatisticsRead<SiteStatistics>;

/**
 * How often a page re-reads: five minutes for the last 24 hours, whose hours
 * fill as they pass, fifteen for a period of days or months, which a few more
 * minutes barely move (spec section 7).
 */
export function statisticsRefreshMs(period: Period): number {
  return period === "24h" ? 300_000 : 900_000;
}

export function useSiteStatistics({ siteIds, accountIds, period, enabled }: {
  /** The deployables to read; absent reads every one the caller may read. */
  siteIds?: readonly string[];
  /** With no `siteIds`, keeps the read to these organizations; `[]` is all of them. */
  accountIds?: readonly string[];
  period: Period;
  /** False reads nothing: a system-owned or draft deployable, a page not ready to ask. */
  enabled: boolean;
}): StatisticsReading {
  const sites = siteIds === undefined ? undefined : [...siteIds];
  const accounts = accountIds === undefined ? undefined : [...accountIds];
  return useStatisticsRead({
    // THE IDENTITY, as it always was: the deployables, the organizations and the period.
    identity: [sites ?? null, accounts ?? null, period],
    enabled,
    fetch: (query, signal) => fetchSiteStatistics(query, { siteIds: sites, accountIds: accounts, period }, signal),
  });
}
