// The public state API reports whether the first owner passkey is still required.
// Browser /setup responses are redirects, so their status cannot answer this.
import { identityBaseUrlFor } from "../connection/endpoint.js";
import type { ClusterConfig } from "./model.js";

export type ClaimState = "unclaimed" | "claimed" | "unknown";
export const CLAIM_PROBE_TIMEOUT_MS = 5_000;

export function claimProbeSignal(cancel?: AbortSignal): AbortSignal {
  const deadline = AbortSignal.timeout(CLAIM_PROBE_TIMEOUT_MS);
  return cancel === undefined ? deadline : AbortSignal.any([deadline, cancel]);
}

/** Read the explicit state API: /setup itself redirects browsers into MemQL OS. */
export async function readOwnerSetupState(cluster: ClusterConfig, fetcher: (url: string, init: RequestInit) => Promise<{ status: number; text(): Promise<string> }>, cancel?: AbortSignal): Promise<ClaimState> {
  const issuer = identityBaseUrlFor(cluster);
  if (!issuer) return "unknown";
  try {
    const response = await fetcher(`${issuer.replace(/\/+$/, "")}/auth/setup/state`, {
      method: "GET", headers: { accept: "application/json" },
      redirect: "manual", signal: claimProbeSignal(cancel),
    });
    const raw = await response.text();
    if (response.status !== 200) return "unknown";
    const body = JSON.parse(raw) as { state?: unknown };
    return body.state === "claimed" || body.state === "unclaimed" ? body.state : "unknown";
  } catch { return "unknown"; }
}
