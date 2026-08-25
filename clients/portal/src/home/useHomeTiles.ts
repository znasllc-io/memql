import { useEffect, useMemo } from "react";
import {
  browseConceptPage,
  countConcept,
  getRowByConceptAndId,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useLive, useLiveValue } from "../cluster/useLive";

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
// Counts are PER-CALLER by construction: the reads run under the viewer's
// own actor and per-row authz, so a reader's console counts the rows a reader
// may see. That is the honest number for the person looking at it.
//
// ===========================================================================
// THE ROWS ARE FOLDED; THE COUNT FOLLOWS THEM (memql#4539)
// ===========================================================================
// The live tiles used to re-run BOTH reads on every CDC event -- the count and
// a page -- so an active audit trail turned the console into a read pair per
// arrival. The rows now ride a LiveCollection and fold, and the count re-reads
// only when the row set actually changed. The count cannot be folded: it is a
// server-side aggregate over the whole concept, and deriving it from a
// five-row page is the "100+" defect this file already fixed once.
//
// A dropped stream used to leave a tile rendering its number with no signal.
// The collection reports `disconnected` and the tile says so.

export interface TileCount {
  count: number;
  loading: boolean;
  error: string;
}

export interface HomeTileState extends TileCount {
  rows: Row[];
  // "live" while the stream is carrying this tile, "disconnected" when it is
  // not, "seeding" / "degraded" in between. A tile that cannot say this is a
  // number an operator has no reason to trust.
  liveness: "seeding" | "live" | "degraded" | "disconnected";
  reload: () => void;
}

export function useConceptTile(
  conceptId: string,
  live: boolean,
  keepRows: number,
): HomeTileState {
  const { query, status } = useCluster();
  const connected = query !== null && status === "connected";

  // The recent-activity page, folded. Only the tiles that render a list open
  // one; a tile that is just a number costs one server-side aggregate and
  // nothing else.
  const rowsLive = useLive<Row>(
    connected && live && keepRows > 0 ? `home:tile:rows:${conceptId}:${keepRows}` : null,
    () => ({
      concept: conceptId,
      // Created only, matching what these tiles are about: what has ARRIVED.
      actions: ["created"],
      paged: false,
      seed: async (_cursor, signal) => {
        if (query === null) return { rows: [], nextCursor: "" };
        const page = await browseConceptPage(query, conceptId, {
          pageSize: keepRows,
          order: "desc",
          signal,
        });
        return { rows: page.rows, nextCursor: "" };
      },
      // An id-only notification resolves through the authorized read, which is
      // exactly the re-read memql#4309 asks a client to do. Folding the event's
      // own payload instead would render a card whose every field is blank --
      // the failure this tile used to be immune to only because it refetched.
      reread: async (rowId, signal) => {
        if (query === null) return null;
        return getRowByConceptAndId(query, conceptId, rowId, { signal });
      },
    }),
  );

  const countValue = useLiveValue<number>(
    connected ? `home:tile:count:${conceptId}` : null,
    async (signal) => {
      if (query === null) return null;
      return countConcept(query, conceptId, { signal });
    },
  );

  // The count follows the rows. `version` is the collection's change counter,
  // so this fires when a row actually arrived rather than on every delivery --
  // and the store's own gap / reconnect re-seed refreshes both sides anyway.
  const rowsVersion = rowsLive.rows.length;
  const refreshCount = countValue.reload;
  useEffect(() => {
    if (!connected || !live || keepRows === 0) return;
    refreshCount();
  }, [rowsVersion, connected, live, keepRows, refreshCount]);

  // The tile shows the newest first, which is the order the seed asked for.
  // The collection preserves arrival order, so late folds land at the end --
  // fine for a set, wrong for a "what just happened" list.
  const rows = useMemo(() => {
    if (keepRows === 0) return [];
    const sorted = [...rowsLive.rows].sort((a, b) => {
      const at = String(a["createdAt"] ?? "");
      const bt = String(b["createdAt"] ?? "");
      return at === bt ? 0 : at < bt ? 1 : -1;
    });
    return sorted.slice(0, keepRows);
  }, [rowsLive.rows, keepRows]);

  // The liveness a tile renders. A count-only tile has no stream behind it, so
  // its honest answer is the connection's own state rather than a collection's.
  const liveness: HomeTileState["liveness"] = !connected
    ? "disconnected"
    : live && keepRows > 0
      ? rowsLive.state
      : countValue.state;

  return {
    rows,
    count: countValue.value ?? 0,
    loading: connected && (countValue.state === "seeding" || (live && rowsLive.state === "seeding")),
    error: countValue.error || rowsLive.error,
    liveness,
    reload: () => {
      countValue.reload();
      rowsLive.reload();
    },
  };
}

export function payloadOf(row: Row): Record<string, unknown> {
  const p = (row as { payload?: unknown }).payload;
  return typeof p === "object" && p !== null
    ? (p as Record<string, unknown>)
    : (row as Record<string, unknown>);
}
