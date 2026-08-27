// The OS's credential seam (spec D7, the portal's identityAuthSource
// pattern at OS size). The access token lives in a closure -- components
// ask for capability ("bearer()"), never for the string, so there is
// exactly one place it can leak from. Refresh goes through the HttpOnly
// cookie; the SDK owns the rotation timer and calls `refresh` through
// `onTokenExpired`.

import type { OsRuntimeConfig } from "../cluster/config";
import { refreshAccessToken, type IdentityFetch } from "./identityClient";

export interface OsAuthSource {
  /** The credential to dial or fetch with right now, or null for none. */
  bearer(): Promise<string | null>;
  /** A FRESH credential (the SDK's rotation hook). Null = give up. */
  refresh(): Promise<string | null>;
}

/** Auth disabled (or signed out): supply nothing, honestly. */
export const anonymousSource: OsAuthSource = {
  bearer: async () => null,
  refresh: async () => null,
};

export function identitySource(
  config: OsRuntimeConfig,
  fetchImpl: IdentityFetch = fetch,
): OsAuthSource {
  let held: string | null = null;
  const renew = async (): Promise<string | null> => {
    // A cluster with auth disabled has no session to refresh -- and quite
    // possibly no identity service to ask. Dial with nothing, which is
    // exactly what that mode admits.
    if (!config.authEnabled) return null;
    const fresh = await refreshAccessToken(config, fetchImpl);
    if (fresh !== null) held = fresh;
    return fresh;
  };
  return {
    bearer: async () => held ?? (await renew()),
    refresh: renew,
  };
}
