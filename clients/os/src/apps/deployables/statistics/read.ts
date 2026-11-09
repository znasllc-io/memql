import type { QueryClient, Row } from "@znasllc-io/memql-sdk-core/client";

import { SITE_PERIODS, bucketLabel, isPeriod, type Bucket, type Period } from "../../../kit/statistics/period";
import type { RankedRow } from "../../../kit/statistics/RankedList";

// A website's statistics, as this app reads them (the 2026-11-08 statistics
// design, section 7): the `siteStatistics` read for one deployable's
// Statistics page, and for every website a person may see on the Overview.
//
// PURE, and separate from every component, for `rows.ts`' reason: what a
// payload MEANS is asserted on functions, not through three layers of render.
//
// ===========================================================================
// A JSON NULL STAYS NULL
// ===========================================================================
// The read writes every figure nothing measured PRESENT AND NULL
// (component/sitetraffic, `Statistics.Payload()`): a period before the edge
// counted visitors, a bucket before a deployable's first row, a rate with no
// visit to divide by. So the parse keeps null as null at every level and never
// goes through `rowNumber()`, which reads null as 0. A zero is a measurement;
// null is the absence of one, and the page draws the two differently.

/** The dimensions the read keeps a top list for (component/sitetraffic, `topDimensions`). */
export type Dimension = "page" | "source" | "country" | "device" | "browser" | "os";

const DIMENSIONS: readonly Dimension[] = ["page", "source", "country", "device", "browser", "os"];

/** A period's figures. Null is "not measured". */
export interface SiteTotals {
  visitors: number | null;
  visits: number | null;
  bounces: number | null;
  pageViews: number | null;
  viewsPerVisit: number | null;
  bounceRate: number | null;
  requests: number | null;
  errors: number | null;
  notFound: number | null;
  bytes: number | null;
  botRequests: number | null;
  medianMs: number | null;
  p95Ms: number | null;
}

/** One bucket of a series, starting `at`. Null is "not measured". */
export interface SitePoint {
  at: string;
  visitors: number | null;
  visits: number | null;
  bounces: number | null;
  pageViews: number | null;
  requests: number | null;
  errors: number | null;
  notFound: number | null;
  p95Ms: number | null;
}

/** One value of a top list, summed over the period. */
export interface TopEntry {
  value: string;
  visitors: number;
  visits: number;
  pageViews: number;
}

/** One deployable's own figures over the period, for the Websites list. */
export interface SiteFigures {
  siteId: string;
  visitors: number | null;
  visits: number | null;
  pageViews: number | null;
}

/**
 * One read's answer.
 *
 * `hasPrevious`, `prevStart` and `prevEnd` are the payload's too, beside the
 * fields the page draws: whether the previous period can be compared with at
 * all is decided from `prevStart` against `measuredSince`
 * (kit `previousComparable`), not from `previous` being present.
 */
export interface SiteStatistics {
  period: Period;
  bucket: Bucket;
  start: string;
  end: string;
  hasPrevious: boolean;
  /** The previous period's start, RFC3339; null without one (twelve months). */
  prevStart: string | null;
  prevEnd: string | null;
  /** The first UTC day with visitor figures, "YYYY-MM-DD"; null when none has any. */
  measuredSince: string | null;
  totals: SiteTotals;
  /** Null without a previous period (twelve months, spec D4). */
  previous: SiteTotals | null;
  series: SitePoint[];
  /** Null without a previous period. */
  previousSeries: SitePoint[] | null;
  top: Record<Dimension, TopEntry[]>;
  sites: SiteFigures[];
}

/**
 * A figure off the wire: a finite number, or a numeric string (the wire may
 * carry a large count as one); anything else, null included, is null.
 */
function figure(raw: unknown): number | null {
  if (typeof raw === "number") return Number.isFinite(raw) ? raw : null;
  if (typeof raw === "string" && raw.trim() !== "") {
    const parsed = Number(raw);
    return Number.isFinite(parsed) ? parsed : null;
  }
  return null;
}

function text(raw: unknown): string {
  return typeof raw === "string" ? raw : "";
}

function textOrNull(raw: unknown): string | null {
  return typeof raw === "string" && raw !== "" ? raw : null;
}

function object(raw: unknown): Record<string, unknown> | null {
  return raw !== null && typeof raw === "object" && !Array.isArray(raw) ? (raw as Record<string, unknown>) : null;
}

function list(raw: unknown): unknown[] {
  return Array.isArray(raw) ? raw : [];
}

