// The shell's time and duration voice.
//
// It was written for the Fleet and moved here when the Users app needed the
// same two readings (memql#4734). That is the rule this directory states: a
// helper earns a place in the kit when a SECOND surface needs it -- promoting
// on the first use invents an abstraction from one example, and waiting for
// the third means the second one has already forked.
//
// Two time formatters, answering different questions. `moment` is "when did
// this happen" for a fact an operator reads once; `freshness` is "how long
// ago" for the value this surface is really about -- a machine's last
// heartbeat, where the ABSOLUTE time says nothing useful (a wall-clock
// reading against a thirty-second window is a subtraction the reader has to
// do) and the elapsed time is the whole point.

/** An absolute instant, for a fact rather than a liveness reading. */
export function formatMoment(value: string): string {
  const trimmed = value.trim();
  if (trimmed === "") return "--";
  const parsed = new Date(trimmed);
  if (Number.isNaN(parsed.getTime())) return trimmed;
  return parsed.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

/**
 * Elapsed time against `now`, coarsening as it grows.
 *
 * "never" and "--" are DIFFERENT answers and both appear: a machine with no
 * `lastSeenAt` has never checked in, which is not the same as one whose value
 * we could not read. A FUTURE timestamp reads as "just now" rather than as a
 * negative duration -- that is clock skew between the cluster and this
 * browser, and "-12s ago" would send an operator to debug the wrong thing.
 */
export function formatFreshness(value: string, now: Date): string {
  const trimmed = value.trim();
  if (trimmed === "") return "never";
  const parsed = Date.parse(trimmed);
  if (Number.isNaN(parsed)) return trimmed;

  const seconds = Math.floor((now.getTime() - parsed) / 1000);
  if (seconds <= 1) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/**
 * Bytes, in the unit a person would pick. IEC units -- the storage caps this
 * shell renders (32 MiB one-shot line, 512 MiB buffer limit, 4 GiB upload
 * cap) are IEC, and mixing 512 MiB in one sentence with "537 MB" in the next
 * would read as two different numbers. Whole numbers under a KiB, one
 * decimal above. Zero renders "0 B": a transfer that has moved nothing yet
 * has moved nothing, which is not the absent answer "--" is for.
 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "--";
  if (bytes < 1024) return `${Math.round(bytes)} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let unit = "B";
  for (const next of units) {
    if (value < 1024) break;
    value /= 1024;
    unit = next;
  }
  return `${value >= 100 ? Math.round(value) : value.toFixed(1)} ${unit}`;
}

/**
 * A count, grouped: "1,204". In ONE locale, the one the statistics chart's
 * axis and bucket labels use (kit/statistics), so a figure and the axis beside
 * it never disagree about what a comma means. A value that is not a finite
 * number renders "--": NaN on screen is an absent number dressed as a measured
 * one, which is what kit/measure exists to prevent.
 */
export function formatCount(value: number): string {
  if (!Number.isFinite(value)) return "--";
  return value.toLocaleString("en-US");
}

/**
 * Money in `currency` (an ISO 4217 code), as a store's statistics write it:
 * "€1,204.50", "¥1,200", "KWD 1.255". In the locale `formatCount` writes in,
 * with the currency's own decimals, so a yen total has none and a dinar total
 * three. Below zero it keeps its sign ("-€36.00"): a net figure a day of
 * refunds outweighed is an amount, not an absence.
 *
 * Built once per currency and returned as the writer for its values, because
 * a chart writes every axis step and readout through it. A value that is not
 * finite renders "--", as `formatCount`. A code Intl refuses never throws: the
 * value is written as its count and the code as given ("1,204.5 EURO"), so a
 * defect in the code shows rather than taking the page down.
 */
export function formatMoney(currency: string): (value: number) => string {
  let money: Intl.NumberFormat | null;
  try {
    money = new Intl.NumberFormat("en-US", { style: "currency", currency });
  } catch {
    money = null;
  }
  return (value: number) => {
    if (!Number.isFinite(value)) return "--";
    if (money !== null) return money.format(value);
    const written = formatCount(value);
    return currency === "" ? written : `${written} ${currency}`;
  };
}

/**
 * A rate as a person reads one: "42%", "4.3%", "0%". Whole percents from 10%
 * up and at zero, one decimal below 10%, where a whole number would round a
 * 4.3% click rate to 4% and hide the difference between two campaigns. Null
 * renders "--": a rate with no denominator is absent, never 0%.
 *
 * It was Email's, and moved here when the website statistics needed the same
 * reading for bounce rate (the second use promotes, as this file's header says).
 */
export function formatRate(rate: number | null): string {
  if (rate === null || !Number.isFinite(rate)) return "--";
  const pct = rate * 100;
  return pct >= 10 || pct === 0 ? `${Math.round(pct)}%` : `${pct.toFixed(1)}%`;
}

/** A call's duration. Zero and negative render as "--": a row whose
 *  `completedAt` never landed has no duration, and "0ms" would claim one. */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "--";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s`;
}
