import { describe, expect, it } from "vitest";

import { hiddenSurfaces } from "../../src/apps/settings/hiddenSurfaces";
import { OS_REGISTRY } from "../../src/apps/registry";
import type { OsRegistry } from "../../src/system/registry";
import { clearEffectiveCapabilities } from "../../src/system/roles";
import { installSeededAccess, roleOpens } from "../seededAccess";

// The permissions self-view (memql#4744), re-keyed to capabilities (epic
// memql#5289): what the effective set does NOT open, and the resource each
// surface asks for. Every case installs the role's seeded set first, because
// the table reads the effective set out of band rather than taking a role.

describe("the permissions self-view (memql#4744)", () => {
  it("lists admin- and writer-gated apps for a reader", () => {
    installSeededAccess("reader");
    const hidden = hiddenSurfaces(OS_REGISTRY);
    const labels = hidden.map((h) => h.label);
    expect(labels).toContain("Users");
    expect(labels).toContain("Training");
    expect(labels).toContain("Settings -- Cluster");
    // THE RESOURCE, NOT A ROLE. What opens Users is `read app:users`, which a
    // role, a group or a grant to this person by name may hold -- so "admin"
    // would be false for somebody granted the app on their own name.
    expect(hidden.find((h) => h.label === "Users")?.requires).toBe("read on app:users");
    expect(hidden.find((h) => h.label === "Training")?.requires).toBe("read on app:training");
  });

  it("a writer keeps Training and still loses the admin surfaces", () => {
    installSeededAccess("writer");
    const labels = hiddenSurfaces(OS_REGISTRY).map((h) => h.label);
    expect(labels).not.toContain("Training");
    expect(labels).toContain("Users");
    expect(labels).toContain("Settings -- Cluster");
  });

  it("an admin loses exactly what the seeds leave them out of", () => {
    // THE EXPECTATION IS DERIVED FROM THE SEEDS rather than written out, so
    // it stays true as surfaces land. It is a different walk from the one
    // under test: it reads each manifest's resource name and asks the seeds
    // directly whether admin's role holds it, which is what keeps it from
    // being a reimplementation of `hiddenSurfaces`.
    //
    // A HIDDEN APP IS THE WHOLE ANSWER -- its sections are not enumerated
    // under it. Listing "Users -- Invitations" beneath a hidden "Users" pads
    // the table with rows that all say the same thing and buries the
    // informative case: a section gated ABOVE an app the person can
    // otherwise open.
    const opens = (resource?: string) => resource === undefined || roleOpens("admin", resource);
    const gated = [
      ...OS_REGISTRY.apps.flatMap((app) =>
        !opens(app.requires)
          ? [app.name]
          : (app.sections ?? []).filter((s) => !opens(s.requires)).map((s) => `${app.name} -- ${s.name}`),
      ),
      ...OS_REGISTRY.widgets.filter((w) => !opens(w.requires)).map((w) => w.name),
    ];
    installSeededAccess("admin");
    expect(hiddenSurfaces(OS_REGISTRY).map((h) => h.label)).toEqual(gated);
    // The anti-vacuous floor: if the seeds ever opened everything to admin,
    // the assertion above would compare two empty lists and pass against a
    // registry that hid everything from one. Integrations and Doors are
    // owner-or-developer by program decision P6; Data origins and the Audit
    // trail are owner-floored because the engine is (row admission returns
    // ZERO ROWS rather than an error there, so a section is the only
    // mechanism that can stop the trail reading as "nothing happened").
    //
    // `Stores` was a fourth until epic memql#5530 deleted the app. Three are
    // still three, so the floor holds without it; Data origins is named here
    // in its place because it is the one the comment above already argues
    // for and the list had never actually asserted.
    expect(gated).toContain("Settings -- Integrations");
    expect(gated).toContain("Settings -- Doors");
    expect(gated).toContain("Cluster -- Audit trail");
    expect(gated).toContain("Cluster -- Data origins");
  });

  it("names the resource when a section is what hid the surface", () => {
    // The copy rule: the resource, as the engine spells it, is the string an
    // owner looks for in Settings > Access when asked to grant it.
    const registry: OsRegistry = {
      apps: [
        {
          id: "settings",
          name: "Settings",
          icon: () => null,
          requires: "app:settings",
          sections: [{ id: "integrations", name: "Integrations", requires: "app:settings/integrations" }],
          settingsSection: "integrations",
          logsSection: "integrations",
          component: () => null,
        },
      ],
      widgets: [],
    };
    installSeededAccess("admin");
    expect(hiddenSurfaces(registry)).toEqual([
      { kind: "section", label: "Settings -- Integrations", requires: "read on app:settings/integrations" },
    ]);
    installSeededAccess("developer");
    expect(hiddenSurfaces(registry)).toEqual([]);
    installSeededAccess("owner");
    expect(hiddenSurfaces(registry)).toEqual([]);
  });

  it("an owner shows nothing hidden", () => {
    installSeededAccess("owner");
    expect(hiddenSurfaces(OS_REGISTRY)).toEqual([]);
  });

  it("does not enumerate the sections of an app that is itself hidden", () => {
    installSeededAccess("reader");
    const labels = hiddenSurfaces(OS_REGISTRY).map((h) => h.label);
    expect(labels).toContain("Users");
    // "Users -- People" would say the same thing three times and bury the
    // informative case: a section gated above an app you CAN open.
    expect(labels.filter((l) => l.startsWith("Users --"))).toEqual([]);
  });

  it("hides every app before the effective set has landed, and names what each needs", () => {
    // Fail-closed: an empty set opens nothing, every app included, because
    // every app is a resource now. The table says what each asks for rather
    // than "requires none", which would be true and useless.
    clearEffectiveCapabilities();
    const hidden = hiddenSurfaces(OS_REGISTRY);
    expect(hidden.length).toBe(OS_REGISTRY.apps.length + OS_REGISTRY.widgets.length);
    expect(hidden.find((h) => h.label === "Users")?.requires).toBe("read on app:users");
    installSeededAccess("owner");
  });
});
