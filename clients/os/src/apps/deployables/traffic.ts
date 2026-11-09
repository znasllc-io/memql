import { rowString, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

// The figure a deployable's LIST ROW shows -- "served 3 minutes ago" -- as this
// app reads it from the request aggregates (epic memql#4906).
//
// It is all that is left of the Traffic page's reading: a deployable's page
// reads its Statistics now (statistics/read.ts, the 2026-11-08 statistics
// design), from the rollup tables, while the list row still asks the two
// continuous aggregates the edge folds every request into, because "when was
// this last served" is a question about minutes and the rollups are not.
//
// PURE, and separate from every component, for `rows.ts`' reason: a reading
// asserted through render() is asserted through three layers that can each
// fail for unrelated reasons.
//
// ===========================================================================
// AN ABSENT FIGURE AND A ZERO ARE DIFFERENT ANSWERS
// ===========================================================================
// The server answers a window it measured nothing in with NO ROW, never with
// a zero-filled one, and this module keeps that distinction to the row: a
// deployable the answer does not cover is absent from the map, and its row
// says nothing rather than "never".

/** The windows the request aggregates are read over. The list reads a week. */
export type TrafficWindow = "hour" | "day" | "week";

/**
 * The window the LIST reads.
 *
 * A WEEK, because a list row answers "is anybody using this at all", and a
 * deployable that served nobody yesterday but two hundred people on Monday is
 * one somebody is using -- a day-wide row would say nothing about it and read
 * as abandoned.
 */
export const LIST_TRAFFIC_WINDOW: TrafficWindow = "week";

interface WindowSpec {
  /** Which aggregate to read. */
  bucket: "1m" | "1h";
  /** How long the window is, in milliseconds. */
  spanMs: number;
  /** How long one bucket is, in milliseconds. */
  bucketMs: number;
  /** How often the figure is re-read while the window is open. */
  refreshMs: number;
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;

/**
 * What each window reads and how often.
 *
 * THE BUCKET IS THE WINDOW'S OWN, not a preference: a week of minute buckets
 * is ten thousand rows to answer one figure.
 *
 * THE CADENCE FOLLOWS THE WINDOW, NOT THE BUCKET: one more request changes an
 * hour's figure visibly and a week's not at all, so a week is read a quarter
 * as often as an hour, and none of them is read more than once a minute -- a
 * list left open on somebody's desk must not become a query loop.
 */
const WINDOW_SPECS: Record<TrafficWindow, WindowSpec> = {
  hour: { bucket: "1m", spanMs: HOUR, bucketMs: MINUTE, refreshMs: MINUTE },
  day: { bucket: "1h", spanMs: 24 * HOUR, bucketMs: HOUR, refreshMs: 5 * MINUTE },
  week: { bucket: "1h", spanMs: 7 * 24 * HOUR, bucketMs: HOUR, refreshMs: 15 * MINUTE },
};

export function windowSpec(window: TrafficWindow): WindowSpec {
  return WINDOW_SPECS[window] ?? WINDOW_SPECS.day;
}

export interface WindowBounds {
  /** Inclusive, aligned down to the bucket. */
  start: string;
  /** Exclusive, aligned UP to the bucket, so the bucket in progress is
   *  included -- which is what makes "last served" say seconds ago. */
  end: string;
}

/**
 * The window's bounds, aligned to its bucket.
 *
 * ALIGNED BY THE CALLER, because the server reads `window_start >= start AND
 * window_start < end` over bucket boundaries: an unaligned start would drop
 * the bucket the window opens in, and an unaligned end would drop the one it
 * closes in. Half-open throughout, so two adjacent windows add up rather than
 * double-counting the instant they share.
 */
export function windowBounds(window: TrafficWindow, now: Date): WindowBounds {
  const spec = windowSpec(window);
  const endMs = Math.ceil(now.getTime() / spec.bucketMs) * spec.bucketMs;
  const startMs = endMs - spec.spanMs;
  return { start: new Date(startMs).toISOString(), end: new Date(endMs).toISOString() };
}

function numberOf(row: Row, key: string): number {
  const v = row[key];
  if (typeof v === "number") return Number.isFinite(v) ? v : 0;
  if (typeof v === "string") {
    const parsed = Number(v);
    return Number.isFinite(parsed) ? parsed : 0;
  }
  return 0;
}

/**
 * How many deployables one traffic read may name, mirroring
 * `maxSitesPerRead` in component/sitetraffic/reader.go.
 *
 * MIRRORED SO THE CLIENT PAGES, not so it can validate: the server refuses a
 * call past its own cap rather than truncating one, which is the honest
 * server behaviour (a silently short answer reads as "those have no
 * traffic") and is precisely why a caller must split. A cluster whose list
 * exceeds this gets several calls, not one refusal.
 */
export const MAX_SITES_PER_READ = 200;

/** What a list row shows: one deployable's totals and last-served, no series. */
export interface TrafficSummary {
  requests: number;
  errors: number;
  lastServedAt: string;
}

/**
 * Read every listed deployable's totals in ONE call.
 *
 * The summary mode exists for exactly this: a list of twenty deployables
 * asking for twenty series would pull twenty times a week of buckets to
 * render twenty timestamps. A deployable with no traffic in the window is
 * ABSENT from the map rather than present with zeroes -- the unmeasured
 * answer again, carried to the row that renders it.
 */
export async function fetchTrafficSummaries(
  query: QueryClient,
  siteIds: readonly string[],
  window: TrafficWindow,
  now: Date,
  signal?: AbortSignal,
): Promise<Map<string, TrafficSummary>> {
  const out = new Map<string, TrafficSummary>();
  if (siteIds.length === 0) return out;
  const bounds = windowBounds(window, now);
  const bucket = windowSpec(window).bucket;

  // PAGED, because the server REFUSES a call past its cap rather than
  // truncating it -- which is the right server behaviour and exactly why the
  // client must not hand it more than it takes. A cluster with three hundred
  // deployables would otherwise get one refusal and a list with no figures at
  // all, which reads as "nobody is using any of these".
  for (let i = 0; i < siteIds.length; i += MAX_SITES_PER_READ) {
    const page = siteIds.slice(i, i + MAX_SITES_PER_READ);
    const result = await query.siteTrafficInWindow({
      siteIds: [...page],
      bucket,
      windowStart: bounds.start,
      windowEnd: bounds.end,
      summary: true,
    }, { signal });
    for (const row of result.rows()) {
      const siteId = rowString(row, "siteId");
      if (siteId === "") continue;
      out.set(siteId, {
        requests: numberOf(row, "requestCount"),
        errors: numberOf(row, "errorCount"),
        lastServedAt: rowString(row, "lastServedAt"),
      });
    }
  }
  return out;
}
