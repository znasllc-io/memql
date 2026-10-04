import { act, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown, openApp: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/chrome/state", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../src/chrome/state")>()),
  useOs: () => ({ actions: { openApp: h.openApp } }),
  useOsIfPresent: () => null,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { AttentionProvider } from "../../src/attention/Attention";
import { OS_REGISTRY } from "../../src/apps/registry";
import { RuntimeSettingsPanel } from "../../src/apps/deployables/page/stops/RuntimeSettings";
import { ALL_PARTS, partsWithout, type PartsHeld } from "../../src/apps/deployables/parts";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { ShopifyStorePanel } from "../../src/apps/deployables/store/ShopifyStorePanel";
import { DEV_STORE, SHOP, STORE, click, fakeConnection, rowsResult, siteRow, type as typeInto, withSession } from "./harness";

// THE STORE PAGE'S VALUES PER STORE (memql#5602), rendered.
//
// The arithmetic is storeValues.test.ts's. What is asserted here is what a
// person SEES and what reaches the wire: which store a value is for, what
// happens for a store with none, the whole map a save sends, a refusal in
// place, and a change made somewhere else never silently replacing an edit.

const SITE_ROW: Row = siteRow({
  ...SHOP,
  id: "site-shop",
  binding: { storeId: "store-example" },
  previewBinding: { storeId: "store-example-dev" },
  settings: { customerAccountClientId: "shp_site", apiBase: "https://api.example.com" },
  storeSettings: {
    "store-example": { customerAccountClientId: "shp_live" },
    "old-shop": { wholesaleAdapter: "customerTag" },
  },
} as never);

function connect(over: { refuse?: string } = {}) {
  const connection = fakeConnection({ sites: [SITE_ROW], stores: [STORE, DEV_STORE] });
  const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
  vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
    if (name === "externalConnectionsMine") return rowsResult([]);
    if (name === "updateSiteStoreSettings") {
      connection.calls.push(call);
      if (over.refuse) throw new Error(over.refuse);
      return rowsResult([]);
    }
    return original(name, call, options);
  });
  h.connection = connection;
  return connection;
}

function panel(row: Row, can: PartsHeld = ALL_PARTS) {
  return withSession(
    <ShopifyStorePanel site={siteFromRow(row)} canBind can={can} trail={[]} back={{ label: "Storefront", onSelect: vi.fn() }} onWritten={vi.fn()} />,
  );
}

async function values(): Promise<HTMLElement> {
  return (await screen.findByRole("region", { name: "Store values" })) as HTMLElement;
}

function group(region: HTMLElement, domain: string): HTMLElement {
  const heading = within(region).getByText(domain, { selector: ".os-store-values-name" });
  return heading.closest("section") as HTMLElement;
}

function saveButton(region: HTMLElement): HTMLButtonElement {
  return within(region).getByRole("button", { name: "Save store values" }) as HTMLButtonElement;
}

afterEach(() => {
  h.connection = null;
  h.openApp.mockClear();
});

describe("which store a value is for", () => {
  it("heads each group with the store's domain and the website it supplies", async () => {
    connect();
    render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    expect(group(region, "example.myshopify.com").querySelector(".os-store-values-where")?.textContent).toBe("Production");
    expect(group(region, "example-dev.myshopify.com").querySelector(".os-store-values-where")?.textContent).toBe("Testing");
  });

  it("says a store with no values of its own uses the app values", async () => {
    connect();
    render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example-dev.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    expect(group(region, "example-dev.myshopify.com").textContent).toContain("No values of its own, so Testing uses the app values.");
  });

  it("says which app value a store's value replaces, on its own row", async () => {
    connect();
    render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    const production = group(region, "example.myshopify.com");
    expect((within(production).getByLabelText("customerAccountClientId for example.myshopify.com") as HTMLInputElement).value).toBe("shp_live");
    expect(production.textContent).toContain("Replaces the app value “shp_site” for this store.");
  });

  it("shows a store both websites use ONCE, for both", async () => {
    const shared = siteRow({ ...SITE_ROW, id: "site-shop", previewBinding: { storeId: "store-example" } } as never);
    connect();
    render(panel(shared));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    expect(region.querySelectorAll(".os-store-values-store")).toHaveLength(1);
    expect(group(region, "example.myshopify.com").querySelector(".os-store-values-where")?.textContent).toBe("Production and Testing");
  });

  it("asks for a connected store before it offers any values", async () => {
    const unbound = siteRow({ ...SITE_ROW, id: "site-shop", binding: {}, previewBinding: {}, storeSettings: {} } as never);
    connect();
    render(panel(unbound));
    const region = await values();
    expect(within(region).getByText("Connect a store to give it values of its own.")).toBeTruthy();
    expect(within(region).queryByRole("button", { name: "Save store values" })).toBeNull();
  });
});

