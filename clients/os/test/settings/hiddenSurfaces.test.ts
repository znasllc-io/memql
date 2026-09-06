import { describe, expect, it } from "vitest";

import { hiddenSurfaces } from "../../src/apps/settings/hiddenSurfaces";
import { OS_REGISTRY } from "../../src/apps/registry";
import type { RoleRequirement } from "../../src/system/roles";

const OWNER_OR_DEVELOPER: RoleRequirement = { any: ["owner", "developer"] };

/** A set requirement that does not name admin. */
function excludesAdmin(requirement?: RoleRequirement): boolean {
  return !!requirement && "any" in requirement && !requirement.any.includes("admin");
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
  return !!requirement && "min" in requirement && requirement.min === "owner";
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

  it("an admin loses only what a role SET leaves them out of", () => {
    // TWO SHAPES CAN HIDE A SURFACE FROM AN ADMIN, and until epic memql#4984
    // only one of them appeared in this registry. A `{ any: [...] }` that does
    // not name admin legitimately hides its surface -- P6's owner-or-developer
    // Integrations section is exactly that. A `{ min }` ABOVE admin does too,
    // and Settings -> AI providers is the first: the provider reads and writes
    // are owner-gated in the engine, so an admin offered that section would
    // meet a refusal on every control in it.
    //
    // The expectation is DERIVED from the manifests rather than written out,
    // so it stays true as sections land. It is a different walk from the one
    // under test: it ignores the ladder entirely and reads the requirement
    // shapes directly, which is what keeps it from being a reimplementation.
    const gated = [
      ...OS_REGISTRY.apps.flatMap((app) => [
        ...(excludesAdmin(app.roles) || aboveAdmin(app.roles) ? [app.name] : []),
        ...(app.sections ?? [])
          .filter((s) => excludesAdmin(s.roles) || aboveAdmin(s.roles))
          .map((s) => `${app.name} -- ${s.name}`),
      ]),
      ...OS_REGISTRY.widgets
        .filter((w) => excludesAdmin(w.roles) || aboveAdmin(w.roles))
        .map((w) => w.name),
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
