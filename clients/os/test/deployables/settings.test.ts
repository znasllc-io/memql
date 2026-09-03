import { describe, expect, it } from "vitest";

import { sectionsForRole, settingsSectionProblem } from "../../src/system/registry";
import { OS_REGISTRY } from "../../src/apps/registry";
import {
  DEFAULT_DEPLOYABLES_SETTINGS,
  DEPLOYABLES_SECTION_IDS,
  DEPLOYABLES_SECTIONS,
  LocalDeployablesSettingsStore,
  RETIRED_SECTIONS,
  sanitizeDeployablesSettings,
} from "../../src/apps/deployables/settings";

// The app's own preferences, and the manifest contract they have to agree with.

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

describe("the manifest", () => {
  const deployables = OS_REGISTRY.apps.find((a) => a.id === "deployables");

  it("is a real app rather than a stub", () => {
    expect(deployables).toBeTruthy();
    expect(deployables?.component.name).toBe("DeployablesApp");
  });

  it("declares exactly Map, Deployables, Logs and Settings, in that order (design D1)", () => {
    // Actions, Sites and Packages retired with the compose epic (memql#4885):
    // one list and one page replaced three sections and two mental models.
    // FOUR, not the three the compose restructure left: Logs is a shell
    // convention every app carries (epic memql#4895) and is not this app's to
    // drop. What retired is this app's own three readings of its subject --
    // Sites, Packages, Actions -- which became one.
    expect(DEPLOYABLES_SECTION_IDS).toEqual(["map", "deployables", "logs", "settings"]);
    expect(DEPLOYABLES_SECTIONS.map((s) => s.name)).toEqual(["Map", "Deployables", "Logs", "Settings"]);
    // The gated one is offered to an admin and withheld below, so the window
    // nav genuinely differs by role -- the assertion the three-section version
    // of this file could not make, because it had nothing gated.
    expect(sectionsForRole(deployables!, "reader").map((s) => s.id)).toEqual(["map", "deployables", "settings"]);
    expect(sectionsForRole(deployables!, "admin").map((s) => s.id)).toEqual(["map", "deployables", "logs", "settings"]);
  });

  it("opens on the MAP", () => {
    // The signature surface, and the reason the epic exists: what serves where
    // is a shape rather than a table. The app's own settings can send somebody
    // to the list instead.
    expect(deployables?.sections?.[0]?.id).toBe("map");
  });

  it("gates Logs at admin and nothing else", () => {
    // The app itself admits every signed-in user, because the concept's
    // composite tier means everyone has deployables of their own to read, and
    // the WRITE half is gated inside the section -- New deployable renders for
    // rank >= 200 -- exactly as Sites gated publishing rather than the list.
    //
    // Logs is the one gated section, and the compose restructure did not
    // choose that floor: every read on the log store is admin-and-above in the
    // engine (epic memql#4895, spec L3), so the section's floor is the store's.
    // Actions used to sit here too and retired with the three-section
    // restructure (epic memql#4885).
    expect(deployables?.roles).toBeUndefined();
    const gated = (deployables?.sections ?? []).filter((s) => s.roles !== undefined);
    expect(gated.map((s) => s.id)).toEqual(["logs"]);
    // Whole-requirement equality rather than `?.min`: RoleRequirement is a
    // union since issue #4826 gave it a set form, and reading `.min` off it
    // no longer typechecks. The assertion is the same one and is stricter --
    // it would also catch this becoming a set that happens to contain admin.
    for (const section of gated) {
      expect(section.roles).toEqual({ min: "admin" });
    }
  });

  it("declares the settings section its gear points at", () => {
    expect(settingsSectionProblem(deployables!)).toBeNull();
  });

  it("shares ONE section list with the settings picker", () => {
    // A second literal is one that can disagree, and a preference naming a
    // section the manifest does not declare leaves the window on the first
    // section with the nav highlighting nothing.
    expect(deployables?.sections).toBe(DEPLOYABLES_SECTIONS);
  });
});