describe("saving", () => {
  it("sends the whole map: the edit, the untouched store, and the store no binding names", async () => {
    const connection = connect();
    render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example-dev.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    expect(saveButton(region).disabled).toBe(true);

    await click(within(region).getByRole("button", { name: "Add a value for example-dev.myshopify.com" }));
    const testing = group(region, "example-dev.myshopify.com");
    await typeInto(within(testing).getByLabelText("Value name for example-dev.myshopify.com") as HTMLInputElement, "customerAccountClientId");
    await typeInto(within(testing).getByLabelText("customerAccountClientId for example-dev.myshopify.com") as HTMLInputElement, "shp_dev");
    expect(saveButton(region).disabled).toBe(false);
    await click(saveButton(region));

    // A REPLACE, so the store this storefront no longer uses is sent back as it
    // is stored -- dropping it would erase values the engine keeps on purpose.
    expect(connection.callsNamed("updateSiteStoreSettings")).toEqual([
      'mutation updateSiteStoreSettings(siteId: "site-shop", storeSettings: {"old-shop": {wholesaleAdapter: "customerTag"}, "store-example": {customerAccountClientId: "shp_live"}, "store-example-dev": {customerAccountClientId: "shp_dev"}})',
    ]);
  });

  it("removes a kept store's values only when saved, and can take the removal back", async () => {
    const connection = connect();
    render(panel(SITE_ROW));
    const region = await values();
    const kept = await within(region).findByRole("list", { name: "Values kept for other stores" });
    expect(kept.textContent).toContain("old-shop");
    expect(kept.textContent).toContain("1 value");

    await click(within(kept).getByRole("button", { name: "Remove the values kept for old-shop" }));
    expect(kept.textContent).toContain("Removed when you save");
    await click(within(kept).getByRole("button", { name: "Keep the values kept for old-shop" }));
    expect(kept.textContent).not.toContain("Removed when you save");
    expect(saveButton(region).disabled).toBe(true);

    await click(within(kept).getByRole("button", { name: "Remove the values kept for old-shop" }));
    await click(saveButton(region));
    expect(connection.callsNamed("updateSiteStoreSettings")).toEqual([
      'mutation updateSiteStoreSettings(siteId: "site-shop", storeSettings: {"store-example": {customerAccountClientId: "shp_live"}})',
    ]);
  });

  it("answers a name a bundle cannot read before the click", async () => {
    connect();
    render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example-dev.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    await click(within(region).getByRole("button", { name: "Add a value for example-dev.myshopify.com" }));
    const testing = group(region, "example-dev.myshopify.com");
    await typeInto(within(testing).getByLabelText("Value name for example-dev.myshopify.com") as HTMLInputElement, "client-id");
    expect(testing.textContent).toContain("A setting's name is a letter followed by letters, digits or underscores");
    expect(saveButton(region).disabled).toBe(true);
  });

  it("renders the engine's refusal in place, and keeps the edit", async () => {
    const refusal =
      'v1:platform:site: storeSettings["store-example-dev"] key "clientRef" ends in Ref, and a setting is never a reference.';
    connect({ refuse: refusal });
    render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example-dev.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    await click(within(region).getByRole("button", { name: "Add a value for example-dev.myshopify.com" }));
    const testing = group(region, "example-dev.myshopify.com");
    await typeInto(within(testing).getByLabelText("Value name for example-dev.myshopify.com") as HTMLInputElement, "clientRef");
    await click(saveButton(region));

    const notice = (await within(region).findByText("Store values were not saved.")).closest(".os-notice") as HTMLElement;
    expect(notice.textContent).toContain(refusal);
    expect((within(testing).getByLabelText("Value name for example-dev.myshopify.com") as HTMLInputElement).value).toBe("clientRef");
  });
});

