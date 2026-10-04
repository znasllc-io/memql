// Desktop sign-in fallback over the host-independent RFC 8628 protocol.
export * from "./deviceGrant.js";
import type { ClusterConfig } from "../clusters/model.js";
import { identityBaseUrlFor } from "../connection/endpoint.js";
import { AuthFlowError } from "./errors.js";
import { runAuthorizationFlow, type AuthFlowDeps, type AuthFlowTokens } from "./flow.js";
import { runDeviceCodeFlow, shouldFallBackToDeviceCode, type DeviceCodeDeps } from "./deviceGrant.js";
export interface DeviceCodeFallbackDeps extends AuthFlowDeps, DeviceCodeDeps {
  /**
   * Called with the loopback failure that triggered the fallback, just before
   * the device flow starts. The caller uses it to explain the switch -- a flow
   * that silently changes shape reads as a bug.
   */
  onFallback?: (reason: AuthFlowError) => void;
}

/**
 * signInWithDeviceCodeFallback runs the loopback flow and, when this host
 * cannot do loopback, the device flow instead.
 *
 * Both grants authorize as the editor's own client (wellKnownClient.ts), so
 * there is nothing to resolve and hand across: the registry's `client_id`
 * belongs to whichever tool wrote it and is never read here.
 */
export async function signInWithDeviceCodeFallback(
  cluster: ClusterConfig,
  deps: DeviceCodeFallbackDeps,
): Promise<AuthFlowTokens> {
  if (!identityBaseUrlFor(cluster)) throw new AuthFlowError("misconfigured", "This cluster has no identity address.");

  let tokens: AuthFlowTokens;
  try {
    tokens = await runAuthorizationFlow(cluster, deps);
  } catch (err) {
    if (!shouldFallBackToDeviceCode(err)) throw err;
    deps.onFallback?.(err as AuthFlowError);
    tokens = await runDeviceCodeFlow(cluster, deps);
  }
  return tokens;
}
