import { useEffect, useState } from "react";
import { useConnectionStatus } from "../chrome/connection";
import { useOsConnection } from "./connection";

/** Virtual reads subscribe to their persisted inputs, never to a synthetic
 * concept that cannot emit. Batch a burst into one invalidation. */
export function useGraphRevision(concepts: readonly string[], enabled = true) {
  const connection = useOsConnection();
  const [revision, setRevision] = useState(0);
  const key = concepts.join(",");
  const status = useConnectionStatus();
  useEffect(() => { if (enabled && status === "connected") setRevision(value => value + 1); }, [enabled, status]);
  useEffect(() => {
    if (!enabled || !connection?.subscriptions) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const changed = () => {
      if (timer !== undefined) return;
      timer = setTimeout(() => { timer = undefined; setRevision(value => value + 1); }, 100);
    };
    const remove = key.split(",").filter(Boolean).map(concept => connection.subscriptions.subscribeGraph(changed, { concept }));
    // Resubscriptions recover delivery; a focused/reconnected surface also
    // rereads inputs to cover events missed while its stream was down.
    window.addEventListener("online", changed);
    window.addEventListener("focus", changed);
    return () => { remove.forEach(stop => stop()); clearTimeout(timer); window.removeEventListener("online", changed); window.removeEventListener("focus", changed); };
  }, [connection, key, enabled]);
  return revision;
}
