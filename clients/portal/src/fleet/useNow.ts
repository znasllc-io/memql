import { useEffect, useState } from "react";

// A clock that re-renders.
//
// The online dot is derived from `lastSeenAt` against NOW (see online.ts), and
// nothing pushes an event when a machine STOPS heartbeating -- the absence of
// a beat is the signal. Without a ticker the dot would stay lit on the last
// state an event happened to paint, so a machine that died two minutes ago
// would still read as reachable until something unrelated re-rendered the page.
//
// Five seconds against a thirty-second window: fine enough that the dot goes
// dark within one tick of the window closing, coarse enough that a list of
// machines is not re-rendering six times a second for a value that changes
// twice a minute.
export const FLEET_TICK_MS = 5000;

export function useNow(intervalMs: number = FLEET_TICK_MS): Date {
  const [now, setNow] = useState<Date>(() => new Date());

  useEffect(() => {
    const timer = globalThis.setInterval(() => setNow(new Date()), intervalMs);
    return () => globalThis.clearInterval(timer);
  }, [intervalMs]);

  return now;
}