function totalsFrom(raw: Record<string, unknown>): SiteTotals {
  return {
    visitors: figure(raw["visitors"]),
    visits: figure(raw["visits"]),
    bounces: figure(raw["bounces"]),
    pageViews: figure(raw["pageViews"]),
    viewsPerVisit: figure(raw["viewsPerVisit"]),
    bounceRate: figure(raw["bounceRate"]),
    requests: figure(raw["requests"]),
    errors: figure(raw["errors"]),
    notFound: figure(raw["notFound"]),
    bytes: figure(raw["bytes"]),
    botRequests: figure(raw["botRequests"]),
    medianMs: figure(raw["medianMs"]),
    p95Ms: figure(raw["p95Ms"]),
  };
}

function pointsFrom(raw: unknown): SitePoint[] {
  return list(raw).flatMap((entry) => {
    const point = object(entry);
    if (point === null) return [];
    return [{
      at: text(point["at"]),
      visitors: figure(point["visitors"]),
      visits: figure(point["visits"]),
      bounces: figure(point["bounces"]),
      pageViews: figure(point["pageViews"]),
      requests: figure(point["requests"]),
      errors: figure(point["errors"]),
      notFound: figure(point["notFound"]),
      p95Ms: figure(point["p95Ms"]),
    }];
  });
}

/**
 * A top list. The read never sends an entry without its counts, so one that
 * arrives without them is DROPPED rather than ranked as nothing: a value that
 * was seen and could not be counted is not a value seen by nobody.
 */
function entriesFrom(raw: unknown): TopEntry[] {
  return list(raw).flatMap((entry) => {
    const item = object(entry);
    const value = text(item?.["value"]);
    const visitors = figure(item?.["visitors"]);
    const visits = figure(item?.["visits"]);
    const pageViews = figure(item?.["pageViews"]);
    if (value === "" || visitors === null || visits === null || pageViews === null) return [];
    return [{ value, visitors, visits, pageViews }];
  });
}

function sitesFrom(raw: unknown): SiteFigures[] {
  return list(raw).flatMap((entry) => {
    const site = object(entry);
    const siteId = text(site?.["siteId"]);
    if (site === null || siteId === "") return [];
    return [{ siteId, visitors: figure(site["visitors"]), visits: figure(site["visits"]), pageViews: figure(site["pageViews"]) }];
  });
}

/** The bucket a period is read in (core/statsperiod): what a payload that names none is read as. */
function bucketOf(period: Period): Bucket {
  return period === "24h" ? "hour" : period === "12m" ? "month" : "day";
}

/**
 * Parse the read's one row: the payload `Statistics.Payload()` writes,
 * flattened onto the row.
 *
 * `requested` is the period the caller asked for. The read echoes it back; it
 * labels an answer that does not, so a payload naming no period the page
 * offers is still read as the period that was asked about.
 */
export function parseSiteStatistics(row: Row, requested: Period = "7d"): SiteStatistics {
  const period = isPeriod(row["period"], SITE_PERIODS) ? row["period"] : requested;
  const bucket = row["bucket"] === "hour" || row["bucket"] === "day" || row["bucket"] === "month" ? row["bucket"] : bucketOf(period);
  const previous = object(row["previous"]);
  const top = object(row["top"]);
  return {
    period,
    bucket,
    start: text(row["start"]),
    end: text(row["end"]),
    hasPrevious: row["hasPrevious"] === true,
    prevStart: textOrNull(row["prevStart"]),
    prevEnd: textOrNull(row["prevEnd"]),
    measuredSince: textOrNull(row["measuredSince"]),
    totals: totalsFrom(object(row["totals"]) ?? {}),
    previous: previous === null ? null : totalsFrom(previous),
    series: pointsFrom(row["series"]),
    previousSeries: Array.isArray(row["previousSeries"]) ? pointsFrom(row["previousSeries"]) : null,
    top: Object.fromEntries(DIMENSIONS.map((dimension) => [dimension, entriesFrom(top?.[dimension])])) as Record<Dimension, TopEntry[]>,
    sites: sitesFrom(row["sites"]),
  };
}

/**
 * Read one period of statistics: one deployable (`siteIds: [id]`), or every
 * deployable the caller may read (no `siteIds`), kept to `accountIds` when
 * named. Through the generated builder, so the call string is the one the
 * engine parses. Null when the read answered no row.
 */
export async function fetchSiteStatistics(
  query: QueryClient,
  args: { siteIds?: readonly string[]; accountIds?: readonly string[]; period: Period },
  signal?: AbortSignal,
): Promise<SiteStatistics | null> {
  const result = await query.siteStatistics({
    ...(args.siteIds !== undefined ? { siteIds: [...args.siteIds] } : {}),
    ...(args.accountIds !== undefined ? { accountIds: [...args.accountIds] } : {}),
    period: args.period,
  }, { signal });
  const row = result.rows()[0];
  return row === undefined ? null : parseSiteStatistics(row, args.period);
}

