import { useCallback, useEffect, useMemo, useState } from "react";
import { browseConceptPage, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

// Observability for one module (memql#4192): n / p95 / err% from the
// v1:observability:codeMetric continuous aggregates, plus recent raw
// invocations -- the same data path the Cockpit's topology overlay reads.
//
// HOW THE ROWS ARE FETCHED, and the honesty that buys. There is no
// module-scoped metric query on the wire; this hook walks the generic
// concept browse newest-first (the sdk's `order: "desc"`) and filters
// client-side by the module's fqnPrefixes. The walk is CAPPED at a few
// pages, and the surface says what window of history that covered rather
// than implying totality. Where a module has no fqnPrefixes (components,
// v1 -- design doc section 7) the section states that instead of charting
// nothing. The engine-side gap -- a codeReference-filtered, bucket-scoped
// metric read -- is real and filed; this surface renders what exists.

const CODE_METRIC_CONCEPT = "v1:observability:codeMetric";
const INVOCATION_CONCEPT = "v1:observability:invocation";
const MAX_METRIC_PAGES = 3;
const METRIC_PAGE_SIZE = 200;
const INVOCATION_PAGE_SIZE = 100;
const MAX_INVOCATIONS_SHOWN = 20;

export interface MetricWindow {
  codeReference: string;
  windowStart: string;
  bucket: string;
  callCount: number;
  errorCount: number;
  p95DurationNs: number;
}

export interface ModuleObservability {
  loading: boolean;
  error: string;
  // Filtered, newest-first windows for the selected bucket.
  windows: MetricWindow[];
  // How many codeMetric rows the capped walk actually examined, and whether
  // it hit the cap -- the honesty line renders from these two facts.
  scannedRows: number;
  truncated: boolean;
  invocations: Row[];
  reload: () => void;
}

function payloadOf(row: Row): Record<string, unknown> {
  const p = (row as { payload?: unknown }).payload;
  return typeof p === "object" && p !== null ? (p as Record<string, unknown>) : (row as Record<string, unknown>);
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

function matchesModule(codeReference: string, prefixes: readonly string[]): boolean {
  return prefixes.some((prefix) => prefix !== "" && codeReference.startsWith(prefix));
}

export function useModuleObservability(
  fqnPrefixes: readonly string[],
  codeReference: string,
  bucket: "1m" | "1h",
  enabled: boolean,
): ModuleObservability {
  const { query, status } = useCluster();
  const [windows, setWindows] = useState<MetricWindow[]>([]);
  const [invocations, setInvocations] = useState<Row[]>([]);
  const [scannedRows, setScannedRows] = useState(0);
  const [truncated, setTruncated] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  const prefixes = useMemo(() => {
    const all = [...fqnPrefixes];
    if (codeReference !== "") all.push(codeReference);
    return all;
  }, [fqnPrefixes, codeReference]);

  useEffect(() => {
    if (!enabled || !query || status !== "connected" || prefixes.length === 0) return;
    let stale = false;
    setLoading(true);
    setError("");
    (async () => {
      const matched: MetricWindow[] = [];
      let cursor: string | undefined;
      let scanned = 0;
      let hitCap = true;
      for (let page = 0; page < MAX_METRIC_PAGES; page++) {
        const result = await browseConceptPage(query, CODE_METRIC_CONCEPT, {
          pageSize: METRIC_PAGE_SIZE,
          order: "desc",
          ...(cursor ? { cursor } : {}),
        });
        scanned += result.rows.length;
        for (const row of result.rows) {
          const p = payloadOf(row);
          const ref = str(p["codeReference"]);
          if (str(p["bucket"]) !== bucket || !matchesModule(ref, prefixes)) continue;
          matched.push({
            codeReference: ref,
            windowStart: str(p["windowStart"]),
            bucket: str(p["bucket"]),
            callCount: num(p["callCount"]),
            errorCount: num(p["errorCount"]),
            p95DurationNs: num(p["p95DurationNs"]),
          });
        }
        cursor = result.nextCursor === "" ? undefined : result.nextCursor;
        if (!cursor) {
          hitCap = false;
          break;
        }
      }

      const recent: Row[] = [];
      const invResult = await browseConceptPage(query, INVOCATION_CONCEPT, {
        pageSize: INVOCATION_PAGE_SIZE,
        order: "desc",
      });
      for (const row of invResult.rows) {
        if (matchesModule(str(payloadOf(row)["codeReference"]), prefixes)) {
          recent.push(row);
          if (recent.length >= MAX_INVOCATIONS_SHOWN) break;
        }
      }

      if (!stale) {
        setWindows(matched);
        setInvocations(recent);
        setScannedRows(scanned);
        setTruncated(hitCap);
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
  }, [enabled, query, status, prefixes, bucket, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { loading, error, windows, scannedRows, truncated, invocations, reload };
}

// Roll the filtered windows into the three headline numbers. p95 cannot be
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
