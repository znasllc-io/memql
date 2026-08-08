// Where the portal dials -- AND the portal's answer to "which clusters exist".
//
// The portal is served BY the bff (component/portal mounts it at /portal/ on
// the bff's HTTP server), so the cluster it talks to is always the origin it
// was loaded from. That is a deliberate property, not a convenience: it means
// there is no endpoint to configure, no CORS to arrange, and no way for the
// bundle to end up pointed at a different cluster than the one that served
// it. `/memql/ws` stays RELATIVE so the SDK resolves it against
// document.location -- the same URL works behind the local traefik front
// door, behind the cloud nginx ingress, and behind a `vite dev` proxy.
//
// ===========================================================================
// THE CLUSTER-REGISTRY DECISION (memql#3315). Read this before adding a
// second source of endpoints anywhere in the portal.
// ===========================================================================
//
// The VS Code panel reads its clusters from ~/.memql/clusters.yaml and
// authenticates with a PAT out of that file. A browser can do neither: it has
// no filesystem, and a long-lived PAT sitting where page JavaScript can read
// it would be strictly worse than the OAuth flow the identity service already
// runs. So the portal needed its own answer, and there were three:
//
//   1. DERIVE FROM THE ORIGIN -- the cluster is wherever this page came from.
//   2. A SERVER-SIDE REGISTRY -- v1:cluster:* rows an operator maintains, so
//      every operator sees the same list from any browser.
//   3. BROWSER-LOCAL STORAGE -- a list in localStorage, per browser profile.
//
// CHOSEN: (1). It costs nothing -- there is no registry, no schema, no CRUD
// surface, no sync problem -- and it matches how an operator actually thinks
// about a web console: the console at cockpit.prod.example.com IS the
// production cluster's console, the way an admin page is part of the system it
// administers. It also makes a whole class of mistake impossible, because the
// page and the stream share one origin: the bundle cannot end up reading
// cluster A while carrying a token minted for cluster B.
//
// The cost, stated plainly: NO MULTI-CLUSTER SWITCHING. An operator with three
// clusters opens three tabs. That is an accepted trade, not an oversight --
// three tabs is a real answer for three clusters, and it is honest about which
// one you are looking at in a way a dropdown is not.
//
// REJECTED (3), and it should stay rejected: browser-local storage is
// per-browser and per-profile, so it is invisible to every other operator and
// to the same operator on another machine, and it drifts silently away from
// the clusters.yaml the cockpit and the VS Code panel share. A registry only
// one person can see is worse than none, because it looks like one.
//
// THE SEAM FOR (2). If multi-cluster is revisited, THIS MODULE is where it
// lands and the only place that changes. A server-side registry would surface
// as a `clusters: [...]` field on the runtime-config document (src/cluster/
// config.ts and component/portal/config.go -- already the one thing the bundle
// reads before it has a connection to read anything else from), and this
// module would gain a resolver that picks an entry and returns its absolute
// bridge URL instead of a relative path. Everything downstream goes through
// `portalBridgePath`, so nothing else moves. Do NOT spread endpoint knowledge
// outward in anticipation: the single choke point is what makes that change
// cheap.

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

// clusterLabelFor names the cluster for a human.
//
// Under the derive-from-origin decision above, the cluster IS the origin, so
// the host is its name -- and it is a name the operator already recognises,
// because it is what they typed into the address bar. The scheme and any path
// prefix are dropped: they say nothing about WHICH cluster this is, and a
// header is not the place for noise. Rendered next to the node id from the
// ServerHello, which names the individual replica within it.
export function clusterLabelFor(location: { host?: string } | undefined): string {
  return location?.host ?? "";
}

// portalRedirectPathFor is the OAuth callback path, derived from the mount for
// the same reason the bridge path is. It must match, byte for byte, a
// redirect_uri registered for the portal's client on the identity service --
// identity matches redirect URIs by exact string (component/identity/config.go
// clientAllowsRedirectURI), so "close enough" is a 400 at /authorize.
export function portalRedirectPathFor(baseUrl: string): string {
  const mount = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
  return mount + "auth/callback";
}

export const portalRedirectPath = portalRedirectPathFor(import.meta.env.BASE_URL);
