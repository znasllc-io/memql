import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { settingsSectionProblem, type OsAppManifest } from "../../src/system/registry";

// The per-app settings contract (memql#4743). `settingsSection` is REQUIRED
// on every manifest -- the type enforces its presence, and this enforces
// that it points somewhere.

function fakeApp(over: Partial<OsAppManifest>): OsAppManifest {
  return {
    id: "test",
    name: "Test",
    icon: () => null,
    sections: [{ id: "main", name: "Main" }],
    settingsSection: "main",
    component: () => null,
    ...over,
  };
}

describe("the settings-section contract", () => {
  it("every shipped app declares a settingsSection naming a declared section", () => {
    const problems = OS_REGISTRY.apps.map(settingsSectionProblem).filter((p) => p !== null);
    expect(problems).toEqual([]);
    // The assertion above is vacuous if the registry is empty, so pin that
    // the sweep actually examined the apps it claims to have examined.
    expect(OS_REGISTRY.apps.length).toBeGreaterThan(0);
  });

  it("every shipped app's gear target is one of its own sections", () => {
    for (const app of OS_REGISTRY.apps) {
      const ids = (app.sections ?? []).map((s) => s.id);
      expect(ids).toContain(app.settingsSection);
    }
  });

  it("fails an app whose settingsSection names no declared section", () => {
    // The negative control: without it, the sweep above proves only that
    // the checker returns null, not that it can ever return anything else.
    expect(settingsSectionProblem(fakeApp({ settingsSection: "nowhere" }))).toMatch(
      /names no declared section/,
    );
  });

  it("fails an app with an empty settingsSection", () => {
    expect(settingsSectionProblem(fakeApp({ settingsSection: "  " }))).toMatch(/is empty/);
  });

  it("fails an app that declares no sections at all", () => {
    expect(settingsSectionProblem(fakeApp({ sections: [], settingsSection: "main" }))).toMatch(
      /declared: none/,
    );
  });

  it("accepts a gear target gated above the viewer -- that is a role, not a defect", () => {
    // The check is over DECLARED sections. A window simply shows no gear to
    // a session that cannot reach the target; a target that exists for
    // nobody is the bug, and only that.
    const app = fakeApp({
      sections: [{ id: "admin", name: "Admin", roles: { min: "admin" } }],
      settingsSection: "admin",
    });
    expect(settingsSectionProblem(app)).toBeNull();
  });

  // Six since Ask voice (memql#4747) added its own section beside Appearance.
  // Ask is CHROME rather than an app, so its preferences have nowhere else to
  // live, and folding them into Appearance would file "hold Space to talk"
  // under how the desktop looks. Seven since Integrations (memql#4826) --
  // per-integration configuration lives in Settings rather than in the app
  // windows that consume it, because a credential is not a campaigns record.
  it("Settings itself declares its sections", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings?.sections?.map((s) => s.id)).toEqual([
      "about",
      "appearance",
      "ask",
      "apps",
      "cluster",
      "diagnostics",
      "integrations",
    ]);
    expect(settings?.sections?.find((s) => s.id === "cluster")?.roles).toEqual({ min: "admin" });
    // THE TWO GATE FORMS ARE PINNED SEPARATELY, because they are different
    // statements and the difference is the point. Cluster is a ladder MINIMUM
    // (admin and above). Integrations is a SET, and it deliberately excludes
    // admin -- a `{ min: "developer" }` here would admit admin and quietly
    // widen a gate the program decided on (P6). A test that only checked
    // "developer can reach it" would pass against exactly that mistake.
    expect(settings?.sections?.find((s) => s.id === "integrations")?.roles).toEqual({
      any: ["owner", "developer"],
    });
  });
});
