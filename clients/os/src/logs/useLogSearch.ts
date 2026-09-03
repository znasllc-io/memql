import { useCallback, useEffect, useRef, useState } from "react";
import type { LogsSearchArgs, Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../kit/rows";
import { useOsConnection } from "../live/connection";
import { errorSentence } from "./errors";
import { logRowFromRow, type LogRow } from "./rows";

// The search read and the facet catalogue (epic memql#4895, spec D).
//
// Both are ON-DEMAND READS that say when they were read, for the reason the
// tail polls: nothing about this concept broadcasts. Search is keyset-paged
// OLDER from the oldest row on screen (`beforeAt` + `beforeId`), newest
// first; the sources read is what feeds the facet selects, so a value that
// never logged in the window is never offered.

/** Rows per page. Passed explicitly so the exhaustion test below reads the
 *  same number the wire carried. */
export const SEARCH_PAGE = 200;

export type ReadState = "idle" | "reading" | "ready" | "error" | "disconnected";

export interface LogSearch {
  rows: LogRow[];
  state: ReadState;
  error: string;
  readAt: Date | null;
  /** The last page came back short: there is nothing older in the window. */
  exhausted: boolean;
  loadingOlder: boolean;
  loadOlder: () => void;
  refresh: () => void;
}

function project(rows: Row[]): LogRow[] {
  return rows.map(logRowFromRow).filter((row) => row.id !== "");
}

/**
 * @param args  The search, or null while the window is not yet a window.
 * @param key   The reading. The rendered call string is the natural key;
 *              a ticking clock must NOT be in it, or every second is a
 *              new search.
 */
export function useLogSearch(args: LogsSearchArgs | null, key: string): LogSearch {
  const connection = useOsConnection();
  const [rows, setRows] = useState<LogRow[]>([]);
  const [phase, setPhase] = useState<"idle" | "reading" | "ready" | "error">("idle");
  const [error, setError] = useState("");
  const [readAt, setReadAt] = useState<Date | null>(null);
  const [exhausted, setExhausted] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [generation, setGeneration] = useState(0);
  const argsRef = useRef(args);
  argsRef.current = args;
  const rowsRef = useRef(rows);
  rowsRef.current = rows;

  useEffect(() => {
    const current = argsRef.current;
    setRows([]);
    setExhausted(false);
    setError("");
    if (connection === null || current === null) {
      setPhase("idle");
      return undefined;
    }
    const controller = new AbortController();
    let live = true;
    setPhase("reading");
    void (async () => {
      try {
        const result = await connection.query.logsSearch(
          { ...current, limit: SEARCH_PAGE },
          { signal: controller.signal },
        );
        if (!live) return;
        const fresh = project(result.rows());
        setRows(fresh);
        setExhausted(fresh.length < SEARCH_PAGE);
        setReadAt(new Date());
        setPhase("ready");
      } catch (err) {
        if (!live || controller.signal.aborted) return;
        setError(errorSentence(err));
        setPhase("error");
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, key, generation]);

  const loadOlder = useCallback(() => {
    const current = argsRef.current;
    const oldest = rowsRef.current[rowsRef.current.length - 1];
    if (connection === null || current === null || oldest === undefined) return;
    setLoadingOlder(true);
    void (async () => {
      try {
        const result = await connection.query.logsSearch({
          ...current,
          limit: SEARCH_PAGE,
          beforeAt: oldest.occurredAt,
          beforeId: oldest.id,
        });
        const fresh = project(result.rows());
        setRows((held) => {
          const seen = new Set(held.map((row) => row.id));
          return [...held, ...fresh.filter((row) => !seen.has(row.id))];
        });
        setExhausted(fresh.length < SEARCH_PAGE);
        setError("");
      } catch (err) {
        setError(errorSentence(err));
      } finally {
        setLoadingOlder(false);
      }
    })();
  }, [connection]);

  const refresh = useCallback(() => setGeneration((g) => g + 1), []);

  const state: ReadState = connection === null ? "disconnected" : phase;
  return { rows, state, error, readAt, exhausted, loadingOlder, loadOlder, refresh };
}

// ---------------------------------------------------------------------------
// Sources: what logged inside the window
// ---------------------------------------------------------------------------

export interface SourceOption {
  value: string;
  count: number;
  /** For a node: the type it belongs to. Blank otherwise. */
  nodeType: string;
}

export interface LogSources {
  components: SourceOption[];
  nodes: SourceOption[];
  apps: SourceOption[];
  state: ReadState;
  error: string;
}

const EMPTY_SOURCES: Pick<LogSources, "components" | "nodes" | "apps"> = {
  components: [],
  nodes: [],
  apps: [],
};

function countOf(value: unknown): number {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value === "string" && value.trim() !== "" && Number.isFinite(Number(value))) return Number(value);
  return 0;
}

/** Fold the wire rows into the three lists. Defensive: an unknown `kind` is
 *  skipped rather than guessed at, and a blank value is not an option. */
export function sourcesFromRows(rows: Row[]): Pick<LogSources, "components" | "nodes" | "apps"> {
  const out = { components: [] as SourceOption[], nodes: [] as SourceOption[], apps: [] as SourceOption[] };
  for (const raw of rows) {
    const row = flatten(raw);
    const value = typeof row.value === "string" ? row.value.trim() : "";
    if (value === "") continue;
    const option: SourceOption = {
      value,
      count: countOf(row.count),
      nodeType: typeof row.nodeType === "string" ? row.nodeType : "",
    };
    switch (row.kind) {
      case "component":
        out.components.push(option);
        break;
      case "node":
        out.nodes.push(option);
        break;
      case "app":
        out.apps.push(option);
        break;
      default:
        break;
    }
  }
  const byCount = (a: SourceOption, b: SourceOption): number =>
    b.count - a.count || a.value.localeCompare(b.value);
  out.components.sort(byCount);
  out.nodes.sort(byCount);
  out.apps.sort(byCount);
  return out;
}

/**
 * @param bounds  The window, or null while there is none.
 * @param key     When to read again. The window PRESET rather than its
 *                instant bounds, so a stream's clock does not re-read the
 *                catalogue every tick.
 */
export function useLogSources(bounds: { start: Date; end: Date } | null, key: string): LogSources {
  const connection = useOsConnection();
  const [lists, setLists] = useState(EMPTY_SOURCES);
  const [phase, setPhase] = useState<"idle" | "reading" | "ready" | "error">("idle");
  const [error, setError] = useState("");
  const boundsRef = useRef(bounds);
  boundsRef.current = bounds;

  useEffect(() => {
    const current = boundsRef.current;
    if (connection === null || current === null) {
      setLists(EMPTY_SOURCES);
      setPhase("idle");
      return undefined;
    }
    const controller = new AbortController();
    let live = true;
    setPhase("reading");
    setError("");
    void (async () => {
      try {
        const result = await connection.query.logsSources(
          { windowStart: current.start.toISOString(), windowEnd: current.end.toISOString() },
          { signal: controller.signal },
        );
        if (!live) return;
        setLists(sourcesFromRows(result.rows()));
        setPhase("ready");
      } catch (err) {
        if (!live || controller.signal.aborted) return;
        setError(errorSentence(err));
        setPhase("error");
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, key]);

  return { ...lists, state: connection === null ? "disconnected" : phase, error };
}
