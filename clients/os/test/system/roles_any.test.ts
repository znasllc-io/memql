import { beforeEach, describe, expect, it } from "vitest";

import { roleAdmits, roleLadder, setRoleLadder } from "../../src/system/roles";
import { SEEDED_LADDER } from "../seededLadder";

// The SET form of RoleRequirement (issue #4826 / program decision P6).
//
// The ladder is still the default and `dock_roles_store.test.ts` pins it rung
// by rung. What is new here is the one requirement the ladder cannot express:
// owner-or-developer, explicitly not admin. `{ min: "developer" }` admits
// admin and is therefore not an approximation of it -- it is a different
// policy that would hand an admin a page of forms the engine refuses.

const OWNER_OR_DEVELOPER = { any: ["owner", "developer"] } as const;

// The ladder is CLUSTER STATE (epic memql#4832, D1), so every case installs
// it. Per test rather than per file: vitest isolates per FILE, so a case
// running after one that cleared the ladder would measure an empty one and
// pass by admitting nothing.
beforeEach(() => setRoleLadder(SEEDED_LADDER));

/** The ladder's slugs, weakest first -- what ROLE_LADDER used to be. */
function ladderSlugs(): string[] {
  return roleLadder().map((rung) => rung.slug);
}

describe("a set requirement admits exactly its members", () => {
  it("admits owner and developer", () => {
    expect(roleAdmits("owner", OWNER_OR_DEVELOPER)).toBe(true);
    expect(roleAdmits("developer", OWNER_OR_DEVELOPER)).toBe(true);
  });

  it("refuses admin -- the rank BETWEEN the two members", () => {
    // The whole reason the form exists. admin outranks developer and is
    // outranked by owner, so no floor on the ladder can admit both members
    // and leave admin out.
    expect(roleAdmits("admin", OWNER_OR_DEVELOPER)).toBe(false);
  });

  it("refuses reader and writer", () => {
    // Both are LEGACY slugs that resolve through their rung's aliases
    // (reader -> viewer, writer -> user), so this also covers the set form
    // matching on RANK rather than on the slug string.
    expect(roleAdmits("reader", OWNER_OR_DEVELOPER)).toBe(false);
    expect(roleAdmits("writer", OWNER_OR_DEVELOPER)).toBe(false);
  });

  it("admits every member of any set, and nothing else", () => {
    // The general property, over every subset the ladder can name, so this
    // does not rest on the one requirement that motivated the form.
    for (const member of ladderSlugs()) {
      const requirement = { any: [member] };
      for (const actor of ladderSlugs()) {
        expect(roleAdmits(actor, requirement)).toBe(actor === member);
      }
    }
  });

  it("admits nobody when the set is empty", () => {
    // Fail closed: "these roles, and this is none of them". An empty set that
    // admitted everyone would turn a mistake into an open door.
    for (const actor of ladderSlugs()) {
      expect(roleAdmits(actor, { any: [] })).toBe(false);
    }
  });
});

describe("the ladder form is unchanged", () => {
  it("still admits every rank at or above the floor, and no rank below it", () => {
    // The negative control for the change: the set form is additive, and a
    // regression in the ladder is the failure that would go unnoticed because
    // every surface but one uses it.
    const slugs = ladderSlugs();
    slugs.forEach((actor, i) => {
      slugs.forEach((min, j) => {
        expect(roleAdmits(actor, { min })).toBe(i >= j);
      });
    });
  });

  it("still admits every actor when there is no requirement", () => {
    for (const actor of [...ladderSlugs(), "", "mystery"]) {
      expect(roleAdmits(actor, undefined)).toBe(true);
    }
  });
});

describe("an unknown actor role is admitted by neither form", () => {
  it("refuses an unrankable role against a floor and against a set", () => {
    for (const actor of ["", "mystery", "Owner", "operator"]) {
      expect(roleAdmits(actor, { min: "reader" })).toBe(false);
      expect(roleAdmits(actor, OWNER_OR_DEVELOPER)).toBe(false);
      // ...and is still admitted where nothing is asked, which is what makes
      // the two lines above a statement about the GATE rather than about a
      // predicate that refuses everything.
      expect(roleAdmits(actor, undefined)).toBe(true);
    }
  });

  it("cannot be smuggled in through a set member that is not on the ladder", () => {
    // The set names SLUGS now (epic memql#4832) rather than a closed union,
    // because the roles are cluster state -- so this shape is reachable from
    // ordinary typed code, not only from untyped data, and the predicate is
    // the only thing standing between a typo and a match. Two unrankable
    // strings must not match each other.
    const forged = { any: ["superuser"] };
    expect(roleAdmits("superuser", forged)).toBe(false);
    expect(roleAdmits("owner", forged)).toBe(false);
  });
});
