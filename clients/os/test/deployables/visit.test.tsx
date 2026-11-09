import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { DeployablePage } from "../../src/apps/deployables/page/DeployablePage";
import { VisitPage } from "../../src/apps/deployables/page/VisitPage";
import { VisitSiteButton } from "../../src/apps/deployables/page/VisitSiteButton";
import { ALL_PARTS, NO_PARTS } from "../../src/apps/deployables/parts";
import { readinessFromRow } from "../../src/apps/deployables/preview/rows";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { SHOP, OPENED_PREVIEW, click, fakeConnection, previewReadinessRow, withSession } from "./harness";

const SHOP_ROW = { ...SHOP, previewBinding: { storeId: "store-example-dev" } };
const site = siteFromRow(SHOP_ROW);
const READY_ROW = previewReadinessRow({
  siteId: site.id, bundleRef: site.bundleRef, candidateRef: "blob://sites/site-shop/v2/", hasCandidate: true,
  storeId: "store-example", storeDomain: "example.myshopify.com",
  previewStoreId: "store-example-dev", previewStoreDomain: "example-dev.myshopify.com",
  testingUrl: `https://test--${site.hostname}/`, canPreview: true, previewRefusal: { code: "", message: "", remedy: "" },
});
const ready = readinessFromRow(READY_ROW);

function mount(over: Partial<Parameters<typeof VisitPage>[0]> = {}, previewOpenError?: string) {
  const connection = fakeConnection({ previewOpenError });
  h.connection = connection;
  const onOpenStore = vi.fn();
  render(withSession(<VisitPage site={site} can={ALL_PARTS} readiness={ready} readinessError="" onWritten={vi.fn()} onOpenStore={onOpenStore} {...over} />));
  return { connection, onOpenStore };
}

function newTab() {
  const tab = { opener: {}, closed: false, location: { replace: vi.fn() }, close: vi.fn() };
  vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
  return tab;
}

