import { useId, useRef, useState } from "react";

import { AutoRefresh, Caption, Fact, Facts, Notice, Subhead } from "../../../kit";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { formatBytes, formatCount, formatRate } from "../../../kit/format";
import { LocalTabs } from "../../../kit/LocalTabs";
import { Measure } from "../../../kit/MeasureView";
import { absent, figureOf, type Figure } from "../../../kit/measure";
import { PageHelp } from "../../../kit/pageHelp";
import { PeriodControl, RankedList, SITE_PERIODS, StatFigures, TimeChart, TimeChartSkeleton, periodLabel, previousComparable, type ChartPoint, type Period, type StatFigure } from "../../../kit/statistics";
import { useWide } from "../../../kit/useWide";
import { useOsConnection } from "../../../live/connection";
import type { SiteRow } from "../rows";
import { useDeployablesSettings } from "../settingsContext";
import {
  DEVICE_TABS,
  SHOW_ALL_LABELS,
  SYSTEM_OWNED_TRAFFIC_NOTE,
  countingStartedIn,
  countingStartedSentence,
  formatMs,
  formatPerVisit,
  nothingRecorded,
  nothingRecordedSentence,
  rankedRows,
  type Dimension,
  type SitePoint,
  type SiteStatistics,
  type SiteTotals,
} from "./read";
import { statisticsRefreshMs, useSiteStatistics } from "./useSiteStatistics";
import "./statistics.css";

// A deployable's Statistics (the 2026-11-08 statistics design, section 9). It
// replaced the Traffic page, one strip of request columns and four facts.
//
// THE KIT'S STATISTICS LANGUAGE, AND NOTHING OF ITS OWN: the figure row
// (StatFigures), the one chart (TimeChart), the ranked lists (RankedList) and
// the period control, in the order the design gives -- who came (Visitors,
// Visits, Page views, Views per visit, Bounce rate) and the chart of the one
// chosen; where they went and came from (Pages, Sources, Countries, Devices);
// then Health, a change of subject under its own heading.
//
// ===========================================================================
// A CHANGE IS DRAWN ONLY AGAINST A PERIOD THAT CAN BE COMPARED WITH
// ===========================================================================
// `previousComparable`: the previous period exists (twelve months has none,
// D4) and began on or after the first measured day. A previous period that
// began before visitor counting holds days nothing counted, which add up as
// nothing, so every change against it would overstate growth. Then no figure
// carries a change and no chart draws a previous line -- Health's included,
// one rule for the page.
//
// ===========================================================================
// ABSENT IS NOT ZERO, AND SAYS WHY ONCE
// ===========================================================================
// A figure nothing measured is the kit's em dash. When the period began
// before visitor counting, the page says when counting started, once, and an
// absent visitor figure carries the same sentence as its reason; the chart's
// buckets before it are gaps. A period with no row at all says nothing was
// recorded and draws no figure, as the Traffic page did.

/** What a statistics list says after an empty answer. */
export const EMPTY_LIST = "Nothing in this period.";

/** Whether a deployable has statistics to read: the cluster's own surfaces are never recorded (D16), and a draft has never served. */
export function statisticsMeasured(site: SiteRow): boolean {
  return !site.systemOwned && site.status !== "draft";
}

/** One figure of a figure row, and how its series is read per bucket. */
export interface FigureSpec {
  id: string;
  label: string;
  total: (totals: SiteTotals) => number | null;
  point: (point: SitePoint) => number | null;
  format: (value: number) => string;
  /** A visitor figure: absent before visitor counting began, for that reason. */
  counted?: boolean;
}

/** A per-bucket rate: nothing to divide by is no rate, never zero. */
function ratio(numerator: number | null, denominator: number | null): number | null {
  return numerator === null || denominator === null || denominator <= 0 ? null : numerator / denominator;
}

/** Who came. A bucket's views per visit and bounce rate are worked out from its page views, visits and bounces. */
export const WEBSITE_FIGURES: readonly FigureSpec[] = [
  { id: "visitors", label: "Visitors", total: (t) => t.visitors, point: (p) => p.visitors, format: formatCount, counted: true },
  { id: "visits", label: "Visits", total: (t) => t.visits, point: (p) => p.visits, format: formatCount, counted: true },
  { id: "pageViews", label: "Page views", total: (t) => t.pageViews, point: (p) => p.pageViews, format: formatCount, counted: true },
  { id: "viewsPerVisit", label: "Views per visit", total: (t) => t.viewsPerVisit, point: (p) => ratio(p.pageViews, p.visits), format: formatPerVisit, counted: true },
  { id: "bounceRate", label: "Bounce rate", total: (t) => t.bounceRate, point: (p) => ratio(p.bounces, p.visits), format: formatRate, counted: true },
];

