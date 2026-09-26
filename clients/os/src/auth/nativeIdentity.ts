import type { OsRuntimeConfig } from "../cluster/config";

export const IDENTITY_MEDIA = "application/vnd.memql.identity+json";
export interface IdentityData {
  [key: string]: unknown;
  Layout?: { Title: string; BrandName: string };
  Flash?: { Kind: string; Message: string };
}
export interface IdentityPage {
  page?: string;
  data?: IdentityData;
  csrf?: string;
  redirect?: string;
  error?: string;
  state?: "claimed" | "unclaimed";
}

export function identityLocation(path: string): string {
  const url = new URL(path, "https://identity.invalid");
  return `/identity${url.pathname}#${encodeURIComponent(url.search.slice(1))}`;
}

export function identityEntry(): string | null {
  if (!window.location.pathname.startsWith("/identity/")) return null;
  try {
    return window.location.pathname.slice("/identity".length) +
      (window.location.hash ? `?${decodeURIComponent(window.location.hash.slice(1))}` : window.location.search);
  } catch { return "/error"; }
}

export async function nativeIdentity(
  config: OsRuntimeConfig, path: string,
  options: { form?: Record<string, string>; csrf?: string; bearer?: string; signal?: AbortSignal } = {},
): Promise<IdentityPage> {
  // A backend redirect may name an external OAuth client. Only the navigation
  // layer follows it; this function never sends identity headers to that host.
  const base = config.identityApiBaseUrl || config.identityUrl;
  const target = new URL(path, base);
  if (target.origin !== new URL(base).origin) throw new Error("Identity request origin refused");
  const response = await fetch(target, {
    method: options.form ? "POST" : "GET", credentials: "include", redirect: "error",
    signal: options.signal ?? AbortSignal.timeout(15000),
    headers: {
      Accept: IDENTITY_MEDIA,
      ...(options.csrf ? { "X-CSRF-Token": options.csrf } : {}),
      ...(options.bearer ? { Authorization: `Bearer ${options.bearer}` } : {}),
    },
    body: options.form ? new URLSearchParams(options.form) : undefined,
  });
  const result = await response.json() as IdentityPage;
  if (!response.ok && !result.page) throw new Error(result.error || "Identity is temporarily unavailable");
  return result;
}

export async function ownershipState(config: OsRuntimeConfig): Promise<"claimed" | "unclaimed"> {
  const result = await nativeIdentity(config, "/auth/setup/state");
  if (result.state !== "claimed" && result.state !== "unclaimed") throw new Error("Ownership state is unavailable");
  return result.state;
}

export const value = (data: IdentityData, key: string): string => typeof data[key] === "string" ? data[key] as string : "";
export const oauthFields = (data: IdentityData): Record<string, string> => ({
  return_to: value(data, "ReturnTo"), client_id: value(data, "ClientID"),
  redirect_uri: value(data, "RedirectURI"), state: value(data, "OAuthState"),
  code_challenge: value(data, "CodeChallenge"), code_challenge_method: value(data, "CodeChallengeMethod"),
});
