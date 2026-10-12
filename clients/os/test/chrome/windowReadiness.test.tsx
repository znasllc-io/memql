import { cleanup, render, screen } from "@testing-library/react";
import { Layers } from "lucide-react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { AskProvider } from "../../src/ask/AskProvider";
import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { WindowFrame } from "../../src/chrome/WindowFrame";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import type { Readiness } from "../../src/live/readiness";
import type { ModuleId } from "../../src/system/modules";
import { resetIdsForTest } from "../../src/system/desks";
import type { Verdict } from "../../src/system/readinessFold";
import type { OsAppManifest, OsRegistry } from "../../src/system/registry";
import { LocalDesktopStore } from "../../src/system/store";
import { newWindow } from "../../src/system/windows";
import { StubAskTransport } from "../ask/stubTransport";
import { installSeededAccess } from "../seededAccess";

// WHAT A WINDOW NAMES WHEN A MODULE IT NEEDS IS NOT SET UP (memql#6033).
//
// A module the SECTION alone asked for names the section ("Rules is not set up
// yet"); one the APP asked for names the app. The frame decides it from the
// manifest's `needs`. It once read `manifest.requires` -- the app's access
// RESOURCE, a string -- as the module list, and `string.includes` answered true
// for any module spelled inside the resource, so the Email app's own `email`
// module named the whole app for a gate only one section asked for.
//
// A TEST MANIFEST, deliberately: the real apps' needs change with the engine
// work beside them, and this is about the frame. The manifest borrows a real
// resource, `app:email`, so the effective set admits it -- and that resource
// spells the module `email`, which is the trap.

const DEMO_HOME = () => <p>Demo body</p>;

function manifest(over: Partial<OsAppManifest>): OsAppManifest {
  return {
    id: "demo",
    name: "Demo",
    icon: Layers,
    requires: "app:email",
    settingsSection: "settings",
    logsSection: "logs",
    component: DEMO_HOME,
    sections: [
      { id: "home", name: "Home" },
      { id: "rules", name: "Rules" },
      { id: "settings", name: "Settings" },
      { id: "logs", name: "Logs", requires: "app:email/logs" },
    ],
    ...over,
  };
}

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function verdict(module: ModuleId): Verdict {
  return { module, state: "unconfigured", core: false, optional: false, dismissable: false, disagreement: [], nodes: [], lanes: [], unknown: [], stale: [], aside: [] };
}

/** A feed that has landed and says these modules are not configured. */
function unconfigured(...modules: ModuleId[]): Readiness {
  const by = new Map(modules.map((id) => [id, verdict(id)]));
  return { loaded: true, state: "live", of: (id) => by.get(id) ?? null, reseed: () => {} };
}

function mount(app: OsAppManifest, sectionId: string, readiness: Readiness) {
  const registry: OsRegistry = { apps: [app], widgets: [] };
  return render(
    <SessionProvider
      value={{
        access: { userId: "u-me", primaryEmail: "owner@example.com", role: "owner", roleName: "", rank: 0 },
        config: UNKNOWN_RUNTIME_CONFIG,
        ladderLoaded: true,
        readiness,
      }}
    >
      <OsProvider registry={registry} actorRole="owner" grid={{ cols: 12, rows: 8 }} store={new LocalDesktopStore(memStorage())}>
        <AskProvider transport={new StubAskTransport()}>
          <WindowFrame win={newWindow("w1", app.id, sectionId)} manifest={app} rect={{ x: 0, y: 0, w: 640, h: 420 }} focused actorRole="owner" />
        </AskProvider>
      </OsProvider>
    </SessionProvider>,
  );
}

beforeEach(() => {
  resetIdsForTest();
  installSeededAccess("owner");
});
afterEach(cleanup);

describe("the headline of a window whose module is not set up", () => {
  it("names the section when only the section asked for the module", () => {
    // `email` is spelled inside the app's own resource, "app:email": the module
    // list is `needs`, so the app asked for nothing and the section did.
    mount(manifest({ sections: [{ id: "home", name: "Home" }, { id: "rules", name: "Rules", needs: ["email"] }, { id: "settings", name: "Settings" }, { id: "logs", name: "Logs", requires: "app:email/logs" }] }), "rules", unconfigured("email"));
    expect(screen.getByRole("heading", { name: "Rules is not set up yet" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Demo is not set up yet" })).toBeNull();
    expect(screen.queryByText("Demo body")).toBeNull();
  });

  it("names the app when the app asked for the module", () => {
    mount(manifest({ needs: ["storage"] }), "rules", unconfigured("storage"));
    expect(screen.getByRole("heading", { name: "Demo is not set up yet" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Rules is not set up yet" })).toBeNull();
  });

  it("names the app when the app and the section both asked, and the module is the app's", () => {
    mount(manifest({ needs: ["email"], sections: [{ id: "home", name: "Home" }, { id: "rules", name: "Rules", needs: ["email"] }, { id: "settings", name: "Settings" }, { id: "logs", name: "Logs", requires: "app:email/logs" }] }), "rules", unconfigured("email"));
    expect(screen.getByRole("heading", { name: "Demo is not set up yet" })).toBeTruthy();
  });

  it("names the section for the module only the section asked for, beside one the app asked for", () => {
    // Two modules unmet; one is the app's. A frame that named the app for ANY
    // app-level module would hide that the section wants one more.
    mount(manifest({ needs: ["storage"], sections: [{ id: "home", name: "Home" }, { id: "rules", name: "Rules", needs: ["email"] }, { id: "settings", name: "Settings" }, { id: "logs", name: "Logs", requires: "app:email/logs" }] }), "rules", unconfigured("storage", "email"));
    // `unmetIsSectionOnly` is true as soon as ONE unmet module is the section's.
    expect(screen.getByRole("heading", { name: "Rules is not set up yet" })).toBeTruthy();
  });

  it("draws the body where no module the section needs is unmet", () => {
    mount(manifest({ sections: [{ id: "home", name: "Home" }, { id: "rules", name: "Rules", needs: ["email"] }, { id: "settings", name: "Settings" }, { id: "logs", name: "Logs", requires: "app:email/logs" }] }), "home", unconfigured("email"));
    expect(screen.getByText("Demo body")).toBeTruthy();
    expect(screen.queryByText(/is not set up yet/)).toBeNull();
  });
});
