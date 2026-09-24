import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { ShopifyStorePanel } from "../../src/apps/deployables/store/ShopifyStorePanel";
import { captureShopifyReturn, takeShopifyReturn } from "../../src/apps/deployables/store/connectReturn";
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { builtinReply, click, fakeConnection, SHOP, withSession } from "./harness";

const unconnected = { reason: "ok", shopDomain: "example.myshopify.com", storeId: "store-example", appSaved: false, pendingApp: false, connected: false, storefrontTokenSet: false, requiredScopes: ["read_products"], grantedScopes: [] };
function fixture(initial = unconnected) {
  const connection = fakeConnection({ sites: [SHOP] });
  let status = { ...initial };
  const calls: string[] = [];
  const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
  vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
    if (!name.startsWith("shopify")) return original(name, call, options);
    calls.push(call);
    if (name === "shopifyConnectStatus") return builtinReply(name, [status]);
    if (name === "shopifyStoreAppSave") { status = { ...status, appSaved: true, pendingApp: true }; return builtinReply(name, [{ reason: "ok" }]); }
    if (name === "shopifyStorefrontTokenSet") { status = { ...status, storefrontTokenSet: true }; return builtinReply(name, [{ reason: "ok" }]); }
    return builtinReply(name, [{ reason: "permission_lost" }]);
  });
  h.connection = connection;
  const onWritten = vi.fn();
  const panel = (revision = 0) => withSession(<ShopifyStorePanel site={siteFromRow(SHOP)} canBind trail={[]} back={{ label: "Storefront", onSelect: vi.fn() }} onWritten={onWritten} revision={revision} />);
  return { connection, calls, onWritten, panel, setStatus: (value: typeof unconnected) => { status = value; } };
}
afterEach(() => { h.connection = null; history.replaceState(null, "", "/"); takeShopifyReturn(); });

describe("a storefront owns its Shopify connection", () => {
  it("loads a stable placeholder then offers focused app setup with actions in the footer", async () => {
    const f = fixture();
    render(f.panel());
    expect(screen.getByRole("status", { name: "" }).getAttribute("aria-busy")).toBe("true");
    await screen.findByLabelText("Client ID");
    fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "client" } });
    fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: "secret" } });
    const footer = within(document.querySelector(".os-actbar")!);
    await click(footer.getByRole("button", { name: "Save" }));
    expect(await footer.findByRole("button", { name: "Connect Shopify" })).toBeTruthy();
    expect(screen.queryByLabelText("Client secret")).toBeNull();
    expect(f.onWritten).toHaveBeenCalled();
    expect(f.calls.filter(call => call.startsWith("builtin shopifyStoreAppSave"))).toHaveLength(1);
  });
  it("shows connection readiness separately from site publication and re-reads token-only changes", async () => {
    const f = fixture({ ...unconnected, connected: true, appSaved: true });
    const view = render(f.panel());
    expect(await screen.findByText("Not connected")).toBeTruthy();
    f.setStatus({ ...unconnected, connected: true, appSaved: true, storefrontTokenSet: true });
    view.rerender(f.panel(1));
    expect(await screen.findByText("Available")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Re-read|Refresh/ })).toBeNull();
  });
  it("returns directly to the Store page for the named deployable", async () => {
    const f = fixture({ ...unconnected, connected: true, appSaved: true, storefrontTokenSet: true });
    render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} intent={{ id: "shopify-return", payload: { shopify: { siteId: SHOP.id, reason: "connected" } } }} />));
    expect(await screen.findByRole("heading", { name: "Store" })).toBeTruthy();
    await waitFor(() => expect(f.calls.some(call => call.includes(`siteId: "${SHOP.id}"`))).toBe(true));
  });
  it("shows a failed callback even when its state cannot identify a site", async () => {
    fixture();
    render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} intent={{ id: "expired-shopify", payload: { shopify: { siteId: "", reason: "connect_state_invalid" } } }} />));
    expect(await screen.findByText("This connection link expired. Connect Shopify again.")).toBeTruthy();
  });
  it("scrubs only Shopify return fields and preserves sign-in state", () => {
    history.replaceState(null, "", "/auth/callback?shopify=connected&site=shop&code=login&state=sign-in#keep");
    const result = captureShopifyReturn(window);
    expect(result).toEqual({ reason: "connected", siteId: "shop" });
    expect(location.search).toBe("?code=login&state=sign-in");
    expect(location.hash).toBe("#keep");
    expect(takeShopifyReturn()).toEqual(result);
    expect(takeShopifyReturn()).toBeNull();
  });
  it("does not treat ordinary navigation as a failed connection", () => {
    expect(captureShopifyReturn(window)).toBeNull();
  });
});
