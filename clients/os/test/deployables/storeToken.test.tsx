import { readFileSync } from "node:fs";
import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown, openApp: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/chrome/state", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../src/chrome/state")>()),
  useOs: () => ({ actions: { openApp: h.openApp } }),
  useOsIfPresent: () => null,
}));

import { ShopifyStorePanel } from "../../src/apps/deployables/store/ShopifyStorePanel";
import { StorePanel } from "../../src/apps/deployables/store/StorePanel";
import {
  storeConnected,
  storeFromRow,
  storefrontTokenMisnamed,
  storefrontTokenSecretName,
} from "../../src/apps/deployables/store/rows";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { click, fakeConnection, rowsResult, SHOP, siteRow, type as typeInto, withSession } from "./harness";

// THE STOREFRONT TOKEN IS PUBLISHED ONLY UNDER THE STORE'S OWN NAME
// (memql#5626, followed through in the OS).
//
// The edge serves a store's Storefront token only when the store's
// storefrontTokenRef is exactly `SHOPIFY_<STOREID>_STOREFRONT_TOKEN`
// (component/edge/runtimeconfig.go). Before this the OS read "Connected"
// whenever both references were non-empty, so a store registered under any
// other name -- the name the register form's own placeholder suggested --
// served no token at the edge while every surface here said it was connected.

afterEach(() => {
  h.connection = null;
  h.openApp.mockClear();
});

describe("the name the edge publishes", () => {
  it("is SHOPIFY_<STOREID>_STOREFRONT_TOKEN, the id upper-cased and its hyphens kept", () => {
    expect(storefrontTokenSecretName("acme-widgets")).toBe("SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN");
    expect(storefrontTokenSecretName("  shop1 ")).toBe("SHOPIFY_SHOP1_STOREFRONT_TOKEN");
    expect(storefrontTokenSecretName("")).toBe("");
  });

  it("is spelled the way the edge spells it -- the mirror and its source cannot drift apart", () => {
    // A MIRROR, for the register form's read-only answer and the readiness
    // below, until the engine carries the verdict on the row. This holds the
    // two spellings together the way TestFleetOnlineWindowMatchesTheClients
    // holds the online window: if the Go rule moves, this fails.
    const go = readFileSync(`${process.cwd()}/../../component/edge/runtimeconfig.go`, "utf8");
    const body = /func StorefrontTokenSecretName\(storeID string\) string \{([\s\S]*?)\n\}/.exec(go)?.[1] ?? "";
    expect(body).toContain('"SHOPIFY_" + strings.ToUpper(storeID) + "_STOREFRONT_TOKEN"');
    expect(body).toContain("strings.TrimSpace(storeID)");
  });
});

describe("whether a store is connected", () => {
  const own = storeFromRow({
    id: "acme-widgets",
    domain: "acme-widgets.myshopify.com",
    adminTokenRef: "SHOPIFY_ACME-WIDGETS_ADMIN_TOKEN",
    storefrontTokenRef: "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN",
  });

  it("is connected with both references and the Storefront one under the store's own name", () => {
    expect(storefrontTokenMisnamed(own)).toBe(false);
    expect(storeConnected(own)).toBe(true);
  });

  it("is NOT connected when the Storefront token is registered under another name", () => {
    const misnamed = { ...own, storefrontTokenRef: "ACME_STOREFRONT_TOKEN" };
    expect(storefrontTokenMisnamed(misnamed)).toBe(true);
    expect(storeConnected(misnamed)).toBe(false);
  });

  it("is not connected with a reference missing, and that is not the misnamed case", () => {
    expect(storeConnected({ ...own, adminTokenRef: "" })).toBe(false);
    expect(storefrontTokenMisnamed({ ...own, storefrontTokenRef: "" })).toBe(false);
    expect(storeConnected({ ...own, storefrontTokenRef: "" })).toBe(false);
  });
});

