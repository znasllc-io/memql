import { render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown, openApp: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/chrome/state", async importOriginal => ({ ...await importOriginal<typeof import("../../src/chrome/state")>(), useOs: () => ({ actions: { openApp: h.openApp } }), useOsIfPresent: () => null }));
import { ShopifyStorePanel } from "../../src/apps/deployables/store/ShopifyStorePanel";
import { ShopifyConnections } from "../../src/modules/connections/ShopifyConnections";
import { captureShopifyReturn, takeShopifyReturn } from "../../src/apps/deployables/store/connectReturn";
import { ALL_PARTS } from "../../src/apps/deployables/parts";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { builtinReply, click, fakeConnection, rowsResult, SHOP, withSession } from "./harness";

const saved = { id: "connection", ownerUserId: "u-me", provider: "shopify", resourceId: "my-store", label: "my-store.myshopify.com", status: "active" };
function fixture(connections = [saved], configured = false, development = false) {
  const connection = fakeConnection({ sites: [SHOP], stores: [{ id: "my-store", domain: saved.label, adminTokenRef: "admin", storefrontTokenRef: "public", isDevelopment: development, plan: "Basic" }] });
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
    await click(screen.getByRole("button", { name: "Configure production store" }));
    await click(await screen.findByRole("button", { name: `Select Shopify store ${saved.label}` }));
    await click(screen.getByRole("button", { name: "Continue" }));
    await click(within(document.querySelector(".os-actbar")!).getByRole("button", { name: "Connect store" }));
    expect(connection.callsNamed("updateSiteStoreBinding")[0]).toContain('storeId: "my-store"');
    expect(connection.callsNamed("shopifyStoreAppSave")).toHaveLength(0);
  });
  it("disconnects a personal selection without changing a store or site, then returns to the list", async () => {
    const connection = fixture();
    render(withSession(<ShopifyConnections header={<h2>Settings</h2>} appId="settings" />));
    await click(await screen.findByRole("button", { name: `Manage Shopify store ${saved.label}` }));
    await click(screen.getByRole("button", { name: "Disconnect" }));
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
    expect(screen.getByText("Sandbox")).toBeTruthy();
    await click(screen.getByRole("button", { name: "Continue" }));
    expect(screen.getByText("Uses the sandbox catalog and test checkout in previews.")).toBeTruthy();
    await click(screen.getByRole("button", { name: "Connect store" }));
    expect(connection.callsNamed("updateSitePreviewBinding")[0]).toContain('storeId: "my-store"');
    expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(0);
  });
  it("never offers a sandbox in the production picker", async () => {
    fixture([saved], true, true);
    render(withSession(<ShopifyStorePanel site={siteFromRow({ ...SHOP, binding: {} })} canBind trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    await click(screen.getByRole("button", { name: "Configure production store" }));
    expect(await screen.findByText("No production stores connected.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: `Select Shopify store ${saved.label}` })).toBeNull();
  });
  it("moves an incorrectly serving sandbox to testing without binding a production store", async () => {
    const connection = fixture([saved], true, true);
    render(withSession(<ShopifyStorePanel site={siteFromRow({ ...SHOP, binding: { storeId: "my-store" } })} canBind trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    await click(await screen.findByRole("button", { name: "Move sandbox to testing" }));
    await click(await screen.findByRole("button", { name: `Select Shopify store ${saved.label}` }));
    await click(screen.getByRole("button", { name: "Continue" }));
    await click(screen.getByRole("button", { name: "Connect store" }));
    await vi.waitFor(() => expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(1));
    expect(connection.callsNamed("updateSitePreviewBinding")[0]).toContain('storeId: "my-store"');
    expect(connection.callsNamed("updateSiteStoreBinding")[0]).toContain('storeId: ""');
  });
  it("can select the first built version for sandbox testing without entering a bundle reference", async () => {
    const connection = fixture();
    const site = siteFromRow({ ...SHOP, binding: {} });
    render(withSession(<ShopifyStorePanel site={site} canBind can={ALL_PARTS} trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    const picker = await screen.findByRole("combobox", { name: "A version to exercise" });
    await click(picker);
    await click(screen.getByRole("option", { name: "v1" }));
    await click(screen.getByRole("button", { name: "Exercise this version" }));
    expect(connection.callsNamed("setSiteCandidate")[0]).toContain(`candidateRef: "${site.bundleRef}"`);
    expect(screen.queryByRole("textbox", { name: "A bundle reference to exercise" })).toBeNull();
  });
  it("keeps the serving binding if saving the sandbox for testing fails", async () => {
    const connection = fixture([saved], true, true);
    const execute = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    vi.mocked(connection.query.executeNamed).mockImplementation(async (name, call, options) => {
      if (name === "updateSitePreviewBinding") throw new Error("Store access changed");
      return execute(name, call, options);
    });
    render(withSession(<ShopifyStorePanel site={siteFromRow({ ...SHOP, binding: { storeId: "my-store" } })} canBind trail={[]} back={{ label: "Deployables", onSelect: vi.fn() }} onWritten={vi.fn()} />));
    await click(await screen.findByRole("button", { name: "Move sandbox to testing" }));
    await click(await screen.findByRole("button", { name: `Select Shopify store ${saved.label}` }));
    await click(screen.getByRole("button", { name: "Continue" }));
    await click(screen.getByRole("button", { name: "Connect store" }));
    expect(await screen.findByText("Store access changed")).toBeTruthy();
    expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(0);
  });
  it("clears the old store display when its binding is removed", async () => {
    fixture([saved], true, true);
    const props = { canBind: true, trail: [], back: { label: "Deployables", onSelect: vi.fn() }, onWritten: vi.fn() };
    const view = render(withSession(<ShopifyStorePanel {...props} site={siteFromRow({ ...SHOP, binding: { storeId: "my-store" } })} />));
    expect(await screen.findByRole("button", { name: "Move sandbox to testing" })).toBeTruthy();
    view.rerender(withSession(<ShopifyStorePanel {...props} site={siteFromRow({ ...SHOP, binding: {}, previewBinding: { storeId: "my-store" } })} />));
    await vi.waitFor(() => expect(screen.queryByRole("button", { name: "Move sandbox to testing" })).toBeNull());
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
