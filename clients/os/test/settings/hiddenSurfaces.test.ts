import { describe, expect, it } from "vitest";

import { hiddenSurfaces } from "../../src/apps/settings/hiddenSurfaces";
import { OS_REGISTRY } from "../../src/apps/registry";
import { roleRank, type RoleRequirement } from "../../src/system/roles";

const OWNER_OR_DEVELOPER: RoleRequirement = { any: ["owner", "developer"] };

/**
 * A SET requirement that does not name admin.
 *
 * One of the two shapes that can hide a surface from an admin; `aboveAdmin`
 * below is the other. They stay separate predicates because they are
 * different statements -- "these roles, and admin is not one of them" versus
 * "a floor an admin does not clear" -- and a single helper answering both
 * would make the expectation below unreadable.
 */
function excludesAdmin(requirement?: RoleRequirement): boolean {
  if (!requirement) return false;
  if ("any" in requirement) return !requirement.any.includes("admin");
  // A floor ABOVE admin excludes one. `roleRank` reads the seeded ladder the
  // test setup installs, so this is the cluster's ordering rather than a
  // second copy of it -- the thing memql#4832 deleted the literal to stop.
  return roleRank(requirement.min) > roleRank("admin");
}

/**
 * A ladder MINIMUM an admin does not clear.
 *
 * This used not to exist, and the comment where it is used explains why: every
 * `{ min }` in the registry sat at or below admin, so a set was the only shape
 * that could hide anything from one. Settings -> AI providers (epic
 * memql#4984) is the first floor above admin -- `providerAuthStatus` and both
 * provider writes are owner-gated, so offering it to an admin would be a
 * section whose every control answers with a refusal.
 *
 * `owner` is named rather than compared through the ladder on purpose: the
 * ladder is loaded from the cluster at runtime and is empty in a unit test, so
 * a rank comparison here would answer "admin clears everything" and make the
 * assertion vacuous.
 */
function aboveAdmin(requirement?: RoleRequirement): boolean {
  if (!requirement || !("min" in requirement)) return false;
  // RANK, NOT `=== "owner"`. The ladder is cluster state -- ranks are spaced
  // 50/100/200/300/400 precisely so a customer-defined role can slot between
  // two base ones -- and a literal here would be the second hand-maintained
  // ordering that epic memql#4832 deleted the first one to stop. It also
  // happens to be wrong already for a shipped rung: developer ranks 300,
  // ABOVE admin's 200, so `{ min: "developer" }` excludes an admin too and a
  // string comparison against "owner" would miss it.
  return roleRank(requirement.min) > roleRank("admin");
}

