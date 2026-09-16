import { ArrowRight } from "lucide-react";
import type { TaskPolicy } from "./taskPolicies";
export function PolicyChain({ policy }: { policy: TaskPolicy }) {
  return <ol className="fleet-policy-chain" aria-label={`${policy.name} source order`}>
    {policy.chain.map((entry, index) => <li key={`${index}:${entry}`}><small>{index === 0 ? "Preferred" : `Fallback ${index}`}</small><strong>{sourceLabel(entry)}</strong><span className="fleet-source-id">{entry}</span>{index < policy.chain.length - 1 ? <ArrowRight size={14} aria-hidden /> : null}</li>)}
    {policy.chain.length === 0 ? <li>Source order not reported</li> : null}
  </ol>;
}


function sourceLabel(source: string): string {
  return ({ "fleet:strongest": "Strongest local model", "fleet:fastest": "Fast local model", "app:*": "Eligible signed-in app", "federation:cheapest": "Lowest-cost federation", "federation:strongest": "Strongest federation", "embedder:active": "Active embedding model" } as Record<string, string>)[source] ?? source;
}
