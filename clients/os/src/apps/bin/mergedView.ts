import { useMemo, useRef } from "react";
import type { LiveSnapshot, LiveState } from "@znasllc-io/memql-sdk-core/client";

import type { LiveListSource } from "../../live/LiveList";
import type { LiveView } from "../../live/liveView";

// ONE LIST OVER TWO FEEDS.
//
// ===========================================================================
// WHY `useLiveView` IS NOT ENOUGH HERE
// ===========================================================================
// `useLiveView` caches its transformed snapshot against ONE upstream
// snapshot's identity, which is exactly right for a view that narrows one
// feed. The Bin is not that: it lists archived artifacts AND archived folders,
// which arrive on two separate reads over two separate concepts.
//
// Reading the second feed from inside a `useLiveView` transform LOOKS like it
// works and does not. The cache is keyed on the artifacts snapshot, so a
// folder that arrives while the artifacts are unchanged is folded into
// nothing: the cached rows are returned, the list never moves, and there is no
// error anywhere -- the folder is simply missing. That is the failure this
// module exists to make impossible, and it was found by a test asserting the
// folder was on screen rather than by anything going wrong at runtime.
//
// So the cache is keyed on BOTH snapshots and the version is the SUM of both,
// which is what makes the arrival cue fire for a folder as well as a file:
// `useArrivals` re-folds on a version change and nothing else.

export function useTwoFeedView<A, B, U>(
  first: LiveListSource<A> | null,
  second: LiveListSource<B> | null,
  viewKey: string,
  transform: (a: readonly A[], b: readonly B[]) => U[],
): LiveView<U> | null {
  const transformRef = useRef(transform);
  transformRef.current = transform;

  return useMemo(() => {
    if (first === null || second === null) return null;
    let cachedFrom: [LiveSnapshot<A>, LiveSnapshot<B>] | null = null;
    let cached: LiveSnapshot<U> | null = null;
    return {
      subscribe: (listener: () => void) => {
        const offFirst = first.subscribe(listener);
        const offSecond = second.subscribe(listener);
        return () => {
          offFirst();
          offSecond();
        };
      },
      get snapshot(): LiveSnapshot<U> {
        const a = first.snapshot;
        const b = second.snapshot;
        if (cachedFrom !== null && cachedFrom[0] === a && cachedFrom[1] === b && cached !== null) {
          return cached;
        }
        cachedFrom = [a, b];
        cached = {
          rows: transformRef.current(a.rows, b.rows),
          // THE WORSE OF THE TWO, because the caption has to describe the
          // whole list: a surface half of whose rows have not arrived is not
          // "live", and claiming it is would leave somebody reading a partial
          // Bin as if it were complete.
          state: worseState(a.state, b.state),
          error: a.error || b.error,
          // Summed, so a change in EITHER feed is a change to this view. The
          // arrival cue re-folds on a version change and nothing else.
          version: a.version + b.version,
        };
        return cached;
      },
    };
  }, [first, second, viewKey]);
}

/** Worst-first, so the caption describes the whole list rather than its
 *  better half. */
const STATE_RANK: LiveState[] = ["live", "seeding", "degraded", "disconnected"];

function worseState(a: LiveState, b: LiveState): LiveState {
  return STATE_RANK.indexOf(a) >= STATE_RANK.indexOf(b) ? a : b;
}
