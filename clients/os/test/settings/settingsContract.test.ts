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
    sections: [{ id: "main", name: "Main" }, { id: "logs", name: "Logs", roles: { min: "admin" } }],
    settingsSection: "main",
    logsSection: "logs",
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
  // Eight since Logs (epic memql#4895): every app carries a Logs section,
  // and this app has no settings section to put it before, so it is last.
  // Nine since Benchmarks (epic memql#4993), which sits BESIDE Diagnostics
  // rather than inside it: Diagnostics is three panels about this session,
  // and folding a fact about the deployment across releases into it would
  // change what its "copy diagnostics" button means.
  // Twelve since the portal was retired (epic memql#4984): AI providers,
  // Tokens and Keys are the operator capabilities that had no other home, and
  // they sit between Integrations and Logs -- beside the other section that
  // configures the cluster rather than describes it.
  it("Settings itself declares its sections", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings?.sections?.map((s) => s.id)).toEqual([
      "about",
      "appearance",
      "ask",
      "apps",
      "cluster",
      "diagnostics",
      "benchmarks",
      "integrations",
      "providers",
      "tokens",
      "keys",
      "logs",
    ]);
    expect(settings?.sections?.find((s) => s.id === "cluster")?.roles).toEqual({ min: "admin" });
    // Benchmarks is a MINIMUM and matches Cluster: v1:bench:run and
    // v1:bench:sample declare @rowAuthz(clusterOwner), so a reader below the
    // floor would be shown an empty section with no explanation. The gate here
    // is presentation over one the engine already holds.
    expect(settings?.sections?.find((s) => s.id === "benchmarks")?.roles).toEqual({ min: "admin" });
    // THE TWO GATE FORMS ARE PINNED SEPARATELY, because they are different
    // statements and the difference is the point. Cluster is a ladder MINIMUM
    // (admin and above). Integrations is a SET, and it deliberately excludes
    // admin -- a `{ min: "developer" }` here would admit admin and quietly
    // widen a gate the program decided on (P6). A test that only checked
    // "developer can reach it" would pass against exactly that mistake.
    expect(settings?.sections?.find((s) => s.id === "integrations")?.roles).toEqual({
      any: ["owner", "developer"],
    });
    // AI providers is the registry's FIRST floor above admin, and pinning it
    // is what keeps somebody from rounding it down. `providerAuthStatus` and
    // both provider writes are owner-gated in the engine, so `{ min: "admin" }`
    // here would offer an admin a section whose every control refuses -- the
    // exact failure the P6 note above describes in the other direction.
    expect(settings?.sections?.find((s) => s.id === "providers")?.roles).toEqual({
      min: "owner",
    });
    expect(settings?.sections?.find((s) => s.id === "tokens")?.roles).toEqual({ min: "admin" });
    expect(settings?.sections?.find((s) => s.id === "keys")?.roles).toEqual({ min: "admin" });
  });
});
