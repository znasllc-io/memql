import { rowString, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

// A deployable's traffic, as this app reads it (epic memql#4906).
//
// PURE, and separate from every component, for `rows.ts`' reason: a reading
// asserted through render() is asserted through three layers that can each
// fail for unrelated reasons. Everything here is a function of rows or of a
// window, unit-testable with no browser, no cluster and no React -- which is
// what lets the LIST row's figure and the detail panel's figure be checked
// against the same fixtures and therefore against each other.
//
// ===========================================================================
// AN ABSENT FIGURE AND A ZERO ARE DIFFERENT ANSWERS
// ===========================================================================
// The server answers a window it measured nothing in with NO ROW, never with
// a zero-filled one, and this module keeps that distinction all the way to
// the sentence a person reads: `null` means unmeasured and says so in words,
// while a reading with `requests: 0` says zero. Folding the two would tell
// somebody nobody is visiting their app when what happened is that the edge
// was not recording -- and those send a person to two different places.

/** The windows a person can pick. Three, and no more: an hour answers "is it
 *  up", a day answers "is anybody using it", a week answers "is that normal".
 *  A fourth would be a fourth pill for a question nobody asked. */
export const TRAFFIC_WINDOWS = ["hour", "day", "week"] as const;

export type TrafficWindow = (typeof TRAFFIC_WINDOWS)[number];

/**
 * The window a page opens on when nobody has chosen one.
 *
 * THE HOUR (not the day, which this was): "is it up" is the question somebody
 * opening a deployable is nearly always asking. A choice they make instead is
 * remembered -- `DeployablesSettings.trafficWindow` -- so this is the first
 * answer rather than the only one.
 */
export const DEFAULT_TRAFFIC_WINDOW: TrafficWindow = "hour";

/**
 * The window the LIST reads.
 *
 * A WEEK, wider than the stop's default day, and the difference is the
 * question each answers. The panel asks "how is it doing", which is a question
 * about now. A list row answers "is anybody using this at all", and a
 * deployable that served nobody yesterday but two hundred people on Monday is
 * one somebody is using -- a day-wide row would say nothing about it and read
 * as abandoned.
 */
export const LIST_TRAFFIC_WINDOW: TrafficWindow = "week";

/** How a window is labelled on its pill. */
export function windowLabel(window: TrafficWindow): string {
  switch (window) {
    case "hour":
      return "Last hour";
    case "day":
      return "Last day";
    default:
      return "Last week";
  }
}

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
 * is ten thousand rows to draw one line, and an hour of hour buckets is one
 * column.
 *
 * THE CADENCE FOLLOWS THE WINDOW, NOT THE BUCKET, and the difference is worth
 * stating because the bucket looks like the obvious answer. The aggregate
 * answers its newest, not-yet-materialized bucket live from the raw rows, so
 * an hour bucket in progress really does change every minute -- what does not
 * change usefully is a WEEK'S total after one more request. So a week is read
 * a quarter as often as an hour, and none of them is read more than once a
 * minute: a figure left open on somebody's desk must not become a query loop.
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

/** One bucket of the series. */
export interface TrafficBucket {
  /** The bucket's start, ISO. */
  at: string;
  requests: number;
  errors: number;
  notFound: number;
}

/** What a window came back with. `null` anywhere means unmeasured. */
export interface TrafficReading {
  requests: number;
  errors: number;
  notFound: number;
  /** ISO instant of the newest request, or "" when the server sent none. */
  lastServedAt: string;
  /** The buckets, oldest first, gaps filled with zeroes. */
  buckets: TrafficBucket[];
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
 * Fold the server's rows into one reading, or null.
 *
 * NULL WHEN THERE ARE NO ROWS. That is the unmeasured answer arriving intact:
 * the server sends no row for a window it measured nothing in, and this
 * returns null rather than a reading of zeroes.
 *
 * GAPS INSIDE A MEASURED WINDOW ARE ZEROES, though, and the difference is the
 * point. Once a window has any traffic at all, a bucket with no row is a
 * bucket in which nothing was served -- a real zero -- and the strip has to
 * draw it as a gap rather than closing up, or a quiet night would look like
 * an hour of steady traffic.
 */
export function readingFromRows(rows: readonly Row[], window: TrafficWindow, bounds: WindowBounds): TrafficReading | null {
  if (rows.length === 0) return null;

  const spec = windowSpec(window);
  const startMs = Date.parse(bounds.start);
  const endMs = Date.parse(bounds.end);
  if (!Number.isFinite(startMs) || !Number.isFinite(endMs)) return null;

  const byBucket = new Map<number, TrafficBucket>();
  let lastServedAt = "";

  for (const row of rows) {
    const at = Date.parse(rowString(row, "windowStart"));
    if (!Number.isFinite(at) || at < startMs || at >= endMs) continue;
    byBucket.set(at, {
      at: new Date(at).toISOString(),
      requests: numberOf(row, "requestCount"),
      errors: numberOf(row, "errorCount"),
      notFound: numberOf(row, "clientErrorCount"),
    });
    const served = rowString(row, "lastServedAt");
    if (served !== "" && served > lastServedAt) lastServedAt = served;
  }

  // THE FACTS ARE THE SUM OF THE STRIP, and that is the point of summing here
  // rather than as the rows arrive. A row outside the window, or off the
  // bucket grid, contributes to NEITHER -- so the picture and the numbers
  // beneath it can never disagree, which is the same property the continuous
  // aggregate gives the server side. The server only ever sends aligned rows
  // inside the window, so in practice nothing is dropped; the guarantee is
  // what matters, because "the total says 1,330 and the columns are empty" is
  // a state a reader has no way to resolve.
  const buckets: TrafficBucket[] = [];
  let requests = 0;
  let errors = 0;
  let notFound = 0;
  for (let at = startMs; at < endMs; at += spec.bucketMs) {
    const bucket = byBucket.get(at) ?? { at: new Date(at).toISOString(), requests: 0, errors: 0, notFound: 0 };
    requests += bucket.requests;
    errors += bucket.errors;
    notFound += bucket.notFound;
    buckets.push(bucket);
  }

  return { requests, errors, notFound, lastServedAt, buckets };
}

/**
 * Read one deployable's series for a window.
 *
 * Through the generated builder, which is the point of sdk-gen: the call
 * string it produces is the one the engine parses, and a hand-built one is a
 * second spelling that can drift from the builtin's declared arguments.
 */
export async function fetchTraffic(
  query: QueryClient,
  siteId: string,
  window: TrafficWindow,
  now: Date,
): Promise<{ reading: TrafficReading | null; bounds: WindowBounds }> {
  const bounds = windowBounds(window, now);
  if (siteId === "") return { reading: null, bounds };
  const result = await query.siteTrafficInWindow({
    siteIds: [siteId],
    bucket: windowSpec(window).bucket,
    windowStart: bounds.start,
    windowEnd: bounds.end,
  });
  return { reading: readingFromRows(result.rows(), window, bounds), bounds };
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
    });
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

// ---------------------------------------------------------------------------
// The words
// ---------------------------------------------------------------------------

/**
 * What the stop says when a window measured nothing.
 *
 * IN WORDS, AND IT NAMES BOTH CAUSES, because a person looking at their own
 * app's page cannot tell them apart and the wrong guess is expensive: "nobody
 * came" is a business fact and "nothing was recording" is a cluster fact.
 * Naming a third possibility would be padding; these two are the whole of it.
 */
export function unmeasuredSentence(window: TrafficWindow): string {
  return `Nothing was recorded for ${windowLabel(window).toLowerCase()}. Either nobody visited, or this cluster was not recording traffic.`;
}

/** The sentence a system-owned deployable gets instead of a figure. */
export const SYSTEM_OWNED_TRAFFIC_NOTE =
  "This is one of the cluster's own surfaces, and its traffic is not recorded -- measuring the console somebody reads a figure in would be measuring the act of looking.";

/**
 * How a count reads.
 *
 * Grouped past a thousand and never abbreviated: "1.2k requests" is a figure
 * somebody has to decide how much to trust, and there is no room pressure
 * here that would buy.
 */
export function countLabel(n: number): string {
  return n.toLocaleString();
}

/**
 * The strip's accessible summary -- the whole series in one sentence, for a
 * reader who cannot see the columns.
 *
 * A chart that could only be read by looking at it would leave its values
 * reachable through hover alone, which is the one thing a chart may not do.
 * The Facts beside it carry the totals; this carries the shape.
 */
export function seriesSummary(reading: TrafficReading, window: TrafficWindow): string {
  const busiest = reading.buckets.reduce(
    (best, b) => (b.requests > best.requests ? b : best),
    reading.buckets[0] ?? { at: "", requests: 0, errors: 0, notFound: 0 },
  );
  const quiet = reading.buckets.filter((b) => b.requests === 0).length;
  const unit = windowSpec(window).bucket === "1m" ? "minute" : "hour";
  return (
    `${countLabel(reading.requests)} requests over ${reading.buckets.length} ${unit} buckets. ` +
    `The busiest held ${countLabel(busiest.requests)}; ${countLabel(quiet)} were empty.`
  );
}
