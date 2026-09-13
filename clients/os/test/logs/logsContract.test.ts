import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { appsFor, logsSectionProblem, sectionsFor, type OsAppManifest } from "../../src/system/registry";
import { installSeededAccess, rolesOpening } from "../seededAccess";

// The per-app logs contract (epic memql#4895, spec H "The convention"):
// `logsSection` is REQUIRED on every manifest -- the type enforces its
// presence -- and this enforces that it points at a declared section that
// carries a resource of its own, `app:<id>/<section>`, mirroring
// `settingsContract`. Which roles hold that resource is the seeds' business
// (epic memql#5289), and the role pins below read them.

function fakeApp(over: Partial<OsAppManifest>): OsAppManifest {
  return {
    id: "test",
    name: "Test",
    icon: () => null,
    requires: "app:test",
    sections: [
      { id: "main", name: "Main" },
      { id: "logs", name: "Logs", requires: "app:test/logs" },
    ],
    settingsSection: "main",
    logsSection: "logs",
    component: () => null,
    ...over,
  };
}

describe("the logs-section contract over the real registry", () => {
  it("every shipped app declares a logsSection naming a declared section with its own resource", () => {
    const problems = OS_REGISTRY.apps.map(logsSectionProblem).filter((p) => p !== null);
    expect(problems).toEqual([]);
    // Vacuous over an empty registry, so pin that the sweep examined apps.
    expect(OS_REGISTRY.apps.length).toBeGreaterThan(0);
  });

  it("the role pins: admin, developer and owner see the section; writer and reader do not", () => {
    for (const app of OS_REGISTRY.apps) {
      const logs = (app.sections ?? []).find((s) => s.id === app.logsSection);
      // The seeds put every logs section on exactly the log store's floor.
      expect(rolesOpening(logs!.requires!), `${app.id}'s logs resource`).toEqual(["owner", "developer", "admin"]);
      for (const role of ["admin", "developer", "owner"]) {
        installSeededAccess(role);
        expect(sectionsFor(app).map((s) => s.id), `${app.id} for ${role}`).toContain(app.logsSection);
      }
      for (const role of ["writer", "reader"]) {
        installSeededAccess(role);
        // `sectionsFor` filters on SECTION resources only; an app whose door
        // the role does not hold never renders a nav at all, and that is the
        // answer for it (the Logs app is the one, and its door is pinned
        // below).
        if (!appsFor(OS_REGISTRY).some((a) => a.id === app.id)) continue;
        expect(sectionsFor(app).map((s) => s.id), `${app.id} for ${role}`).not.toContain(app.logsSection);
      }
    }
    installSeededAccess("owner");
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

  it("the Logs app's door is seeded at admin, and its Stream is its own logs section", () => {
    const logs = OS_REGISTRY.apps.find((a) => a.id === "logs");
    expect(logs?.requires).toBe("app:logs");
    expect(rolesOpening("app:logs")).toEqual(["owner", "developer", "admin"]);
    expect(logs?.logsSection).toBe("stream");
    expect(logs?.settingsSection).toBe("settings");
    expect(logs?.sections?.map((s) => s.id)).toEqual(["stream", "search", "settings"]);
    // ONE SHAPE FOR EVERY APP: the Stream carries its own resource like every
    // other logs section, seeded identically to the door, so the contract
    // reads the same for fourteen apps and this one.
    expect(logs?.sections?.find((s) => s.id === "stream")?.requires).toBe("app:logs/stream");
    for (const role of ["admin", "developer", "owner"]) {
      installSeededAccess(role);
      expect(appsFor(OS_REGISTRY).map((a) => a.id)).toContain("logs");
    }
    for (const role of ["writer", "reader", ""]) {
      installSeededAccess(role);
      expect(appsFor(OS_REGISTRY).map((a) => a.id)).not.toContain("logs");
    }
    installSeededAccess("owner");
  });
});

describe("the checker can actually fail", () => {
  it("fails an app whose logsSection names no declared section", () => {
    expect(logsSectionProblem(fakeApp({ logsSection: "nowhere" }))).toMatch(/names no declared section/);
  });

  it("fails an empty logsSection", () => {
    expect(logsSectionProblem(fakeApp({ logsSection: "  " }))).toMatch(/is empty/);
  });

  it("fails a logs section with no resource of its own, or the wrong one", () => {
    // Reached through the app's door alone, the section opens on the engine's
    // refusal for everyone the door admits below the log store's floor.
    expect(logsSectionProblem(fakeApp({ sections: [{ id: "logs", name: "Logs" }] }))).toMatch(
      /must carry requires: "app:test\/logs"/,
    );
    // A resource under another app, or the app's own door, is not this
    // section's.
    expect(
      logsSectionProblem(fakeApp({ sections: [{ id: "logs", name: "Logs", requires: "app:other/logs" }] })),
    ).toMatch(/must carry requires: "app:test\/logs"/);
    expect(
      logsSectionProblem(fakeApp({ sections: [{ id: "logs", name: "Logs", requires: "app:test" }] })),
    ).toMatch(/must carry requires: "app:test\/logs"/);
  });

  it("accepts a logs section that names its own resource, whatever the section is called", () => {
    // The Logs app's own shape: the Stream is the logs section.
    expect(
      logsSectionProblem(
        fakeApp({
          sections: [{ id: "stream", name: "Stream", requires: "app:test/stream" }],
          settingsSection: "stream",
          logsSection: "stream",
        }),
      ),
    ).toBeNull();
  });
});
