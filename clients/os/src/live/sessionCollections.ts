import { LiveStore, type Connection, type LiveCollection, type LiveCollectionSpec } from "@znasllc-io/memql-sdk-core/client";
import { onAppResume } from "./resume";

// One cache and one event-continuity observer per authenticated connection.
// Only committed, visible consumers retain a feed. Closing a window pauses its
// subscription after the short navigation grace period; rows and page cursors
// remain in memory until sign-out or a document reload.
const sessions = new WeakMap<Connection, SessionCollections>();

class SessionCollections {
  private readonly store: LiveStore;
  private readonly active = new Map<LiveCollection<unknown>, number>();
  private stopResume: (() => void) | null = null;
  private recoveredAt = -Infinity;

  constructor(private readonly connection: Connection) {
    this.store = new LiveStore({
      subscriptions: connection.subscriptions ?? null,
      onStatusChange: connection.onStatusChange?.bind(connection),
      onConnectionCycle: connection.onConnectionCycle ? handler => connection.onConnectionCycle(cycle => {
        this.recoveredAt = Date.now();
        handler(cycle);
      }) : undefined,
    }, { lingerMs: 5_000, cacheReleased: true });

  }

  prepare<T>(key: string, spec: LiveCollectionSpec<T>): LiveCollection<T> {
    return this.store.prepareCollection(key, spec);
  }

  // Component-owned readers still share the connection's continuity judge.
  // Their committed effect releases this listener when the component leaves;
  // no component closures or rows enter the session cache.
  onGap(handler: () => void): () => void {
    return this.store.onGap(handler);
  }

  retain<T>(collection: LiveCollection<T>): () => void {
    const shared = collection as LiveCollection<unknown>;
    if (!this.stopResume) this.stopResume = onAppResume(() => {
      if (this.connection.status !== "connected" || Date.now() - this.recoveredAt < 1_000) return;
      this.recoveredAt = Date.now();
      for (const active of this.active.keys()) active.reseed();
    });
    this.active.set(shared, (this.active.get(shared) ?? 0) + 1);
    collection.retain();
    let released = false;
    return () => {
      if (released) return;
      released = true;
      const remaining = (this.active.get(shared) ?? 1) - 1;
      if (remaining > 0) this.active.set(shared, remaining);
      else this.active.delete(shared);
      collection.release(() => {});
      if (!this.active.size) {
        this.stopResume?.();
        this.stopResume = null;
      }
    };
  }

  dispose(): void {
    this.stopResume?.();
    this.stopResume = null;
    this.active.clear();
    this.store.dispose();
  }
}

export function sessionCollections(connection: Connection): SessionCollections {
  let session = sessions.get(connection);
  if (!session) {
    session = new SessionCollections(connection);
    sessions.set(connection, session);
  }
  return session;
}

export function disposeSessionCollections(connection: Connection): void {
  sessions.get(connection)?.dispose();
  sessions.delete(connection);
}
