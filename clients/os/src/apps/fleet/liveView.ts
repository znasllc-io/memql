import { useMemo, useRef } from "react";
import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";

import type { LiveListSource } from "../../live/LiveList";

// A view over a live source: the same feed, narrowed and ordered for one
// surface.
//
// LiveList's `source` is deliberately a minimal seam -- "anything with the
// useSyncExternalStore shape plugs in" -- and this is what that seam is for.
// The Fleet's two toggles (show revoked machines, show released workspaces)
// and the workbenches' replica grouping are all questions about which of the
// rows already here belong on screen, and in what order. None of them is a
// reason to open a second subscription or re-read.
//
// ===========================================================================
// THE SNAPSHOT MUST BE IDENTITY-STABLE
// ===========================================================================
// useSyncExternalStore calls getSnapshot on every render and compares with
// Object.is. A view that recomputed on each call would return a fresh array
// every time, which React reads as "changed again" and re-renders forever.
// So the transformed snapshot is cached against the UPSTREAM SNAPSHOT'S
// IDENTITY -- the collection already caches that until something actually
// changes, so one memo here is enough.
//
// `viewKey` is the transform's inputs written down. The transform itself is
// read through a ref, so a closure recreated on every render does not rebuild
// the view; the key is what says the transform now MEANS something different.

export interface LiveView<U> extends LiveListSource<U> {
  readonly snapshot: LiveSnapshot<U>;
}

/**
 * `T` is the collection's row type and `U` the surface's. They differ, and
 * that is the point.
 *
 * A collection holds RAW wire rows, because its fold has to: an arriving
 * event's payload is upserted AS the row type with no projection hook in
 * between (`liveCollection.ts`: `upsert(id, payload as unknown as T)`). A
 * collection typed with a projected row is therefore correct exactly until
 * the first update, and then holds a raw row that every derived field is
 * missing from -- which is not a rendering glitch but a throw, the moment a
 * predicate touches a field the wire row does not carry.
 *
 * So projection belongs HERE, on the read side, where it runs over whatever
 * the collection currently holds. That is also what the portal does
 * (`useLive<Row>` everywhere, projected in a useMemo); this is the same rule
 * with the filtering and ordering folded into the same pass.
 */
export function useLiveView<T, U = T>(
  source: LiveListSource<T> | null,
  viewKey: string,
  transform: (rows: readonly T[]) => U[],
): LiveView<U> | null {
  const transformRef = useRef(transform);
  transformRef.current = transform;

  return useMemo(() => {
    if (source === null) return null;
    let cachedFrom: LiveSnapshot<T> | null = null;
    let cached: LiveSnapshot<U> | null = null;
    return {
      subscribe: (listener: () => void) => source.subscribe(listener),
      get snapshot(): LiveSnapshot<U> {
        const upstream = source.snapshot;
        if (cachedFrom === upstream && cached !== null) return cached;
        cachedFrom = upstream;
        // State, error and version pass through unchanged: they describe the
        // FEED, and a view that reorders rows must not also claim the feed is
        // in a different condition than it is.
        cached = { ...upstream, rows: transformRef.current(upstream.rows) };
        return cached;
      },
    };
  }, [source, viewKey]);
}