/** How the site served. */
const HEALTH_FIGURES: readonly FigureSpec[] = [
  { id: "requests", label: "Requests", total: (t) => t.requests, point: (p) => p.requests, format: formatCount },
  { id: "errors", label: "Errors", total: (t) => t.errors, point: (p) => p.errors, format: formatCount },
  { id: "notFound", label: "Not found", total: (t) => t.notFound, point: (p) => p.notFound, format: formatCount },
  { id: "p95", label: "Response time, p95", total: (t) => t.p95Ms, point: (p) => p.p95Ms, format: formatMs },
];

function measuredOrAbsent(value: number | null, reason = ""): Figure {
  return value === null ? absent("unmeasured", reason) : figureOf(value);
}

/**
 * A figure row from a read, or its shapes while there is none. The previous
 * period's figures are given only when it can be compared with.
 */
export function figuresOf(specs: readonly FigureSpec[], stats: SiteStatistics | null): StatFigure[] {
  if (stats === null) return specs.map((spec) => ({ id: spec.id, label: spec.label, figure: absent("unread"), format: spec.format, chartable: true, loading: true }));
  const comparable = previousComparable(stats.prevStart, stats.measuredSince);
  const counting = countingStartedIn(stats);
  return specs.map((spec) => {
    const before = spec.counted && counting !== null ? countingStartedSentence(counting) : "";
    const previous = comparable && stats.previous !== null ? measuredOrAbsent(spec.total(stats.previous)) : undefined;
    return {
      id: spec.id,
      label: spec.label,
      figure: measuredOrAbsent(spec.total(stats.totals), before),
      ...(previous !== undefined ? { previous } : {}),
      format: spec.format,
      chartable: true,
    };
  });
}

function pointsOf(spec: FigureSpec, series: readonly SitePoint[]): ChartPoint[] {
  return series.map((point) => ({ at: point.at, value: spec.point(point) }));
}

/**
 * The chart of one figure, or its shape while there is no read: the kit's
 * chart has no loading state of its own, and its shape is not words.
 */
export function FigureChart({ specs, selected, stats }: { specs: readonly FigureSpec[]; selected: string; stats: SiteStatistics | null }) {
  const spec = specs.find((candidate) => candidate.id === selected) ?? specs[0]!;
  if (stats === null) {
    return <TimeChartSkeleton label={`Loading ${spec.label.toLowerCase()} over time`} />;
  }
  const comparable = previousComparable(stats.prevStart, stats.measuredSince);
  return (
    <TimeChart
      label={spec.label}
      bucket={stats.bucket}
      points={pointsOf(spec, stats.series)}
      previous={comparable && stats.previousSeries !== null ? pointsOf(spec, stats.previousSeries) : undefined}
      format={spec.format}
    />
  );
}

/** The country data's attribution, under every Countries list (CC BY 4.0, D8). */
export function DbIpCredit() {
  return <a href="https://db-ip.com" target="_blank" rel="noreferrer">IP Geolocation by DB-IP</a>;
}

/**
 * What the figures mean, said once in the window's one information control
 * (DESIGN.md "Applying them"), never as standing copy on the page.
 */
export function StatisticsHelp({ title, health = true }: { title: string; /** Whether the page has a Health section to point bots at. */ health?: boolean }) {
  return (
    <PageHelp title={title}>
      <p>A visitor is counted once a day, without cookies, so somebody who comes back on three days counts three times in a week. Days are UTC, from midnight UTC.</p>
      <p>A visit is one visitor&rsquo;s page views with no gap of more than 30 minutes. A bounce is a visit of one page.</p>
      <p>A page view is a page load, or a page change inside a single-page app.</p>
      <p>Bots are left out of these figures{health ? " and counted under Health" : ""}. A bot that passes for a browser is still counted as a visitor.</p>
      <p>Countries come from DB-IP&rsquo;s free database.</p>
    </PageHelp>
  );
}

/** A refused read, in the engine's own words, with the retry that is already coming. */
export function StatisticsRefusal({ error }: { error: string }) {
  return <Notice tone="error" sentence="Statistics could not be loaded." detail={error || undefined} next="Retrying automatically." />;
}

/**
 * The period a deployable's statistics are read for, at the right of the
 * Statistics page's Head (spec 8), remembered in the app's settings document.
 */
export function StatisticsPeriodControl() {
  const id = useId();
  const { settings, update } = useDeployablesSettings();
  return (
    <PeriodControl
      id={`deployable-statistics-period${id}`}
      value={settings.statisticsPeriod}
      periods={SITE_PERIODS}
      onChange={(period) => update({ statisticsPeriod: period })}
    />
  );
}

export function SiteStatisticsView({ site, onShowAll }: { site: SiteRow; onShowAll: (dimension: Dimension) => void }) {
  const { settings } = useDeployablesSettings();
  if (site.systemOwned) {
    return (
      <section className="os-report-part">
        <Caption>{SYSTEM_OWNED_TRAFFIC_NOTE}</Caption>
      </section>
    );
  }
  // A DRAFT HAS NEVER SERVED ANYTHING, and figures about nobody visiting would
  // answer a question it is too early to ask. Its page offers no Statistics
  // (DeployablePage); this holds for a page opened some other way.
  if (!statisticsMeasured(site)) return null;
  return <SiteStatisticsBody siteId={site.id} period={settings.statisticsPeriod} onShowAll={onShowAll} />;
}

