import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { useLiveCollection } from "../../live/useLiveCollection";
import { buildTrends, figureFromRow, runFromRow, type Figure, type Run, type Trend } from "./benchmarks";

/**
 * How many runs the trend rails cover.
 *
 * Bounded on purpose. The rail is a shape rather than a chart, and a shape
 * made of sixty marks in a 200-pixel column is a smear; twelve is enough to
 * see a movement and few enough that the per-run sample reads stay one screen
 * of work.
 */
const TREND_RUNS = 12;

export interface Benchmarks {
  readonly runs: readonly Run[];
  readonly newest: Run | null;
  readonly trends: readonly Trend[];
  readonly seeding: boolean;
  /** RFC3339 instant the samples were read, "" before the first read. A string
   *  rather than a number because that is what kit's formatFreshness takes,
   *  and a second time representation is a second thing that can disagree. */
  readonly readAt: string;
  readonly loadingSamples: boolean;
  /** The engine's own sentence, verbatim. "" when nothing refused. */
  readonly error: string;
  readonly reload: () => void;
}

/**
 * The Benchmarks feed.
 *
 * TWO READ SHAPES, chosen by what each thing is:
 *
 *   - The RUNS are LIVE. `v1:bench:run` carries a broadcast routing rule
 *     (component/node/routing.go), so a suite that finishes on a CI runner
 *     reaches this page without a poll. A run row is written once and never
 *     updated, which also makes it the honest case for an arrival cue: a new
 *     benchmark run appearing genuinely is news.
 *
 *   - The SAMPLES are an ON-DEMAND READ, and the surface prints when it looked.
 *     They are written once with their run and never move, so subscribing to
 *     sixty-six rows per run would buy a live feed of things that do not
 *     change.
 */
export function useBenchmarks(enabled: boolean): Benchmarks {
  const connection = useOsConnection();

  const runsHandle = useLiveCollection<Row>(connection === null || !enabled ? null : "settings:benchRuns", (conn) => ({
    concept: Concepts.BENCH_RUN,
    seed: async (_cursor, signal) => {
      const result = await conn.query.benchRuns({}, { signal });
      return { rows: [...result.rows()], nextCursor: "" };
    },
  }));

  const runs = useMemo(() => runsHandle.snapshot.rows.map(runFromRow), [runsHandle.snapshot.rows]);
  const shown = useMemo(() => runs.slice(0, TREND_RUNS), [runs]);

  const [samples, setSamples] = useState<Map<string, Figure[]>>(new Map());
  const [readAt, setReadAt] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  // A stale-answer guard. Two reads in flight can land out of order, and the
  // older one would overwrite the newer with figures from a run that is no
  // longer newest.
  const asked = useRef(0);

  // The identity of what is being read, so the effect re-runs when the run
  // list changes and NOT on every render. Deriving it from the ids rather
  // than the array keeps a re-seed that returned the same runs from
  // re-reading sixty-six rows per run.
  const runKey = useMemo(() => shown.map((r) => r.id).join(","), [shown]);

  useEffect(() => {
    if (connection === null || !enabled || runKey === "") {
      setSamples(new Map());
      setLoading(false);
      return;
    }
    const controller = new AbortController();
    const mine = (asked.current += 1);
    setLoading(true);
    setError("");
    void (async () => {
      try {
        const ids = runKey.split(",");
        const results = await Promise.all(
          ids.map((id) => connection.query.benchSamplesForRun({ benchRunId: id }, { signal: controller.signal })),
        );
        if (mine !== asked.current) return;
        const next = new Map<string, Figure[]>();
        ids.forEach((id, i) => {
          next.set(
            id,
            [...results[i]!.rows()].map(figureFromRow),
          );
        });
        setSamples(next);
        setReadAt(new Date().toISOString());
        setLoading(false);
      } catch (err: unknown) {
        if (mine !== asked.current) return;
        // A server-side refusal arrives carrying the engine's own words. It is
        // rendered in surface, where the figures would be, and never rewritten.
        setError(err instanceof Error ? err.message : String(err));
        setLoading(false);
      }
    })();
    return () => {
      controller.abort();
    };
  }, [connection, enabled, runKey, epoch]);

  const trends = useMemo(() => buildTrends(shown, samples), [shown, samples]);

  return {
    runs,
    newest: runs.length > 0 ? runs[0]! : null,
    trends,
    seeding: runsHandle.snapshot.state === "seeding",
    readAt,
    loadingSamples: loading,
    error,
    reload,
  };
}
