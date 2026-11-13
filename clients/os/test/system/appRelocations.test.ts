import { afterEach, describe, expect, it } from "vitest";
import { OS_REGISTRY } from "../../src/apps/registry";
import { appDestination, relocatedSectionSource, SECTION_RELOCATIONS } from "../../src/system/appDestinations";
import { appsFor, listedAppsFor, canOpen, sectionsFor } from "../../src/system/registry";
import { navigationTarget } from "../../src/system/navigation";
import { clearEffectiveCapabilities, setEffectiveCapabilities } from "../../src/system/roles";
import { installSeededAccess } from "../seededAccess";

const allow = (...resources: string[]) => setEffectiveCapabilities(resources.map(resource => ({ verb: "read", resource, effect: "allow", source: "user" })));
const sections = (app: string) => sectionsFor(OS_REGISTRY.apps.find(item => item.id === app)!).map(item => item.id);
afterEach(clearEffectiveCapabilities);

describe("approved app relocations preserve existing authority", () => {
  for (const move of SECTION_RELOCATIONS) it(`${move.fromApp}/${move.fromSection} retains its original grant conjunction`, () => {
    allow(`app:${move.fromApp}`, move.resource);
    expect(appDestination(move.fromApp, move.fromSection)).toEqual({ appId: move.toApp, sectionId: move.toSection });
    expect(canOpen(OS_REGISTRY, move.toApp)).toBe(true);
    expect(sections(move.toApp)).toContain(move.toSection);
    expect(navigationTarget(OS_REGISTRY, move.fromApp, { section: move.fromSection })?.section).toBe(move.toSection);
    if (move.resource !== `app:${move.fromApp}`) {
      allow(move.resource);
      expect(sections(move.toApp)).not.toContain(move.toSection);
      expect(navigationTarget(OS_REGISTRY, move.fromApp, { section: move.fromSection })).toBeNull();
    }
    allow(`app:${move.toApp}`);
    expect(sections(move.toApp)).not.toContain(move.toSection);
  });

  it("a Language custom grant does not unlock the Concepts registry or its settings", () => {
    allow("app:settings", "app:settings/language");
    expect(sections("concepts")).toEqual(["language"]);
    expect(navigationTarget(OS_REGISTRY, "concepts", { section: "registry" })).toBeNull();
    expect(navigationTarget(OS_REGISTRY, "concepts", {})).toEqual({ app: "concepts", section: "language", payload: {} });
  });

  it("neither an unknown destination nor the wrong resource inherits a moved grant", () => {
    expect(relocatedSectionSource("profile", "service-tokens", "app:settings/tokens")).toBeUndefined();
    expect(relocatedSectionSource("identity", "tokens", "app:settings/tokens")).toBeUndefined();
    expect(relocatedSectionSource("identity", "service-tokens", "app:settings/keys")).toBeUndefined();
  });

  for (const role of ["owner", "developer", "admin", "user", "viewer"]) it(`${role} reaches Profile from the dedicated entry, never the app list`, () => {
    installSeededAccess(role);
    expect(canOpen(OS_REGISTRY, "profile")).toBe(true);
    expect(appsFor(OS_REGISTRY).some(app => app.id === "profile")).toBe(true);
    expect(listedAppsFor(OS_REGISTRY).some(app => app.id === "profile")).toBe(false);
    expect(navigationTarget(OS_REGISTRY, "identity", { section: "profile" })?.app).toBe("profile");
    expect(navigationTarget(OS_REGISTRY, "settings", { section: "appearance" })?.app).toBe("profile");
    if (role === "user" || role === "viewer") {
      expect(canOpen(OS_REGISTRY, "identity")).toBe(false);
      expect(canOpen(OS_REGISTRY, "settings")).toBe(false);
    }
  });

  it("preserves older custom personal grants without broadening their surfaces", () => {
    allow("app:identity");
    expect(sections("profile")).toEqual(["profile", "devices", "tokens", "settings"]);
    expect(navigationTarget(OS_REGISTRY, "identity", { section: "tokens" })?.app).toBe("profile");
    expect(navigationTarget(OS_REGISTRY, "settings", { section: "appearance" })).toBeNull();
    allow("app:settings");
    expect(sections("profile")).toEqual(["appearance", "ask", "apps", "about", "diagnostics"]);
    expect(navigationTarget(OS_REGISTRY, "settings", { section: "appearance" })?.app).toBe("profile");
    expect(navigationTarget(OS_REGISTRY, "identity", { section: "tokens" })).toBeNull();
    expect(navigationTarget(OS_REGISTRY, "profile", { section: "logs" })).toBeNull();
    setEffectiveCapabilities([
      { verb: "read", resource: "app:identity", effect: "allow", source: "user" },
      { verb: "read", resource: "app:profile", effect: "deny", source: "group" },
    ]);
    expect(canOpen(OS_REGISTRY, "profile")).toBe(false);
    expect(navigationTarget(OS_REGISTRY, "identity", { section: "profile" })).toBeNull();
  });

  it("retires the unusable account-credential UI without redirecting to personal tokens", () => {
    installSeededAccess("owner");
    expect(sections("accounts")).not.toContain("credentials");
    expect(appDestination("accounts", "credentials")).toEqual({ appId: "accounts", sectionId: "accounts" });
    expect(appDestination("identity", "tokens")).toEqual({ appId: "profile", sectionId: "tokens" });
    expect(appDestination("settings", "tokens")).toEqual({ appId: "identity", sectionId: "service-tokens" });
  });
});
