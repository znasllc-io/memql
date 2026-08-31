import { useEffect, useRef, useState } from "react";
import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";

import { decayTicks, emptyArrivals, observeSnapshot, TICK_TTL_MS, type ArrivalTick } from "./arrival";

// The arrival cue, as a hook over a snapshot.
//
// ===========================================================================
// WHY THIS IS NOT INSIDE LiveList ANY MORE
// ===========================================================================
// It was, and for two apps that was right: every live surface in the OS was a
// list, so the reducer, the render-time fold and the decay timer lived in the
// one component that rendered rows. The deploy map is the third surface and
// the first that is NOT a list -- it draws the same rows as a graph -- and a
// site whose bundle just flipped has to announce itself there exactly as it
// does in the list beside it, or the two halves of one app disagree about
// whether anything happened.
//
// So the mechanism moves up and `LiveList` becomes its first caller. Promoted
// rather than copied, which is the rule the README states for `kit/`: a second
// copy of a cue is a cue that drifts, and "the map pulses on a heartbeat while
// the list does not" is precisely the bug the fingerprint rule exists to stop.
//
// The FOLD RUNS DURING RENDER, guarded by the snapshot's version, and that is
// deliberate: a tick computed in an effect would paint one frame of the new
// row without its cue, which reads as the row having always been there.

/**
 * Ticks currently worth showing, by row id.
 *
 * `fingerprint` decides what counts as a change, and it is the one thing a
 * caller has to get right: A HEARTBEAT IS NOT NEWS. Naming a field the engine
 * churns -- `lastSeenAt`, `lastUsedAt` -- turns the whole surface into a
 * strobe on a timer. Fingerprint what a person would call a change.
 */
export function useArrivals<T>(
  snapshot: LiveSnapshot<T>,
  rowId: (row: T) => string,
  fingerprint: (row: T) => string,
): Map<string, ArrivalTick> {
  const arrivals = useRef(emptyArrivals());
  const [, bump] = useState(0);
  const seenVersion = useRef(-1);

  if (seenVersion.current !== snapshot.version) {
    seenVersion.current = snapshot.version;
    arrivals.current = observeSnapshot(
      arrivals.current,
      snapshot.rows.map((row) => ({ id: rowId(row), fingerprint: fingerprint(row) })),
      snapshot.state,
      Date.now(),
    );
  }

  // Ticks decay on the CLOCK, not on the next data change, so a quiet surface
  // settles by itself rather than holding a cue until something else happens.
  useEffect(() => {
    if (arrivals.current.ticks.size === 0) return;
    const t = setTimeout(() => {
      arrivals.current = {
        ...arrivals.current,
        ticks: decayTicks(arrivals.current.ticks, Date.now()),
      };
      bump((v) => v + 1);
    }, TICK_TTL_MS + 50);
    return () => clearTimeout(t);
  });

  return arrivals.current.ticks;
}
