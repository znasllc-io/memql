import { useCallback, useContext, useEffect, useMemo, useRef, useSyncExternalStore } from "react";
import {
  LiveCollection,
  versionOrder,
  type Connection,
  type LiveCollectionSpec,
  type LiveSnapshot,
  type LiveState,
  type LiveReseedOptions,
} from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "./connection";
import { sessionCollections } from "./sessionCollections";
import { LiveReadVisibility } from "./useGraphRevision";
import type { LiveListSource } from "./LiveList";
import { onAppResume } from "./resume";

// One live collection per connection and resource key, shared across windows.
//
// ===========================================================================
// WHY THIS EXISTS RATHER THAN new LiveCollection(...) AT EACH CALL SITE
// ===========================================================================
// A LiveCollection does nothing until it is retained. `start()` -- which
// opens the subscription and runs the seed -- is reachable from `retain()`
// and from nowhere else; `subscribe()` only registers a change listener. So a
// call site that constructs one and subscribes gets a collection that stays
// in `seeding` with no rows, forever, with nothing logged and nothing thrown.
// It renders as "Loading from the cluster" and never resolves, and an empty
// list is a completely plausible answer, which is what makes the mistake
// survive review.
//
// The foundation's machines feed made exactly that mistake. One hook, used by
// every live surface in the OS, is how it stops being possible to make twice.
//
// RETAIN INSIDE THE EFFECT, never during render: React 19's StrictMode
// renders twice before committing, so a retain taken during render is never
// balanced by the cleanup and the collection outlives its consumer. The
// double MOUNT is harmless -- release lingers, so the second retain reuses
// the same collection and issues no new read.

const EMPTY: LiveSnapshot<never> = { rows: [], state: "disconnected", error: "", version: 0 };

/**
 * Whether a feed is BEHIND -- showing rows it can no longer promise are
 * current.
 *
 * `seeding` is deliberately not behind: it is work in progress, and offering
 * a re-read for it invites a second read of the one already running.
 * `degraded` and `disconnected` are the two states where the rows on screen
 * are the last known answer, which is the only condition under which a manual
 * re-read is worth offering -- on a healthy feed a refresh control quietly
 * contradicts the thing it sits next to. A failed initial seed has no rows
 * to degrade and can report `live` with an error; that also needs recovery.
 */
export function feedIsBehind(state: LiveState, error = ""): boolean {
  return error !== "" || state === "degraded" || state === "disconnected";
}

export interface LiveCollectionHandle<T> {
  /** Null until the connection exists. LiveList renders the disconnected
   *  caption for null rather than a fake empty list. */
  source: LiveListSource<T> | null;
  snapshot: LiveSnapshot<T>;
  /** Re-run the read. The honest answer to "this looks stale", and the same
   *  mechanism a gap and a reconnect already use. */
  reseed(): void;
  reseed(options: LiveReseedOptions): void;
}

export interface LiveCollectionOptions {
  cacheReleased?: boolean;
  /** Explicit connection for injected read clients; omitted uses the OS session. */
  connection?: Connection | null;
}

/**
 * `key` is the collection's IDENTITY. It must encode everything the spec
 * closes over that changes what is READ -- and must NOT encode anything that
 * merely arrives late, because a key change restarts the collection from
 * empty, which unmounts whatever the operator had open.
 */
