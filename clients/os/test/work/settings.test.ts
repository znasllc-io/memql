import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import {
  DEFAULT_WORK_SETTINGS,
  LocalWorkSettingsStore,
  WORK_SECTIONS,
  WORK_SECTION_IDS,
  sanitizeWorkSettings,
} from "../../src/apps/work/settings";

describe("the settings document", () => {
  it("repairs each field independently, so one bad value costs nothing else", () => {
    // A garbage `defaultSection` must not cost somebody their finished-runs
    // preference. A wrong VERSION is wholesale, because then the field names
    // cannot be trusted at all.
    expect(
      sanitizeWorkSettings({
        version: 1,
        defaultSection: "nowhere",
        showFinishedRuns: false,
      }),
    ).toEqual({
      version: 1,
      defaultSection: DEFAULT_WORK_SETTINGS.defaultSection,
      showFinishedRuns: false,
    });
    expect(sanitizeWorkSettings({ version: 2, showFinishedRuns: false })).toEqual(
      DEFAULT_WORK_SETTINGS,
    );
    expect(sanitizeWorkSettings(null)).toEqual(DEFAULT_WORK_SETTINGS);
    expect(sanitizeWorkSettings("nonsense")).toEqual(DEFAULT_WORK_SETTINGS);
  });

  it("survives a browser with no storage at all", () => {
    // A private window and a full quota are normal cases, not failures.
    const store = new LocalWorkSettingsStore(null);
    expect(store.load()).toEqual(DEFAULT_WORK_SETTINGS);
    expect(() => store.save(DEFAULT_WORK_SETTINGS)).not.toThrow();
  });

  it("survives unparseable JSON in the key", () => {
    const store = new LocalWorkSettingsStore({
      getItem: () => "{not json",
      setItem: () => {},
    });
    expect(store.load()).toEqual(DEFAULT_WORK_SETTINGS);
  });

  it("keeps its own key, so an app learning a checkbox cannot cost anyone their desks", () => {
    const written: string[] = [];
    new LocalWorkSettingsStore({
      getItem: () => null,
      setItem: (key: string) => void written.push(key),
    }).save(DEFAULT_WORK_SETTINGS);
    expect(written).toEqual(["memql-os-work-v1"]);
  });
});

describe("the section list", () => {
  it("is the SAME list the manifest declares -- a second copy is one that can disagree", () => {
    // A preference naming a section the manifest does not declare leaves the
    // window on the first section with the nav highlighting nothing.
    const manifest = OS_REGISTRY.apps.find((app) => app.id === "work");
    expect(manifest).toBeTruthy();
    expect(manifest?.sections).toBe(WORK_SECTIONS);
    expect(WORK_SECTION_IDS).toContain(DEFAULT_WORK_SETTINGS.defaultSection);
  });

  it("opens on Goals, and names that default rather than reading it off the array", () => {
    expect(WORK_SECTIONS[0]?.id).toBe("goals");
    expect(DEFAULT_WORK_SETTINGS.defaultSection).toBe("goals");
  });

  it("floors the Logs section at admin, which is the engine's floor and not this app's choice", () => {
    const logs = WORK_SECTIONS.find((section) => section.id === "logs");
    expect(logs?.roles).toEqual({ min: "admin" });
  });

  it("carries NO app-level role, because the concepts declare the composite tier", () => {
    // Gating here would be presentation pretending to be authorization: every
    // signed-in person has goals of their own and the engine decides how far
    // each list reaches.
    const manifest = OS_REGISTRY.apps.find((app) => app.id === "work");
    expect(manifest?.roles).toBeUndefined();
    expect(
      WORK_SECTIONS.filter((s) => s.id !== "logs").every((s) => s.roles === undefined),
    ).toBe(true);
  });
});
