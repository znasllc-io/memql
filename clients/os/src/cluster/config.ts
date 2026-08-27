const CONFIG_FILE = "runtime-config.json";

export function runtimeConfigPathFor(baseUrl: string): string {
  const mount = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
  return mount + CONFIG_FILE;
}

export const runtimeConfigPath = runtimeConfigPathFor(import.meta.env.BASE_URL);

export interface OsRuntimeConfig {
  identityUrl: string;
  identityApiBaseUrl: string;
  oauthClientId: string;
  authEnabled: boolean;
  domain: string;
}

export const UNKNOWN_RUNTIME_CONFIG: OsRuntimeConfig = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: true,
  domain: "",
};

export function isRuntimeConfigReady(config: OsRuntimeConfig): boolean {
  return Boolean(config.identityUrl && config.oauthClientId);
}

export async function loadRuntimeConfig(
  fetchImpl: (input: string, init?: RequestInit) => Promise<Response> = globalThis.fetch,
): Promise<OsRuntimeConfig> {
  const response = await fetchImpl(runtimeConfigPath, { credentials: "same-origin" });
  if (!response.ok) {
    throw new Error(`MemQL OS: runtime-config.json responded ${response.status}`);
  }
  const raw = (await response.json()) as Partial<OsRuntimeConfig>;
  return {
    identityUrl: typeof raw.identityUrl === "string" ? raw.identityUrl : "",
    identityApiBaseUrl: typeof raw.identityApiBaseUrl === "string" ? raw.identityApiBaseUrl : "",
    oauthClientId: typeof raw.oauthClientId === "string" ? raw.oauthClientId : "",
    authEnabled: raw.authEnabled !== false,
    domain: typeof raw.domain === "string" ? raw.domain : "",
  };
}

export const osRedirectPath = "/auth/callback";
