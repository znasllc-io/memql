import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  OsConnectionProvider: ({ children }: { children: unknown }) => children,
  osBridgePath: "/_memql/ws",
  bridgePathFor: () => "/_memql/ws",
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { OS_REGISTRY } from "../../src/apps/registry";
import { appsFor } from "../../src/system/registry";
import { setEffectiveCapabilities, type OrganizationCapability } from "../../src/system/roles";
import { installSeededAccess, seededAccessWithout } from "../seededAccess";
import { fakeConnection, siteRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

// A MISSING PART HIDES ITS CONTROL (epic memql#5289, task memql#5305; design
// section 4 "A missing part"). The pure half is acts.test.ts; this is the
// RENDERED half the task's acceptance names: with `publish` withheld, Go live
// is absent while the deploy acts remain, and with the app's door withheld
// the app is absent from the desktop. Both are read from the effective set
// installed for the session -- a developer's seeded set minus one resource --
// which is what a deny by name resolves to.

const SHOP = siteRow({ id: "site-shop", hostname: "shop.memql.example.com", status: "live" });
const BUILT = siteRow({ id: "site-new", hostname: "new.memql.example.com", status: "draft" });

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection, capabilities: ReturnType<typeof seededAccessWithout>, organizationEntries: OrganizationCapability[] = []) {
  h.connection = connection;
  const content = withSession(
      <DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />,
      { role: organizationEntries.length ? "writer" : "developer", userId: "u-me", capabilities },
    );
  setEffectiveCapabilities(capabilities, organizationEntries);
  return render(content);
}

async function open(hostname: string): Promise<HTMLElement> {
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
  const row = (await screen.findByText(hostname)).closest("button");
  row?.click();
  return screen.findByRole("region", { name: `Deployable ${hostname}` });
}

function barActs(): string[] {
  const bar = document.querySelector(".os-actbar") as HTMLElement | null;
  if (bar === null) return [];
  return within(bar)
    .queryAllByRole("button")
    .map((b) => (b.textContent ?? "").trim())
    .filter((t) => t !== "");
}

beforeEach(() => {
  h.connection = null;
});

afterEach(() => {
  installSeededAccess("owner");
});

describe("a missing part hides its control", () => {
  it("with publish withheld, a live deployable offers Redeploy and no Take offline", async () => {
    const seed: FakeSeed = { sites: [SHOP] };
    mount(fakeConnection(seed), seededAccessWithout("developer", "app:deployables/publish"));
    await open("shop.memql.example.com");
    await waitFor(() => expect(barActs()).toEqual(["Deploy"]));
    expect(screen.queryByRole("button", { name: "Take offline" })).toBeNull();
  });

  it("with publish withheld, a built draft offers Discard and no Go live", async () => {
    const seed: FakeSeed = { sites: [BUILT] };
    mount(fakeConnection(seed), seededAccessWithout("developer", "app:deployables/publish"));
    await open("new.memql.example.com");
    await waitFor(() => expect(barActs()).toEqual(["Discard"]));
    expect(screen.queryByRole("button", { name: "Go live" })).toBeNull();
  });

  it("with every part held, the same draft offers both", async () => {
    const seed: FakeSeed = { sites: [BUILT] };
    mount(fakeConnection(seed), seededAccessWithout("developer"));
    await open("new.memql.example.com");
    await waitFor(() => expect(barActs()).toEqual(["Discard", "Go live"]));
  });

  it("with deploy withheld, Add a deployable is absent from the list", async () => {
    const seed: FakeSeed = { sites: [SHOP] };
    mount(fakeConnection(seed), seededAccessWithout("developer", "app:deployables/deploy"));
    await waitFor(() =>
      expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
    );
    expect(screen.queryByRole("button", { name: /Add a deployable/ })).toBeNull();
  });
});

describe("a missing door hides the app", () => {
  it("with app:deployables withheld, Deployables is absent from the desktop", () => {
    setEffectiveCapabilities(seededAccessWithout("developer", "app:deployables"));
    expect(appsFor(OS_REGISTRY).map((a) => a.id)).not.toContain("deployables");
    // ...and the parts alone open nothing: the door is `read` on the app.
    expect(appsFor(OS_REGISTRY).map((a) => a.id)).toContain("files");
    setEffectiveCapabilities(seededAccessWithout("developer"));
    expect(appsFor(OS_REGISTRY).map((a) => a.id)).toContain("deployables");
  });
});


describe("organization-specific deployable controls", () => {
  it("shows Acme's publish action despite Beta's deny and never offers it on Beta's site", async () => {
    const organizationEntries: OrganizationCapability[] = ["acme", "beta"].flatMap(accountId => [
      ["read", "app:deployables"], ["read", "data"], ["update", "data"], ["execute", "app:deployables/publish"],
    ].map(([verb, resource]) => ({ accountId, verb: verb!, resource: resource!, effect: accountId === "acme" ? "allow" : "deny" })));
    const seed: FakeSeed = { sites: [siteRow({ ...BUILT, id: "site-new", accountId: "acme" })] };
    const globalDenied = seededAccessWithout("developer", "app:deployables/publish");
    const first = mount(fakeConnection(seed), globalDenied, organizationEntries);
    await open("new.memql.example.com");
    await waitFor(() => expect(barActs()).toContain("Go live"));
    first.unmount();
    mount(fakeConnection({ sites: [siteRow({ ...BUILT, id: "site-new", accountId: "beta" })] }), globalDenied, organizationEntries);
    await open("new.memql.example.com");
    expect(screen.queryByRole("button", { name: "Go live" })).toBeNull();
  });
});
