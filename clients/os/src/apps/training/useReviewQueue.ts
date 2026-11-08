import { useCallback, useState, useSyncExternalStore } from "react";
import type { Connection, Row } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../live/connection";
import { usePagedCollection } from "../../live/useLiveCollection";
import { TEXT_DEBOUNCE_MS, useDebouncedValue } from "../../logs/useDebouncedValue";
import { flatten } from "../../kit/rows";
import { chunkFromRow, type Chunk } from "./rows";

export interface ReviewQueue {
  chunks: Chunk[];
  state: "idle" | "loading" | "ready" | "error";
  error: string;
  pagesRead: number;
  exhausted: boolean;
  loadMore: () => void;
  reload: () => void;
  applyDecision: (chunkId: string, status: string) => void;
  search: string;
  setSearch: (search: string) => void;
}

type Decision = { status: string; version: string };
class Decisions {
  rows = new Map<string, Decision>();
  version = 0;
  listeners = new Set<() => void>();
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  snapshot = () => this.version;
  set(id: string, decision: Decision) { this.rows.set(id, decision); this.version++; for (const listener of this.listeners) listener(); }
}
const decisions = new WeakMap<Connection, Decisions>();
const disconnected = new Decisions();
function decisionStore(connection: Connection | null): Decisions {
  if (!connection) return disconnected;
  let store = decisions.get(connection);
  if (!store) { store = new Decisions(); decisions.set(connection, store); }
  return store;
}

/** The server filters pending chunks before paging, so opening Review never
 * walks unrelated domains or full pages of already-reviewed history. */
export function useReviewQueue(enabled = true): ReviewQueue {
  const connection = useOsConnection();
  const [search, setSearch] = useState("");
  const query = useDebouncedValue(search.trim(), TEXT_DEBOUNCE_MS);
  const feed = usePagedCollection<Row>(enabled ? `training:review:${query}` : null, conn => ({
    concept: "v1:knowledge:documentChunk", actions: [],
    seed: async (cursor, signal) => {
      const result = await conn.query.trainingChunksBrowse({ pending: true, search: query, cursor: cursor || undefined }, { signal });
      const page = result.rows()[0];
      if (!page) throw new Error("The review queue returned no result.");
      const data = flatten(page);
      return { rows: (data.rows ?? []) as Row[], nextCursor: typeof data.nextCursor === "string" ? data.nextCursor : "" };
    },
  }));
  const store = decisionStore(connection);
  useSyncExternalStore(store.subscribe, store.snapshot);
  const chunks = feed.snapshot.rows.map(raw => {
    const chunk = chunkFromRow(raw);
    const decision = store.rows.get(chunk.id);
    return decision && decision.version === chunk.createdAt ? { ...chunk, validationStatus: decision.status } : chunk;
  });
  const applyDecision = useCallback((chunkId: string, status: string) => {
    const chunk = feed.snapshot.rows.map(chunkFromRow).find(row => row.id === chunkId);
    if (chunk) store.set(chunkId, { status, version: chunk.createdAt });
  }, [store, feed.snapshot.rows]);
  const error = feed.snapshot.error || feed.loadMoreError;
  const state = error ? "error" : !enabled || !connection ? "idle" : feed.snapshot.state === "seeding" || feed.loadingMore ? "loading" : "ready";
  return { chunks, state, error, search, setSearch,
    pagesRead: chunks.length ? Math.ceil(chunks.length / 10) : state === "ready" ? 1 : 0,
    exhausted: !feed.hasMore, loadMore: () => void feed.loadMore(), reload: feed.reseed, applyDecision };
}