describe("sanitizing a stored document", () => {
  it("keeps a good one", () => {
    const doc = { version: 1, defaultSection: "deployables", density: "compact" };
    expect(sanitizeDeployablesSettings(doc)).toEqual(doc);
  });

  it("maps a retired default -- sites, packages, actions -- to Deployables", () => {
    // A person's stored preference must not open a section that no longer
    // exists, and must not be silently reset to the map either: somebody who
    // asked for the list is still asking for the list.
    for (const retired of ["sites", "packages", "actions"]) {
      expect(sanitizeDeployablesSettings({ version: 1, defaultSection: retired, density: "compact" }))
        .toEqual({ version: 1, defaultSection: "deployables", density: "compact" });
    }
    // The map is exhaustive over the three, and names nothing that still exists.
    expect(Object.keys(RETIRED_SECTIONS).sort()).toEqual(["actions", "packages", "sites"]);
    for (const target of Object.values(RETIRED_SECTIONS)) expect(DEPLOYABLES_SECTION_IDS).toContain(target);
    for (const retired of Object.keys(RETIRED_SECTIONS)) expect(DEPLOYABLES_SECTION_IDS).not.toContain(retired);
  });

  it("repairs each field INDEPENDENTLY", () => {
    // A garbage section must not cost somebody their density choice.
    expect(sanitizeDeployablesSettings({ version: 1, defaultSection: "nope", density: "compact" }))
      .toEqual({ version: 1, defaultSection: "map", density: "compact" });
    expect(sanitizeDeployablesSettings({ version: 1, defaultSection: "deployables", density: "huge" }))
      .toEqual({ version: 1, defaultSection: "deployables", density: "comfortable" });
  });

  it("rejects a wrong version WHOLESALE -- the field names cannot be trusted", () => {
    expect(sanitizeDeployablesSettings({ version: 2, defaultSection: "deployables", density: "compact" }))
      .toEqual(DEFAULT_DEPLOYABLES_SETTINGS);
  });

  it("survives every shape a corrupt value can take", () => {
    for (const junk of [null, undefined, 7, "settings", [], { version: "1" }]) {
      expect(sanitizeDeployablesSettings(junk)).toEqual(DEFAULT_DEPLOYABLES_SETTINGS);
    }
  });
});

describe("the store", () => {
  it("round-trips", () => {
    const storage = memStorage();
    const store = new LocalDeployablesSettingsStore(storage);
    store.save({ version: 1, defaultSection: "deployables", density: "compact" });
    expect(new LocalDeployablesSettingsStore(storage).load()).toEqual({
      version: 1,
      defaultSection: "deployables",
      density: "compact",
    });
  });

  it("loads a document saved before the restructure onto the list", () => {
    // The mapping runs on LOAD, which is the only place a stored document is
    // read, so a window that opens tomorrow lands where the person meant.
    const storage = memStorage();
    storage.setItem(
      "memql-os-deployables-v1",
      JSON.stringify({ version: 1, defaultSection: "sites", density: "compact" }),
    );
    expect(new LocalDeployablesSettingsStore(storage).load()).toEqual({
      version: 1,
      defaultSection: "deployables",
      density: "compact",
    });
  });

  it("resets on unparseable JSON rather than throwing", () => {
    const storage = memStorage();
    storage.setItem("memql-os-deployables-v1", "{not json");
    expect(new LocalDeployablesSettingsStore(storage).load()).toEqual(DEFAULT_DEPLOYABLES_SETTINGS);
  });

  it("treats no storage as a normal case", () => {
    // A private window and a full quota are conditions, not failures.
    const store = new LocalDeployablesSettingsStore(null);
    expect(store.load()).toEqual(DEFAULT_DEPLOYABLES_SETTINGS);
    expect(() => store.save(DEFAULT_DEPLOYABLES_SETTINGS)).not.toThrow();
  });

  it("does not fail an interaction when the write is refused", () => {
    const store = new LocalDeployablesSettingsStore({
      getItem: () => null,
      setItem: () => {
        throw new Error("QuotaExceededError");
      },
    });
    expect(() => store.save(DEFAULT_DEPLOYABLES_SETTINGS)).not.toThrow();
  });
});
