import { useMemo, useRef } from "react";
import type { LiveSnapshot, LiveState } from "@znasllc-io/memql-sdk-core/client";

import type { LiveListSource } from "./LiveList";
import type { LiveView } from "./liveView";

// ONE LIST OVER SEVERAL FEEDS.
//
// ===========================================================================
// WHY `useLiveView` IS NOT ENOUGH HERE
// ===========================================================================
// `useLiveView` caches its transformed snapshot against ONE upstream
// snapshot's identity, which is exactly right for a view that narrows one
// feed. A list that JOINS feeds is not that: the Bin lists archived artifacts
// AND archived folders, and the Deployables list joins the site feed to the
// package feed and to the parked runs, each arriving on its own read over its
// own concept.
//
// Reading the second feed from inside a `useLiveView` transform LOOKS like it
// works and does not. The cache is keyed on the first snapshot, so a row that
// arrives on the second feed while the first is unchanged is folded into
// nothing: the cached rows are returned, the list never moves, and there is no
// error anywhere -- the row is simply missing. That is the failure this module
// exists to make impossible, and it was found by a test asserting a folder was
// on screen rather than by anything going wrong at runtime.
//
// So the cache is keyed on EVERY snapshot and the version is the SUM of them,
// which is what makes the arrival cue fire for a change on any feed:
// `useArrivals` re-folds on a version change and nothing else. A change to a
// package -- the update chip's flip -- therefore rings the rows it produced,
// which is the README's rule that the update needs both the cue and the chip.
//
// PROMOTED FROM apps/bin (epic memql#4885), where it was local as
// `useTwoFeedView` with a note that a second surface would move it here. The
// three-feed form is the same view over one more source; both are wrappers
// over `mergedView`, so the state and version rules exist once.

/** The transformed snapshot's caption describes the WHOLE list: a surface
 *  half of whose rows have not arrived is not "live", and claiming it is
 *  would leave somebody reading a partial list as if it were complete. */
const STATE_RANK: LiveState[] = ["live", "seeding", "degraded", "disconnected"];

function worseState(a: LiveState, b: LiveState): LiveState {
  return STATE_RANK.indexOf(a) >= STATE_RANK.indexOf(b) ? a : b;
}

/**
 * The view over N sources, untyped at the seam: the typed wrappers below are
 * what callers use, and they are the only place a row type is named.
 */
function mergedView<U>(
  sources: readonly LiveListSource<unknown>[],
  transform: () => (rows: readonly (readonly unknown[])[]) => U[],
): LiveView<U> {
  let cachedFrom: LiveSnapshot<unknown>[] | null = null;
  let cached: LiveSnapshot<U> | null = null;
  return {
    subscribe: (listener: () => void) => {
      const offs = sources.map((source) => source.subscribe(listener));
      return () => {
        for (const off of offs) off();
      };
    },
    get snapshot(): LiveSnapshot<U> {
      const snapshots = sources.map((source) => source.snapshot);
      if (cachedFrom !== null && cached !== null && snapshots.every((s, i) => s === cachedFrom?.[i])) {
        return cached;
      }
      cachedFrom = snapshots;
      cached = {
        rows: transform()(snapshots.map((s) => s.rows)),
        // The worst of them, so the caption describes the whole list rather
        // than its better half.
        state: snapshots.map((s) => s.state).reduce(worseState),
        error: snapshots.map((s) => s.error).find((e) => e !== "") ?? "",
        // Summed, so a change in ANY feed is a change to this view. The
        // arrival cue re-folds on a version change and nothing else.
        version: snapshots.reduce((sum, s) => sum + s.version, 0),
      };
      return cached;
    },
  };
}

export function useTwoFeedView<A, B, U>(
  first: LiveListSource<A> | null,
  second: LiveListSource<B> | null,
  viewKey: string,
  transform: (a: readonly A[], b: readonly B[]) => U[],
): LiveView<U> | null {
  // The transform is read through a ref, so a closure recreated on every
  // render does not rebuild the view; `viewKey` is what says the transform
  // now MEANS something different (a filter changed), and a rebuild on it is
  // how a filter re-baselines the arrival cue rather than announcing every
  // newly-visible row.
  const transformRef = useRef(transform);
  transformRef.current = transform;

  return useMemo(() => {
    if (first === null || second === null) return null;
    return mergedView<U>([first, second], () => (rows) => transformRef.current(rows[0] as A[], rows[1] as B[]));
  }, [first, second, viewKey]);
}

export function useThreeFeedView<A, B, C, U>(
  first: LiveListSource<A> | null,
  second: LiveListSource<B> | null,
  third: LiveListSource<C> | null,
  viewKey: string,
  transform: (a: readonly A[], b: readonly B[], c: readonly C[]) => U[],
): LiveView<U> | null {
  const transformRef = useRef(transform);
  transformRef.current = transform;

  return useMemo(() => {
    if (first === null || second === null || third === null) return null;
    return mergedView<U>(
      [first, second, third],
      () => (rows) => transformRef.current(rows[0] as A[], rows[1] as B[], rows[2] as C[]),
    );
  }, [first, second, third, viewKey]);
}