// ---------------------------------------------------------------------------
// The words
// ---------------------------------------------------------------------------

/** The sentence a system-owned deployable gets instead of a figure. */
export const SYSTEM_OWNED_TRAFFIC_NOTE =
  "This is one of the cluster's own surfaces, and its traffic is not recorded -- measuring the console somebody reads a figure in would be measuring the act of looking.";

/** Whether a period had any row at all: a period with none says so in words and draws no figure. */
export function nothingRecorded(stats: SiteStatistics): boolean {
  return Object.values(stats.totals).every((value) => value === null);
}

/** "Nothing was recorded for last 7 days." */
export function nothingRecordedSentence(periodWords: string): string {
  return `Nothing was recorded for ${periodWords}.`;
}

/**
 * The first measured day, when the period began before it: the line in the
 * chart starts there, and the figures count only the days since.
 */
export function countingStartedIn(stats: SiteStatistics): string | null {
  if (stats.measuredSince === null) return null;
  const start = Date.parse(stats.start);
  const since = Date.parse(stats.measuredSince);
  if (Number.isNaN(start) || Number.isNaN(since)) return null;
  return start < since ? stats.measuredSince : null;
}

/** "Visitor counting started on Nov 8, 2026.", the day read in UTC as every bucket is. */
export function countingStartedSentence(day: string): string {
  const ms = Date.parse(day);
  if (Number.isNaN(ms)) return `Visitor counting started on ${day}.`;
  return `Visitor counting started on ${bucketLabel(day, "day")}, ${new Date(ms).getUTCFullYear()}.`;
}

/** A response time: "120 ms", or "1.4 s" from a second up. */
export function formatMs(ms: number): string {
  if (!Number.isFinite(ms)) return "--";
  const whole = Math.round(ms);
  if (whole < 1000) return whole === 0 && ms > 0 ? "<1 ms" : `${whole} ms`;
  return `${(ms / 1000).toFixed(1)} s`;
}

/** Page views per visit: one decimal, "2.8". */
export function formatPerVisit(value: number): string {
  if (!Number.isFinite(value)) return "--";
  return value.toLocaleString("en-US", { minimumFractionDigits: 1, maximumFractionDigits: 1 });
}

/** Each list's title and its show-all words, in the order the page draws them. */
export const LIST_TITLES: Readonly<Record<Dimension, string>> = {
  page: "Pages",
  source: "Sources",
  country: "Countries",
  device: "Devices",
  browser: "Devices",
  os: "Devices",
};

export const SHOW_ALL_LABELS: Readonly<Record<Dimension, string>> = {
  page: "All pages",
  source: "All sources",
  country: "All countries",
  device: "All devices",
  browser: "All devices",
  os: "All devices",
};

/** The Devices list's three views, as its LocalTabs offer them. */
export const DEVICE_TABS: readonly (readonly ["device" | "browser" | "os", string])[] = [["device", "Device"], ["browser", "Browser"], ["os", "OS"]];

export function isDeviceDimension(dimension: Dimension): dimension is "device" | "browser" | "os" {
  return dimension === "device" || dimension === "browser" || dimension === "os";
}

let regionNames: Intl.DisplayNames | null | undefined;

/** A country's English name for its ISO code, or the code where none is known. */
function countryName(code: string): string {
  if (regionNames === undefined) {
    try {
      regionNames = new Intl.DisplayNames(["en"], { type: "region", fallback: "code" });
    } catch {
      regionNames = null;
    }
  }
  try {
    return regionNames?.of(code.toUpperCase()) ?? code;
  } catch {
    // Not a well-formed region code: shown as the rollup kept it.
    return code;
  }
}

/**
 * A top-list value as a person reads it. The rollup's two placeholders read
 * "Other" (everything past the kept values) and "Unknown" (what the edge
 * could not tell); a source the visitor typed or bookmarked reads "Direct"; a
 * country is named; a device class is capitalised. Pages, browsers and
 * systems read as recorded.
 */
export function valueWord(dimension: Dimension, value: string): string {
  if (value === "(other)") return "Other";
  if (value === "(unknown)") return "Unknown";
  if (dimension === "source" && value === "direct") return "Direct";
  if (dimension === "country" && /^[A-Za-z]{2}$/.test(value)) return countryName(value);
  if (dimension === "device" && value !== "") return value.charAt(0).toUpperCase() + value.slice(1);
  return value;
}

/** A top list as ranked rows, valued by visitors. */
export function rankedRows(entries: readonly TopEntry[], dimension: Dimension): RankedRow[] {
  return entries.map((entry) => ({ key: `${dimension}:${entry.value}`, label: valueWord(dimension, entry.value), value: entry.visitors }));
}
