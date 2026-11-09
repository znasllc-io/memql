// The periods a statistics page reads, and the words it says for them.
//
// ONE VOCABULARY FOR BOTH READS. `siteStatistics` and `emailStatistics` take
// the same period words (core/statsperiod parses them on the server), so the
// pages share one type and one set of labels rather than each spelling "Last
// 30 days" for itself. A website also offers the last 24 hours, read by the
// hour; email facts are counted by the day, so Email has no 24 hours.
//
// TWELVE MONTHS IS THE CURRENT MONTH AND THE TWELVE BEFORE IT, in thirteen
// monthly buckets, with no previous period to compare against: the twelve
// months before them are past what the rollups keep (spec D4).
//
// DAYS ARE UTC (spec D5). The rollups turn over at 00:00 UTC, so a bucket is
// labelled in UTC as well: a reader at UTC+2 sees the "Nov 8" the cluster
// counted, not a day moved by their own clock.

export type Period = "24h" | "7d" | "30d" | "90d" | "12m";

/** The size of one bucket of a period's series: 24 hours by the hour, 7 to 90
 *  days by the day, twelve months by the month. */
export type Bucket = "hour" | "day" | "month";

/** What a website's statistics offer, shortest first. */
export const SITE_PERIODS: readonly Period[] = Object.freeze(["24h", "7d", "30d", "90d", "12m"]);

/** What Email's statistics offer: no hourly period, because its facts are counted by day. */
export const EMAIL_PERIODS: readonly Period[] = Object.freeze(["7d", "30d", "90d", "12m"]);

const PERIOD_LABELS: Readonly<Record<Period, string>> = {
  "24h": "Last 24 hours",
  "7d": "Last 7 days",
  "30d": "Last 30 days",
  "90d": "Last 90 days",
  "12m": "Last 12 months",
};

export function periodLabel(period: Period): string {
  return PERIOD_LABELS[period];
}

/**
 * Whether a value is one of the periods a page offers.
 *
 * Against the page's OWN list, not every period: a choice remembered in a
 * settings document is whatever was written there, and "24h" read back into
 * Email would ask its read for a period it refuses.
 */
export function isPeriod(value: unknown, allowed: readonly Period[]): value is Period {
  return typeof value === "string" && (allowed as readonly string[]).includes(value);
}

// Written out rather than asked of Intl: a short month name is not the same
// string in every runtime ("Sep" or "Sept"), and an axis label that changes
// with the engine's locale data is a label no test can hold still.
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"] as const;

function twoDigits(value: number): string {
  return String(value).padStart(2, "0");
}

/**
 * A bucket's label, in UTC: "14:00" for an hour, "Nov 8" for a day, "Nov 2026"
 * for a month.
 *
 * An instant that does not parse is returned as written. It is a defect in
 * whatever produced it, and a label that shows the defect is more use than a
 * plausible date nobody stated.
 */
export function bucketLabel(at: string, bucket: Bucket): string {
  const ms = Date.parse(at);
  if (Number.isNaN(ms)) return at;
  const instant = new Date(ms);
  if (bucket === "hour") return `${twoDigits(instant.getUTCHours())}:${twoDigits(instant.getUTCMinutes())}`;
  const month = MONTHS[instant.getUTCMonth()] ?? "";
  if (bucket === "day") return `${month} ${instant.getUTCDate()}`;
  return `${month} ${instant.getUTCFullYear()}`;
}
