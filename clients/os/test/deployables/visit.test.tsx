import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { VisitSiteButton } from "../../src/apps/deployables/page/VisitSiteButton";
import { ALL_PARTS, NO_PARTS } from "../../src/apps/deployables/parts";
import { readinessFromRow } from "../../src/apps/deployables/preview/rows";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { SHOP, OPENED_PREVIEW, click, fakeConnection, previewReadinessRow, withSession } from "./harness";

const site = siteFromRow({ ...SHOP, previewBinding: { storeId: "store-example-dev" } });
const ready = readinessFromRow(previewReadinessRow({
  siteId: site.id, bundleRef: site.bundleRef, candidateRef: "blob://sites/site-shop/v2/", hasCandidate: true,
  storeId: "store-example", storeDomain: "example.myshopify.com",
  previewStoreId: "store-example-dev", previewStoreDomain: "example-dev.myshopify.com",
  testingUrl: `https://test--${site.hostname}/`, canPreview: true, previewRefusal: { code: "", message: "", remedy: "" },
}));

function mount(over: Partial<Parameters<typeof VisitSiteButton>[0]> = {}, previewOpenError?: string) {
  const connection = fakeConnection({ previewOpenError });
  h.connection = connection;
  const onOpenStore = vi.fn();
  const onClose = vi.fn();
  render(withSession(<VisitSiteButton site={site} name="Storefront" can={ALL_PARTS} readiness={ready} readinessError="" onWritten={onClose} onOpenStore={onOpenStore} {...over} />));
  return { connection, onOpenStore };
}

function newTab() {
  const tab = { opener: {}, closed: false, location: { replace: vi.fn() }, close: vi.fn() };
  vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
  return tab;
}

afterEach(() => vi.restoreAllMocks());

describe("storefront visit destinations", () => {
  it("offers Testing and Production with their own store, without opening the public site immediately", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    mount();
    await click(screen.getByRole("button", { name: "Visit Storefront" }));
    expect(screen.getByRole("dialog", { name: "Visit website" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open testing website" }).textContent).toContain("example-dev.myshopify.com");
    expect(screen.getByRole("button", { name: "Open production website" }).textContent).toContain("example.myshopify.com");
    expect(open).not.toHaveBeenCalled();
  });

  it("opens an existing testing version through its grant without changing the candidate or store", async () => {
    const tab = newTab();
    const { connection } = mount();
    await click(screen.getByRole("button", { name: "Visit Storefront" }));
    await click(screen.getByRole("button", { name: "Open testing website" }));
    await waitFor(() => expect(tab.location.replace).toHaveBeenCalledWith(OPENED_PREVIEW.url));
    expect(tab.opener).toBeNull();
    expect(connection.callsNamed("setSiteCandidate")).toHaveLength(0);
    expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(0);
    expect(connection.callsNamed("promoteSiteCandidate")).toHaveLength(0);
  });

  it("starts testing the published build when only a sandbox has been assigned", async () => {
    const tab = newTab();
    const { connection } = mount({ readiness: { ...ready, hasCandidate: false, candidateRef: "", canPreview: true } });
    await click(screen.getByRole("button", { name: "Visit Storefront" }));
    await click(screen.getByRole("button", { name: "Open testing website" }));
    await waitFor(() => expect(tab.location.replace).toHaveBeenCalled());
    expect(connection.callsNamed("setSiteCandidate")).toHaveLength(0);
    expect(connection.callsNamed("sitePreviewOpen")).toHaveLength(1);
  });

  it("opens Production directly and makes no cluster write", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const { connection } = mount();
    await click(screen.getByRole("button", { name: "Visit Storefront" }));
    await click(screen.getByRole("button", { name: "Open production website" }));
    expect(open).toHaveBeenCalledWith(`https://${site.hostname}/`, "_blank", "noopener,noreferrer");
    expect(connection.callsNamed("sitePreviewOpen")).toHaveLength(0);
    expect(connection.callsNamed("setSiteCandidate")).toHaveLength(0);
  });

  it("keeps a testing link when the browser blocks the new tab", async () => {
    vi.spyOn(window, "open").mockReturnValue(null);
    mount();
    await click(screen.getByRole("button", { name: "Visit Storefront" }));
    await click(screen.getByRole("button", { name: "Open testing website" }));
    expect((await screen.findByRole("link", { name: "Open testing website" })).getAttribute("href")).toBe(OPENED_PREVIEW.url);
  });

  it("shows a refused preview and closes the empty tab without navigating to production", async () => {
    const tab = newTab();
    mount({}, "The sandbox is unavailable");
    await click(screen.getByRole("button", { name: "Visit Storefront" }));
    await click(screen.getByRole("button", { name: "Open testing website" }));
    expect(await screen.findByText("The sandbox is unavailable")).toBeTruthy();
    expect(tab.close).toHaveBeenCalled();
    expect(tab.location.replace).not.toHaveBeenCalled();
  });

  it("opens an unconnected Testing website as design preview without asking for a version or store", async () => {
    const tab = newTab();
    const { connection } = mount({ readiness: { ...ready, previewStoreId: "", previewStoreDomain: "", hasCandidate: false, candidateRef: "" } });
    await click(screen.getByRole("button", { name: "Visit Storefront" }));
    expect(screen.getByRole("button", { name: "Open testing website" }).textContent).toContain("Design preview");
    await click(screen.getByRole("button", { name: "Open testing website" }));
    await waitFor(() => expect(tab.location.replace).toHaveBeenCalled());
    expect(connection.callsNamed("sitePreviewOpen")).toHaveLength(1);
    expect(connection.callsNamed("setSiteCandidate")).toHaveLength(0);
  });

  it("does not offer testing without permission or production before publication", async () => {
    mount({ can: NO_PARTS, site: { ...site, status: "draft" } });
    await click(screen.getByRole("button", { name: "Visit Storefront" }));
    expect(screen.getByText("Testing")).toBeTruthy();
    expect(screen.getByText("Production")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open testing website" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Open production website" })).toBeNull();
  });

  it("keeps ordinary websites as direct links", () => {
    mount({ site: { ...site, kind: "static" } });
    expect(screen.getByRole("link", { name: "Open Storefront in a new tab" }).getAttribute("href")).toBe(`https://${site.hostname}/`);
    expect(screen.queryByRole("button", { name: "Visit Storefront" })).toBeNull();
  });
});
