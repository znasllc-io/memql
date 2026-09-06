import { useEffect, useMemo, useRef, useState } from "react";
import type { Concept } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";

// The concept registry, followed.
//
// ===========================================================================
// WHY THIS IS NOT A useLiveCollection
// ===========================================================================
// Every other live surface in the OS reads ROWS, and rows arrive as
// `graph.node.*` CDC events that `LiveCollection` folds. The concept
// registry is not rows: it is the engine's own declaration set, and it has
// its own subscription (`ConceptsSubscribeMsg` with `follow=true`,
// memql#4238) that answers with a snapshot and then add/remove deltas.
// There is no graph topic to subscribe to, so `useLiveCollection` over it
// would seed once and then never move -- the same failure the Logs app
// records for `v1:observability:logLine`.
//
// ===========================================================================
// THE GENERATION GAP IS THE PART THAT MATTERS
// ===========================================================================
// The engine numbers the deltas. A gap means a delta was dropped -- a slow
// consumer -- and from that moment this browser's registry is WRONG in a way
// it cannot repair by waiting, because the missing add or remove is never
// resent. The SDK's own contract says to unsubscribe and re-subscribe, which
// re-snapshots. Doing nothing instead leaves a registry that looks live and
// is quietly missing a concept, which is worse than one that is visibly
// reloading.
//
// A resubscribe is NOT an arrival: the snapshot that follows replaces the
// whole registry, and the list must not animate every row as new. The
// section keys its `LiveList` off the generation for exactly this reason.

export type RegistryState = "seeding" | "live" | "failed";

export interface ConceptRegistry {
  concepts: Concept[];
  state: RegistryState;
  error: string;
  /** The engine's delta counter. Changes on every registry change. */
  generation: number;
}

export function useConceptRegistry(): ConceptRegistry {
  const connection = useOsConnection();
  const query = connection?.query ?? null;

  const [concepts, setConcepts] = useState<Concept[]>([]);
  const [state, setState] = useState<RegistryState>("seeding");
  const [error, setError] = useState("");
  const [generation, setGeneration] = useState(0);
  // Bumping this re-runs the effect, which is how a generation gap
  // re-subscribes. A counter rather than a flag: it has to be able to
  // happen more than once.
  const [restarts, setRestarts] = useState(0);
  const lastGeneration = useRef<number | null>(null);

  useEffect(() => {
    if (query === null) {
      setState("seeding");
      return;
    }
    let live = true;
    lastGeneration.current = null;

    const follow = query.subscribeConceptRegistry(
      (delta) => {
        if (!live) return;

        // A GAP. Everything after it would be applied to a registry that is
        // already missing something, so stop applying and re-snapshot.
        const previous = lastGeneration.current;
        if (previous !== null && !delta.reset && delta.generation > previous + 1) {
          live = false;
          follow.unsubscribe();
          setRestarts((n) => n + 1);
          return;
        }
        lastGeneration.current = delta.generation;
        setGeneration(delta.generation);

        setConcepts((current) => {
          // A reset REPLACES. Merging a snapshot into what is already held
          // would keep a concept the cluster has dropped, which is the exact
          // staleness the snapshot exists to clear.
          const base = delta.reset ? [] : current;
          const byId = new Map(base.map((c) => [c.id, c]));
          for (const added of delta.added) byId.set(added.id, added);
          for (const removed of delta.removed) byId.delete(removed);
          return [...byId.values()].sort((a, b) => a.id.localeCompare(b.id));
        });
        setState("live");
        setError("");
      },
      {
        onError: (err) => {
          if (!live) return;
          // The engine refuses a follow on a node with no engine, and that
          // refusal carries this request id rather than vanishing. Surfacing
          // it is what keeps the surface from waiting forever on a snapshot
          // that is not coming.
          setError(err.message);
          setState("failed");
        },
      },
    );

    return () => {
      live = false;
      follow.unsubscribe();
    };
  }, [query, restarts]);

  return useMemo(
    () => ({ concepts, state, error, generation }),
    [concepts, state, error, generation],
  );
}
