import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { READINESS_MODULES, isModuleId } from "../../src/system/modules";
import { readinessProblem, requirementsFor, type OsAppManifest } from "../../src/system/registry";

// The readiness contract (design record 2026-09-06-configuration-readiness,
// section 5.1): every `requires` and `wants` id names a module the engine
// declares. The list lives in src/system/modules.ts and a Go gate pins it to
// the env manifest, so a typo here fails the build on both sides.

function fakeApp(over: Partial<OsAppManifest>): OsAppManifest {
  return {
    id: "test",
    name: "Test",
    icon: () => null,
    requires: "app:test",
    sections: [
      { id: "main", name: "Main" },
      { id: "logs", name: "Logs", requires: "app:test/logs" },
      { id: "settings", name: "Settings" },
    ],
    settingsSection: "settings",
    logsSection: "logs",
    component: () => null,
    ...over,
  };
}

describe("the readiness contract", () => {
  it("every shipped requirement names a declared module", () => {
    const problems = OS_REGISTRY.apps.map(readinessProblem).filter((p) => p !== null);
    expect(problems).toEqual([]);
    expect(READINESS_MODULES.length).toBeGreaterThan(0);
  });

  // The reachable positive for the case above. Without it, an empty sweep --
  // no app declaring anything at all -- passes with the same green.
  it("at least one shipped app declares a requirement, so the sweep examined something", () => {
    const declared = OS_REGISTRY.apps.filter(
      (a) =>
        (a.needs?.length ?? 0) > 0 ||
        (a.wants?.length ?? 0) > 0 ||
        (a.sections ?? []).some((s) => (s.needs?.length ?? 0) > 0 || (s.wants?.length ?? 0) > 0),
    );
    expect(declared.length).toBeGreaterThan(0);
  });

  it("fails an app naming an unknown module", () => {
    expect(readinessProblem(fakeApp({ needs: ["storagee"] as never }))).toMatch(/storagee/);
    expect(
      readinessProblem(
        fakeApp({
          sections: [{ id: "main", name: "Main", wants: ["nope"] as never }],
          settingsSection: "main",
          logsSection: "main",
        }),
      ),
    ).toMatch(/nope/);
  });

  it("fails an app that needs a module on its settings or logs section", () => {
    const app = fakeApp({
      sections: [
        { id: "settings", name: "Settings", needs: ["storage"] },
        { id: "logs", name: "Logs", requires: "app:test/logs" },
      ],
    });
    expect(readinessProblem(app)).toMatch(/settings/);
  });

  it("folds app-level and section-level requirements for a section", () => {
    const app = fakeApp({
      needs: ["email"],
      sections: [
        { id: "main", name: "Main", needs: ["storage"], wants: ["ai"] },
        { id: "logs", name: "Logs", requires: "app:test/logs" },
        { id: "settings", name: "Settings" },
      ],
    });
    expect(requirementsFor(app, "main")).toEqual({ requires: ["email", "storage"], wants: ["ai"] });
    expect(requirementsFor(app, "settings")).toEqual({ requires: [], wants: [] });
    expect(requirementsFor(app, "logs")).toEqual({ requires: [], wants: [] });
  });

  it("does not repeat a module named by both the app and the section", () => {
    const app = fakeApp({
      needs: ["ai"],
      sections: [
        { id: "main", name: "Main", needs: ["ai"] },
        { id: "logs", name: "Logs", requires: "app:test/logs" },
        { id: "settings", name: "Settings" },
      ],
    });
    expect(requirementsFor(app, "main").requires).toEqual(["ai"]);
  });

  it("isModuleId is a real predicate", () => {
    expect(isModuleId("storage")).toBe(true);
    expect(isModuleId("Storage")).toBe(false);
    expect(isModuleId("")).toBe(false);
  });
});
