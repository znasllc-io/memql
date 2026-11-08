import { useState } from "react";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "./connection";
import { useTwoFeedView } from "./mergedView";
import type { LiveCollectionHandle, PagedCollectionHandle } from "./useLiveCollection";

type MergeFeed<T> = LiveCollectionHandle<T> & Partial<Pick<PagedCollectionHandle<T>, "hasMore" | "loadingMore" | "loadMoreError" | "loadMore">>;
const windows = new WeakMap<Connection, Map<string, number>>();

/** Merge two identically ordered server streams into ten visible rows at a
 * time. Both frontiers advance before exposing the next window; a failed side
 * retries without downloading its already successful sibling again. The
 * visible window belongs to the authenticated session, like the source pages.
 * `manual:false` supports explicit complete-catalog views with computed sorts. */
export function useMergedPagedCollections<A, B, T>(
  key: string,
  first: MergeFeed<A>,
  second: MergeFeed<B>,
  project: (first: readonly A[], second: readonly B[]) => T[],
  { manual = true }: { manual?: boolean } = {},
) {
  const connection = useOsConnection();
  const [, redraw] = useState(0);
  let held = connection ? windows.get(connection) : undefined;
  if (connection && !held) { held = new Map(); windows.set(connection, held); }
  const limit = held?.get(key) ?? 10;
  const available = project(first.snapshot.rows, second.snapshot.rows).length;
  const feeds = [first, second];
  const source = useTwoFeedView(first.source, second.source, `${key}:${limit}`, (a, b) => project(a, b).slice(0, limit));
  return {
    source,
    limit,
    hasMore: (manual && feeds.some(feed => feed.hasMore)) || available > limit,
    loadingMore: feeds.some(feed => feed.loadingMore),
    loadMoreError: first.loadMoreError || second.loadMoreError || "",
    loadMore: async () => {
      if (manual) {
        const target = limit / 10 + 1;
        await Promise.all(feeds.filter(feed => feed.hasMore && (feed.snapshot.pageCount ?? 1) < target).map(feed => feed.loadMore?.()));
        if (feeds.some(feed => feed.source?.snapshot.loadMoreError)) return;
      }
      held?.set(key, limit + 10);
      redraw(value => value + 1);
    },
  };
}
