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
// avoid.
//
// component/portal/config.go used to be the other half of this contract --
// it served this document directly, co-located with the bundle on the same
// bff node, keyed on nothing but "this is the portal". The portal is now
// site #1, served by component/edge (memql#3711), which is a generic bundle
// server and must not carry portal-specific identity-config logic
// (TestPortalHasNoSpecialCaseInTheServingPath forbids exactly that) -- so
// component/edge/runtimeconfig.go answers this document for EVERY hosted
// site alike, ahead of the bundle lookup, deriving identityUrl /
// identityApiBaseUrl / authEnabled from the cluster's own domain-derived
// config and oauthClientId from a lookup of the SITE'S OWN hostname against
// MEMQL_IDENTITY_REGISTERED_CLIENTS -- data, not a name it recognises. A
// site whose hostname is not registered with identity still gets a 200,
// just with an empty oauthClientId.
//
// Additive-only shape. A cached index.html can outlive a node restart, so an
// older bundle has to keep working against a newer node: read fields
// defensively, never require one that did not exist before.

const CONFIG_FILE = "runtime-config.json";

// runtimeConfigPathFor derives the document's path from the mount, exactly
// as bridgePathFor derives the WebSocket path. Same reasoning: the mount
// point is decided once (vite.config.ts `base`) and everything else is
// derived. Since memql#3711 that mount is "/" (the portal is site #1,
// served at its own origin's root), so this now lands on
// component/edge/runtimeconfig.go's runtimeConfigPath ("/runtime-config.json")
// with no code change here -- the function was already general enough for a
// root mount, only its production INPUT changed.
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
  // The cluster's configured domain (MEMQL_DOMAIN), served by the node since
  // memql#4249. Empty when an older node omits it or none is configured;
  // src/cluster/editorLink.ts derives a fallback from identityUrl.
  domain: string;
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
  domain: "",
};

// isRuntimeConfigReady is the gate for anything that posts to identity
// with client_id / authorize URL fields. UNKNOWN (and a half-loaded
// document) has empty oauthClientId; exchanging then produces identity's
// "code, client_id, and redirect_uri are required" (token.go lists all
// three whenever any one is missing). Empty identityApiBaseUrl is fine --
// that is the same-origin proxy shape.
export function isRuntimeConfigReady(config: PortalRuntimeConfig): boolean {
  return Boolean(config.identityUrl && config.oauthClientId);
}

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
    domain: asString(doc.domain),
  };
}

function asString(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function trimTrailingSlash(value: string): string {
  return value.replace(/\/+$/, "");
}
