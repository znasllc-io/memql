import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown, openApp: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/chrome/state", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../src/chrome/state")>()),
  useOs: () => ({ actions: { openApp: h.openApp } }),
  useOsIfPresent: () => null,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readinessFromRow } from "../../src/apps/deployables/preview/rows";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { ShopifyStorePanel } from "../../src/apps/deployables/store/ShopifyStorePanel";
import { StorePanel } from "../../src/apps/deployables/store/StorePanel";
import { storeConnection, storeFromRow } from "../../src/apps/deployables/store/rows";
import { click, fakeConnection, previewReadinessRow, rowsResult, SHOP, siteRow, type as typeInto, withSession } from "./harness";

// WHETHER A STORE IS CONNECTED, AS THE ENGINE ANSWERS IT (memql#5626, #5638).
//
// "Connected" used to be decided here from two references being non-empty. It
// is the engine's answer now: sitePreviewReadiness reports whether the edge
// will serve each bound store's Storefront token (the store names its OWN
// token), and the store row says whether Shopify uninstalled the app
// (uninstalledAt) or purged the store's data (redactedAt). The OS keeps no
// copy of the naming rule.

afterEach(() => {
  h.connection = null;
  h.openApp.mockClear();
});

const OWN: Row = {
  id: "acme-widgets",
  domain: "acme-widgets.myshopify.com",
  adminTokenRef: "SHOPIFY_ACME-WIDGETS_ADMIN_TOKEN",
  storefrontTokenRef: "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN",
  isDevelopment: false,
};

describe("the facts", () => {
  it("reads whether the edge serves each store's Storefront token", () => {
    const facts = readinessFromRow(
      previewReadinessRow({ siteId: "site-shop", storeHasStorefrontToken: true, previewStoreHasStorefrontToken: false } as never),
    );
    expect(facts.storeHasStorefrontToken).toBe(true);
    expect(facts.previewStoreHasStorefrontToken).toBe(false);
  });

  it("reads when Shopify uninstalled the app or removed the store's data", () => {
    const store = storeFromRow({ ...OWN, uninstalledAt: "2026-10-03T08:00:00Z", redactedAt: "2026-10-04T08:00:00Z" });
    expect(store.uninstalledAt).toBe("2026-10-03T08:00:00Z");
    expect(store.redactedAt).toBe("2026-10-04T08:00:00Z");
    expect(storeFromRow(OWN).uninstalledAt).toBe("");
  });
});

describe("what a store's connection is", () => {
  const store = storeFromRow(OWN);

  it("is connected when the edge serves its token and Shopify has not cut it off", () => {
    expect(storeConnection(store, true)).toBe("connected");
  });

  it("is uninstalled while Shopify reports the app uninstalled, whatever else is true", () => {
    expect(storeConnection({ ...store, uninstalledAt: "2026-10-03T08:00:00Z", adminTokenRef: "" }, true)).toBe("uninstalled");
  });

  it("is redacted once Shopify purged its data -- the stronger statement of the two", () => {
    expect(storeConnection({ ...store, uninstalledAt: "2026-10-03T08:00:00Z", redactedAt: "2026-10-04T08:00:00Z" }, false)).toBe("redacted");
  });

  it("names a token the edge does not serve as registered under a different name", () => {
    // The store names A token and the engine says it is not the one it
    // serves: that is the only way the two can disagree.
    expect(storeConnection(store, false)).toBe("token-misnamed");
  });

  it("needs setup with no Storefront token, or no Admin token for the mirror", () => {
    expect(storeConnection({ ...store, storefrontTokenRef: "" }, false)).toBe("setup-needed");
    expect(storeConnection({ ...store, adminTokenRef: "" }, true)).toBe("setup-needed");
  });

  it("is unknown until the engine has answered -- never connected by default", () => {
    expect(storeConnection(store, null)).toBe("unknown");
  });
});

