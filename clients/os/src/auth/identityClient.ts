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

export async function probeSession(
  config: OsRuntimeConfig,
  fetchImpl: IdentityFetch = fetch,
): Promise<{ signedIn: boolean }> {
  const response = await fetchImpl(apiUrl(config, "/auth/refresh"), {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
  return { signedIn: response.ok };
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