/**
 * The page itself, mounted only for a deployable that has statistics, so the
 * element whose width decides the lists' columns exists from its first render
 * (kit `useWide` measures the element it is given when it mounts).
 */
function SiteStatisticsBody({ siteId, period, onShowAll }: { siteId: string; period: Period; onShowAll: (dimension: Dimension) => void }) {
  const connection = useOsConnection();
  const reading = useSiteStatistics({ siteIds: [siteId], period, enabled: siteId !== "" });
  const root = useRef<HTMLDivElement | null>(null);
  // TWO COLUMNS FROM 760px OF PANE: Pages beside Sources, then Countries
  // beside Devices; one column below that.
  const wide = useWide(root, 760);
  const [selected, setSelected] = useState("visitors");
  const [health, setHealth] = useState("requests");
  const [devices, setDevices] = useState<"device" | "browser" | "os">("device");

  const stats = reading.value;
  // No answer yet: the page draws the SHAPES of its figures, chart and lists.
  const pending = stats === null && (reading.state === "idle" || reading.state === "loading");
  // An answer with no row in the period at all.
  const nothing = stats === null ? reading.state === "ready" : nothingRecorded(stats);
  const counting = stats === null ? null : countingStartedIn(stats);
  const list = (dimension: Dimension) => (stats === null ? [] : rankedRows(stats.top[dimension], dimension));

  return (
    <div ref={root} className="deployable-statistics">
      <AutoRefresh onRefresh={reading.reload} busy={reading.state === "loading"} enabled={connection !== null} intervalMs={statisticsRefreshMs(period)} />
      <StatisticsHelp title="Statistics" />
      {reading.state === "error" ? <StatisticsRefusal error={reading.error} /> : null}
      {nothing ? (
        <Caption>{nothingRecordedSentence(periodLabel(period).toLowerCase())}</Caption>
      ) : stats === null && !pending ? null : (
        <>
          <div className="deployable-statistics-figures">
            <StatFigures label="Website figures" figures={figuresOf(WEBSITE_FIGURES, stats)} selected={selected} onSelect={setSelected} />
            {counting !== null ? <Caption>{countingStartedSentence(counting)}</Caption> : null}
            <FigureChart specs={WEBSITE_FIGURES} selected={selected} stats={stats} />
          </div>

          <div className="deployable-statistics-lists" data-wide={wide || undefined}>
            <RankedList title="Pages" rows={list("page")} onShowAll={() => onShowAll("page")} showAllLabel={SHOW_ALL_LABELS.page} loading={stats === null} empty={EMPTY_LIST} />
            <RankedList title="Sources" rows={list("source")} onShowAll={() => onShowAll("source")} showAllLabel={SHOW_ALL_LABELS.source} loading={stats === null} empty={EMPTY_LIST} />
            <RankedList title="Countries" rows={list("country")} onShowAll={() => onShowAll("country")} showAllLabel={SHOW_ALL_LABELS.country} loading={stats === null} empty={EMPTY_LIST} footer={<DbIpCredit />} />
            <RankedList
              title="Devices"
              rows={list(devices)}
              onShowAll={() => onShowAll(devices)}
              showAllLabel={SHOW_ALL_LABELS[devices]}
              loading={stats === null}
              empty={EMPTY_LIST}
              control={
                // THE TABS SWITCH ONE LIST IN PLACE, not the page, so the shared
                // tabs' return to the top of the page (kit/pageScroll, "Tabs
                // start at the top") must not apply: the person is halfway down it.
                <div className="deployable-statistics-device-tabs" data-os-scroll-ignore>
                  <LocalTabs label="Device lists" value={devices} onChange={setDevices} options={DEVICE_TABS} />
                </div>
              }
            />
          </div>

          <section className="deployable-statistics-health" aria-label="Health">
            <Subhead>Health</Subhead>
            <StatFigures label="Health figures" figures={figuresOf(HEALTH_FIGURES, stats)} selected={health} onSelect={setHealth} />
            <FigureChart specs={HEALTH_FIGURES} selected={health} stats={stats} />
            <Facts>
              <Fact label="Data served" value={factOf(stats, (t) => t.bytes, formatBytes)} />
              <Fact label="Median response" value={factOf(stats, (t) => t.medianMs, formatMs)} />
              <Fact label="Bot requests" value={factOf(stats, (t) => t.botRequests, formatCount)} />
            </Facts>
          </section>
        </>
      )}
    </div>
  );
}

/** A health fact: its figure, the em dash when nothing measured it, its shape while there is no read. */
function factOf(stats: SiteStatistics | null, total: (totals: SiteTotals) => number | null, format: (value: number) => string) {
  if (stats === null) return <InlineSkeleton />;
  return <Measure figure={measuredOrAbsent(total(stats.totals))} format={format} />;
}
