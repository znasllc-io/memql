import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { appById, sectionsForRole, settingsSectionProblem } from "../../src/system/registry";
import {
  DEFAULT_TRAINING_SETTINGS,
  LocalTrainingSettingsStore,
  TRAINING_SECTIONS,
  TRAINING_SECTION_IDS,
  sanitizeTrainingSettings,
} from "../../src/apps/training/settings";

// The app's own preferences, and the manifest they have to agree with.

describe("sanitizeTrainingSettings", () => {
  it("repairs each field INDEPENDENTLY", () => {
    // A garbage `defaultSection` must not cost somebody their auto-open
    // choice: rejecting the document on the first bad value throws away
    // preferences that were fine.
    const repaired = sanitizeTrainingSettings({
      version: 1,
      defaultSection: "nowhere",
      autoOpenReview: true,
    });
    expect(repaired.defaultSection).toBe(DEFAULT_TRAINING_SETTINGS.defaultSection);
    expect(repaired.autoOpenReview).toBe(true);
  });

  it("discards a document of an unknown VERSION wholesale", () => {
    // Then the field names cannot be trusted at all.
    expect(sanitizeTrainingSettings({ version: 2, autoOpenReview: true })).toEqual(
      DEFAULT_TRAINING_SETTINGS,
    );
  });

  it("falls back for a non-object, an array and null", () => {
    for (const raw of [null, undefined, 7, "x", []]) {
      expect(sanitizeTrainingSettings(raw)).toEqual(DEFAULT_TRAINING_SETTINGS);
    }
  });

  it("accepts every section the manifest declares", () => {
    for (const id of TRAINING_SECTION_IDS) {
      expect(sanitizeTrainingSettings({ version: 1, defaultSection: id }).defaultSection).toBe(id);
    }
  });

  it("keeps auto-open OFF by default", () => {
    // An analysis finishing is not a reason to move somebody mid-batch.
    expect(DEFAULT_TRAINING_SETTINGS.autoOpenReview).toBe(false);
  });
});

describe("LocalTrainingSettingsStore", () => {
  it("round-trips through storage", () => {
    const data = new Map<string, string>();
    const store = new LocalTrainingSettingsStore({
      getItem: (k) => data.get(k) ?? null,
      setItem: (k, v) => void data.set(k, v),
    });
    store.save({ version: 1, defaultSection: "review", autoOpenReview: true });
    expect(store.load()).toEqual({ version: 1, defaultSection: "review", autoOpenReview: true });
  });

  it("survives NO storage, unparseable JSON and a throwing setter", () => {
    // A private window and a full quota are normal cases, not failures.
    expect(new LocalTrainingSettingsStore(null).load()).toEqual(DEFAULT_TRAINING_SETTINGS);
    expect(
      new LocalTrainingSettingsStore({ getItem: () => "{not json", setItem: () => {} }).load(),
    ).toEqual(DEFAULT_TRAINING_SETTINGS);
    const throwing = new LocalTrainingSettingsStore({
      getItem: () => null,
      setItem: () => {
        throw new Error("quota");
      },
    });
    expect(() => throwing.save(DEFAULT_TRAINING_SETTINGS)).not.toThrow();
  });
});

describe("the Training manifest", () => {
  it("is the REAL app, not a stub", () => {
    const app = appById(OS_REGISTRY, "training");
    expect(app).toBeTruthy();
    expect(app?.component.name).toBe("TrainingApp");
  });

  it("declares the SAME sections the settings picker offers", () => {
    // A second literal is one that can disagree, and a preference naming a
    // section the manifest does not declare leaves the window on the first
    // section with the nav highlighting nothing.
    const app = appById(OS_REGISTRY, "training");
    expect(app?.sections).toBe(TRAINING_SECTIONS);
    expect(settingsSectionProblem(app!)).toBeNull();
  });

  it("gates the APP at writer and no section of its own above it", () => {
    // Every surface here reads or writes the same two populations, so there is
    // no line inside the app where the answer changes -- except the Logs
    // section (epic memql#4895), whose floor is the log store's and not this
    // app's to choose.
    const app = appById(OS_REGISTRY, "training");
    expect(app?.roles).toEqual({ min: "writer" });
    for (const section of app?.sections ?? []) {
      if (section.id === "logs") {
        expect(section.roles).toEqual({ min: "admin" });
        continue;
      }
      expect(section.roles).toBeUndefined();
    }
    // A writer therefore sees the four that are this app's, and a reader sees
    // the app not at all.
    expect(sectionsForRole(app!, "writer")).toHaveLength(4);
  });

  it("opens on Upload", () => {
    expect(TRAINING_SECTIONS[0]?.id).toBe("upload");
    expect(DEFAULT_TRAINING_SETTINGS.defaultSection).toBe("upload");
  });
});
