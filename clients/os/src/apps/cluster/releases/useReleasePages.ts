import type { Connection } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../../live/connection";
import { usePagedCollection } from "../../../live/useLiveCollection";

// Browser components accept a query seam so their lifecycle inspection can be
// exercised without a websocket. Real OS callers share the authenticated cache;
// injected read-only clients receive one stable cache identity of their own.
const adapters = new WeakMap<object, Connection>();
function adapter(query: object): Connection {
  let held = adapters.get(query);
  if (!held) {
    held = { query, subscriptions: null, status: "connected" } as unknown as Connection;
    adapters.set(query, held);
  }
  return held;
}

export function useReleasePages<T>(key: string, query: object | null, read: ((cursor: string, signal: AbortSignal) => Promise<{ rows: T[]; nextCursor: string }>) | null, visible: boolean, connected: boolean) {
  const session = useOsConnection();
  const connection = query ? session?.query === query ? session : adapter(query) : null;
  const feed = usePagedCollection<T>(read ? `cluster:${key}` : null, () => ({
    concept: "v1:cluster:releaseHistory", actions: [],
    seed: (cursor, signal) => read!(cursor, signal),
  }), { connection });
  const ready = feed.snapshot.state !== "seeding" && feed.snapshot.state !== "disconnected";
  return { ...feed, value: ready || feed.snapshot.rows.length ? feed.snapshot.rows : null,
    error: feed.snapshot.error, retry: feed.reseed, fresh: ready && !feed.snapshot.error && visible && connected };
}