describe("the Store page", () => {
  function open(store: Row, facts: { production?: boolean; testing?: boolean } = {}) {
    const site = siteRow({ ...SHOP, id: "site-shop", binding: { storeId: String(store["id"]) } } as never);
    const connection = fakeConnection({
      sites: [site],
      stores: [store as never],
      previewReadiness: {
        "site-shop": previewReadinessRow({
          siteId: "site-shop",
          storeId: String(store["id"]),
          storeReadable: true,
          storeHasStorefrontToken: facts.production ?? false,
          previewStoreHasStorefrontToken: facts.testing ?? false,
        } as never),
      },
    });
    const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
      if (name === "externalConnectionsMine") return rowsResult([]);
      return original(name, call, options);
    });
    h.connection = connection;
    render(withSession(<ShopifyStorePanel site={siteFromRow(site)} canBind trail={[]} back={{ label: "Storefront", onSelect: vi.fn() }} onWritten={vi.fn()} />));
  }

  async function productionRow(): Promise<HTMLElement> {
    const row = await screen.findByRole("button", { name: "Configure production store" });
    await waitFor(() => expect(row.textContent).toContain("acme-widgets.myshopify.com"));
    return row;
  }

  function noticeWith(text: RegExp): HTMLElement {
    return screen.getByText(text).closest(".os-notice") as HTMLElement;
  }

  it("calls a store connected when the engine says its token is served", async () => {
    open(OWN, { production: true });
    const row = await productionRow();
    await waitFor(() => expect(row.textContent).toContain("Connected"));
    expect(screen.queryByText(/registered under a different name/)).toBeNull();
  });

  it("does not call a store connected whose token the engine will not serve, and says what fixes it", async () => {
    open({ ...OWN, storefrontTokenRef: "ACME_STOREFRONT_TOKEN" }, { production: false });
    const row = await productionRow();
    await waitFor(() => expect(row.textContent).toContain("Needs attention"));
    expect(row.textContent).not.toContain("Connected");
    const notice = noticeWith(/Storefront token for acme-widgets\.myshopify\.com is registered under a different name/);
    expect(notice.textContent).toContain("Reconnect the store to seal it under its own name.");
    await click(within(notice).getByRole("button", { name: "Reconnect in Settings" }));
    expect(h.openApp).toHaveBeenCalledWith("deployables", "settings", { provider: "shopify" });
    expect(document.querySelector(".os-actbar-word")?.textContent).not.toBe("Connected");
  });

  it("says a store Shopify uninstalled is uninstalled, calmly, with the way back", async () => {
    open({ ...OWN, adminTokenRef: "", uninstalledAt: "2026-10-03T08:00:00Z" }, { production: true });
    const row = await productionRow();
    await waitFor(() => expect(row.textContent).toContain("Uninstalled"));
    const notice = noticeWith(/acme-widgets\.myshopify\.com was uninstalled in Shopify/);
    expect(notice.textContent).toContain("Reconnect the store to bring its catalog back.");
    await click(within(notice).getByRole("button", { name: "Reconnect in Settings" }));
    expect(h.openApp).toHaveBeenCalledWith("deployables", "settings", { provider: "shopify" });
    expect(document.querySelector(".os-actbar-word")?.textContent).not.toBe("Connected");
  });

  it("says a store Shopify purged has had its data removed, with the way back", async () => {
    open({ ...OWN, adminTokenRef: "", uninstalledAt: "2026-10-03T08:00:00Z", redactedAt: "2026-10-04T08:00:00Z" });
    const row = await productionRow();
    await waitFor(() => expect(row.textContent).toContain("Data removed"));
    const notice = noticeWith(/Shopify removed the data of acme-widgets\.myshopify\.com/);
    expect(notice.textContent).toContain("Reconnect the store to start again.");
    expect(within(notice).getByRole("button", { name: "Reconnect in Settings" })).toBeTruthy();
    // ONE notice for the store: a purged store is not also told it is uninstalled.
    expect(screen.queryByText(/was uninstalled in Shopify/)).toBeNull();
  });
});

describe("registering a store", () => {
  function openRegister(over: { createStoreError?: string } = {}) {
    const unbound = siteRow({ id: "site-new", hostname: "new.memql.example.com", kind: "shopify_storefront", status: "draft", binding: {} });
    const connection = fakeConnection({ sites: [unbound], stores: [], ...over });
    h.connection = connection;
    render(
      withSession(
        <section aria-label="Store picker">
          <StorePanel site={siteFromRow(unbound)} canBind trail={[]} back={{ label: "Store", onSelect: vi.fn() }} />
        </section>,
        { role: "owner", userId: "u-me" },
      ),
    );
    return connection;
  }

  it("asks for no Storefront token, and registers on a domain alone", async () => {
    const connection = openRegister();
    const pane = await screen.findByRole("region", { name: "Store picker" });
    await click(within(pane).getByRole("button", { name: "Register a store" }));
    // NOTHING TO TYPE: the token is sealed under the store's own name when the
    // store is connected, and the engine refuses any other name.
    expect(within(pane).queryByText("Storefront token")).toBeNull();
    expect(within(pane).queryByLabelText(/Storefront API token/)).toBeNull();
    await typeInto(within(pane).getByLabelText("The store's myshopify.com domain") as HTMLInputElement, "acme-widgets.myshopify.com");
    expect(within(pane).getByText(/Its Storefront token is added when the store is connected in Settings/)).toBeTruthy();
    await click(within(pane).getByRole("button", { name: "Register and attach" }));
    const create = connection.callsNamed("createStore")[0] ?? "";
    expect(create).toContain('storeId: "acme-widgets"');
    expect(create).not.toContain("storefrontTokenRef");
  });

  it("shows the engine's refusal in place, and keeps what was typed", async () => {
    const refusal = 'v1:shopify:store: storefrontTokenRef "ACME_STOREFRONT_TOKEN" is not this store\'s own Storefront token -- name SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN.';
    openRegister({ createStoreError: refusal });
    const pane = await screen.findByRole("region", { name: "Store picker" });
    await click(within(pane).getByRole("button", { name: "Register a store" }));
    const domain = within(pane).getByLabelText("The store's myshopify.com domain") as HTMLInputElement;
    await typeInto(domain, "acme-widgets.myshopify.com");
    await click(within(pane).getByRole("button", { name: "Register and attach" }));
    await waitFor(() => expect(screen.getByText(refusal)).toBeTruthy());
    expect(domain.value).toBe("acme-widgets.myshopify.com");
  });
});
