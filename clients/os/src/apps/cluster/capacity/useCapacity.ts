import { useCallback, useEffect, useState } from "react";
import type { QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { readCapacity, type Capacity } from "./model";

export type CapacityQueries = Pick<QueryClient, "clusterBackgroundCapacity" | "clusterSetBackgroundCapacity">;

export function useCapacity(query: CapacityQueries | null, enabled: boolean) {
  const [revision, bump] = useState(0);
  const refresh = useCallback(() => bump(n => n + 1), []);
  const [snapshot, setSnapshot] = useState<{ source: CapacityQueries | null; value: Capacity | null; error: string; busy: boolean; fresh: boolean }>({ source: query, value: null, error: "", busy: false, fresh: false });
  useEffect(() => {
    if (!query || !enabled || document.visibilityState === "hidden" || navigator.onLine === false) return;
    let current = true;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 10_000);
    const abortHidden = () => { if (document.visibilityState === "hidden" || navigator.onLine === false) {
      controller.abort();
      if (current) setSnapshot(old => ({ ...old, busy: false, fresh: false }));
    } };
    document.addEventListener("visibilitychange", abortHidden);
    window.addEventListener("offline", abortHidden);
    setSnapshot(old => ({ source: query, value: old.source === query ? old.value : null, error: "", busy: true, fresh: false }));
    void query.clusterBackgroundCapacity({}, { signal: controller.signal, priority: "background" }).then(result => {
      if (!current) return;
      if (controller.signal.aborted) {
        setSnapshot(old => ({ ...old, busy: false, fresh: false }));
        return;
      }
      setSnapshot({ source: query, value: readCapacity(result.single()), error: "", busy: false, fresh: true });
    }).catch((e: unknown) => {
      if (current) setSnapshot(old => ({ ...old, busy: false, fresh: false, error: controller.signal.aborted ? "Capacity could not be refreshed." : e instanceof Error ? e.message : String(e) }));
    }).finally(() => { clearTimeout(timeout); });
    return () => { current = false; clearTimeout(timeout); controller.abort(); document.removeEventListener("visibilitychange", abortHidden); window.removeEventListener("offline", abortHidden); };
  }, [query, enabled, revision]);
  const same = snapshot.source === query;
  return { value: same ? snapshot.value : null, error: same ? snapshot.error : "", busy: same && snapshot.busy,
    fresh: enabled && same && snapshot.fresh && document.visibilityState !== "hidden" && navigator.onLine !== false, refresh };
}
