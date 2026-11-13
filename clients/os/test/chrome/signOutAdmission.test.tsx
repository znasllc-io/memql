import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { Shell } from "../../src/chrome/Shell";
import { resetIdsForTest } from "../../src/system/desks";
import { navigationTarget } from "../../src/system/navigation";
import { canOpen } from "../../src/system/registry";
import { setEffectiveCapabilities, type EffectiveCapability } from "../../src/system/roles";
import { LocalDesktopStore } from "../../src/system/store";
import { StubAskTransport } from "../ask/stubTransport";
import { installSeededAccess } from "../seededAccess";

afterEach(() => installSeededAccess("owner"));

const cases: [string, EffectiveCapability[]][] = [
  ["a custom role without personal app grants", []],
  ["an explicit Profile deny despite older personal grants", [
    { verb: "read", resource: "app:identity", effect: "allow", source: "user" },
    { verb: "read", resource: "app:settings", effect: "allow", source: "user" },
    { verb: "read", resource: "app:profile", effect: "deny", source: "group" },
  ]],
];

for (const layout of ["desktop", "phone"] as const) {
  for (const [name, capabilities] of cases) {
    it(`${layout} retains sign out for ${name} without opening Profile`, () => {
      resetIdsForTest();
      setEffectiveCapabilities(capabilities);
      const onSignOut = vi.fn();
      const storage = new Map<string, string>();
      render(<Shell layout={layout} onSignOut={onSignOut}
        access={{ userId: "custom-user", primaryEmail: "custom@example.test", role: "custom", roleName: "Custom", rank: 0 }}
        config={{ identityUrl: "https://identity.example.test", identityApiBaseUrl: "", oauthClientId: "client", authEnabled: true, domain: "example.test" }}
        ports={{ store: new LocalDesktopStore({ getItem: key => storage.get(key) ?? null, setItem: (key, value) => { storage.set(key, value); } }), disableConnection: true, askTransport: new StubAskTransport(), askVoice: null }} />);

      expect(canOpen(OS_REGISTRY, "profile")).toBe(false);
      expect(navigationTarget(OS_REGISTRY, "identity", { section: "profile" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Open Profile" })).toBeNull();
      const signOut = screen.getByRole("button", { name: "Sign out" });
      expect(signOut.closest(layout === "phone" ? ".os-phone-bar" : "[data-os-dock]")).not.toBeNull();
      expect(signOut.tabIndex).toBe(0);
      fireEvent.click(signOut);
      expect(onSignOut).toHaveBeenCalledOnce();
      expect(screen.queryByRole("dialog", { name: "Profile" })).toBeNull();
      expect(canOpen(OS_REGISTRY, "profile")).toBe(false);
    });
  }
}
