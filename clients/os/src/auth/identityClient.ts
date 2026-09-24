import type { OsRuntimeConfig } from "../cluster/config";
import { osRedirectPath } from "../cluster/config";

export type IdentityFetch = (input: string, init?: RequestInit) => Promise<Response>;

export function redirectUriFor(origin: string): string {
  return origin.replace(/\/$/, "") + osRedirectPath;
}

export function apiUrl(config: OsRuntimeConfig, path: string): string {
  const base = config.identityApiBaseUrl;
  return base ? base + path : path;
}

export function authorizeUrl(
  config: OsRuntimeConfig,
  params: { redirectUri: string; state: string; codeChallenge: string },
): string {
  if (!config.identityUrl) {
    throw new Error("MemQL OS: this cluster published no identity URL.");
  }
  if (!config.oauthClientId) {
    throw new Error("MemQL OS: this cluster published no OAuth client id.");
  }
  const url = new URL(config.identityUrl + "/authorize");
  url.searchParams.set("response_type", "code");
  url.searchParams.set("client_id", config.oauthClientId);
  url.searchParams.set("redirect_uri", params.redirectUri);
  url.searchParams.set("state", params.state);
  url.searchParams.set("code_challenge", params.codeChallenge);
  url.searchParams.set("code_challenge_method", "S256");
  return url.toString();
}

export function canCoordinateIdentityRefresh(): boolean {
  return typeof navigator !== "undefined" && typeof navigator.locks?.request === "function";
}

/** Refresh cookies are shared by tabs. Hold the origin-wide lock through
 * the response headers, when the browser has applied Set-Cookie, so another
 * document never rotates the predecessor concurrently. No credential enters
 * local storage or a broadcast channel. */
async function fetchIdentityRefresh(config: OsRuntimeConfig, fetchImpl: IdentityFetch): Promise<Response> {
  if (!canCoordinateIdentityRefresh()) {
    return Promise.reject(new Error("This browser needs Web Locks support to keep sign-in in sync across tabs. Update your browser and try again."));
  }
  const signal = AbortSignal.timeout(15_000);
  return navigator.locks.request("memql:identity:refresh", { mode: "exclusive", signal }, () =>
    fetchImpl(apiUrl(config, "/auth/refresh"), {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: "{}",
      signal,
    }));
}

export async function probeSession(
  config: OsRuntimeConfig,
  fetchImpl: IdentityFetch = fetch,
): Promise<{ signedIn: boolean }> {
  const response = await fetchIdentityRefresh(config, fetchImpl);
  if (response.status >= 500 || response.status === 429) {
    throw new Error("Identity is temporarily unavailable");
  }
  return { signedIn: response.ok };
}

/**
 * Refresh the access token through the HttpOnly cookie (memql#4719). The
 * credential rides the BODY and the cookie, never a query parameter. Null =
 * no session (or no parsable token) -- the caller treats that as signed out.
 * Keep the OAuth `expires_in` lifetime so HTTP consumers can renew even when
 * the SDK stopped rotating during an outage. It is relative, so browser/server
 * clock skew cannot turn credential reads into a refresh loop.
 */
export async function refreshAccessCredential(
  config: OsRuntimeConfig,
  fetchImpl: IdentityFetch = fetch,
): Promise<{ bearer: string; expiresInSeconds: number } | null> {
  const response = await fetchIdentityRefresh(config, fetchImpl);
  if (!response.ok) return null;
  try {
    const payload = (await response.json()) as { access_token?: unknown; expires_in?: unknown };
    if (
      typeof payload.access_token !== "string" || payload.access_token === "" ||
      typeof payload.expires_in !== "number" || !Number.isFinite(payload.expires_in) || payload.expires_in <= 0
    ) return null;
    return { bearer: payload.access_token, expiresInSeconds: payload.expires_in };
  } catch {
    return null;
  }
}

export async function exchangeCode(
  config: OsRuntimeConfig,
  params: { code: string; codeVerifier: string; redirectUri: string },
  fetchImpl: IdentityFetch = fetch,
): Promise<boolean> {
  const response = await fetchImpl(apiUrl(config, "/oauth/token"), {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      grant_type: "authorization_code",
      client_id: config.oauthClientId,
      code: params.code,
      code_verifier: params.codeVerifier,
      redirect_uri: params.redirectUri,
    }),
  });
  return response.ok;
}

export async function logout(
  config: OsRuntimeConfig,
  fetchImpl: IdentityFetch = fetch,
): Promise<void> {
  try {
    await fetchImpl(apiUrl(config, "/auth/logout"), {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
  } catch {
    // Best-effort: the chrome has already dropped the local session.
  }
}
