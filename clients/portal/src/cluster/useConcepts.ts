import { useEffect, useState } from "react";
import type { Concept } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "./ClusterProvider";

export interface ConceptsState {
  concepts: Concept[];
  loading: boolean;
  error: string;
}

// useConcepts reads the cluster's concept registry (ConceptsListMsg).
//
// Re-fetched whenever the QueryClient identity changes, which is exactly once
// per successful dial -- so a reconnect refreshes the registry and a
// disconnect empties it rather than leaving a stale list on screen looking
// live.
export function useConcepts(): ConceptsState {
  const { query } = useCluster();
  const [concepts, setConcepts] = useState<Concept[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (query === null) {
      setConcepts([]);
      setLoading(false);
      setError("");
      return;
    }

    // Dropped rather than aborted on unmount: the SDK's QueryCallOptions do
    // take an AbortSignal, but the reply still has to be read off the shared
    // stream, and a settle that lands after unmount must not write state.
    let live = true;
    setLoading(true);
    setError("");

    void query
      .listConcepts()
      .then((list) => {
        if (!live) return;
        // Sorted here rather than at each render site: the wire order is the
        // registry's iteration order, which is stable but arbitrary, and two
        // screens listing the same registry differently is its own small bug.
        setConcepts([...list].sort((a, b) => a.id.localeCompare(b.id)));
      })
      .catch((err: unknown) => {
        if (!live) return;
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query]);

  return { concepts, loading, error };
}