describe("a change made somewhere else", () => {
  const moved = (values: Record<string, Record<string, string>>): Row =>
    siteRow({ ...SITE_ROW, id: "site-shop", storeSettings: values } as never);

  it("follows the row while nothing is being edited", async () => {
    connect();
    const view = render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    view.rerender(panel(moved({ "store-example": { customerAccountClientId: "shp_rotated" }, "old-shop": { wholesaleAdapter: "customerTag" } })));
    const field = within(group(region, "example.myshopify.com")).getByLabelText("customerAccountClientId for example.myshopify.com") as HTMLInputElement;
    expect(field.value).toBe("shp_rotated");
    expect(within(region).queryByText(/changed somewhere else/)).toBeNull();
  });

  it("does not replace an edit: it says so, and Save waits for an answer", async () => {
    const connection = connect();
    const view = render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    const label = "customerAccountClientId for example.myshopify.com";
    await typeInto(within(group(region, "example.myshopify.com")).getByLabelText(label) as HTMLInputElement, "shp_mine");

    view.rerender(panel(moved({ "store-example": { customerAccountClientId: "shp_rotated" }, "old-shop": { wholesaleAdapter: "customerTag" } })));
    // THE EDIT SURVIVES THE ROW MOVING.
    expect((within(group(region, "example.myshopify.com")).getByLabelText(label) as HTMLInputElement).value).toBe("shp_mine");
    expect(within(region).getByText("Store values changed somewhere else while you were editing.")).toBeTruthy();
    expect(saveButton(region).disabled).toBe(true);

    await click(within(region).getByRole("button", { name: "Keep my changes" }));
    expect(saveButton(region).disabled).toBe(false);
    await click(saveButton(region));
    expect(connection.callsNamed("updateSiteStoreSettings")[0]).toContain('"store-example": {customerAccountClientId: "shp_mine"}');
  });

  it("takes the new values when asked, for the store that changed", async () => {
    connect();
    const view = render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    const label = "customerAccountClientId for example.myshopify.com";
    await typeInto(within(group(region, "example.myshopify.com")).getByLabelText(label) as HTMLInputElement, "shp_mine");
    view.rerender(panel(moved({ "store-example": { customerAccountClientId: "shp_rotated" }, "old-shop": { wholesaleAdapter: "customerTag" } })));

    await click(within(region).getByRole("button", { name: "Use the new values" }));
    expect((within(group(region, "example.myshopify.com")).getByLabelText(label) as HTMLInputElement).value).toBe("shp_rotated");
    expect(within(region).queryByText(/changed somewhere else/)).toBeNull();
    expect(saveButton(region).disabled).toBe(true);
  });

  it("lets go of an edit once the row says what it says -- this save landing", async () => {
    connect();
    const view = render(panel(SITE_ROW));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    const label = "customerAccountClientId for example.myshopify.com";
    await typeInto(within(group(region, "example.myshopify.com")).getByLabelText(label) as HTMLInputElement, "shp_mine");
    await click(saveButton(region));
    await act(async () => {
      view.rerender(panel(moved({ "store-example": { customerAccountClientId: "shp_mine" }, "old-shop": { wholesaleAdapter: "customerTag" } })));
    });
    expect(within(region).queryByText(/changed somewhere else/)).toBeNull();
    expect(saveButton(region).disabled).toBe(true);
    // ...and a later change elsewhere is followed again, because nothing is held.
    view.rerender(panel(moved({ "store-example": { customerAccountClientId: "shp_next" }, "old-shop": { wholesaleAdapter: "customerTag" } })));
    expect((within(group(region, "example.myshopify.com")).getByLabelText(label) as HTMLInputElement).value).toBe("shp_next");
  });
});

describe("who may change them", () => {
  it("shows the values and no controls without the publish part, App values' own gate", async () => {
    connect();
    render(panel(SITE_ROW, partsWithout("publish")));
    const region = await values();
    await waitFor(() => expect(within(region).getByText("example.myshopify.com", { selector: ".os-store-values-name" })).toBeTruthy());
    expect(within(region).getByText("shp_live")).toBeTruthy();
    expect(within(region).queryByRole("textbox")).toBeNull();
    expect(within(region).queryByRole("button", { name: "Save store values" })).toBeNull();
    expect(within(region).queryByRole("button", { name: /Add a value/ })).toBeNull();
    expect(within(region).queryByRole("button", { name: /Remove the values kept/ })).toBeNull();
  });
});

describe("App values says where a store's values went", () => {
  it("points a storefront's App values at the Store page", async () => {
    connect();
    const onOpenStore = vi.fn();
    render(withSession(<RuntimeSettingsPanel site={siteFromRow(SITE_ROW)} canEdit onOpenStore={onOpenStore} />));
    await click(screen.getByRole("button", { name: "Store page" }));
    expect(onOpenStore).toHaveBeenCalledTimes(1);
    expect(screen.getByText(/Values that belong to one store/)).toBeTruthy();
  });

  it("says nothing of the kind for a deployable that has no stores", () => {
    connect();
    render(withSession(<RuntimeSettingsPanel site={siteFromRow(siteRow({ id: "site-web", kind: "spa" }))} canEdit />));
    expect(screen.queryByText(/Values that belong to one store/)).toBeNull();
  });
});

describe("telling a storefront owner where their values go", () => {
  it("is acknowledged when the Store page is visible, and not while it is hidden", async () => {
    connect();
    const deployables = OS_REGISTRY.apps.find((app) => app.id === "deployables")!;
    const feature = deployables.attentionChanges!.find((change) => change.id === "deployables:store-values")!;
    expect(feature.target).toBe("shopify-store");
    const acknowledged: string[] = [];
    const connection = h.connection as ReturnType<typeof fakeConnection>;
    const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    vi.mocked(connection.query.executeNamed).mockImplementation(async (name, call, options) => {
      if (name === "myAttentionReceipts") return rowsResult([]);
      if (name === "acknowledgeAttention") {
        acknowledged.push(call);
        return rowsResult([]);
      }
      return original(name, call, options);
    });
    const apps = [{ ...deployables, attentionChanges: [feature] }];
    function Page({ visible }: { visible: boolean }) {
      return (
        <AttentionProvider apps={apps}>
          {visible ? (
            <ShopifyStorePanel site={siteFromRow(SITE_ROW)} canBind can={ALL_PARTS} trail={[]} back={{ label: "Storefront", onSelect: vi.fn() }} onWritten={vi.fn()} />
          ) : null}
        </AttentionProvider>
      );
    }
    const view = render(withSession(<Page visible={false} />));
    await act(async () => {});
    expect(acknowledged).toHaveLength(0);
    view.rerender(withSession(<Page visible />));
    await waitFor(() => expect(acknowledged.some((c) => c.includes('changeId: "deployables:store-values"'))).toBe(true));
  });
});
