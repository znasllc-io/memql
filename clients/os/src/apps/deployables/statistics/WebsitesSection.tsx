import { useCallback, useRef, useState } from "react";

import { AutoRefresh, Caption, Subhead } from "../../../kit";
import { formatCount } from "../../../kit/format";
import { PeriodControl, RankedList, SITE_PERIODS, StatFigures, periodLabel, type RankedRow } from "../../../kit/statistics";
import { useWide } from "../../../kit/useWide";
import { useOsConnection } from "../../../live/connection";
import { useClientScope } from "../../accounts/clientScope";
import { ScopeControl } from "../../accounts/ScopeControl";
import type { SiteRow } from "../rows";
import { useDeployablesSettings } from "../settingsContext";
import { countingStartedIn, countingStartedSentence, nothingRecorded, nothingRecordedSentence, rankedRows, type SiteStatistics } from "./read";
import { DbIpCredit, EMPTY_LIST, FigureChart, StatisticsHelp, StatisticsRefusal, WEBSITE_FIGURES, figuresOf } from "./SiteStatisticsView";
import { statisticsRefreshMs, useSiteStatistics } from "./useSiteStatistics";
import "./statistics.css";

// EVERY WEBSITE A PERSON MAY SEE, TOGETHER (the 2026-11-08 statistics design,
// section 9, D14): the Deployables Overview's Websites section. Who came to
// all of them (Visitors, Visits, Page views), the chart, each website ranked
// -- a row opens that deployable's Statistics -- and where they came from.
//
// ONE READ, NAMING NO DEPLOYABLE. `siteStatistics` with no `siteIds` sums
// every deployable the caller may read, kept to the organization chosen, so
// the page never pages through ids to add them up, and a deployable a person
// cannot read is never in the sum (D11: the server narrows, not the picker).
//
// THE CLIENT CONTROL IS THE SCOPE'S (apps/accounts/clientScope, D13): drawn
// only where there is a choice, and remembered in this app's settings beside
// the period, which is the one every deployable's Statistics page reads too.

/** The Overview's figures: who came. The rates are each website's own page. */
const OVERVIEW_FIGURES = WEBSITE_FIGURES.filter((spec) => spec.id === "visitors" || spec.id === "visits" || spec.id === "pageViews");

/** How many websites the list shows; the figures above add up every one in scope. */
const WEBSITES_SHOWN = 8;

/**
 * Each website in the read, named by its title, else its hostname, and
 * valued by its visitors -- else its page views, said so on the row, because
 * a figure beside its neighbours must be the same kind of figure or say it is
 * not. A website the feed does not hold yet is named by its id and opens
 * nothing.
 *
 * A WEBSITE NEITHER OF WHOSE FIGURES WAS MEASURED IS LEFT OUT: there is
 * nothing to rank it by, and the figures above do not count it either. A row
 * of "--" with an empty bar would read as a website nobody visited.
 */
function websiteRows(stats: SiteStatistics, sites: readonly SiteRow[], onOpen: (siteId: string) => void): RankedRow[] {
  return stats.sites.flatMap((entry) => {
    const value = entry.visitors ?? entry.pageViews;
    if (value === null) return [];
    const site = sites.find((candidate) => candidate.id === entry.siteId);
    const label = site === undefined ? entry.siteId : site.title.trim() || site.hostname.trim() || entry.siteId;
    return [{
      key: entry.siteId,
      label,
      value,
      ...(entry.visitors === null ? { secondary: "Page views" } : {}),
      ...(site === undefined ? {} : { onOpen: () => onOpen(site.id) }),
    }];
  });
}

/**
 * The list's quiet last line when it stops short of every website: "The 8
 * busiest of 23 websites." Without it, eight rows under figures that add up
 * twenty-three websites read as all of them.
 */
function busiestOf(rows: readonly RankedRow[]): string | undefined {
  return rows.length > WEBSITES_SHOWN ? `The ${WEBSITES_SHOWN} busiest of ${formatCount(rows.length)} websites.` : undefined;
}

export function WebsitesSection({ sites, onOpenStatistics }: { sites: readonly SiteRow[]; onOpenStatistics: (siteId: string) => void }) {
  // THE CLUSTER'S OWN SURFACES ARE NEVER RECORDED (D16): with nothing else,
  // there is no website to show and no read to make.
  if (!sites.some((site) => !site.systemOwned)) return null;
  return <Websites sites={sites} onOpenStatistics={onOpenStatistics} />;
}

/** The section itself, mounted only when there is a website, so the element its lists' columns are measured from exists from its first render. */
function Websites({ sites, onOpenStatistics }: { sites: readonly SiteRow[]; onOpenStatistics: (siteId: string) => void }) {
  const connection = useOsConnection();
  const { settings, update } = useDeployablesSettings();
  const period = settings.statisticsPeriod;
  const storeScope = useCallback((value: string) => update({ statisticsScope: value }), [update]);
  const scope = useClientScope(settings.statisticsScope, storeScope);
  // `scope.accountIds` AS IT IS: for staff it holds the stored client from the
  // first render, before the account list answers (clientScope.ts).
  const reading = useSiteStatistics({ accountIds: scope.accountIds, period, enabled: true });
  const root = useRef<HTMLElement | null>(null);
  const wide = useWide(root, 760);
  const [selected, setSelected] = useState("visitors");

  const stats = reading.value;
  const pending = stats === null && (reading.state === "idle" || reading.state === "loading");
  const nothing = stats === null ? reading.state === "ready" : nothingRecorded(stats);
  const counting = stats === null ? null : countingStartedIn(stats);
  const websites = stats === null ? [] : websiteRows(stats, sites, onOpenStatistics);

  return (
    <section ref={root} className="deployable-statistics deployable-websites" aria-label="Website statistics">
      <AutoRefresh onRefresh={reading.reload} busy={reading.state === "loading"} enabled={connection !== null} intervalMs={statisticsRefreshMs(period)} />
      <StatisticsHelp title="Websites" health={false} />
      <Subhead
        actions={
          <>
            <ScopeControl id="deployables-websites-scope" scope={scope} />
            <PeriodControl id="deployables-websites-period" value={period} periods={SITE_PERIODS} onChange={(next) => update({ statisticsPeriod: next })} />
          </>
        }
      >
        Websites
      </Subhead>
      {reading.state === "error" ? <StatisticsRefusal error={reading.error} /> : null}
      {nothing ? (
        <Caption>{nothingRecordedSentence(periodLabel(period).toLowerCase())}</Caption>
      ) : stats === null && !pending ? null : (
        <>
          <div className="deployable-statistics-figures">
            <StatFigures label="Website figures" figures={figuresOf(OVERVIEW_FIGURES, stats)} selected={selected} onSelect={setSelected} />
            {counting !== null ? <Caption>{countingStartedSentence(counting)}</Caption> : null}
            <FigureChart specs={OVERVIEW_FIGURES} selected={selected} stats={stats} />
          </div>
          <RankedList title="Websites" rows={websites} limit={WEBSITES_SHOWN} loading={stats === null} empty={EMPTY_LIST} footer={busiestOf(websites)} />
          <div className="deployable-statistics-lists" data-wide={wide || undefined}>
            <RankedList title="Sources" rows={stats === null ? [] : rankedRows(stats.top.source, "source")} loading={stats === null} empty={EMPTY_LIST} />
            <RankedList title="Countries" rows={stats === null ? [] : rankedRows(stats.top.country, "country")} loading={stats === null} empty={EMPTY_LIST} footer={<DbIpCredit />} />
          </div>
        </>
      )}
    </section>
  );
}
