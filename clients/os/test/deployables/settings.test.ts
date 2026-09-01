import { describe, expect, it } from "vitest";

import { settingsSectionProblem } from "../../src/system/registry";
import { OS_REGISTRY } from "../../src/apps/registry";
import {
  DEFAULT_DEPLOYABLES_SETTINGS,
  DEPLOYABLES_SECTIONS,
  LocalDeployablesSettingsStore,
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

  it("opens on the MAP", () => {
    // The signature surface, and the reason the epic exists: what serves where
    // is a shape rather than a table. The app's own settings can send somebody
    // to the list instead.
    expect(deployables?.sections?.[0]?.id).toBe("map");
  });

  it("gates Actions at admin and nothing else", () => {
    // The app itself admits every signed-in user, because the concept's
    // composite tier means everyone has deployables of their own to read.
    expect(deployables?.roles).toBeUndefined();
    const gated = (deployables?.sections ?? []).filter((s) => s.roles !== undefined);
    expect(gated.map((s) => s.id)).toEqual(["actions"]);
    // Whole-requirement equality rather than `?.min`: RoleRequirement is a
    // union since issue #4826 gave it a set form, and reading `.min` off it
    // no longer typechecks. The assertion is the same one and is stricter --
    // it would also catch this becoming a set that happens to contain admin.
    expect(gated[0]?.roles).toEqual({ min: "admin" });
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
    const doc = { version: 1, defaultSection: "sites", density: "compact" };
    expect(sanitizeDeployablesSettings(doc)).toEqual(doc);
  });

  it("repairs each field INDEPENDENTLY", () => {
    // A garbage section must not cost somebody their density choice.
    expect(sanitizeDeployablesSettings({ version: 1, defaultSection: "nope", density: "compact" }))
      .toEqual({ version: 1, defaultSection: "map", density: "compact" });
    expect(sanitizeDeployablesSettings({ version: 1, defaultSection: "sites", density: "huge" }))
      .toEqual({ version: 1, defaultSection: "sites", density: "comfortable" });
  });

  it("rejects a wrong version WHOLESALE -- the field names cannot be trusted", () => {
    expect(sanitizeDeployablesSettings({ version: 2, defaultSection: "sites", density: "compact" }))
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
    store.save({ version: 1, defaultSection: "sites", density: "compact" });
    expect(new LocalDeployablesSettingsStore(storage).load()).toEqual({
      version: 1,
      defaultSection: "sites",
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
