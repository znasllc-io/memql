import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown, openApp: vi.fn() }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/chrome/state", async (importOriginal) => {
  const real = await importOriginal<typeof import("../../src/chrome/state")>();
  return { ...real, useOs: () => ({ actions: { openApp: h.openApp } }), useOsIfPresent: () => null };
});

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { siteFromRow } from "../../src/apps/deployables/rows";
import { ShopifyStorePanel } from "../../src/apps/deployables/store/ShopifyStorePanel";
import { readTheBar } from "../readTheBar";
import {
  click,
  fakeConnection,
  previewReadinessRow,
  rowsResult,
  SHOP,
  siteRow,
  storeVerdictRow,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// PAUSE RECORDING AND RESUME ARE THE ENGINE'S BUILTIN (Task 10b).
//
// The store row's own write is a cluster owner's, so a store's owner who is not
// one was offered both acts and refused by the row. The page now asks
// shopifyStoreSetRecording, which asks who is calling and writes as the
// deployment; the client mutation setStoreStatus is never sent from here.

afterEach(() => {
  h.connection = null;
  h.openApp.mockClear();
});

const ID = "acme-widgets";

const OWN: Row = {
  id: ID,
  domain: "acme-widgets.myshopify.com",
  adminTokenRef: "SHOPIFY_ACME-WIDGETS_ADMIN_TOKEN",
  storefrontTokenRef: "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN",
  status: "live",
  isDevelopment: false,
};

function seedFor(state: string, over: Partial<FakeSeed> = {}, status = state === "paused" ? "paused" : "live"): FakeSeed {
  return {
    sites: [siteRow({ ...SHOP, binding: { storeId: ID } } as never)],
    stores: [{ ...OWN, status } as never],
    previewReadiness: {
      "site-shop": previewReadinessRow({ siteId: "site-shop", storeId: ID, storeReadable: true, storeHasStorefrontToken: true } as never),
    },
    storeVerdict: { [ID]: storeVerdictRow({ storeId: ID, domain: "acme-widgets.myshopify.com", state }) },
    ...over,
  };
}

function openPage(seed: FakeSeed): FakeConnection {
  const connection = fakeConnection(seed);
  const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
  vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
    if (name === "externalConnectionsMine") return rowsResult([]);
    return original(name, call, options);
  });
  h.connection = connection;
  const site = (seed.sites ?? [])[0]!;
  render(withSession(<ShopifyStorePanel site={siteFromRow(site)} canBind trail={[]} back={{ label: "Storefront", onSelect: vi.fn() }} onWritten={vi.fn()} />));
  return connection;
}

function bar(): HTMLElement {
  return document.querySelector(".os-actbar") as HTMLElement;
}

async function press(label: string): Promise<void> {
  await waitFor(() => expect(within(bar()).getByRole("button", { name: label })).toBeTruthy());
  await readTheBar();
  await click(within(bar()).getByRole("button", { name: label }));
}

describe("the Store page's pause and resume", () => {
  it("pauses through shopifyStoreSetRecording, never through setStoreStatus", async () => {
    const connection = openPage(seedFor("recording"));
    await press("Pause recording");
    await waitFor(() => expect(connection.callsNamed("shopifyStoreSetRecording")).toHaveLength(1));
    expect(connection.callsNamed("shopifyStoreSetRecording")[0]).toContain(`storeId: "${ID}"`);
    expect(connection.callsNamed("shopifyStoreSetRecording")[0]).toContain("recording: false");
    expect(connection.callsNamed("setStoreStatus")).toHaveLength(0);
  });

  it("pauses a store Connect Shopify left configured, whose verdict says recording", async () => {
    const connection = openPage(seedFor("recording", {}, "configured"));
    await press("Pause recording");
    await waitFor(() => expect(connection.callsNamed("shopifyStoreSetRecording")).toHaveLength(1));
    expect(connection.callsNamed("shopifyStoreSetRecording")[0]).toContain("recording: false");
    expect(document.querySelector(".os-notice[data-tone='error']")).toBeNull();
    expect(screen.queryByText(/cannot be paused or resumed/)).toBeNull();
  });

  it("resumes through shopifyStoreSetRecording", async () => {
    const connection = openPage(seedFor("paused"));
    await press("Resume");
    await waitFor(() => expect(connection.callsNamed("shopifyStoreSetRecording")).toHaveLength(1));
    expect(connection.callsNamed("shopifyStoreSetRecording")[0]).toContain(`storeId: "${ID}"`);
    expect(connection.callsNamed("shopifyStoreSetRecording")[0]).toContain("recording: true");
    expect(connection.callsNamed("setStoreStatus")).toHaveLength(0);
  });

  it("shows the engine's refusal when a pause is refused", async () => {
    const refusal = "shopify: shopifyStoreSetRecording is for the store's owner or a cluster owner";
    const connection = openPage(seedFor("recording", { storeSetRecordingError: refusal }));
    await press("Pause recording");
    const shown = await screen.findByText(refusal);
    expect(shown.closest(".os-notice")).not.toBeNull();
    expect(connection.callsNamed("setStoreStatus")).toHaveLength(0);
  });
});
