import { useContext, useEffect, useMemo, useSyncExternalStore } from "react";
import type { Connection, LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "./connection";
import { useTwoFeedView } from "./mergedView";
import { LiveReadVisibility } from "./useGraphRevision";
import type { LiveCollectionHandle, PagedCollectionHandle } from "./useLiveCollection";

type MergeFeed<T> = LiveCollectionHandle<T> & Partial<Pick<PagedCollectionHandle<T>, "hasMore" | "loadingMore" | "loadMoreError" | "loadMore">>;
class MergeWindow {
  limit = 10;
  pending: Promise<void> | null = null;
  pendingAdvance = false;
  readers = 0;
  private listeners = new Set<() => void>();
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  snapshot = () => this.limit;
  advance(limit: number) {
    this.limit = Math.max(this.limit, limit);
    for (const listener of this.listeners) listener();
  }
}
const windows = new WeakMap<Connection, Map<string, MergeWindow>>();

/** Merge two identically ordered server streams into ten visible rows at a
 * time. Both frontiers advance before exposing the next window; a failed side
 * retries without downloading its already successful sibling again. Live
 * removals refill the requested window without exposing an older sibling ahead
 * of unread rows. The window and pending work belong to the authenticated
 * session, like the source pages. `manual:false` supports explicit complete
 * catalogs with computed sorts. */
export function useMergedPagedCollections<A, B, T>(
  key: string,
  first: MergeFeed<A>,
  second: MergeFeed<B>,
  project: (first: readonly A[], second: readonly B[]) => T[],
  { manual = true }: { manual?: boolean } = {},
) {
  const connection = useOsConnection();
  const visible = useContext(LiveReadVisibility);
  const held = useMemo(() => {
    if (!connection) return new MergeWindow();
    let session = windows.get(connection);
    if (!session) { session = new Map(); windows.set(connection, session); }
    let window = session.get(key);
    if (!window) { window = new MergeWindow(); session.set(key, window); }
    return window;
  }, [connection, key]);
  const limit = useSyncExternalStore(held.subscribe, held.snapshot);
  useEffect(() => {
    if (!visible) return;
    held.readers++;
    return () => { held.readers--; };
  }, [held, visible]);
  const available = project(first.snapshot.rows, second.snapshot.rows).length;
  const feeds = [first, second];
  const frontiers = [
    { feed: first, count: () => project(first.source?.snapshot.rows ?? [], []).length },
    { feed: second, count: () => project([], second.source?.snapshot.rows ?? []).length },
  ];
  const reliableLimit = () => {
    let reliable = limit;
    if (manual) {
      for (const { feed, count } of frontiers) {
        // Read the source NOW: subscribers may inspect this view between the
        // graph event and React's next render. An unresolved initial seed is
        // an unknown frontier even before its cursor arrives.
        const snapshot = feed.source?.snapshot;
        if (!snapshot || snapshot.hasMore || snapshot.state === "seeding"
          || (!snapshot.rows.length && (snapshot.state !== "live" || snapshot.error))) {
          reliable = Math.min(reliable, count());
        }
      }
    }
    return reliable;
  };
  const merged = useTwoFeedView(first.source, second.source, `${key}:${limit}`, (a, b) => project(a, b).slice(0, reliableLimit()));
  const source = useMemo(() => {
    if (!merged) return null;
    let previous: LiveSnapshot<T> | null = null;
    let cached: LiveSnapshot<T>;
    return {
      subscribe: merged.subscribe,
      get snapshot(): LiveSnapshot<T> {
        const snapshot = merged.snapshot;
        if (snapshot !== previous) {
          previous = snapshot;
          // Withholding an unknown frontier is not a settled empty list.
          // Keep the ordinary loading/refusal presentation until it is known.
          cached = snapshot.state === "live" && reliableLimit() < limit
            ? { ...snapshot, state: frontiers.some(({ feed }) => feed.source?.snapshot.loadMoreError) ? "degraded" : "seeding" }
            : snapshot;
        }
        return cached;
      },
    };
  }, [merged, manual]);

  const fill = (target: number, advance: boolean): Promise<void> => {
    const work = (async () => {
      if (manual) {
        // Page count proves progress, not coverage: live removals may leave
        // fewer eligible rows than the number of pages suggests.
        while (true) {
          if (held.readers === 0) return;
          if (frontiers.some(({ feed }) => {
            const snapshot = feed.source?.snapshot;
            return !snapshot || snapshot.state !== "live" || snapshot.error || snapshot.loadingMore;
          })) return;
          const incomplete = frontiers.filter(({ feed, count }) => {
            const snapshot = feed.source!.snapshot;
            return snapshot.hasMore && (snapshot.loadMoreError || count() < target);
          });
          if (!incomplete.length) break;
          const before = incomplete.map(({ feed }) => feed.source!.snapshot.pageCount ?? 1);
          await Promise.all(incomplete.map(({ feed }) => feed.loadMore?.()));
          // Interrupted, overlapping, or seed-busy reads must not be mistaken
          // for a completed page, and must never spin on the same frontier.
          if (incomplete.some(({ feed }, i) => {
            const snapshot = feed.source?.snapshot;
            return !snapshot || snapshot.state !== "live" || snapshot.error || snapshot.loadMoreError
              || snapshot.loadingMore || (snapshot.pageCount ?? 1) <= before[i]!;
          })) return;
        }
      }
      if (advance) held.advance(target);
    })();
    const pending = work.finally(() => { if (held.pending === pending) held.pending = null; });
    held.pendingAdvance = advance;
    held.pending = pending;
    return pending;
  };

  useEffect(() => {
    if (!manual || !visible || held.pending) return;
    if (frontiers.some(({ feed }) => {
      const snapshot = feed.source?.snapshot;
      return !snapshot || snapshot.state !== "live" || snapshot.error || snapshot.loadingMore || snapshot.loadMoreError;
    })) return;
    if (frontiers.some(({ feed, count }) => feed.source!.snapshot.hasMore && count() < held.limit)) {
      // Repair only the window already requested. Errors require an explicit
      // retry; a background repair never grows the visible limit.
      void fill(held.limit, false);
    }
    // Project semantics are part of `key`; snapshots are the only reason to
    // reconsider a stopped repair. Unrelated renders cannot retry it.
  }, [held, manual, visible, first.snapshot, second.snapshot]);

  const loadMore = async (): Promise<void> => {
    if (held.pending) {
      if (held.pendingAdvance) return held.pending;
      // A click during current-window repair still asks for the next ten.
      // Concurrent clicks join the same subsequent advance.
      await held.pending;
      return loadMore();
    }
    return fill(held.limit + 10, true);
  };
  return {
    source,
    limit,
    hasMore: (manual && feeds.some(feed => feed.hasMore)) || available > limit,
    loadingMore: feeds.some(feed => feed.loadingMore),
    loadMoreError: first.loadMoreError || second.loadMoreError || "",
    loadMore,
  };
}