describe("the Store page", () => {
  const MISNAMED = {
    id: "acme-widgets",
    domain: "acme-widgets.myshopify.com",
    adminTokenRef: "SHOPIFY_ACME-WIDGETS_ADMIN_TOKEN",
    storefrontTokenRef: "ACME_STOREFRONT_TOKEN",
    isDevelopment: false,
  };

  function open(store: Record<string, unknown>) {
    const connection = fakeConnection({ sites: [SHOP], stores: [store as never] });
    const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
      if (name === "externalConnectionsMine") return rowsResult([]);
      return original(name, call, options);
    });
    h.connection = connection;
    const site = siteFromRow(siteRow({ ...SHOP, id: "site-shop", binding: { storeId: "acme-widgets" } } as never));
    render(withSession(<ShopifyStorePanel site={site} canBind trail={[]} back={{ label: "Storefront", onSelect: vi.fn() }} onWritten={vi.fn()} />));
  }

  it("does not call a store connected whose token is registered under another name, and says what fixes it", async () => {
    open(MISNAMED);
    const production = await screen.findByRole("button", { name: "Configure production store" });
    await waitFor(() => expect(production.textContent).toContain("acme-widgets.myshopify.com"));
    expect(production.textContent).toContain("Needs attention");
    expect(production.textContent).not.toContain("Connected");
    const notice = await screen.findByText(/Storefront token for acme-widgets\.myshopify\.com is registered under a different name/);
    const box = notice.closest(".os-notice") as HTMLElement;
    expect(box.textContent).toContain("acme-widgets.myshopify.com");
    expect(box.textContent).toContain("SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN");
    await click(within(box).getByRole("button", { name: "Reconnect in Settings" }));
    expect(h.openApp).toHaveBeenCalledWith("deployables", "settings", { provider: "shopify" });
    // The bar does not claim it either.
    expect(document.querySelector(".os-actbar-word")?.textContent).not.toBe("Connected");
  });

  it("calls a store connected whose token is under its own name, with nothing to fix", async () => {
    open({ ...MISNAMED, storefrontTokenRef: "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN" });
    const production = await screen.findByRole("button", { name: "Configure production store" });
    await waitFor(() => expect(production.textContent).toContain("Connected"));
    expect(screen.queryByText(/registered under a different name/)).toBeNull();
  });
});

describe("registering a store", () => {
  it("shows the Storefront token's secret name read-only, derived from the store, and registers it under that name", async () => {
    const unbound = siteRow({ id: "site-new", hostname: "new.memql.example.com", kind: "shopify_storefront", status: "draft", binding: {} });
    const connection = fakeConnection({ sites: [unbound], stores: [] });
    h.connection = connection;
    render(
      withSession(
        <section aria-label="Store picker">
          <StorePanel site={siteFromRow(unbound)} canBind trail={[]} back={{ label: "Store", onSelect: vi.fn() }} />
        </section>,
        { role: "owner", userId: "u-me" },
      ),
    );
    const pane = await screen.findByRole("region", { name: "Store picker" });
    await click(within(pane).getByRole("button", { name: "Register a store" }));
    // NO FREE TEXT FOR IT. Any name but the store's own is a token the edge
    // will not publish, so there is nothing to choose.
    expect(within(pane).queryByLabelText("The name of the secret holding the Storefront API token")).toBeNull();
    await typeInto(within(pane).getByLabelText("The store's myshopify.com domain") as HTMLInputElement, "acme-widgets.myshopify.com");
    const derived = within(pane).getByDisplayValue("SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN") as HTMLInputElement;
    expect(derived.readOnly).toBe(true);
    // A domain is now enough to register.
    await click(within(pane).getByRole("button", { name: "Register and attach" }));
    const create = connection.callsNamed("createStore")[0] ?? "";
    expect(create).toContain('storeId: "acme-widgets"');
    expect(create).toContain('storefrontTokenRef: "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN"');
  });
});
