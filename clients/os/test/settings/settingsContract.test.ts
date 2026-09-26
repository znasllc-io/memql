import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { settingsSectionProblem, type OsAppManifest } from "../../src/system/registry";
import { rolesOpening } from "../seededAccess";

// The per-app settings contract (memql#4743). `settingsSection` is REQUIRED
// on every manifest -- the type enforces its presence, and this enforces
// that it points somewhere.

function fakeApp(over: Partial<OsAppManifest>): OsAppManifest {
  return {
    id: "test",
    name: "Test",
    icon: () => null,
    requires: "app:test",
    sections: [{ id: "main", name: "Main" }, { id: "logs", name: "Logs", requires: "app:test/logs" }],
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
      sections: [{ id: "admin", name: "Admin", requires: "app:test/admin" }],
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
  //
  // FIFTEEN since the AI redesign (epic memql#5153, D1/D2). "AI providers"
  // became four sections named for the questions they answer -- Doors, Levels,
  // Rules, Decisions -- and they sit together, in that order, because the
  // order is the order somebody learns them in: where a model comes from, how
  // much a call needs, who gets what, and what actually happened.
  //
  // THE FIRST OF THEM KEEPS THE ID `providers`. Doors is that section's
  // successor, and two things in another epic's tree reach for
  // `settings/providers` by name -- the core gate's inference stop and
  // MODULE_SETTINGS_SECTION. Renaming the id buys a nicer string and costs a
  // cross-epic edit to a screen an owner cannot dismiss; the id is not
  // user-visible and the NAME is.
  //
  // SIXTEEN since Access (epic memql#5289, task memql#5307), which sits after
  // Apps: the directory of what is installed, then who may open what.
  //
  // SEVENTEEN since Language (memql#5390), directly after Cluster and on its
  // roles: the MemQL line the cluster speaks is the same kind of engineering
  // fact as the versions Cluster lists.
  it("Settings itself declares its sections", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings?.sections?.map((s) => s.id)).toEqual([
      "about",
      "appearance",
      "ask",
      "apps",
      "access",
      "cluster",
      "language",
      "diagnostics",
      "benchmarks",
      "connections",
      "integrations",
      "providers",
      "levels",
      "rules",
      "decisions",
      "tokens",
      "keys",
      "logs",
    ]);
    expect(settings?.sections?.find((s) => s.id === "cluster")?.requires).toBe("app:settings/cluster");
    expect(rolesOpening("app:settings/cluster")).toEqual(["owner", "developer", "admin"]);
    // Language (memql#5390): the same three roles, and the same name the
    // languageStatus builtin declares -- so the engine refuses the read to
    // exactly the people the shell hides the section from.
    expect(settings?.sections?.find((s) => s.id === "language")?.requires).toBe("app:settings/language");
    expect(rolesOpening("app:settings/language")).toEqual(["owner", "developer", "admin"]);
    // Access (epic memql#5289): seeded on the roles the grant reads admit.
    expect(settings?.sections?.find((s) => s.id === "access")?.requires).toBe("app:settings/access");
    expect(rolesOpening("app:settings/access")).toEqual(["owner", "developer", "admin"]);
    // Benchmarks is a MINIMUM and matches Cluster: v1:bench:run and
    // v1:bench:sample declare @rowAuthz(clusterOwner), so a reader below the
    // floor would be shown an empty section with no explanation. The gate here
    // is presentation over one the engine already holds.
    expect(settings?.sections?.find((s) => s.id === "benchmarks")?.requires).toBe("app:settings/benchmarks");
    expect(rolesOpening("app:settings/benchmarks")).toEqual(["owner", "developer", "admin"]);
    // THE TWO GATE FORMS ARE PINNED SEPARATELY, because they are different
    // statements and the difference is the point. Cluster is a ladder MINIMUM
    // (admin and above). Integrations is a SET, and it deliberately excludes
    // admin -- a `{ min: "developer" }` here would admit admin and quietly
    // widen a gate the program decided on (P6). A test that only checked
    // "developer can reach it" would pass against exactly that mistake.
    expect(settings?.sections?.find((s) => s.id === "integrations")?.requires).toBe("app:settings/integrations");
    expect(rolesOpening("app:settings/integrations")).toEqual(["owner", "developer"]);
    // AI providers is the SECOND set in this app (epic memql#5088, D7), and it
    // is pinned separately from Integrations for the reason the note above
    // gives: the two gate forms are different statements. It was
    // `{ min: "owner" }` while the four provider builtins were owner-only;
    // they now admit owner-or-developer, because a developer helps an owner
    // through setup, and admin stays out because an admin's concern is user
    // administration. Rounding this to `{ min: "developer" }` would admit
    // admin -- the ladder ranks developer 300 above admin 200 -- and offer
    // them a section of forms the engine refuses one by one.
    // Rules is the same set as Doors and for the same reason: writing a rule
    // is configuration, and an admin's concern is user administration.
    //
    // LEVELS AND DECISIONS ARE WIDER, not narrower, and the shape says so
    // (D7). `{ min: "admin" }` on this ladder admits admin (200), developer
    // (300) AND owner -- so a floor here is a SUPERSET of the owner-or-
    // developer set above. It is safe because a decision record carries
    // neither the prompt nor the error message: the engine's projection omits
    // both, so an admin answering "why did this go to a vendor" can have the
    // list without being shown anything they should not see.
    expect(settings?.sections?.find((s) => s.id === "rules")?.requires).toBe("app:settings/rules");
    expect(rolesOpening("app:settings/rules")).toEqual(["owner", "developer"]);
    expect(settings?.sections?.find((s) => s.id === "levels")?.requires).toBe("app:settings/levels");
    expect(rolesOpening("app:settings/levels")).toEqual(["owner", "developer", "admin"]);
    expect(settings?.sections?.find((s) => s.id === "decisions")?.requires).toBe("app:settings/decisions");
    expect(rolesOpening("app:settings/decisions")).toEqual(["owner", "developer", "admin"]);
    expect(settings?.sections?.find((s) => s.id === "providers")?.requires).toBe("app:settings/providers");
    expect(rolesOpening("app:settings/providers")).toEqual(["owner", "developer"]);
    expect(settings?.sections?.find((s) => s.id === "tokens")?.requires).toBe("app:settings/tokens");
    expect(rolesOpening("app:settings/tokens")).toEqual(["owner", "developer", "admin"]);
    expect(settings?.sections?.find((s) => s.id === "keys")?.requires).toBe("app:settings/keys");
    expect(rolesOpening("app:settings/keys")).toEqual(["owner", "developer", "admin"]);
  });
});
