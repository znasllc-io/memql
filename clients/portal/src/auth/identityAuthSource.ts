// The identity-service-backed PortalAuthSource (memql#3315).
//
// This is what src/cluster/auth.ts marked out a seam for. It is the ONLY place
// in the portal that holds a credential, and it holds it in exactly one way.
//
// ===========================================================================
// TOKEN STORAGE -- the decision and the threat model
// ===========================================================================
//
// TWO CREDENTIALS, TWO HOMES:
//
//   * The ACCESS token (~15 minutes) lives in a CLOSURE VARIABLE and nowhere
//     else. Not localStorage, not sessionStorage, not a cookie, not a URL. It
//     dies with the page.
//   * The REFRESH token (~30 days) is never seen by this code at all. The
//     identity service puts it in the HttpOnly `memql_refresh` cookie
//     (component/identity/http/refresh.go), and the portal gets a new access
//     token by POSTing /auth/refresh with credentials:"include" -- the browser
//     attaches the cookie, script never reads it. identity ALSO returns the
//     refresh token in the JSON body; identityClient.ts deliberately discards
//     that field, and the comment there explains why taking it would be the
//     vulnerability.
//
// THE TRADE, in three sentences. An XSS on the portal's origin can read the
// in-memory access token, so the split does not make XSS harmless -- it caps
// the damage at one short-lived token for one live page instead of a
// 30-day refresh token an attacker could exfiltrate and use from anywhere,
// which is the difference between an incident and a persistent backdoor. The
// CSRF exposure this accepts in return is that the refresh cookie rides
// automatically on requests the browser makes, and identity applies NO CSRF
// token to /auth/refresh (component/identity/http is mounted without the
// web package's CSRF middleware) -- the defences are SameSite=Lax on the
// cookie plus identity's exact-match CORS allowlist, which together mean a
// forged cross-site POST either does not carry the cookie or cannot read the
// response. That is the right way round: a token an attacker cannot READ is
// worth more than one they cannot cause to be SENT, because the sent-but-
// unreadable case yields them nothing.
//
// WHY NOT localStorage for the access token, since XSS reads it either way:
// because localStorage OUTLIVES the page. A token there is readable by any
// script that ever runs on the origin, at any later time, including after the
// operator has closed the tab and walked away -- and it survives into a
// browser profile that syncs. In-memory is strictly smaller exposure for no
// functional loss, because a cold load rebuilds the session from the cookie
// in one request anyway.

import {
  IdentityHttpError,
  refreshAccessToken,
  type IdentityFetch,
} from "./identityClient";
import type { PortalRuntimeConfig } from "../cluster/config";
import type { PortalAuthSource } from "../cluster/auth";

export interface IdentityAuthSourceOptions {
  // An ACCESSOR, not a value. The source is built once and its object
  // identity is held stable for the life of the page (ClusterProvider
  // re-dials when its `auth` prop changes identity), but the runtime config
  // arrives asynchronously after the first render -- so the source has to be
  // able to see a config it did not have when it was constructed.
  config: () => PortalRuntimeConfig;
  fetchImpl: IdentityFetch;
  // Called when identity says the session is gone (401/403 on refresh), so
  // the UI can drop back to the sign-in view. NOT called for a transient
  // network failure -- signing an operator out because their wifi blinked is
  // its own bug.
  onSessionEnded?: () => void;
  // Called after every successful token acquisition, so a caller that wants
  // to render claims (or log a rotation) can, without reaching in for the
  // token itself.
  onTokenAcquired?: (accessToken: string) => void;
}

export interface IdentityAuthSource extends PortalAuthSource {
  // adopt installs a token obtained elsewhere -- specifically the one the
  // callback page gets from the authorization-code exchange.
  adopt(accessToken: string): void;
  // forget drops the in-memory token. Used on sign-out, and by the callback
  // page if the exchange fails partway.
  forget(): void;
  // hasToken reports whether a credential is currently held. Exposed so route
  // gating can ask "is there a session?" WITHOUT being handed the token --
  // the fewer callers that can touch the string, the fewer places it can be
  // logged or serialised by accident.
  hasToken(): boolean;
}

export function createIdentityAuthSource(
  opts: IdentityAuthSourceOptions,
): IdentityAuthSource {
  // THE credential. A closure variable, deliberately: there is no object
  // property for a stray console.log(authSource) to print, and no way to
  // reach it except through the methods below.
  let accessToken: string | null = null;

  // Single-flight. The SDK's rotation timer and a fresh dial's bearer() can
  // both want a token at once; without this they race and burn two refresh
  // round-trips, and -- worse -- identity ROTATES the refresh token on every
  // call, so two concurrent refreshes have one of them presenting a
  // superseded token. (There is a 30s grace window in
  // component/identity/refresh/rotate.go that would usually absorb it, which
  // is exactly the kind of "usually" that fails under load.)
  let inFlight: Promise<string | null> | null = null;

  function store(token: string): string {
    accessToken = token;
    opts.onTokenAcquired?.(token);
    return token;
  }

  async function fetchFreshToken(): Promise<string | null> {
    // A cluster running with MEMQL_IDENTITY_ENABLED=false has no session to
    // refresh and, quite possibly, no identity service to ask. Calling
    // /auth/refresh there is a guaranteed failure on every dial -- and worse,
    // a guaranteed CORS error in the console that reads like a portal bug
    // rather than "this cluster has auth turned off". Dial with no credential
    // instead, which is exactly what that mode expects.
    if (!opts.config().authEnabled) return null;
    try {
      const tokens = await refreshAccessToken(opts.fetchImpl, opts.config());
      return store(tokens.accessToken);
    } catch (err) {
      if (err instanceof IdentityHttpError && err.unauthenticated) {
        // The session is genuinely over: revoked, expired, or never existed
        // (the cold-load-while-signed-out case, which is not an error).
        accessToken = null;
        opts.onSessionEnded?.();
        return null;
      }
      // Transient: DNS, offline, a 502 from the front door, CORS. Keep
      // whatever token we have -- it may still be valid -- and let the SDK's
      // retry (it re-invokes the hook within the remaining TTL) try again.
      // Returning null here means "not now", not "signed out".
      return null;
    }
  }

  function refreshOnce(): Promise<string | null> {
    if (inFlight) return inFlight;
    const attempt = fetchFreshToken().finally(() => {
      if (inFlight === attempt) inFlight = null;
    });
    inFlight = attempt;
    return attempt;
  }

  return {
    // bearer is read once per DIAL. Returning the held token keeps a reconnect
    // instant; falling through to a refresh covers the case where the page has
    // been alive long enough to have dropped its token but the cookie is still
    // good, which is what makes a reconnect after a laptop sleep work.
    async bearer(): Promise<string | null> {
      if (accessToken) return accessToken;
      return refreshOnce();
    },

    // refresh is the SDK's onTokenExpired hook. The SDK owns the timer (70% of
    // the JWT's TTL) and the rotateAuth round-trip that swaps the credential on
    // the LIVE stream -- see sdk/ts Connection.performAutoRotate. That is the
    // whole reason this returns a token instead of reconnecting: a portal left
    // open on a dashboard must not drop its subscriptions every fifteen
    // minutes.
    refresh(): Promise<string | null> {
      return refreshOnce();
    },

    adopt(token: string): void {
      const trimmed = token.trim();
      if (trimmed) store(trimmed);
    },

    forget(): void {
      accessToken = null;
    },

    hasToken(): boolean {
      return accessToken !== null;
    },
  };
}
