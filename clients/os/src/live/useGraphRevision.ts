import { createContext, useContext, useEffect, useLayoutEffect, useRef, useState } from "react";
import { useConnectionStatus } from "../chrome/connection";
import { useOsConnection } from "./connection";
import { onAppResume } from "./resume";

// The shell keeps parked windows mounted so drafts survive. Their computed
// reads must not keep generating background load while no one can see them.
export const LiveReadVisibility = createContext(true);

/** Virtual reads subscribe to their persisted inputs, never to a synthetic
 * concept that cannot emit. Batch a burst into one invalidation. */
export function useGraphRevision(concepts: readonly string[], enabled = true, { externalRecovery = false, onPaneResume }: { externalRecovery?: boolean; onPaneResume?: () => void } = {}) {
  const connection = useOsConnection();
  const resume = useRef(onPaneResume);
  useLayoutEffect(() => { resume.current = onPaneResume; }, [onPaneResume]);
  const windowVisible = useContext(LiveReadVisibility);
  const [tabVisible, setTabVisible] = useState(() => document.visibilityState !== "hidden");
  useEffect(() => {
    const changed = () => setTabVisible(document.visibilityState !== "hidden");
    document.addEventListener("visibilitychange", changed);
    return () => document.removeEventListener("visibilitychange", changed);
  }, []);
  const paneActive = enabled && windowVisible;
  const active = paneActive && tabVisible;
  const [revision, setRevision] = useState(0);
  const key = concepts.join(",");
  const status = useConnectionStatus();
  const previous = useRef({ active, paneActive, status });
  const lastRecovery = useRef(-Infinity);
  useEffect(() => {
    const before = previous.current;
    previous.current = { active, paneActive, status };
    // Mount already starts its read. Only recovery needs another snapshot.
    // Session collections already recover browser visibility and connection
    // changes. Their computed inputs still need a fresh read when a parked
    // pane is opened, since its input subscriptions were paused.
    const needsRecovery = externalRecovery ? !before.paneActive : !before.active || before.status !== "connected";
    if (active && status === "connected" && needsRecovery) {
      lastRecovery.current = Date.now();
      // A session starts its own repair after a paused collection is retained.
      // Callers can coalesce this same pane-resume signal with that read, while
      // actual graph changes below still request a trailing invalidation.
      if (externalRecovery && resume.current) resume.current();
      else setRevision(value => value + 1);
    }
  }, [active, paneActive, status, externalRecovery]);
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
    // Computed readers only need invalidation, never the potentially large
    // record body. Share compact notifications with other invalidators.
    const remove = key.split(",").filter(Boolean).map(concept => connection.subscriptions.subscribeGraph(changed, { concept, idOnly: true }));
    // Resubscriptions recover delivery; a focused/reconnected surface also
    // rereads inputs to cover events missed while its stream was down.
    const stopResume = externalRecovery ? () => {} : onAppResume(() => {
      if (Date.now() - lastRecovery.current < 1_000) return;
      lastRecovery.current = Date.now();
      changed();
    });
    return () => { remove.forEach(stop => stop()); clearTimeout(timer); stopResume(); };
  }, [connection, key, active, externalRecovery]);
  return revision;
}
