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

export interface LiveView<T> extends LiveListSource<T> {
  readonly snapshot: LiveSnapshot<T>;
}

export function useLiveView<T>(
  source: LiveListSource<T> | null,
  viewKey: string,
  transform: (rows: readonly T[]) => T[],
): LiveView<T> | null {
  const transformRef = useRef(transform);
  transformRef.current = transform;

  return useMemo(() => {
    if (source === null) return null;
    let cachedFrom: LiveSnapshot<T> | null = null;
    let cached: LiveSnapshot<T> | null = null;
    return {
      subscribe: (listener: () => void) => source.subscribe(listener),
      get snapshot(): LiveSnapshot<T> {
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