describe("the permissions self-view (memql#4744)", () => {
  it("lists admin- and writer-gated apps for a reader", () => {
    const hidden = hiddenSurfaces(OS_REGISTRY, "reader");
    const labels = hidden.map((h) => h.label);
    expect(labels).toContain("Users");
    expect(labels).toContain("Training");
    expect(labels).toContain("Settings -- Cluster");
    // "admin", because Users states a FLOOR now that developer holds the
    // admission capability. The SET rendering this table does is still
    // exercised, by the Integrations section below.
    expect(hidden.find((h) => h.label === "Users")?.requires).toBe("admin");
    expect(hidden.find((h) => h.label === "Training")?.requires).toBe("writer");
  });

  it("a writer keeps Training and still loses the admin surfaces", () => {
    const labels = hiddenSurfaces(OS_REGISTRY, "writer").map((h) => h.label);
    expect(labels).not.toContain("Training");
    expect(labels).toContain("Users");
    expect(labels).toContain("Settings -- Cluster");
  });

  it("an admin loses exactly what the manifests leave them out of", () => {
    // TWO SHAPES CAN HIDE A SURFACE FROM AN ADMIN, and until recently only
    // one of them appeared in this registry. A `{ any: [...] }` that does not
    // name admin legitimately hides its surface -- P6's owner-or-developer
    // Integrations section is exactly that. A `{ min }` ABOVE admin does too:
    // Settings -> AI providers (epic memql#4984), and then the Cluster app's
    // Data origins and Audit trail sections and the whole Stores app (epic
    // memql#5009), all of which are owner-floored because the ENGINE is.
    //
    // The Audit trail's is the one worth knowing about. Row admission returns
    // ZERO ROWS rather than an error, so an admin admitted to that section
    // would be shown an empty trail with no refusal to render -- "nothing
    // happened", to the one person who cannot check. The floor is the only
    // mechanism that can prevent it, so this table listing it as hidden from
    // an admin is correct and must stay correct.
    //
    // The expectation is DERIVED from the manifests rather than written out,
    // so it stays true as sections land. It is a different walk from the one
    // under test: it ignores the ladder's own predicate and reads the
    // requirement shapes directly, which is what keeps it from being a
    // reimplementation.
    const hides = (r?: RoleRequirement) => excludesAdmin(r) || aboveAdmin(r);
    const gated = [
      ...OS_REGISTRY.apps.flatMap((app) =>
        // A HIDDEN APP IS THE WHOLE ANSWER -- its sections are not enumerated
        // under it. Listing "Stores -- Stores" beneath a hidden "Stores" pads
        // the table with rows that all say the same thing and buries the
        // informative case: a section gated ABOVE an app the person can
        // otherwise open. `hiddenSurfaces` states that rule, and this walk has
        // to share it or it is testing a different function. It only began to
        // matter when an owner-floored APP landed; before that, no app in the
        // registry was hidden from an admin at all.
        hides(app.roles)
          ? [app.name]
          : (app.sections ?? []).filter((s) => hides(s.roles)).map((s) => `${app.name} -- ${s.name}`),
      ),
      ...OS_REGISTRY.widgets.filter((w) => hides(w.roles)).map((w) => w.name),
    ];
    expect(hiddenSurfaces(OS_REGISTRY, "admin").map((h) => h.label)).toEqual(gated);
    // The anti-vacuous floor: if both helpers ever stopped matching anything,
    // the assertion above would compare two empty lists and pass against a
    // registry that hid everything from an admin.
    expect(gated).toContain("Settings -- Integrations");
    expect(gated).toContain("Settings -- AI providers");
  });

  it("names both roles when a set requirement is what hid the surface", () => {
    // The negative control for the line above, and the copy rule it enforces:
    // a set renders as its members, never as its lowest one. "developer"
    // beside a surface an admin outranks and still cannot open is the one
    // sentence somebody reading this table for an explanation must not get.
    const registry = {
      apps: [
        {
          id: "settings",
          name: "Settings",
          icon: () => null,
          sections: [{ id: "integrations", name: "Integrations", roles: OWNER_OR_DEVELOPER }],
          settingsSection: "integrations",
          logsSection: "integrations",
          component: () => null,
        },
      ],
      widgets: [],
    };
    expect(hiddenSurfaces(registry, "admin")).toEqual([
      { kind: "section", label: "Settings -- Integrations", requires: "owner or developer" },
    ]);
    expect(hiddenSurfaces(registry, "developer")).toEqual([]);
    expect(hiddenSurfaces(registry, "owner")).toEqual([]);
  });

  it("an owner shows nothing hidden", () => {
    expect(hiddenSurfaces(OS_REGISTRY, "owner")).toEqual([]);
  });

  it("does not enumerate the sections of an app that is itself hidden", () => {
    const labels = hiddenSurfaces(OS_REGISTRY, "reader").map((h) => h.label);
    expect(labels).toContain("Users");
    // "Users -- People" would say the same thing three times and bury the
    // informative case: a section gated above an app you CAN open.
    expect(labels.filter((l) => l.startsWith("Users --"))).toEqual([]);
  });

  it("names the cause when the actor's own role is unrankable", () => {
    // roleAdmits refuses to let an unknown role unlock anything gated, so an
    // empty role hides everything -- including surfaces that ask for
    // nothing. "requires none" would be true and useless there.
    const hidden = hiddenSurfaces(OS_REGISTRY, "");
    expect(hidden.length).toBeGreaterThan(0);
    expect(hidden.find((h) => h.label === "Users")?.requires).toBe("admin");
  });
});
