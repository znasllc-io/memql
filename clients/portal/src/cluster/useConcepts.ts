import { useEffect, useRef, useState } from "react";
import type { Concept, ConceptRegistryFollow } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "./ClusterProvider";
import { EMPTY_REGISTRY, applyRegistryDelta, type RegistryState } from "../concepts/registryDelta";

export interface ConceptsState {
  concepts: Concept[];
  loading: boolean;
  error: string;
}

// useConcepts reads the cluster's concept registry and keeps it LIVE (memql#4238).
//
// It opens a follow-mode subscription (QueryClient.subscribeConceptRegistry):
// the engine sends a snapshot then add/remove deltas, so a concept promoted,
// retired or removed into the running cluster appears WITHOUT a reconnect. The
// deltas are folded in place by applyRegistryDelta, preserving the sorted-by-id
// invariant; a generation gap (a dropped delta) triggers a re-snapshot rather
// than a corrupted list.
//
// Re-subscribed whenever the QueryClient identity changes (once per dial); a
// disconnect empties it rather than leaving a stale list on screen looking live.
export function useConcepts(): ConceptsState {
  const { query } = useCluster();
  const [concepts, setConcepts] = useState<Concept[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  // The reduced registry state lives in a ref, not React state: the delta
  // handler must read the CURRENT state to fold onto (and to detect a gap)
  // without re-running the subscribe effect on every delta.
  const stateRef = useRef<RegistryState>(EMPTY_REGISTRY);

  useEffect(() => {
    if (query === null) {
      stateRef.current = EMPTY_REGISTRY;
      setConcepts([]);
      setLoading(false);
      setError("");
      return;
    }

    let live = true;
    let follow: ConceptRegistryFollow | null = null;
    setLoading(true);
    setError("");
    stateRef.current = EMPTY_REGISTRY;

    // subscribe (re-)opens the follow stream. A generation gap re-runs exactly
    // this, after tearing the previous stream down and resetting the
    // accumulator -- the honest response to "you missed a delta".
    const subscribe = (): void => {
      try {
        follow = query.subscribeConceptRegistry((delta) => {
          if (!live) return;
          const { state: next, gap } = applyRegistryDelta(stateRef.current, delta);
          if (gap) {
            follow?.unsubscribe();
            stateRef.current = EMPTY_REGISTRY;
            subscribe();
            return;
          }
          stateRef.current = next;
          setConcepts(next.concepts);
          setLoading(false);
        });
      } catch (err: unknown) {
        if (!live) return;
        setError(err instanceof Error ? err.message : String(err));
        setLoading(false);
      }
    };
    subscribe();

    return () => {
      live = false;
      follow?.unsubscribe();
    };
  }, [query]);

  return { concepts, loading, error };
}
