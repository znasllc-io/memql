import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import {
  appsForRole,
  logsSectionProblem,
  sectionsForRole,
  type OsAppManifest,
} from "../../src/system/registry";
import { roleAdmits } from "../../src/system/roles";

// The per-app logs contract (epic memql#4895, spec H "The convention"):
// `logsSection` is REQUIRED on every manifest -- the type enforces its
// presence -- and this enforces that it points at a declared section floored
// at admin, mirroring `settingsContract`.

function fakeApp(over: Partial<OsAppManifest>): OsAppManifest {
  return {
    id: "test",
    name: "Test",
    icon: () => null,
    sections: [
      { id: "main", name: "Main" },
      { id: "logs", name: "Logs", roles: { min: "admin" } },
    ],
    settingsSection: "main",
    logsSection: "logs",
    component: () => null,
    ...over,
  };
}

describe("the logs-section contract over the real registry", () => {
  it("every shipped app declares a logsSection naming a declared, admin-floored section", () => {
    const problems = OS_REGISTRY.apps.map(logsSectionProblem).filter((p) => p !== null);
    expect(problems).toEqual([]);
    // Vacuous over an empty registry, so pin that the sweep examined apps.
    expect(OS_REGISTRY.apps.length).toBeGreaterThan(0);
  });

  it("the role pins: admin, developer and owner see the section; writer and reader do not", () => {
    for (const app of OS_REGISTRY.apps) {
      for (const role of ["admin", "developer", "owner"]) {
        expect(sectionsForRole(app, role).map((s) => s.id), `${app.id} for ${role}`).toContain(app.logsSection);
      }
      for (const role of ["writer", "reader"]) {
        // `sectionsForRole` filters on SECTION floors only; an app floored
        // above the role never renders a nav at all, and that is the answer
        // for it (the Logs app is the one, and its floor is pinned below).
        if (!roleAdmits(role, app.roles)) {
          expect(appsForRole(OS_REGISTRY, role).map((a) => a.id)).not.toContain(app.id);
          continue;
        }
        expect(sectionsForRole(app, role).map((s) => s.id), `${app.id} for ${role}`).not.toContain(
          app.logsSection,
        );
      }
    }
  });

  it("sits immediately before the settings section in every app that has one", () => {
    let examined = 0;
    for (const app of OS_REGISTRY.apps) {
      // The Logs app names its Stream: the whole app IS logs, and its own
      // settings section follows Search, not a Logs section.
      if (app.id === "logs") continue;
      const ids = (app.sections ?? []).map((s) => s.id);
      const at = ids.indexOf("settings");
      if (at < 0) continue;
      examined += 1;
      expect(ids[at - 1], app.id).toBe("logs");
    }
    expect(examined).toBeGreaterThan(0);
  });

  it("the Logs app is admin-floored, and offered to exactly the rungs at or above it", () => {
    const logs = OS_REGISTRY.apps.find((a) => a.id === "logs");
    expect(logs?.roles).toEqual({ min: "admin" });
    expect(logs?.logsSection).toBe("stream");
    expect(logs?.settingsSection).toBe("settings");
    expect(logs?.sections?.map((s) => s.id)).toEqual(["stream", "search", "settings"]);
    for (const role of ["admin", "developer", "owner"]) {
      expect(appsForRole(OS_REGISTRY, role).map((a) => a.id)).toContain("logs");
    }
    for (const role of ["writer", "reader", ""]) {
      expect(appsForRole(OS_REGISTRY, role).map((a) => a.id)).not.toContain("logs");
    }
  });
});

describe("the checker can actually fail", () => {
  it("fails an app whose logsSection names no declared section", () => {
    expect(logsSectionProblem(fakeApp({ logsSection: "nowhere" }))).toMatch(/names no declared section/);
  });

  it("fails an empty logsSection", () => {
    expect(logsSectionProblem(fakeApp({ logsSection: "  " }))).toMatch(/is empty/);
  });

  it("fails a section that is not floored at admin on either the section or the app", () => {
    expect(logsSectionProblem(fakeApp({ sections: [{ id: "logs", name: "Logs" }] }))).toMatch(/not floored at admin/);
    expect(
      logsSectionProblem(fakeApp({ roles: { min: "writer" }, sections: [{ id: "logs", name: "Logs" }] })),
    ).toMatch(/not floored at admin/);
    // A set that reaches below the floor is a floor that admits below it.
    expect(
      logsSectionProblem(fakeApp({ sections: [{ id: "logs", name: "Logs", roles: { any: ["writer", "owner"] } }] })),
    ).toMatch(/not floored at admin/);
  });

  it("accepts the floor on the app, and a floor above admin", () => {
    // The Logs app's own shape: the app carries the floor and names a plain section.
    expect(
      logsSectionProblem(
        fakeApp({
          roles: { min: "admin" },
          sections: [{ id: "stream", name: "Stream" }],
          settingsSection: "stream",
          logsSection: "stream",
        }),
      ),
    ).toBeNull();
    expect(
      logsSectionProblem(fakeApp({ sections: [{ id: "logs", name: "Logs", roles: { min: "owner" } }] })),
    ).toBeNull();
    expect(
      logsSectionProblem(fakeApp({ sections: [{ id: "logs", name: "Logs", roles: { any: ["owner", "developer"] } }] })),
    ).toBeNull();
  });
});
