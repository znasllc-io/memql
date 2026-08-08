// Where the portal dials.
//
// The portal is served BY the bff (component/portal mounts it at /portal/ on
// the bff's HTTP server), so the cluster it talks to is always the origin it
// was loaded from. That is a deliberate property, not a convenience: it means
// there is no endpoint to configure, no CORS to arrange, and no way for the
// bundle to end up pointed at a different cluster than the one that served
// it. `/memql/ws` stays RELATIVE so the SDK resolves it against
// document.location -- the same URL works behind the local traefik front
// door, behind the cloud nginx ingress, and behind a `vite dev` proxy.

const BRIDGE_PATH = "/memql/ws";

// A deployment may serve the whole memQL HTTP surface under a base path
// (SERVER_PUBLIC_PATH on the Go side registers `/{prefix}/memql/ws` and
// `/{prefix}/portal/` together). The portal knows its own prefix from Vite's
// `base`, so it can derive the bridge path instead of being told twice.
export function bridgePathFor(baseUrl: string): string {
  // BASE_URL is always "<prefix>/portal/" -- strip the mount segment to get
  // whatever deployment prefix sits in front of it ("" in the normal case).
  const prefix = baseUrl.replace(/\/portal\/?$/, "").replace(/\/+$/, "");
  return prefix + BRIDGE_PATH;
}

// portalBridgePath is the value the running bundle uses. import.meta.env.
// BASE_URL is Vite's compile-time `base`, so this is resolved at build time
// and carries no runtime configuration.
export const portalBridgePath = bridgePathFor(import.meta.env.BASE_URL);
