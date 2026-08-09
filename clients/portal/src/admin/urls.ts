// The addresses under /admin, and the four surfaces that answer to them.
//
// A sibling module rather than a block at the top of AdminRoutes, for the
// reason src/integrations/urls.ts is one: a path is referenced from the route
// table, the sub-nav and the tests, and three hand-written string literals is
// how a link ends up pointing one segment away from the route that serves it.
//
// The route table (src/app/routes.tsx) mounts this module as a SPLAT, so
// nothing here is repeated there -- adding a fifth surface is a row in
// ADMIN_SURFACES plus a Route, and no edit outside this directory.

export const ADMIN_ROOT = "/admin";

// A surface's slug, its label, and the sentence that says what an operator
// came here to do. The blurb is interface copy: it names the operator's job,
// not the schema.
export interface AdminSurface {
  // "" is the index surface -- /admin itself, not /admin/overview. An
  // operations console's landing page should not need a word after the slash.
  readonly id: string;
  readonly label: string;
  readonly title: string;
  readonly blurb: string;
}

export const ADMIN_SURFACES: readonly AdminSurface[] = [
  {
    id: "",
    label: "Overview",
    title: "Cluster overview",
    blurb:
      "Who can sign in, what the cluster has been doing, and which key it is " +
      "signing with right now.",
  },
  {
    id: "tokens",
    label: "Tokens",
    title: "Sessions and tokens",
    blurb:
      "Every personal access token issued against this cluster, and who holds " +
      "it. Revoke one the moment it is out of the owner's hands.",
  },
  {
    id: "keys",
    label: "Signing keys",
    title: "Signing keys",
    blurb:
      "The Ed25519 keys this cluster publishes for verifying access tokens, " +
      "and when it last rotated them.",
  },
  {
    id: "settings",
    label: "Settings",
    title: "Cluster settings",
    blurb:
      "The runtime-editable settings in force: who may register, how long a " +
      "token lives, and how the cluster brands itself.",
  },
];

export function adminPath(surfaceId = ""): string {
  return surfaceId === "" ? ADMIN_ROOT : `${ADMIN_ROOT}/${surfaceId}`;
}

export function surfaceById(id: string): AdminSurface | undefined {
  return ADMIN_SURFACES.find((surface) => surface.id === id);
}
