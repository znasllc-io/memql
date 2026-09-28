import { useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";

export interface TaskPolicy { name: string; description: string; chain: string[]; primary?: string; fallbacks?: string[]; defaultChain?: string[]; shipped?: boolean; customized?: boolean; protected?: boolean; revision?: number }
export function policyFromRow(row: Row): TaskPolicy {
  const value = flatten(row as Record<string, unknown>);
  return { primary: typeof value.primary === "string" ? value.primary : "", fallbacks: Array.isArray(value.fallbacks) ? value.fallbacks as string[] : [], defaultChain: Array.isArray(value.defaultChain) ? value.defaultChain as string[] : [], shipped: value.shipped === true, customized: value.customized === true, protected: value.protected === true, revision: typeof value.revision === "number" ? value.revision : undefined, name: typeof value.name === "string" ? value.name : "", description: typeof value.description === "string" ? value.description : "", chain: Array.isArray(value.chain) ? value.chain.filter((entry): entry is string => typeof entry === "string") : [] };
}
/**
 * The route catalog. `settled` counts the reads that have come back, landed or
 * refused -- which is how a page waiting on "the NEXT read" (a route just
 * created, not in the list until the re-read lands) tells that read from the
 * one before it.
 */
export function useTaskPolicies(epoch: number, enabled = true) {
  const connection = useOsConnection();
  const [state, setState] = useState<{ policies: TaskPolicy[]; loading: boolean; error: string; settled: number }>({ policies: [], loading: true, error: "", settled: 0 });
  useEffect(() => {
    if (!enabled) return;
    const read = (connection?.query as unknown as Record<string, unknown> | undefined)?.routerListPolicies;
    if (typeof read !== "function") { setState(held => ({ policies: [], loading: false, error: "This connection cannot read routes.", settled: held.settled + 1 })); return; }
    let stale = false;
    const controller = new AbortController();
    setState(held => ({ ...held, loading: true, error: "" }));
    void (read as (args: object, opts: object) => Promise<{ rows(): Iterable<Row> }>).call(connection!.query, {}, { signal: controller.signal }).then(result => {
      if (!stale) setState(held => ({ policies: [...result.rows()].map(policyFromRow).filter(p => p.name), loading: false, error: "", settled: held.settled + 1 }));
    }).catch((error: unknown) => { if (!stale) setState(held => ({ ...held, loading: false, error: error instanceof Error ? error.message : String(error), settled: held.settled + 1 })); });
    return () => { stale = true; controller.abort(); };
  }, [connection, epoch, enabled]);
  return state;
}

/**
 * The shipped routes described in product words; a custom route's description
 * is the person's own and is shown as written. One line each: the chain is on
 * the screen beside it, so this says what the route is FOR, not what it holds.
 */
const SHIPPED_DESCRIPTIONS: Record<string, string> = {
  localFirst: "Your machines first, then a signed-in app, then the least expensive vendor.",
  fastLocalFirst: "A quick local model first, then a signed-in app, then a vendor.",
  localOnly: "Stays on your machines. When none can serve, the rule decides whether to wait.",
  federationStrongest: "A strong local model, then a signed-in app, then the strongest vendor. Vendors bill per call.",
  embeddingsBinding: "The cluster's active embedding model, so new vectors match the index. Changing it needs an embedding migration.",
};
export function policyDescription(policy: TaskPolicy): string {
  return policy.shipped && !policy.customized ? SHIPPED_DESCRIPTIONS[policy.name] ?? policy.description : policy.description;
}
