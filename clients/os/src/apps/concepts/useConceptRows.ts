import { useCallback, useEffect, useRef, useState } from "react";
import { browseConceptPage, getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { usePagedCollection } from "../../live/useLiveCollection";
import { useOsConnection } from "../../live/connection";

// A retained, newest-first snapshot of the selected concept, loaded on demand.
// Generic inspection keeps a separate change band so opening a raw record
// remains a view of the same snapshot until the person asks to reload it.
// Id-only events are authorized by rereading before they are counted.

export type WalkStatus = "loading" | "more" | "exhausted" | "failed";

export interface ConceptRowsWalk {
  rows: Row[];
  status: WalkStatus;
  /** The server's own sentence when the walk failed. */
  error: string;
  /** Rows created since this walk started that it has not loaded. */
  arrivedCount: number;
  /** Loaded rows the cluster has changed or removed since. */
  changedCount: number;
  loadMore: () => void;
  /** Refresh the loaded page depth from the first page, clearing the band. */
  reload: () => void;
  retry?: () => void;
}

export function useConceptRows(conceptId: string, pageSize: number, search = ""): ConceptRowsWalk {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const subscriptions = connection?.subscriptions ?? null;
  const [arrivedCount, setArrivedCount] = useState(0);
  const [changedCount, setChangedCount] = useState(0);
  const [attempt, setAttempt] = useState(0);
  const loadedIds = useRef<Set<string>>(new Set());
  const arrivedIds = useRef<Set<string>>(new Set());
  const feed = usePagedCollection<Row>(conceptId ? `concept:rows:${JSON.stringify([conceptId, pageSize, search.trim()])}:recent` : null, connection => ({
    concept: conceptId,
    // The generic inspector deliberately keeps its explicit change band.
    // Rows are a retained snapshot; reload is the single refresh point.
    actions: [],
    seed: (cursor, signal) => browseConceptPage(connection.query, conceptId, { pageSize, order: "desc", search, signal, ...(cursor ? { cursor } : {}) }),
    reread: (id, signal) => getRowByConceptAndId(connection.query, conceptId, id, { signal }),
  }));
  const rows = feed.snapshot.rows;
  loadedIds.current = new Set(rows.map(rowIdOf));
  const identity = `${conceptId}:${pageSize}:${search.trim()}`;
  useEffect(() => {
    arrivedIds.current.clear();
    setArrivedCount(0);
    setChangedCount(0);
  }, [identity]);
  useEffect(() => {
    for (const id of loadedIds.current) arrivedIds.current.delete(id);
    setArrivedCount(arrivedIds.current.size);
  }, [feed.snapshot]);
  const reload = useCallback(() => {
    arrivedIds.current.clear();
    setArrivedCount(0);
    setChangedCount(0);
    setAttempt(n => n + 1);
    feed.reseed();
  }, [feed.reseed]);
  const error = feed.loadMoreError || feed.snapshot.error;
  const status: WalkStatus = error ? "failed" : feed.snapshot.state === "seeding" || feed.loadingMore ? "loading" : feed.hasMore ? "more" : "exhausted";
  const loadMore = () => { void feed.loadMore(); };
  const retry = feed.loadMoreError ? loadMore : reload;

  // ---- the band --------------------------------------------------------

  useEffect(() => {
    if (subscriptions === null || conceptId === "" || query === null) return;
    let live = true;

    const unregister = subscriptions.subscribeGraph(
      (event) => {
        if (!live) return;
        const payload = event.payload ?? {};
        const id = typeof payload["id"] === "string" ? payload["id"] : "";
        if (id === "") return;

        const record = () => {
          if (loadedIds.current.has(id)) {
            setChangedCount((n) => n + 1);
            return;
          }
          if (arrivedIds.current.has(id)) return;
          arrivedIds.current.add(id);
          setArrivedCount(arrivedIds.current.size);
        };

        if (!event.payloadOmitted) {
          record();
          return;
        }
        // Id-only: re-read under this caller's own authority. A refusal
        // means the row was never theirs to be told about, so it is dropped
        // rather than counted.
        getRowByConceptAndId(query, conceptId, id)
          .then((row) => {
            if (!live || row === null) return;
            record();
          })
          .catch(() => {
            // Silence is the correct answer here, and the only one.
          });
      },
      { concept: conceptId },
    );

    return () => {
      live = false;
      unregister();
    };
    // The handler reads its live state through refs, so this registers once
    // per (connection, concept) and stays registered -- a registration
    // effect that keyed on the loaded rows would tear down and re-register
    // on every page and lose whatever arrived in between.
  }, [subscriptions, query, conceptId, attempt]);

  return { rows: [...rows], status, error, arrivedCount, changedCount, loadMore, reload, retry };
}

/**
 * A row's id, from either shape.
 *
 * `browseConceptPage` returns RAW nodes, which carry the intrinsics nested;
 * a shaped read flattens them to the top level. Reading only one spelling
 * makes every row look id-less, which silently disables the band's dedupe.
 */
export function rowIdOf(row: Row): string {
  const direct = row["id"];
  if (typeof direct === "string" && direct !== "") return direct;
  const nested = row["node"];
  if (nested !== null && typeof nested === "object") {
    const inner = (nested as Record<string, unknown>)["id"];
    if (typeof inner === "string") return inner;
  }
  return "";
}
