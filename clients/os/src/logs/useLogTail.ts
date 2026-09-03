import { useCallback, useEffect, useRef, useState } from "react";
import type { LogsTailArgs } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../live/connection";
import { errorSentence } from "./errors";
import { logRowFromRow, type LogRow } from "./rows";

// The polling tail (epic memql#4895, spec L6).
//
// ===========================================================================
// THIS CONCEPT DOES NOT BROADCAST, AND THAT IS WHY THIS IS NOT A LiveCollection
// ===========================================================================
// `v1:observability:logLine` rows never enter the graph -- the store answers
// from its own hypertable -- so no `graph.node.*` event exists for a
// subscription to receive. A `useLiveCollection` over it would render
// "Loading from the cluster" and then a list that silently never moved,
// which is the failure clients/os/README.md warns about by name.
//
// So the tail POLLS: one baseline with no cursor (the newest lines, oldest
// first), then `logsTail` with the newest row's `occurredAt` and `id` as the
// keyset cursor every two seconds while the document is visible. An empty
// answer is "nothing new", never an error. A facet change is a different
// reading and re-baselines; `viewKey` is where that is expressed.
//
// The arrival cue is deliberately NOT used here (README: a heartbeat is not
// news, and a stream of lines is nothing but arrivals). New lines land at
// the bottom and the list follows them; when the reader has scrolled up the
// count accumulates and a pill offers the way back.

/** The poll cadence. Two seconds is the spec's number: fast enough to read
 *  as live, slow enough that ten open windows are not a load. */
export const TAIL_POLL_MS = 2_000;
/** Rows held in memory. Past this the OLDEST go, and the count of what went
 *  is kept so the surface can say so. */
export const TAIL_MAX_ROWS = 10_000;

export type TailState = "seeding" | "following" | "paused" | "disconnected" | "error";

export interface LogTailOptions {
  /** The facets, with no cursor. Read through a ref: the effect keys on
   *  `viewKey`, not on this object's identity. */
  args: LogsTailArgs;
  /** Re-baseline when this changes. The rendered call string is the natural
   *  key -- a changed call IS a different reading. */
  viewKey: string;
  /** The reader has scrolled up: polling continues, and what arrives is
   *  counted rather than followed. */
  paused: boolean;
  pollMs?: number;
  maxRows?: number;
}

export interface LogTail {
  rows: LogRow[];
  state: TailState;
  error: string;
  lastPolledAt: Date | null;
  /** Lines that arrived while paused. Zero once the reader is back. */
  newSinceScrolled: number;
  /** Older lines let go to stay under the row cap. */
  trimmed: number;
  /** Re-baseline now. */
  refresh: () => void;
}

/** Append what a poll returned, skipping any id already held. The cursor is
 *  strict, so a duplicate should not happen; a defensive skip costs a Set. */
function appendNew(held: LogRow[], fresh: LogRow[]): LogRow[] {
  if (fresh.length === 0) return held;
  const seen = new Set(held.map((row) => row.id));
  const added = fresh.filter((row) => !seen.has(row.id));
  return added.length === 0 ? held : [...held, ...added];
}

function visible(): boolean {
  return typeof document === "undefined" || document.visibilityState === "visible";
}

export function useLogTail(options: LogTailOptions): LogTail {
  const connection = useOsConnection();
  const pollMs = options.pollMs ?? TAIL_POLL_MS;
  const maxRows = options.maxRows ?? TAIL_MAX_ROWS;

  const [rows, setRows] = useState<LogRow[]>([]);
  const [seeded, setSeeded] = useState(false);
  const [error, setError] = useState("");
  const [lastPolledAt, setLastPolledAt] = useState<Date | null>(null);
  const [newSince, setNewSince] = useState(0);
  const [trimmed, setTrimmed] = useState(0);
  const [generation, setGeneration] = useState(0);

  const argsRef = useRef(options.args);
  argsRef.current = options.args;
  const pausedRef = useRef(options.paused);
  pausedRef.current = options.paused;

  useEffect(() => {
    setRows([]);
    setSeeded(false);
    setError("");
    setNewSince(0);
    setTrimmed(0);
    if (connection === null) return undefined;

    const controller = new AbortController();
    let live = true;
    let inFlight = false;
    let cursor: { afterAt: string; afterId: string } | null = null;
    // The effect owns the rows for its lifetime and hands React a copy on
    // every change, so trimming is arithmetic here rather than a nested
    // state update inside an updater.
    let held: LogRow[] = [];
    let trimmedCount = 0;

    const read = async (baseline: boolean): Promise<void> => {
      if (inFlight) return;
      inFlight = true;
      try {
        const args: LogsTailArgs = baseline || cursor === null ? argsRef.current : { ...argsRef.current, ...cursor };
        const result = await connection.query.logsTail(args, { signal: controller.signal });
        if (!live) return;
        const fresh = result.rows().map(logRowFromRow).filter((row) => row.id !== "");
        const newest = fresh[fresh.length - 1];
        if (newest !== undefined) cursor = { afterAt: newest.occurredAt, afterId: newest.id };
        held = baseline ? fresh : appendNew(held, fresh);
        const overflow = held.length - maxRows;
        if (overflow > 0) {
          held = held.slice(overflow);
          trimmedCount += overflow;
          setTrimmed(trimmedCount);
        }
        setRows(held);
        if (!baseline && fresh.length > 0 && pausedRef.current) setNewSince((n) => n + fresh.length);
        setLastPolledAt(new Date());
        setError("");
        setSeeded(true);
      } catch (err) {
        if (!live || controller.signal.aborted) return;
        setError(errorSentence(err));
        setSeeded(true);
      } finally {
        inFlight = false;
      }
    };

    void read(true);
    // With no cursor yet (an empty store, or a baseline that failed) the poll
    // re-runs the baseline: a cursor that never exists would otherwise leave
    // the first line ever written unseen.
    const tick = (): void => {
      if (!visible()) return;
      void read(cursor === null);
    };
    const timer = setInterval(tick, pollMs);
    const onVisibility = (): void => {
      if (visible()) tick();
    };
    if (typeof document !== "undefined") document.addEventListener("visibilitychange", onVisibility);
    return () => {
      live = false;
      controller.abort();
      clearInterval(timer);
      if (typeof document !== "undefined") document.removeEventListener("visibilitychange", onVisibility);
    };
    // `options.viewKey` is the reading; `generation` is a manual re-baseline.
  }, [connection, options.viewKey, generation, pollMs, maxRows]);

  // Coming back clears the count: the lines it counted are on screen now.
  useEffect(() => {
    if (!options.paused) setNewSince(0);
  }, [options.paused]);

  const refresh = useCallback(() => setGeneration((g) => g + 1), []);

  const state: TailState =
    connection === null
      ? "disconnected"
      : error !== ""
        ? "error"
        : !seeded
          ? "seeding"
          : options.paused
            ? "paused"
            : "following";

  return { rows, state, error, lastPolledAt, newSinceScrolled: newSince, trimmed, refresh };
}
