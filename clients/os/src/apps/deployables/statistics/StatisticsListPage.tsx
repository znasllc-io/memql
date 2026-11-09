import { useEffect, useState } from "react";

import { AutoRefresh } from "../../../kit";
import { LocalTabs } from "../../../kit/LocalTabs";
import { RankedList, periodLabel } from "../../../kit/statistics";
import { useOsConnection } from "../../../live/connection";
import type { SiteRow } from "../rows";
import { useDeployablesSettings } from "../settingsContext";
import { DEVICE_TABS, isDeviceDimension, rankedRows, type Dimension } from "./read";
import { DbIpCredit, EMPTY_LIST, StatisticsHelp, StatisticsRefusal, statisticsMeasured } from "./SiteStatisticsView";
import { statisticsRefreshMs, useSiteStatistics } from "./useSiteStatistics";
import "./statistics.css";

// One of a deployable's lists, whole: what "All pages" (sources, countries,
// devices) opens, as a page in the deployable's place under the trail
// Deployables > <name> > Statistics > Pages -- never a pop-up (spec 10).
//
// THE PAGE'S HEAD NAMES THE LIST, so the list's own heading names the period
// it covers instead: the kit's list always carries a heading, and "Pages"
// twice would say one thing twice (DESIGN.md rule 7). The period is changed
// on the Statistics page, one step back; this page reads the one remembered.
//
// IT READS FOR ITSELF, the same read the Statistics page made, so it holds
// nothing across the hop and a refresh keeps it current; the engine's result
// cache answers the repeat.
//
// THE DEVICES LIST KEEPS ITS THREE VIEWS: the tabs stand under the Head, as
// on any page with sibling views, opening on the one whose "All devices" was
// pressed.

export function StatisticsListPage({ site, dimension }: { site: SiteRow; dimension: Dimension }) {
  const connection = useOsConnection();
  const { settings } = useDeployablesSettings();
  const period = settings.statisticsPeriod;
  const measured = statisticsMeasured(site);
  const [shown, setShown] = useState<Dimension>(dimension);
  useEffect(() => setShown(dimension), [dimension]);
  const reading = useSiteStatistics({ siteIds: [site.id], period, enabled: measured && site.id !== "" });
  if (!measured) return null;

  const stats = reading.value;
  return (
    <div className="deployable-statistics">
      <AutoRefresh onRefresh={reading.reload} busy={reading.state === "loading"} enabled={connection !== null} intervalMs={statisticsRefreshMs(period)} />
      <StatisticsHelp title="Statistics" />
      {isDeviceDimension(shown) ? <LocalTabs label="Device lists" value={shown} onChange={setShown} options={DEVICE_TABS} /> : null}
      {reading.state === "error" ? <StatisticsRefusal error={reading.error} /> : null}
      {stats === null && reading.state === "error" ? null : (
        <RankedList
          title={periodLabel(period)}
          rows={stats === null ? [] : rankedRows(stats.top[shown], shown)}
          limit={Number.POSITIVE_INFINITY}
          loading={stats === null && (reading.state === "idle" || reading.state === "loading")}
          empty={EMPTY_LIST}
          footer={shown === "country" ? <DbIpCredit /> : undefined}
        />
      )}
    </div>
  );
}
