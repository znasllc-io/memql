// The addresses under /me, and the facets that answer to them.
//
// A sibling module rather than a block at the top of MeRoutes, for the reason
// src/admin/urls.ts is one: a path is referenced from the route table, the tab
// strip, the rail's profile row and the tests, and four hand-written string
// literals is how a link ends up one segment away from the route that serves
// it.
//
// The route table mounts this module as a SPLAT (`me/*`), so nothing here is
// repeated there -- adding a facet is a row in ME_FACETS plus a Route, and no
// edit outside this directory.

export const ME_ROOT = "/me";

export interface MeFacet {
  // "" is the index facet -- /me itself, not /me/account. The page a person
  // reaches by clicking their own name should not need a word after the
  // slash.
  readonly id: string;
  readonly label: string;
}

// Settings sits beside the account facts, and the two security-ish tabs stay
// adjacent -- Sessions ("what is signed in") and Security ("how it can be
// entered") answer the same question from two sides and read as a pair.
export const ME_FACETS: readonly MeFacet[] = [
  { id: "", label: "Account" },
  { id: "settings", label: "Settings" },
  { id: "sessions", label: "Sessions" },
  { id: "security", label: "Security" },
];

export function mePath(facetId = ""): string {
  return facetId === "" ? ME_ROOT : `${ME_ROOT}/${facetId}`;
}

// identityPath composes a link into identity's OWN self-service pages.
//
// THE ORIGIN IS CONFIGURATION, NEVER A LITERAL. It comes from the runtime
// config the portal already loads (`PortalRuntimeConfig.identityUrl`), which
// is derived per-cluster from MEMQL_DOMAIN -- so a portal serving
// lab.example.com links to identity.lab.example.com without a build. A
// hardcoded host here would send an operator to somebody else's cluster to
// manage their passkeys, which is the worst possible destination for that
// particular link.
//
// Returns "" when no identity origin is configured, which is the
// auth-disabled cluster. Callers render nothing rather than a dead link: a
// link to nowhere is worse than an absent one, because the reader concludes
// the capability is broken rather than absent.
export function identityPath(identityUrl: string, path: string): string {
  const origin = identityUrl.trim().replace(/\/+$/, "");
  if (origin === "") return "";
  return `${origin}${path.startsWith("/") ? path : `/${path}`}`;
}

// The self-service destinations identity owns. Named here so the pages that
// link to them cannot spell one differently, and so this list reads as what
// it is: the documented split (docs/public/operate/portal.md), enumerated.
//
// NOTE THE COLLISION IN THE NAMES, because it is the thing a reader gets wrong
// here: IDENTITY_SETTINGS is `/me/settings` on the IDENTITY service, and the
// portal now has a `/me/settings` of its own. They are different pages with
// different jobs -- the portal's holds cluster preferences, identity's holds
// email, export and deletion -- and the portal's Settings tab is what links to
// identity's (memql#4523). Both halves are named in
// docs/public/operate/portal.md.
export const IDENTITY_SETTINGS = "/me/settings";
export const IDENTITY_DEVICES = "/me/devices";
export const IDENTITY_TOKENS = "/me/tokens";
// Export is EXPORT ONLY. Deleting the account is on /me/settings, beside the
// cooldown copy that explains what a deletion actually starts -- checked
// against the templates rather than assumed from the route names, because
// "export or delete your data" is the sentence every other product uses and
// it would have sent somebody to the wrong page here.
export const IDENTITY_EXPORT = "/me/export";
