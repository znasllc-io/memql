// The cluster's runtime configuration, read from the node that served this
// page (memql#3315).
//
// Everything the portal needs to authenticate -- which identity service, which
// OAuth client id, whether this cluster enforces auth at all -- arrives HERE,
// at runtime, from the serving origin. None of it is compiled in. The engine
// image is product- and environment-agnostic: the same bytes run against the
// local k3d cluster, staging, and a customer's install, each with a different
// identity host. A build-time `VITE_MEMQL_IDENTITY_URL` would make the bundle
// environment-specific, which is the exact property the image model exists to
// avoid. component/portal/config.go is the other half of this contract.
//
// Additive-only shape. A cached index.html can outlive a node restart, so an
// older bundle has to keep working against a newer node: read fields
// defensively, never require one that did not exist before.

const CONFIG_FILE = "runtime-config.json";

// runtimeConfigPathFor derives the document's path from the portal's mount,
// exactly as bridgePathFor derives the WebSocket path. Same reasoning: the
// mount point is decided once (vite.config.ts `base`, matched by the Go
// handler) and everything else is derived, so a deployment that carries a
// SERVER_PUBLIC_PATH prefix needs no second piece of configuration.
export function runtimeConfigPathFor(baseUrl: string): string {
  const mount = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
  return mount + CONFIG_FILE;
}

export const runtimeConfigPath = runtimeConfigPathFor(import.meta.env.BASE_URL);

export interface PortalRuntimeConfig {
  // Browser-reachable identity origin. Used for the TOP-LEVEL navigation to
  // GET /authorize -- an HTML page with no CORS and no same-origin variant.
  identityUrl: string;
  // Base for the identity JSON calls the portal makes with fetch().
  // Empty string means SAME-ORIGIN: the deployment's front door proxies
  // /oauth/token, /auth/refresh and /auth/logout, which is the topology
  // docs/public/operate/auth/identity-service.md prescribes.
  identityApiBaseUrl: string;
  // Public OAuth client_id (RFC 6749 §2.2 -- public clients have no secret,
  // which is why the portal uses PKCE).
  oauthClientId: string;
  // Whether this cluster enforces authentication.
  //
  // NOT A PERMISSION, and nothing in the portal may treat it as one. It only
  // decides whether a sign-in flow can succeed, so a cluster running with
  // MEMQL_IDENTITY_ENABLED=false for troubleshooting shows the browser
  // instead of a sign-in wall nothing can satisfy. Every read and write is
  // gated server-side on the stream this bundle dials -- see
  // component/grpc's verifier interceptors. A browser that lies to itself
  // about this field gains exactly nothing.
  authEnabled: boolean;
}

// FALLBACK, not a default. Used only when the document cannot be read, so the
// portal renders an explanatory sign-in screen rather than a blank page. It
// deliberately claims auth IS enabled: assuming an unreachable cluster is
// open would be the wrong way to be wrong.
export const UNKNOWN_RUNTIME_CONFIG: PortalRuntimeConfig = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: true,
};

export type RuntimeConfigFetch = (
  input: string,
  init?: RequestInit,
) => Promise<Response>;

// loadRuntimeConfig reads the document from the serving origin.
//
// no-store on the request as well as the response: this is read once per page
// load, and a stale copy points the browser at a decommissioned identity
// service, which presents as a sign-in that silently never completes.
export async function loadRuntimeConfig(
  fetchImpl: RuntimeConfigFetch,
  path: string = runtimeConfigPath,
): Promise<PortalRuntimeConfig> {
  const response = await fetchImpl(path, {
    method: "GET",
    cache: "no-store",
    // Same origin by construction, but stated so a future reader does not
    // wonder whether this call carries the identity cookie. It must not.
    credentials: "omit",
  });
  if (!response.ok) {
    throw new Error(
      `portal runtime config: ${path} responded ${response.status}. The node ` +
        `serving this bundle did not answer with its configuration.`,
    );
  }
  return normalizeRuntimeConfig(await response.json());
}

// normalizeRuntimeConfig coerces the wire document into the shape the rest of
// the portal can rely on. Exported for the test, and separate from the fetch
// so the tolerance rules are readable on their own.
export function normalizeRuntimeConfig(raw: unknown): PortalRuntimeConfig {
  const doc = (raw ?? {}) as Partial<Record<keyof PortalRuntimeConfig, unknown>>;
  return {
    identityUrl: trimTrailingSlash(asString(doc.identityUrl)),
    identityApiBaseUrl: trimTrailingSlash(asString(doc.identityApiBaseUrl)),
    oauthClientId: asString(doc.oauthClientId),
    // Only an explicit `false` disables. An absent field (an older node, a
    // truncated document) means "assume enforcement", for the same
    // fail-closed reason as UNKNOWN_RUNTIME_CONFIG.
    authEnabled: doc.authEnabled !== false,
  };
}

function asString(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function trimTrailingSlash(value: string): string {
  return value.replace(/\/+$/, "");
}
