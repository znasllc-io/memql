import { useCallback, useEffect, useState } from "react";
import { browseConceptPage, countConcept, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

// Data for the console's tiles (memql#4182, memql#4263).
//
// TWO READS PER TILE, and the split is the point:
//
//   the COUNT is the engine's `count` directive -- computed server-side over
//   the whole concept, not over a page (countConcept, memql#1730).
//   the ROWS are a small bounded page, and only for the tiles that show a
//   recent-activity list under the number.
//
// It used to be one read doing both jobs: a 100-row page whose LENGTH was the
// count, which meant a cluster with more than 100 of anything rendered "100+"
// forever. The number on a console is the thing an operator actually reads, so
// it has to be the real one; the trailing plus was an artifact of the read,
// not a fact about the cluster.
//
// Where the concept moves, a graph subscription re-runs both, so the console
// answers "what changed" without polling.
//
// Counts are PER-CALLER by construction: the reads run under the viewer's
// own actor and per-row authz, so a reader's console counts the rows a reader
// may see. That is the honest number for the person looking at it.

export interface TileCount {
  count: number;
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
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (!query || status !== "connected") return;
    let stale = false;
    setLoading(true);
    setError("");
    // The count always; a page of rows only when the tile shows a list under
    // it. A tile that is just a number costs one cheap server-side aggregate.
    const reads: [Promise<number>, Promise<Row[]>] = [
      countConcept(query, conceptId),
      keepRows > 0
        ? browseConceptPage(query, conceptId, { pageSize: keepRows, order: "desc" }).then(
            (page) => page.rows,
          )
        : Promise.resolve<Row[]>([]),
    ];

    Promise.all(reads)
      .then(([total, recent]) => {
        if (stale) return;
        setCount(total);
        setRows(recent);
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
  return { rows, count, loading, error, reload };
}

export function payloadOf(row: Row): Record<string, unknown> {
  const p = (row as { payload?: unknown }).payload;
  return typeof p === "object" && p !== null
    ? (p as Record<string, unknown>)
    : (row as Record<string, unknown>);
}