/** The trail a page's Head names, crumb by crumb. */
function crumbs(page: HTMLElement): string[] {
  return within(within(page).getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((item) => (item.textContent ?? "").trim());
}

afterEach(() => vi.restoreAllMocks());

// VISIT IS A PAGE (the owner, 2026-11-08; design record
// 2026-11-08-site-and-email-statistics, section 10). It used to ask in a
// dialog over the deployable's page; it takes that page's place now, under a
// trail naming where the person is, and Back steps out of it.
describe("Visit on a storefront", () => {
  it("opens a page in the deployable's place with Testing and Production, and no dialog", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    h.connection = fakeConnection({ sites: [SHOP_ROW], previewReadiness: { [site.id]: READY_ROW } });
    render(withSession(<DeployablePage site={site} pkg={null} credentials={[]} viewerUserId="u-me" nameOf={() => ""} can={ALL_PARTS} clusterDomain="memql.example.com" onBack={vi.fn()} />));
    const deployable = await screen.findByRole("region", { name: "Deployable shop.memql.example.com" });
    await click(within(deployable).getByRole("button", { name: "Visit Storefront" }));

    const visit = await screen.findByRole("region", { name: "Visit for shop.memql.example.com" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("region", { name: "Deployable shop.memql.example.com" })).toBeNull();
    expect(crumbs(visit)).toEqual(["Deployables", "Storefront", "Visit"]);
    expect((await within(visit).findByRole("button", { name: "Open testing website" })).textContent).toContain("example-dev.myshopify.com");
    expect(within(visit).getByRole("button", { name: "Open production website" }).textContent).toContain("example.myshopify.com");
    expect(open).not.toHaveBeenCalled();

    await click(within(visit).getByRole("button", { name: "Back to Storefront" }));
    expect(await screen.findByRole("region", { name: "Deployable shop.memql.example.com" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Visit for shop.memql.example.com" })).toBeNull();
  });
});

describe("the Visit control", () => {
  it("on a storefront, asks for the Visit page and opens no dialog", async () => {
    const onOpenVisit = vi.fn();
    render(withSession(<VisitSiteButton site={site} name="Storefront" onOpenVisit={onOpenVisit} />));
    const visit = screen.getByRole("button", { name: "Visit Storefront" });
    expect(visit.getAttribute("aria-haspopup")).toBeNull();
    await click(visit);
    expect(onOpenVisit).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("keeps ordinary websites as direct links", () => {
    const onOpenVisit = vi.fn();
    render(withSession(<VisitSiteButton site={{ ...site, kind: "static" }} name="Storefront" onOpenVisit={onOpenVisit} />));
    expect(screen.getByRole("link", { name: "Open Storefront in a new tab" }).getAttribute("href")).toBe(`https://${site.hostname}/`);
    expect(screen.queryByRole("button", { name: "Visit Storefront" })).toBeNull();
    expect(onOpenVisit).not.toHaveBeenCalled();
  });
});

describe("storefront visit destinations", () => {
  it("offers Testing and Production with their own store, without opening the public site immediately", () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    mount();
    expect(screen.getByRole("button", { name: "Open testing website" }).textContent).toContain("example-dev.myshopify.com");
    expect(screen.getByRole("button", { name: "Open production website" }).textContent).toContain("example.myshopify.com");
    expect(open).not.toHaveBeenCalled();
  });

  it("opens an existing testing version through its grant without changing the candidate or store", async () => {
    const tab = newTab();
    const { connection } = mount();
    await click(screen.getByRole("button", { name: "Open testing website" }));
    await waitFor(() => expect(tab.location.replace).toHaveBeenCalledWith(OPENED_PREVIEW.url));
    expect(tab.opener).toBeNull();
    expect(connection.callsNamed("setSiteCandidate")).toHaveLength(0);
    expect(connection.callsNamed("updateSiteStoreBinding")).toHaveLength(0);
    expect(connection.callsNamed("promoteSiteCandidate")).toHaveLength(0);
    // A page is where the person is: opening the website leaves it standing.
    expect(screen.getByRole("button", { name: "Open production website" })).toBeTruthy();
  });

  it("starts testing the published build when only a sandbox has been assigned", async () => {
    const tab = newTab();
    const { connection } = mount({ readiness: { ...ready, hasCandidate: false, candidateRef: "", canPreview: true } });
    await click(screen.getByRole("button", { name: "Open testing website" }));
    await waitFor(() => expect(tab.location.replace).toHaveBeenCalled());
    expect(connection.callsNamed("setSiteCandidate")).toHaveLength(0);
    expect(connection.callsNamed("sitePreviewOpen")).toHaveLength(1);
  });

  it("opens Production directly and makes no cluster write", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const { connection } = mount();
    await click(screen.getByRole("button", { name: "Open production website" }));
    expect(open).toHaveBeenCalledWith(`https://${site.hostname}/`, "_blank", "noopener,noreferrer");
    expect(connection.callsNamed("sitePreviewOpen")).toHaveLength(0);
    expect(connection.callsNamed("setSiteCandidate")).toHaveLength(0);
  });

  it("keeps a testing link when the browser blocks the new tab", async () => {
    vi.spyOn(window, "open").mockReturnValue(null);
    mount();
    await click(screen.getByRole("button", { name: "Open testing website" }));
    expect((await screen.findByRole("link", { name: "Open testing website" })).getAttribute("href")).toBe(OPENED_PREVIEW.url);
  });

  it("shows a refused preview and closes the empty tab without navigating to production", async () => {
    const tab = newTab();
    mount({}, "The sandbox is unavailable");
    await click(screen.getByRole("button", { name: "Open testing website" }));
    expect(await screen.findByText("The sandbox is unavailable")).toBeTruthy();
    expect(tab.close).toHaveBeenCalled();
    expect(tab.location.replace).not.toHaveBeenCalled();
  });

  it("opens an unconnected Testing website as design preview without asking for a version or store", async () => {
    const tab = newTab();
    const { connection } = mount({ readiness: { ...ready, previewStoreId: "", previewStoreDomain: "", hasCandidate: false, candidateRef: "" } });
    expect(screen.getByRole("button", { name: "Open testing website" }).textContent).toContain("Design preview");
    await click(screen.getByRole("button", { name: "Open testing website" }));
    await waitFor(() => expect(tab.location.replace).toHaveBeenCalled());
    expect(connection.callsNamed("sitePreviewOpen")).toHaveLength(1);
    expect(connection.callsNamed("setSiteCandidate")).toHaveLength(0);
  });

  it("does not offer testing without permission or production before publication", () => {
    mount({ can: NO_PARTS, site: { ...site, status: "draft" } });
    expect(screen.getByText("Testing")).toBeTruthy();
    expect(screen.getByText("Production")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open testing website" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Open production website" })).toBeNull();
  });

  it("says why Testing needs attention, with the way to the store for whoever holds it", async () => {
    const refusal = { code: "no_preview_binding", message: "this storefront has no development store on its preview binding.", remedy: "" };
    const { onOpenStore } = mount({ readiness: { ...ready, previewRefusal: refusal } });
    expect(screen.getByText("Testing needs attention.")).toBeTruthy();
    await click(screen.getByRole("button", { name: "Open store settings" }));
    expect(onOpenStore).toHaveBeenCalledTimes(1);
  });
});
