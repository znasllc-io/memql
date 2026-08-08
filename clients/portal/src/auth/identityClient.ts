// Every HTTP call the portal makes to the identity service.
//
// Concentrated in one module on purpose: this is the only code in the portal
// that touches a credential over HTTP, so "does a token ever end up in a URL?"
// is a question with one place to look. The answer is enforced here and
// asserted in test/identityClient.test.ts, which inspects the URLs this module
// constructs.
//
// ---------------------------------------------------------------------------
// THE ONE THING THAT TRAVELS IN A URL, AND WHY IT IS NOT A CREDENTIAL
// ---------------------------------------------------------------------------
// The authorization CODE arrives on the callback URL as ?code=. That is
// inherent to OAuth 2.1's code flow and it is precisely the design that keeps
// tokens OUT of URLs: the code is single-use, expires in minutes, is bound by
// PKCE to a verifier that never left this browser, and is refused on second
// presentation (component/identity/http/token.go audits the replay). An
// attacker holding the code and nothing else cannot mint a token. Access and
// refresh tokens never appear in any URL this module builds -- they travel in
// a POST body, a response body, and an HttpOnly cookie.
//
// ---------------------------------------------------------------------------
// TWO BASES, NOT ONE
// ---------------------------------------------------------------------------
// `identityUrl` is for the TOP-LEVEL NAVIGATION to /authorize -- an HTML page
// in component/identity/web with no CORS and no same-origin variant.
// `identityApiBaseUrl` is for fetch(): /oauth/token, /auth/refresh,
// /auth/logout. It is separate because the deployment may proxy those three
// same-origin through its front door, which is the topology
// docs/public/operate/auth/identity-service.md prescribes -- it sidesteps a
// Safari HTTP/2 connection-coalescing bug that intermittently fails
// cross-origin credentialed XHR to a sibling host on the same wildcard cert.
// See component/portal/config.go.

import type { PortalRuntimeConfig } from "../cluster/config";

export interface TokenSet {
  accessToken: string;
  // Seconds until the access token expires, as reported by the server. The
  // SDK schedules rotation off the JWT's own `exp` rather than this, so it is
  // carried only for diagnostics -- two sources of truth for one deadline is
  // how they drift.
  expiresInSeconds: number;
}

// IdentityFetch is the injection point for the test. Typed loosely enough to
// accept the global `fetch` without a cast.
export type IdentityFetch = (input: string, init?: RequestInit) => Promise<Response>;

// IdentityHttpError carries the identity service's own OAuth error envelope
// ({"error", "message", "errorId"}), so an operator can be shown "the portal's
// client_id is not registered on this identity service" instead of "500".
export class IdentityHttpError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "IdentityHttpError";
    this.status = status;
    this.code = code;
  }

  // unauthenticated is the "you have no session" case, as distinct from a
  // transport failure. /auth/refresh answers 401 invalid_grant when there is
  // no usable refresh cookie, which is the normal cold-load-while-signed-out
  // outcome, not an error to shout about.
  get unauthenticated(): boolean {
    return this.status === 401 || this.status === 403;
  }
}

// authorizeUrl builds the top-level navigation target.
//
// Note what is NOT here: no token, no verifier. `code_challenge` is the SHA-256
// of the verifier and is public by design -- publishing it is the entire point
// of PKCE.
export function authorizeUrl(
  config: PortalRuntimeConfig,
  params: { redirectUri: string; state: string; codeChallenge: string },
): string {
  if (!config.identityUrl) {
    throw new Error(
      "memQL portal: this cluster published no identity URL, so there is " +
        "nowhere to sign in. The node serving the portal derives it from " +
        "MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER -- see " +
        "docs/public/operate/portal.md.",
    );
  }
  if (!config.oauthClientId) {
    throw new Error(
      "memQL portal: this cluster published no OAuth client id " +
        "(MEMQL_PORTAL_OAUTH_CLIENT_ID).",
    );
  }
  const url = new URL(config.identityUrl + "/authorize");
  url.searchParams.set("response_type", "code");
  url.searchParams.set("client_id", config.oauthClientId);
  url.searchParams.set("redirect_uri", params.redirectUri);
  url.searchParams.set("state", params.state);
  url.searchParams.set("code_challenge", params.codeChallenge);
  // S256 is not optional: component/identity/web/authorize.go rejects a
  // missing challenge or any other method as invalid_request (OAuth 2.1).
  url.searchParams.set("code_challenge_method", "S256");
  return url.toString();
}

