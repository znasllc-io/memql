import { useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";

export interface TaskPolicy { name: string; description: string; chain: string[]; primary?: string; fallbacks?: string[]; defaultChain?: string[]; shipped?: boolean; customized?: boolean; protected?: boolean; revision?: number }
export function policyFromRow(row: Row): TaskPolicy {
  const value = flatten(row as Record<string, unknown>);
  return { primary: typeof value.primary === "string" ? value.primary : "", fallbacks: Array.isArray(value.fallbacks) ? value.fallbacks as string[] : [], defaultChain: Array.isArray(value.defaultChain) ? value.defaultChain as string[] : [], shipped: value.shipped === true, customized: value.customized === true, protected: value.protected === true, revision: typeof value.revision === "number" ? value.revision : undefined, name: typeof value.name === "string" ? value.name : "", description: typeof value.description === "string" ? value.description : "", chain: Array.isArray(value.chain) ? value.chain.filter((entry): entry is string => typeof entry === "string") : [] };
}
export function useTaskPolicies(epoch: number) {
  const connection = useOsConnection();
  const [state, setState] = useState<{ policies: TaskPolicy[]; loading: boolean; error: string }>({ policies: [], loading: true, error: "" });
  useEffect(() => {
    const read = (connection?.query as unknown as Record<string, unknown> | undefined)?.routerListPolicies;
    if (typeof read !== "function") { setState({ policies: [], loading: false, error: "This connection cannot read routing policies." }); return; }
    let stale = false;
    const controller = new AbortController();
    setState(held => ({ ...held, loading: true, error: "" }));
    void (read as (args: object, opts: object) => Promise<{ rows(): Iterable<Row> }>).call(connection!.query, {}, { signal: controller.signal }).then(result => {
      if (!stale) setState({ policies: [...result.rows()].map(policyFromRow).filter(p => p.name), loading: false, error: "" });
    }).catch((error: unknown) => { if (!stale) setState(held => ({ ...held, loading: false, error: error instanceof Error ? error.message : String(error) })); });
    return () => { stale = true; controller.abort(); };
  }, [connection, epoch]);
  return state;
}

/** Describe shipped defaults in product language; custom descriptions remain user content. */
const SHIPPED_DESCRIPTIONS: Record<string, string> = {
  localFirst: "Try a compatible local model, then a signed-in app, then the least expensive compatible federated model.",
  fastLocalFirst: "Prefer a fast local model that meets the quality requirement, then an app, then a compatible federated model.",
  localOnly: "Keep calls on your own machines. If no local model is available, the task rule decides whether to wait.",
  federationStrongest: "Try a strong local model, then an app, then the strongest compatible federated model. Vendor charges may apply.",
  embeddingsBinding: "Use the cluster’s active embedding model so new vectors stay compatible with the existing index. Changing that model requires an embedding migration.",
};
export function policyDescription(policy: TaskPolicy): string {
  return policy.shipped && !policy.customized ? SHIPPED_DESCRIPTIONS[policy.name] ?? policy.description : policy.description;
}
