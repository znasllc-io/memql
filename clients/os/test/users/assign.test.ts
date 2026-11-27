import { describe, expect, it } from "vitest";

import { mayAssign, rungRefusal, type AssignContext, type AssignKind } from "../../src/apps/users/assign";
import type { GrantRow, RoleRow } from "../../src/apps/users/rows";

// THE MIRROR OF auth.MayAssignRole, ASSERTED (memql#5236).
//
// This module decides which rungs a ladder OFFERS, and pickers.tsx passes the
// answer straight to `disabled`. So a rule stricter than the engine's does not
// show a refusal -- it removes the control, and the operator has nothing to
// read. That is exactly what shipped: the engine learned that a developer may
// invite an admin and this file kept refusing it, so the fix was live and
// unreachable.
//
// It had no test at all, in an app with ten of them. These are the cases that
// would have caught it.

const OWNER = 400;
const DEVELOPER = 300;
const ADMIN = 200;
const USER = 100;
const VIEWER = 50;
const RANKS: Record<string, number> = {
  owner: OWNER,
  developer: DEVELOPER,
  admin: ADMIN,
  user: USER,
  viewer: VIEWER,
};

function role(slug: string, over: Partial<RoleRow> = {}): RoleRow {
  return {
    id: `v1:rbac:role:${slug}`,
    slug,
    name: slug,
    rank: RANKS[slug] ?? 0,
    description: "",
    predefined: true,
    active: true,
    aliases: [],
    accountId: "",
    ...over,
  };
}

function grant(roleSlug: string, verb: string, resourceType: string): GrantRow {
  return {
    id: `${roleSlug}:${verb}:${resourceType}`,
    roleSlug,
    verb,
    resourceType,
    effect: "allow",
    predefined: true,
    active: true,
  };
}

// The seeded grant sets, narrowed to what these rules read: `principal` and
// `admission`. Taken from dsl/rbac/seeds.memql -- owner and admin hold all four
// principal verbs, developer holds only read, and all three admit people.
const GRANTS: GrantRow[] = [
  ...(["read", "create", "update", "delete"] as const).flatMap((v) => [
    grant("owner", v, "principal"),
    grant("admin", v, "principal"),
  ]),
  grant("developer", "read", "principal"),
  grant("owner", "create", "admission"),
  grant("admin", "create", "admission"),
  grant("developer", "create", "admission"),
];

function ctx(callerRole: string, kind: AssignKind, over: Partial<AssignContext> = {}): AssignContext {
  return {
    kind,
    callerRole,
    callerRank: RANKS[callerRole] ?? 0,
    callerIsOwner: callerRole === "owner",
    grants: GRANTS,
    // A re-role's target sits on the bottom rung, so rule 3 passes for every
    // caller and each case isolates rules 2, 4 and 5.
    targetRole: kind === "reRole" ? "viewer" : "",
    targetRank: kind === "reRole" ? VIEWER : 0,
    targetIsOwner: false,
    targetAccountIds: [],
    ...over,
  };
}

describe("the invitation kind", () => {
  it("offers admin to a developer -- admitting is not wielding", () => {
    expect(rungRefusal(role("admin"), ctx("developer", "invitation"))).toBe("");
    expect(mayAssign(role("admin"), ctx("developer", "invitation"))).toBe(true);
  });

  it("still refuses owner to a developer -- the rank cap is untouched", () => {
    expect(rungRefusal(role("owner"), ctx("developer", "invitation"))).toBe("ownershipTransfer");
  });

  it("still refuses a peer admin to an admin", () => {
    expect(rungRefusal(role("admin"), ctx("admin", "invitation"))).toBe("aboveCaller");
  });

  it("offers everything below to an owner", () => {
    for (const slug of ["developer", "admin", "user", "viewer"]) {
      expect(rungRefusal(role(slug), ctx("owner", "invitation")), slug).toBe("");
    }
  });

  it("refuses a caller who cannot admit people at all", () => {
    expect(rungRefusal(role("viewer"), ctx("user", "invitation"))).toBe("notAUserManager");
  });
});

describe("the re-role kind", () => {
  it("refuses a developer outright -- it holds no update on principal", () => {
    for (const slug of ["admin", "user", "viewer"]) {
      expect(rungRefusal(role(slug), ctx("developer", "reRole")), slug).toBe("notAUserManager");
    }
  });

  it("lets an admin demote somebody below them", () => {
    expect(rungRefusal(role("user"), ctx("admin", "reRole"))).toBe("");
  });

  it("refuses an admin their own rung", () => {
    expect(rungRefusal(role("admin"), ctx("admin", "reRole"))).toBe("aboveCaller");
  });

  it("refuses a target the caller does not outrank", () => {
    const over = { targetRole: "developer", targetRank: DEVELOPER };
    expect(rungRefusal(role("user"), ctx("admin", "reRole", over))).toBe("targetOutranks");
  });
});

// THE CONTROL THAT KEEPS THE SPLIT HONEST.
//
// Every case above would stay green if rule 5 were DELETED rather than scoped,
// because the only caller in the fixture lacking a principal verb is developer,
// which rule 2 refuses on the re-role kind before rule 5 is reached. So this
// installs a caller that DOES hold update-on-principal and still lacks a verb
// the granted role holds, and asserts the same pair answers differently per
// kind. It mirrors TestPeopleAuthorityClauseRunsOnReRoleOnly in component/auth.
describe("rule 5 runs on the re-role kind only", () => {
  const grants: GrantRow[] = [
    grant("support-lead", "read", "principal"),
    grant("support-lead", "create", "principal"),
    grant("support-lead", "update", "principal"),
    grant("support-lead", "create", "admission"),
    // Ranks below support-lead, so the rank cap passes and rule 5 is the only
    // thing left that can refuse it.
    grant("purger", "delete", "principal"),
  ];
  const lead = role("support-lead", { rank: 150 });
  const purger = role("purger", { rank: 60 });
  const base = { callerRole: lead.slug, callerRank: 150, callerIsOwner: false, grants, targetAccountIds: [] };

  it("refuses on a re-role", () => {
    const c: AssignContext = { ...base, kind: "reRole", targetRole: "viewer", targetRank: 50, targetIsOwner: false };
    expect(rungRefusal(purger, c)).toBe("authorityBeyond");
  });

  it("allows the same pair on an invitation", () => {
    const c: AssignContext = { ...base, kind: "invitation", targetRole: "", targetRank: 0, targetIsOwner: false };
    expect(rungRefusal(purger, c)).toBe("");
  });
});

describe("rules that do not depend on the kind", () => {
  it("refuses an inactive rung", () => {
    expect(rungRefusal(role("admin", { active: false }), ctx("owner", "invitation"))).toBe("inactive");
  });

  it("refuses an account-scoped rung for a non-member", () => {
    const scoped = role("acct-lead", { rank: 140, accountId: "acct-1" });
    expect(rungRefusal(scoped, ctx("owner", "invitation"))).toBe("notAMember");
  });

  it("offers an account-scoped rung to a member", () => {
    const scoped = role("acct-lead", { rank: 140, accountId: "acct-1" });
    const c = ctx("owner", "invitation", { targetAccountIds: ["acct-1"] });
    expect(rungRefusal(scoped, c)).toBe("");
  });
});
