import { useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useSyncExternalStore } from "react";
import {
  LiveCollection,
  type Connection,
  type LiveCollectionSpec,
  type LiveSnapshot,
  type LiveState,
} from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "./connection";
import { onAppResume } from "./resume";
import { LiveReadVisibility } from "./useGraphRevision";
import type { LiveListSource } from "./LiveList";

// One live collection, retained for the life of the component.
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

const LINGER_MS = 5_000;

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
  reseed: () => void;
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
): LiveCollectionHandle<T> {
  const connection = useOsConnection();
  const visible = useContext(LiveReadVisibility);
  const visibleRef = useRef(visible);
  useLayoutEffect(() => { visibleRef.current = visible; }, [visible]);

  // The spec is read once per (connection, key) and never again: it closes
  // over the caller's props, and re-reading it on every render would rebuild
  // the collection on every render.
  const collection = useMemo(() => {
    if (connection === null || key === null) return null;
    return new LiveCollection<T>(spec(connection), connection.subscriptions ?? null, LINGER_MS);
    // DEPS ARE (connection, key) ON PURPOSE: `spec` closes over the caller's
    // props and is a fresh function on every render, so depending on it would
    // rebuild the collection -- and therefore re-seed and re-subscribe -- on
    // every render. `key` is the contract that says when the read changed.
  }, [connection, key]);

  useEffect(() => {
    if (collection === null) return;
    collection.retain();
    // This hook owns its collection directly, outside the SDK's LiveStore.
    // It must therefore carry the same reconnect lifecycle: subscription
    // replay alone cannot repair a failed seed or recover missed rows.
    // The cycle fires AFTER subscriptions are restored, avoiding a gap
    // between the fresh read and the stream that keeps it current.
    let recoveredAt = -Infinity;
    const stopCycle = connection?.onConnectionCycle?.(() => { recoveredAt = Date.now(); collection.reseed(); });
    const stopStatus = connection?.onStatusChange?.(({ status }) => {
      if (status !== "connected") collection.markDisconnected();
    });
    const stopResume = onAppResume(() => {
      if (visibleRef.current && connection?.status === "connected" && Date.now() - recoveredAt >= 1_000) {
        recoveredAt = Date.now(); collection.reseed();
      }
    });
    return () => {
      stopCycle?.();
      stopStatus?.();
      stopResume();
      collection.release(() => {});
    };
  }, [collection, connection]);

  const snapshot = useSyncExternalStore(
    useMemo(
      () => (collection ? collection.subscribe.bind(collection) : () => () => {}),
      [collection],
    ),
    () => (collection ? collection.snapshot : (EMPTY as LiveSnapshot<T>)),
  );

  const reseed = useCallback(() => collection?.reseed(), [collection]);
  return {
    source: collection,
    snapshot,
    reseed,
  };
}
