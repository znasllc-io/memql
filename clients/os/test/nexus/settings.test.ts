import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import {
  DEFAULT_NEXUS_SETTINGS,
  LocalNexusSettingsStore,
  NEXUS_SECTIONS,
  NEXUS_SECTION_IDS,
  sanitizeNexusSettings,
} from "../../src/apps/nexus/settings";

describe("the settings document", () => {
  it("repairs each field independently, so one bad value costs nothing else", () => {
    // A garbage `defaultSection` must not cost somebody their finished-runs
    // preference. A wrong VERSION is wholesale, because then the field names
    // cannot be trusted at all.
    expect(
      sanitizeNexusSettings({
        version: 1,
        defaultSection: "nowhere",
        showFinishedRuns: false,
      }),
    ).toEqual({
      version: 1,
      defaultSection: DEFAULT_NEXUS_SETTINGS.defaultSection,
      showFinishedRuns: false,
    });
    expect(sanitizeNexusSettings({ version: 2, showFinishedRuns: false })).toEqual(
      DEFAULT_NEXUS_SETTINGS,
    );
    expect(sanitizeNexusSettings(null)).toEqual(DEFAULT_NEXUS_SETTINGS);
    expect(sanitizeNexusSettings("nonsense")).toEqual(DEFAULT_NEXUS_SETTINGS);
  });

  it("survives a browser with no storage at all", () => {
    // A private window and a full quota are normal cases, not failures.
    const store = new LocalNexusSettingsStore(null);
    expect(store.load()).toEqual(DEFAULT_NEXUS_SETTINGS);
    expect(() => store.save(DEFAULT_NEXUS_SETTINGS)).not.toThrow();
  });

  it("survives unparseable JSON in the key", () => {
    const store = new LocalNexusSettingsStore({
      getItem: () => "{not json",
      setItem: () => {},
    });
    expect(store.load()).toEqual(DEFAULT_NEXUS_SETTINGS);
  });

  it("keeps its own key, so an app learning a checkbox cannot cost anyone their desks", () => {
    const written: string[] = [];
    new LocalNexusSettingsStore({
      getItem: () => null,
      setItem: (key: string) => void written.push(key),
    }).save(DEFAULT_NEXUS_SETTINGS);
    expect(written).toEqual(["memql-os-nexus-v1"]);
  });
});

describe("the section list", () => {
  it("is the SAME list the manifest declares -- a second copy is one that can disagree", () => {
    // A preference naming a section the manifest does not declare leaves the
    // window on the first section with the nav highlighting nothing.
    const manifest = OS_REGISTRY.apps.find((app) => app.id === "nexus");
    expect(manifest).toBeTruthy();
    expect(manifest?.sections).toBe(NEXUS_SECTIONS);
    expect(NEXUS_SECTION_IDS).toContain(DEFAULT_NEXUS_SETTINGS.defaultSection);
  });

  it("opens on Goals, and names that default rather than reading it off the array", () => {
    expect(NEXUS_SECTIONS[0]?.id).toBe("goals");
    // RUNS STAYS, and it is a decision (design record D3): `v1:work:run.goalId`
    // is EMPTY for an automation run no goal asked for, and in a goal-only app
    // those runs would have no home at all.
    expect(NEXUS_SECTIONS.map((section) => section.id)).toEqual([
      "goals",
      "runs",
      "automations",
      "approvals",
      "logs",
      "settings",
    ]);
    expect(DEFAULT_NEXUS_SETTINGS.defaultSection).toBe("goals");
  });

  it("floors the Logs section at admin, which is the engine's floor and not this app's choice", () => {
    const logs = NEXUS_SECTIONS.find((section) => section.id === "logs");
    expect(logs?.roles).toEqual({ min: "admin" });
  });

  it("carries NO app-level role, because the concepts declare the composite tier", () => {
    // Gating here would be presentation pretending to be authorization: every
    // signed-in person has goals of their own and the engine decides how far
    // each list reaches.
    const manifest = OS_REGISTRY.apps.find((app) => app.id === "nexus");
    expect(manifest?.roles).toBeUndefined();
    expect(
      NEXUS_SECTIONS.filter((s) => s.id !== "logs").every((s) => s.roles === undefined),
    ).toBe(true);
  });
});
