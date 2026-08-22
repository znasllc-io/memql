// The addresses under /admin, and the surfaces that answer to them.
//
// A sibling module rather than a block at the top of AdminRoutes, for the
// reason src/integrations/urls.ts is one: a path is referenced from the route
// table, the sub-nav and the tests, and three hand-written string literals is
// how a link ends up pointing one segment away from the route that serves it.
//
// The route table (src/app/routes.tsx) mounts this module as a SPLAT, so
// nothing here is repeated there -- adding a surface is a row in
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

// TWO SURFACES RETIRED HERE (memql#4264), and both for the same reason:
// they were second doors to something the portal already had.
//
//   "" (Cluster overview)  answered the same question as the console -- "what
//     is the state of this cluster" -- with the same tiles and the same "By
//     role" band. Two landing pages is one too many; /admin now redirects to
//     the console, and the readings it carried that were NOT duplicated (the
//     signing key, the last rotation, the registration mode) already render on
//     the Signing keys and Settings surfaces.
//
//   people                 was the CHANGE surface for a person, sitting beside
//     a People VIEW that listed the same population. The list was always the
//     view's job; the verbs moved to the view's row detail
//     (src/people/PersonActions.tsx), and /admin/people redirects there.
export const ADMIN_SURFACES: readonly AdminSurface[] = [
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
