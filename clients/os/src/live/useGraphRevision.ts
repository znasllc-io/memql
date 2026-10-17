import { createContext, useContext, useEffect, useRef, useState } from "react";
import { useConnectionStatus } from "../chrome/connection";
import { useOsConnection } from "./connection";

// The shell keeps parked windows mounted so drafts survive. Their computed
// reads must not keep generating background load while no one can see them.
export const LiveReadVisibility = createContext(true);

/** Virtual reads subscribe to their persisted inputs, never to a synthetic
 * concept that cannot emit. Batch a burst into one invalidation. */
export function useGraphRevision(concepts: readonly string[], enabled = true) {
  const connection = useOsConnection();
  const windowVisible = useContext(LiveReadVisibility);
  const [tabVisible, setTabVisible] = useState(() => document.visibilityState !== "hidden");
  useEffect(() => {
    const changed = () => setTabVisible(document.visibilityState !== "hidden");
    document.addEventListener("visibilitychange", changed);
    return () => document.removeEventListener("visibilitychange", changed);
  }, []);
  const active = enabled && windowVisible && tabVisible;
  const [revision, setRevision] = useState(0);
  const key = concepts.join(",");
  const status = useConnectionStatus();
  const previous = useRef({ active, status });
  useEffect(() => {
    const before = previous.current;
    previous.current = { active, status };
    // Mount already starts its read. Only recovery needs another snapshot.
    if (active && status === "connected" && (!before.active || before.status !== "connected")) {
      setRevision(value => value + 1);
    }
  }, [active, status]);
  useEffect(() => {
    if (!active || !connection?.subscriptions) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let lastInvalidated = -Infinity;
    const changed = () => {
      if (timer !== undefined) return;
      // Respond to an isolated change promptly, but continuous heartbeats
      // cannot ask every computed surface to reread ten times per second.
      const delay = Math.max(100, 1_000 - (Date.now() - lastInvalidated));
      timer = setTimeout(() => {
        timer = undefined;
        lastInvalidated = Date.now();
        setRevision(value => value + 1);
      }, delay);
    };
    const remove = key.split(",").filter(Boolean).map(concept => connection.subscriptions.subscribeGraph(changed, { concept }));
    // Resubscriptions recover delivery; a focused/reconnected surface also
    // rereads inputs to cover events missed while its stream was down.
    window.addEventListener("online", changed);
    window.addEventListener("focus", changed);
    return () => { remove.forEach(stop => stop()); clearTimeout(timer); window.removeEventListener("online", changed); window.removeEventListener("focus", changed); };
  }, [connection, key, active]);
  return revision;
}
