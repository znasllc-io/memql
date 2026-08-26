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
  // OWNER, not owner-or-admin (epic memql#4440). Every other surface here
  // shares one floor, and `providers` is the first that does not: seeding a
  // vendor credential and rotating it across the fleet is a cluster-owner act,
  // and the engine's builtins refuse below owner regardless of what this
  // console renders.
  //
  // Absent (the default) means the console's ordinary owner-or-admin floor.
  readonly ownerOnly?: boolean;
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
//     a VIEW that listed the same population (People then, Users since
//     memql#4526). The list was always the
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
  {
    id: "providers",
    label: "AI providers",
    title: "AI providers",
    ownerOnly: true,
    blurb:
      "Which models this cluster can call, and how it authenticates to them. " +
      "Nothing here is needed to install or run the cluster -- configure it " +
      "when you want agents to think.",
  },
];

// WHICH SURFACES A ROLE IS OFFERED IS NOT DECIDED HERE ANY MORE (memql#4655).
//
// These four are tabs on the Cluster destination now, beside Integrations,
// Data origins and Stores, so their visibility is one `access` field per tab
// in src/app/nav.ts and the rule that reads it is nav.ts's `maySee`. This
// module kept its own copy of that filter until the restructure; two filters
// over one tab strip is how a strip and the rail row that opens it come to
// disagree about what an area contains.
//
// The RULE is unchanged and still worth stating: a surface above the caller's
// floor is ABSENT, not disabled (epic memql#4440). A greyed-out tab is an
// advertisement for a capability whose only explanation is being told you may
// not have it. And an unresolved role is treated as below every floor -- the
// safe direction, because the tab appears when the role arrives.

export function adminPath(surfaceId = ""): string {
  return surfaceId === "" ? ADMIN_ROOT : `${ADMIN_ROOT}/${surfaceId}`;
}

export function surfaceById(id: string): AdminSurface | undefined {
  return ADMIN_SURFACES.find((surface) => surface.id === id);
}
