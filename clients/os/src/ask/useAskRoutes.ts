import { useCallback, useEffect, useRef, useState } from "react";

import type { AskTransport } from "./askController";
import { routeOptionsFrom, type AskRouteOption } from "./askRoute";

/** Auto alone: what the picker offers before the cluster answers, and on a
 *  cluster that has no routes. Auto is the absence of a choice, so it is
 *  never unavailable. */
export const AUTO_ONLY: readonly AskRouteOption[] = Object.freeze(routeOptionsFrom(null));

/**
 * The routes this person can pick, read from the cluster on mount and again
 * whenever the picker opens -- readiness moves (a machine wakes, an app signs
 * in), and a list read once at sign-in would offer a route that has since
 * gone dark, or hide one that came back.
 *
 * A failed read keeps the last good list rather than collapsing it, so a
 * dropped connection does not take somebody's own routes out from under the
 * choice they already made. A cluster that does not know `routesForActor`
 * answers Auto alone.
 */
export function useAskRoutes(transport: AskTransport): { routes: readonly AskRouteOption[]; refresh: () => void } {
  const [routes, setRoutes] = useState<readonly AskRouteOption[]>(AUTO_ONLY);
  const read = useRef(0);
  const refresh = useCallback(() => {
    if (!transport.routes) return;
    const epoch = ++read.current;
    transport.routes().then(
      next => { if (epoch === read.current) setRoutes(next.length > 0 ? next : AUTO_ONLY); },
      () => { /* keep the last good list */ },
    );
  }, [transport]);
  useEffect(() => {
    refresh();
    return () => { read.current++; };
  }, [refresh]);
  return { routes, refresh };
}
