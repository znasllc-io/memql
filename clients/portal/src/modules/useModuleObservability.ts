import { useCallback, useEffect, useMemo, useState } from "react";
import { browseConceptPage, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

// Observability for one module (memql#4192, memql#4208): n / p95 / err% from
// the v1:observability:codeMetric continuous aggregates, plus recent raw
// invocations -- the same data path the Cockpit's topology overlay reads.
//
// HOW THE ROWS ARE FETCHED. The metric windows come from ONE engine query,
// codeMetricsInWindow (dsl/observability/queries.memql): the selected
// bucket, the selected [start, end) window, and the module's join keys --
// its fqnPrefixes (codeReference starts with ANY of them) plus, when set,
// its exact codeReference. The engine does the join; this hook walks the
// query's keyset cursor to exhaustion, so the result is the whole window,
// not the newest N rows cluster-wide filtered down (the 3 x 200 cap this
// replaced, and the coverage footer that cap made necessary).
//
// A module with no join keys (components, v1 -- design doc section 7) is
// NOT queried: the engine would return nothing for it by construction
// (empty prefixes and no codeReference select no rows), and the section
// states the fact instead of charting an empty result.

const INVOCATION_CONCEPT = "v1:observability:invocation";
const INVOCATION_PAGE_SIZE = 100;
const MAX_INVOCATIONS_SHOWN = 20;

export type Bucket = "1m" | "1h";

// One bucket's size, and how many of them the selected window spans: the
// last hour of 1m buckets, the last week of 1h buckets.
const BUCKET_MS: Record<Bucket, number> = { "1m": 60_000, "1h": 3_600_000 };
const WINDOW_BUCKETS: Record<Bucket, number> = { "1m": 60, "1h": 7 * 24 };

export interface MetricWindow {
  codeReference: string;
  windowStart: string;
  bucket: string;
  callCount: number;
  errorCount: number;
  p95DurationNs: number;
}

export interface SelectedWindow {
  // RFC3339 instants at second precision, aligned to the bucket: start is
  // inclusive, end is exclusive and sits one bucket past the bucket `now`
  // falls in, so the in-progress bucket is inside the window.
  start: string;
  end: string;
  buckets: number;
  label: string;
}

export interface JoinKeys {
  prefixes: string[];
  codeReference: string;
}

export interface ModuleObservability {
  loading: boolean;
  error: string;
  // Every window the engine holds for the selected bucket inside the
  // selected range, oldest first.
  windows: MetricWindow[];
  window: SelectedWindow;
  invocations: Row[];
  reload: () => void;
}

// rfc3339Seconds renders an instant the way the aggregates stamp their
// bucket boundaries -- whole seconds, `Z` -- so the engine's lexicographic
// datetime comparison sees the same spelling on both sides.
function rfc3339Seconds(ms: number): string {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}

export function windowFor(bucket: Bucket, now: number = Date.now()): SelectedWindow {
  const size = BUCKET_MS[bucket];
  const buckets = WINDOW_BUCKETS[bucket];
  const end = Math.floor(now / size) * size + size;
  const start = end - buckets * size;
  return {
    start: rfc3339Seconds(start),
    end: rfc3339Seconds(end),
    buckets,
    label: bucket === "1m" ? "the last 60 minutes" : "the last 7 days",
  };
}

// joinKeysOf normalises a module's join keys: blank prefixes are dropped
// (the engine would drop them too -- a blank is not a prefix) and the exact
// key is trimmed.
export function joinKeysOf(fqnPrefixes: readonly string[], codeReference: string): JoinKeys {
  return {
    prefixes: fqnPrefixes.filter((p) => p.trim() !== ""),
    codeReference: codeReference.trim(),
  };
}

export function isJoinable(keys: JoinKeys): boolean {
  return keys.prefixes.length > 0 || keys.codeReference !== "";
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

// metricWindowOf reads the fields the chart and the readings use off a
// flattened result row (Result.rows() merges the payload into the row).
function metricWindowOf(row: Row): MetricWindow {
  return {
    codeReference: str(row["codeReference"]),
    windowStart: str(row["windowStart"]),
    bucket: str(row["bucket"]),
    callCount: num(row["callCount"]),
    errorCount: num(row["errorCount"]),
    p95DurationNs: num(row["p95DurationNs"]),
  };
}

function matchesModule(codeReference: string, keys: JoinKeys): boolean {
  if (keys.codeReference !== "" && codeReference === keys.codeReference) return true;
  return keys.prefixes.some((prefix) => codeReference.startsWith(prefix));
}

export function useModuleObservability(
  fqnPrefixes: readonly string[],
  codeReference: string,
  bucket: Bucket,
  enabled: boolean,
): ModuleObservability {
  const { query, status } = useCluster();
  const [windows, setWindows] = useState<MetricWindow[]>([]);
  const [invocations, setInvocations] = useState<Row[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  const keys = useMemo(() => joinKeysOf(fqnPrefixes, codeReference), [fqnPrefixes, codeReference]);
  const joinable = isJoinable(keys);
  // The window is fixed per fetch so a slow page walk does not drift its
  // bounds; it is recomputed on reload (epoch) and on a bucket change.
  const selected = useMemo(() => windowFor(bucket), [bucket, epoch]);

  useEffect(() => {
    if (!enabled || !query || status !== "connected" || !joinable) return;
    let stale = false;
    setLoading(true);
    setError("");
    (async () => {
      const matched: MetricWindow[] = [];
      let cursor: string | undefined;
      do {
        const result = await query.codeMetricsInWindow(
          {
            prefixes: keys.prefixes,
            ...(keys.codeReference !== "" ? { codeReference: keys.codeReference } : {}),
            bucket,
            windowStart: selected.start,
            windowEnd: selected.end,
          },
          cursor ? { cursor } : {},
        );
        for (const row of result.rows()) matched.push(metricWindowOf(row));
        const next = result.meta()?.cursor ?? "";
        cursor = next === "" ? undefined : next;
      } while (cursor);
      // The engine pages in row.createdAt order (the keyset-eligible sort);
      // the chart reads time from windowStart.
      matched.sort((a, b) => a.windowStart.localeCompare(b.windowStart));

      // Recent raw invocations stay a bounded forensic browse -- the
      // concept's own doc blesses a "last N calls to X" lookup -- and the
      // section labels it as the newest page cluster-wide, filtered here.
      const recent: Row[] = [];
      const invResult = await browseConceptPage(query, INVOCATION_CONCEPT, {
        pageSize: INVOCATION_PAGE_SIZE,
        order: "desc",
      });
      for (const row of invResult.rows) {
        const payload = (row as { payload?: unknown }).payload;
        const ref = str(
          payload && typeof payload === "object"
            ? (payload as Record<string, unknown>)["codeReference"]
            : row["codeReference"],
        );
        if (matchesModule(ref, keys)) {
          recent.push(row);
          if (recent.length >= MAX_INVOCATIONS_SHOWN) break;
        }
      }

      if (!stale) {
        setWindows(matched);
        setInvocations(recent);
      }
    })()
      .catch((err: unknown) => {
        if (!stale) setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
    };
  }, [enabled, query, status, keys, joinable, bucket, selected]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { loading, error, windows, window: selected, invocations, reload };
}

// Roll the windows into the three headline numbers. p95 cannot be
// re-aggregated exactly across windows, so the summary reports the WORST
// window's p95 and labels it that way -- a max is honest where an average
// of percentiles would be fiction.
export function summarize(windows: readonly MetricWindow[]): {
  calls: number;
  errorRate: number;
  worstP95Ms: number;
} {
  let calls = 0;
  let errors = 0;
  let worstP95 = 0;
  for (const w of windows) {
    calls += w.callCount;
    errors += w.errorCount;
    if (w.p95DurationNs > worstP95) worstP95 = w.p95DurationNs;
  }
  return {
    calls,
    errorRate: calls === 0 ? 0 : errors / calls,
    worstP95Ms: worstP95 / 1_000_000,
  };
}