// apiUrl resolves a path against the XHR base. An empty base means same-origin
// (the front door proxies), in which case the path is returned relative and
// the browser resolves it -- no origin to get wrong.
export function apiUrl(config: PortalRuntimeConfig, path: string): string {
  const base = config.identityApiBaseUrl;
  return base ? base + path : path;
}

// exchangeAuthorizationCode redeems the callback's code for an access token.
//
// credentials: "include" is REQUIRED, not incidental: the response's
// Set-Cookie carries the HttpOnly `memql_refresh` cookie, and the browser
// discards it on a cross-origin response unless the request was credentialed.
// Without it the sign-in appears to work and then cannot refresh -- the
// session simply dies at the first token expiry with no error anywhere.
export async function exchangeAuthorizationCode(
  fetchImpl: IdentityFetch,
  config: PortalRuntimeConfig,
  params: { code: string; codeVerifier: string; redirectUri: string },
): Promise<TokenSet> {
  return postForTokens(fetchImpl, apiUrl(config, "/oauth/token"), {
    grant_type: "authorization_code",
    code: params.code,
    client_id: config.oauthClientId,
    redirect_uri: params.redirectUri,
    code_verifier: params.codeVerifier,
  });
}

// refreshAccessToken swaps the HttpOnly refresh cookie for a fresh access
// token. Throws IdentityHttpError with `unauthenticated` set when there is no
// session left to refresh.
//
// The body is a literal empty object, matching what identity's own /me pages
// send (component/identity/web/static/app.js): the refresh token is read from
// the cookie server-side. The portal never puts it in a body, because the
// portal never HAS it -- see below.
export async function refreshAccessToken(
  fetchImpl: IdentityFetch,
  config: PortalRuntimeConfig,
): Promise<TokenSet> {
  return postForTokens(fetchImpl, apiUrl(config, "/auth/refresh"), {});
}

// endIdentitySession clears the refresh cookie server-side. Best-effort by
// design: the portal has already dropped its in-memory token by the time this
// runs, so a failure here leaves a cookie that the next /auth/refresh will
// either honour (the operator is still signed in -- correct) or reject.
// Throwing would turn "sign out" into an error dialog over an action the user
// experiences as already done.
export async function endIdentitySession(
  fetchImpl: IdentityFetch,
  config: PortalRuntimeConfig,
): Promise<void> {
  try {
    await fetchImpl(apiUrl(config, "/auth/logout"), {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
  } catch {
    // Deliberately swallowed; see above.
  }
}

async function postForTokens(
  fetchImpl: IdentityFetch,
  url: string,
  body: Record<string, string>,
): Promise<TokenSet> {
  const response = await fetchImpl(url, {
    method: "POST",
    // See exchangeAuthorizationCode: the HttpOnly refresh cookie rides on
    // this, in both directions.
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    // The credential is in the BODY. Never a query parameter: a request line
    // is recorded verbatim by every proxy, ingress and browser-history entry
    // between here and the server, and a body is not.
    body: JSON.stringify(body),
  });

  const payload = (await readJson(response)) as {
    access_token?: unknown;
    expires_in?: unknown;
    error?: unknown;
    message?: unknown;
  };

  if (!response.ok) {
    throw new IdentityHttpError(
      response.status,
      typeof payload.error === "string" ? payload.error : "",
      typeof payload.message === "string" && payload.message
        ? payload.message
        : `identity responded ${response.status}`,
    );
  }

  const accessToken = typeof payload.access_token === "string" ? payload.access_token : "";
  if (!accessToken) {
    throw new IdentityHttpError(response.status, "invalid_response", "identity returned no access token");
  }

  // THE REFRESH TOKEN IS DELIBERATELY NOT READ. identity returns it in this
  // body as well as in the HttpOnly cookie
  // (component/identity/http/refresh.go), and taking it would hand a
  // ~30-day credential to page JavaScript -- exactly the long-lived secret
  // the HttpOnly cookie exists to keep away from an XSS. The portal has no
  // use for it: /auth/refresh reads the cookie. Do not "helpfully" plumb it
  // through as a fallback; the fallback IS the vulnerability.
  return {
    accessToken,
    expiresInSeconds: typeof payload.expires_in === "number" ? payload.expires_in : 0,
  };
}

// readJson tolerates a non-JSON body (an ingress 502 page, an empty 204) so a
// transport failure surfaces as an honest status rather than a SyntaxError
// thrown from inside the auth layer.
async function readJson(response: Response): Promise<unknown> {
  try {
    return await response.json();
  } catch {
    return {};
  }
}
