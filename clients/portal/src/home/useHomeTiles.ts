import { useCallback, useEffect, useState } from "react";
import { browseConceptPage, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

// Data for the console home's tiles (memql#4182). Each tile is one bounded
// read -- a single newest-first page -- plus, where the concept moves, a
// graph subscription that re-reads it, so the home answers "what changed"
// without polling and without pretending to totals it has not loaded:
// a count that hit the page boundary renders with a trailing plus.
//
// Counts are PER-CALLER by construction: the reads run under the viewer's
// own actor and per-row authz, so a reader's home counts the rows a reader
// may see. That is the honest number for the person looking at it.

const TILE_PAGE = 100;

export interface TileCount {
  count: number;
  more: boolean;
  loading: boolean;
  error: string;
}

export interface HomeTileState extends TileCount {
  rows: Row[];
  reload: () => void;
}

export function useConceptTile(
  conceptId: string,
  live: boolean,
  keepRows: number,
): HomeTileState {
  const { query, subscriptions, status } = useCluster();
  const [rows, setRows] = useState<Row[]>([]);
  const [count, setCount] = useState(0);
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (!query || status !== "connected") return;
    let stale = false;
    setLoading(true);
    setError("");
    browseConceptPage(query, conceptId, { pageSize: TILE_PAGE, order: "desc" })
      .then((page) => {
        if (stale) return;
        setRows(page.rows.slice(0, keepRows));
        setCount(page.rows.length);
        setMore(page.nextCursor !== "");
      })
      .catch((err: unknown) => {
        if (!stale) setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
    };
  }, [query, status, conceptId, keepRows, epoch]);

  useEffect(() => {
    if (!live || !subscriptions || status !== "connected") return;
    try {
      return subscriptions.subscribeGraph(() => setEpoch((n) => n + 1), {
        concept: conceptId,
        actions: ["created"],
      });
    } catch {
      return;
    }
  }, [live, subscriptions, status, conceptId]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { rows, count, more, loading, error, reload };
}

export function payloadOf(row: Row): Record<string, unknown> {
  const p = (row as { payload?: unknown }).payload;
  return typeof p === "object" && p !== null
    ? (p as Record<string, unknown>)
    : (row as Record<string, unknown>);
}
