import { render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { readTheBar } from "../readTheBar";
const h = vi.hoisted(() => ({ connection: null as unknown, openApp: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/chrome/state", async importOriginal => ({ ...await importOriginal<typeof import("../../src/chrome/state")>(), useOs: () => ({ actions: { openApp: h.openApp } }), useOsIfPresent: () => null }));
import { ShopifyStorePanel } from "../../src/apps/deployables/store/ShopifyStorePanel";
import { ShopifyConnections } from "../../src/modules/connections/ShopifyConnections";
import { captureShopifyReturn, takeShopifyReturn } from "../../src/apps/deployables/store/connectReturn";
import { ALL_PARTS } from "../../src/apps/deployables/parts";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { builtinReply, click, fakeConnection, rowsResult, SHOP, type as typeInto, withSession, type FakeSeed } from "./harness";

const saved = { id: "connection", ownerUserId: "u-me", provider: "shopify", resourceId: "my-store", label: "my-store.myshopify.com", status: "active" };
function fixture(connections = [saved], configured = false, development = false, seed: FakeSeed = {}) {
  const connection = fakeConnection({ sites: [SHOP], stores: [{ id: "my-store", domain: saved.label, adminTokenRef: "admin", storefrontTokenRef: "public", isDevelopment: development, plan: "Basic" }], ...seed });
  let rows = [...connections];
  const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
  vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
    if (name === "externalConnectionsMine") return rowsResult(rows);
    if (name === "disconnectExternalConnection") { rows = []; return rowsResult([]); }
    if (name === "shopifyConnectionProviderStatus") return builtinReply(name, [{ reason: "ok", configured, installUrl: configured ? "https://apps.shopify.com/memql" : "" }]);
    return original(name, call, options);
  });
  h.connection = connection;
  return connection;
}
afterEach(() => { h.connection = null; h.openApp.mockClear(); history.replaceState(null, "", "/"); takeShopifyReturn(); });

describe("shared Shopify connections", () => {
  it("keeps an unconfigured storefront in Store and links to shared connection settings", async () => {
    fixture([]);
    render(withSession(<ShopifyStorePanel site={siteFromRow({ ...SHOP, binding: {} })} canBind trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    await click(screen.getByRole("button", { name: "Manage Shopify connections in Settings" }));
    await vi.waitFor(() => expect(h.openApp).toHaveBeenCalledWith("deployables", "settings", { provider: "shopify" }));
    expect(screen.queryByLabelText("Client secret")).toBeNull();
    expect(screen.queryByLabelText("Storefront API token")).toBeNull();
  });
  it("selects an authorized store and confirms its binding in the shared wizard footer", async () => {
    const connection = fixture();
    render(withSession(<ShopifyStorePanel site={siteFromRow({ ...SHOP, binding: {} })} canBind trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    await click(await screen.findByRole("button", { name: "Configure production store" }));
    await click(await screen.findByRole("button", { name: `Select Shopify store ${saved.label}` }));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Continue" }));
    await readTheBar();
    await click(within(document.querySelector(".os-actbar")!).getByRole("button", { name: "Connect store" }));
    expect(connection.callsNamed("updateSiteStoreBinding")[0]).toContain('storeId: "my-store"');
    expect(connection.callsNamed("shopifyStoreAppSave")).toHaveLength(0);
  });
  it("disconnects a personal selection without changing a store or site, then returns to the list", async () => {
    const connection = fixture();
    render(withSession(<ShopifyConnections header={<h2>Settings</h2>} appId="settings" />));
    await click(await screen.findByRole("button", { name: `Manage Shopify store ${saved.label}` }));
    await click(screen.getByRole("button", { name: "Disconnect" }));
    await readTheBar();
    await click(within(document.querySelector(".os-actbar")!).getByRole("button", { name: "Disconnect" }));
    expect(await screen.findByText("No Shopify stores connected")).toBeTruthy();
    expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(0);
    expect(connection.callsNamed("setStoreStatus")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: /Reconnect/ })).toBeNull();
  });
  it("shows operator setup as unavailable without a developer credential form", async () => {
    fixture([]);
    render(withSession(<ShopifyConnections header={<h2>Settings</h2>} appId="settings" />));
    await click(screen.getByRole("button", { name: "Add Shopify store" }));
    expect(await screen.findByText("Shopify connections are not available yet.")).toBeTruthy();
    expect(screen.queryByLabelText("Client ID")).toBeNull();
    expect(screen.queryByRole("button", { name: "Continue to Shopify" })).toBeNull();
  });
  it("opens Shopify installation without requesting a store address or credentials", async () => {
    fixture([], true);
    render(withSession(<ShopifyConnections header={<h2>Settings</h2>} appId="settings" />));
    await click(screen.getByRole("button", { name: "Add Shopify store" }));
    expect(await screen.findByRole("button", { name: "Continue to Shopify" })).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
  });
  it("identifies a sandbox before explicitly attaching it", async () => {
    const connection = fixture([saved], true, true);
    render(withSession(<ShopifyStorePanel site={siteFromRow({ ...SHOP, binding: {} })} canBind trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    await click(screen.getByRole("button", { name: "Configure testing store" }));
    await click(await screen.findByRole("button", { name: `Select Shopify store ${saved.label}` }));
    expect(screen.getByText("Sandbox store")).toBeTruthy();
    await readTheBar();
    await click(screen.getByRole("button", { name: "Continue" }));
    expect(screen.getByText("Sandbox store — purchases follow Shopify’s test-store rules.")).toBeTruthy();
    await readTheBar();
    await click(screen.getByRole("button", { name: "Connect store" }));
    expect(connection.callsNamed("updateSitePreviewBinding")[0]).toContain('storeId: "my-store"');
    expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(0);
  });
  it("offers a sandbox on Production without changing Testing", async () => {
    const connection = fixture([saved], true, true);
    render(withSession(<ShopifyStorePanel site={siteFromRow({ ...SHOP, previewBinding: { storeId: "my-store" } })} canBind trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    await click(await screen.findByRole("button", { name: "Configure production store" }));
    await click(await screen.findByRole("button", { name: `Select Shopify store ${saved.label}` }));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Continue" }));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Connect store" }));
    await vi.waitFor(() => expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(1));
    expect(connection.callsNamed("updateSitePreviewBinding")).toHaveLength(0);
  });
  it("can use the same sandbox for Testing while retaining Production", async () => {
    const connection = fixture([saved], true, true);
    render(withSession(<ShopifyStorePanel site={siteFromRow({ ...SHOP, binding: { storeId: "my-store" } })} canBind trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    await click(await screen.findByRole("button", { name: "Configure testing store" }));
    await click(await screen.findByRole("button", { name: `Select Shopify store ${saved.label}` }));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Continue" }));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Connect store" }));
    await vi.waitFor(() => expect(connection.callsNamed("updateSitePreviewBinding")).toHaveLength(1));
    expect(connection.callsNamed("updateSitePreviewBinding")[0]).toContain('storeId: "my-store"');
    expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(0);
  });
  it("shows the two store connections without version or grant controls", async () => {
    const connection = fixture();
    render(withSession(<ShopifyStorePanel site={siteFromRow(SHOP)} canBind can={ALL_PARTS} trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    expect(await screen.findByRole("button", { name: "Configure testing store" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Configure production store" })).toBeTruthy();
    expect(screen.queryByRole("combobox", { name: "A version to exercise" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Exercise this version" })).toBeNull();
    expect(connection.callsNamed("setSiteCandidate")).toHaveLength(0);
  });
  it("keeps the serving binding if saving the sandbox for testing fails", async () => {
    const connection = fixture([saved], true, true);
    const execute = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    vi.mocked(connection.query.executeNamed).mockImplementation(async (name, call, options) => {
      if (name === "updateSitePreviewBinding") throw new Error("Store access changed");
      return execute(name, call, options);
    });
    render(withSession(<ShopifyStorePanel site={siteFromRow({ ...SHOP, binding: { storeId: "my-store" } })} canBind trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    await click(await screen.findByRole("button", { name: "Configure testing store" }));
    await click(await screen.findByRole("button", { name: `Select Shopify store ${saved.label}` }));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Continue" }));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Connect store" }));
    expect(await screen.findByText("Store access changed")).toBeTruthy();
    expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(0);
  });
  it("clears the old store display when its binding is removed", async () => {
    fixture([saved], true, true);
    const props = { canBind: true, trail: [], back: { label: "Deployables", onSelect: vi.fn() }, onWritten: vi.fn() };
    const view = render(withSession(<ShopifyStorePanel {...props} site={siteFromRow({ ...SHOP, binding: { storeId: "my-store" } })} />));
    expect(await screen.findByRole("button", { name: "Configure testing store" })).toBeTruthy();
    view.rerender(withSession(<ShopifyStorePanel {...props} site={siteFromRow({ ...SHOP, binding: {}, previewBinding: { storeId: "my-store" } })} />));
    await vi.waitFor(() => expect(screen.getByRole("button", { name: "Configure production store" }).textContent).toContain("Design preview"));
    expect((await screen.findByRole("button", { name: "Configure production store" })).textContent).not.toContain(saved.label);
  });
  it.each(["settings", "deployables"])("returns to the %s connections surface and preserves sign-in parameters", appId => {
    const section = appId === "settings" ? "connections" : "settings";
    history.replaceState(null, "", `/auth/callback?connect=${section}&connectApp=${appId}&shopify=connected&code=login&state=sign-in#keep`);
    const result = captureShopifyReturn(window);
    expect(result).toEqual({ reason: "connected", siteId: "", appId, section });
    expect(location.search).toBe("?code=login&state=sign-in");
    expect(location.hash).toBe("#keep");
    expect(takeShopifyReturn()).toEqual(result);
    expect(takeShopifyReturn()).toBeNull();
  });
  it("does not treat ordinary navigation as a failed connection", () => expect(captureShopifyReturn(window)).toBeNull());
});

// THE ALERT WAIT (U7): how long a store goes without recording before its
// owner is emailed. A cluster owner's to set; the engine answers who may
// (`maySet`) and refuses anything outside 1 to 1440 in its own words.
describe("the alert wait in Settings > Shopify", () => {
  const settings = () => render(withSession(<ShopifyConnections header={<h2>Settings</h2>} appId="settings" />));

  it("shows the alert wait to a cluster owner and saves five", async () => {
    const connection = fixture([saved], false, false, { storeAlertWait: { minutes: 30, isDefault: true, maySet: true } as never });
    settings();
    const minutes = await screen.findByLabelText("Minutes without recording") as HTMLInputElement;
    await vi.waitFor(() => expect(minutes.value).toBe("30"));
    const group = minutes.closest("section") as HTMLElement;
    expect(group.textContent).toContain("Email a store's owner after");
    expect(group.textContent).toContain("minutes without recording");
    await typeInto(minutes, "5");
    await click(within(group).getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(connection.callsNamed("shopifyStoreAlertWaitSet")).toHaveLength(1));
    expect(connection.callsNamed("shopifyStoreAlertWaitSet")[0]).toBe("builtin shopifyStoreAlertWaitSet(minutes: 5)");
    await vi.waitFor(() => expect(minutes.value).toBe("5"));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("draws no alert wait for somebody who may not set it", async () => {
    const connection = fixture([saved], false, false, { storeAlertWait: { minutes: 30, isDefault: true, maySet: false } as never });
    settings();
    expect(await screen.findByRole("button", { name: `Manage Shopify store ${saved.label}` })).toBeTruthy();
    await vi.waitFor(() => expect(connection.callsNamed("shopifyStoreAlertWait").length).toBeGreaterThan(0));
    expect(screen.queryByText(/Email a store's owner/)).toBeNull();
    expect(screen.queryByLabelText("Minutes without recording")).toBeNull();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("shows the engine's refusal for a wait of zero", async () => {
    const connection = fixture([saved], false, false, { storeAlertWait: { minutes: 30, isDefault: true, maySet: true } as never });
    settings();
    const minutes = await screen.findByLabelText("Minutes without recording") as HTMLInputElement;
    await vi.waitFor(() => expect(minutes.value).toBe("30"));
    await typeInto(minutes, "0");
    await click(within(minutes.closest("section") as HTMLElement).getByRole("button", { name: "Save" }));
    expect(await screen.findByText("shopify: the alert wait is a whole number of minutes from 1 to 1440")).toBeTruthy();
    expect(connection.callsNamed("shopifyStoreAlertWaitSet")[0]).toBe("builtin shopifyStoreAlertWaitSet(minutes: 0)");
    expect(minutes.value).toBe("0");
  });
});