export function useLiveCollection<T>(
  key: string | null,
  spec: (connection: Connection) => LiveCollectionSpec<T>,
  { cacheReleased = true, connection: suppliedConnection }: LiveCollectionOptions = {},
): LiveCollectionHandle<T> {
  const sessionConnection = useOsConnection();
  const connection = suppliedConnection === undefined ? sessionConnection : suppliedConnection;
  const visible = useContext(LiveReadVisibility);
  const visibleRef = useRef(visible);
  visibleRef.current = visible;

  // The spec is read once per (connection, key) and never again: it closes
  // over the caller's props, and re-reading it on every render would rebuild
  // the collection on every render.
  const collection = useMemo(() => {
    if (connection === null || key === null) return null;
    // Legacy composed readers own React setters and mutable dependency refs.
    // They cannot survive their component's lifetime in the session cache.
    // Ordinary immutable resource/page specs always use the shared store.
    if (!cacheReleased) return new LiveCollection<T>(spec(connection), connection.subscriptions ?? null, 5_000);
    return sessionCollections(connection).prepare(key, spec(connection));
    // DEPS ARE (connection, key) ON PURPOSE: `spec` closes over the caller's
    // props and is a fresh function on every render, so depending on it would
    // rebuild the collection -- and therefore re-seed and re-subscribe -- on
    // every render. `key` is the contract that says when the read changed.
  }, [connection, key, cacheReleased]);

  useEffect(() => {
    if (collection === null || connection === null) return;
    if (cacheReleased) return visible ? sessionCollections(connection).retain(collection) : undefined;
    let recoveredAt = -Infinity;
    // The session observes stream-wide sequence gaps, including gaps delivered
    // to another concept. Its onGap also covers reconnect after replay, so a
    // second connection-cycle listener would ask for the same seed twice.
    const stopGap = sessionCollections(connection).onGap(() => { recoveredAt = Date.now(); collection.reseed(); });
    collection.retain();
    const stopStatus = connection.onStatusChange?.(({ status }) => { if (status !== "connected") collection.markDisconnected(); });
    const stopResume = onAppResume(() => {
      if (visibleRef.current && connection.status === "connected" && Date.now() - recoveredAt >= 1_000) {
        recoveredAt = Date.now(); collection.reseed();
      }
    });
    return () => { stopGap(); stopStatus?.(); stopResume(); collection.release(() => {}); };
  }, [collection, connection, cacheReleased, cacheReleased ? visible : true]);

  const snapshot = useSyncExternalStore(
    useMemo(
      () => (collection ? collection.subscribe.bind(collection) : () => () => {}),
      [collection],
    ),
    () => (collection ? collection.snapshot : (EMPTY as LiveSnapshot<T>)),
  );

  const reseed = useCallback((options?: LiveReseedOptions) => collection?.reseed(options), [collection]);
  return {
    source: collection,
    snapshot,
    reseed,
  };
}

export interface PagedCollectionHandle<T> extends LiveCollectionHandle<T> {
  loadMore: () => Promise<void>;
  hasMore: boolean;
  loadingMore: boolean;
  loadMoreError: string;
  progress?: number;
  enabled?: boolean;
  scope?: unknown;
}

/** The server query supplies a ten-row page and searches before pagination. */
export function usePagedCollection<T>(
  key: string | null,
  spec: (connection: Connection) => LiveCollectionSpec<T>,
  options: LiveCollectionOptions = {},
): PagedCollectionHandle<T> {
  const handle = useLiveCollection<T>(key, connection => ({ comparePageRows: compareRecentRows, ...spec(connection), manualPages: true }), options);
  const loadMore = useCallback(async () => {
    // The source is the collection prepared above; keeping the ordinary list
    // interface small lets projected views continue to wrap the same source.
    const source = handle.source;
    if (source && "loadMore" in source && typeof source.loadMore === "function") await source.loadMore();
  }, [handle.source]);
  return { ...handle, loadMore, hasMore: handle.snapshot.hasMore ?? false,
    progress: handle.snapshot.pageCount ?? 0, scope: handle.source,
    enabled: handle.snapshot.state === "live" && !handle.snapshot.error,
    loadingMore: handle.snapshot.loadingMore ?? false, loadMoreError: handle.snapshot.loadMoreError ?? "" };
}

/** Match the engine's ordinary row.createdAt DESC, row.id ASC page order.
 * Resource timestamps (queuedAt/registeredAt) supply their own comparator. */
function compareRecentRows<T>(left: T, right: T): number {
  const fields = (value: T): Record<string, unknown> => value && typeof value === "object" ? value as Record<string, unknown> : {};
  const a = fields(left), b = fields(right);
  if (typeof a.createdAt !== "string" || typeof b.createdAt !== "string") return 0;
  const order = versionOrder(b.createdAt, a.createdAt);
  if (order === null) return 0;
  if (order !== 0) return order;
  const aid = String(a.id ?? ""), bid = String(b.id ?? "");
  return aid < bid ? -1 : aid > bid ? 1 : 0;
}
