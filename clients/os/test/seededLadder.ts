import type { RoleRung } from "../src/system/roles";

/**
 * The role ladder this cluster seeds, as a test fixture.
 *
 * IT IS A COPY, AND A GO GATE PINS IT. `dsl/rbac/seeds.memql` is the source;
 * this file exists because a jsdom test has no cluster to read from, and
 * `component/auth/role_ladder_client_parity_test.go` fails the build the
 * moment the two disagree. That gate is the whole point -- an unpinned copy
 * of an ordering is exactly what this epic deleted from the shell, and
 * reintroducing one in the test tree would be the same defect wearing a
 * different extension.
 *
 * NOTE THE ORDER: developer (300) OUTRANKS admin (200). The shell used to
 * rank them the other way round.
 */
export const SEEDED_LADDER: RoleRung[] = [
  { slug: "viewer", name: "Viewer", rank: 50, aliases: ["reader"] },
  { slug: "user", name: "Member", rank: 100, aliases: ["writer"] },
  { slug: "admin", name: "Admin", rank: 200, aliases: [] },
  { slug: "developer", name: "Developer", rank: 300, aliases: [] },
  { slug: "owner", name: "Owner", rank: 400, aliases: [] },
];
