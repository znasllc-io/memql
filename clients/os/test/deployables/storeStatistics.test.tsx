import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useFakeDate } from "../readTheBar";
import { chooseOption } from "../selectControl";
import { installSeededAccess, seededAccessWithout } from "../seededAccess";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection hook is replaced, as in reviews.test.tsx; the fake beneath
// answers at `executeNamed`, so the call STRING is what is asserted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { DeployablePage } from "../../src/apps/deployables/page/DeployablePage";
import { partsWithout } from "../../src/apps/deployables/parts";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { setEffectiveCapabilities, type OrganizationCapability } from "../../src/system/roles";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { statisticsRefreshMs } from "../../src/apps/deployables/statistics/useSiteStatistics";
import { SHOP, click, fakeConnection, siteRow, storeStatisticsRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

// A STOREFRONT'S STORE STATISTICS: the button beside Statistics, for a holder
// of the sales part, on a storefront that has a store, and the page's head and
// context (apps/deployables/storeStatistics). The figures are the next task's.

const HOST = "shop.memql.example.com";
const OLDER = "Refunds, edits and cancellations of orders placed more than 60 days ago are not recorded.";

function seed(over: Partial<Row> = {}, more: Partial<FakeSeed> = {}): FakeSeed {
  return { sites: [SHOP], storeStatistics: { "store-example": storeStatisticsRow({ storeId: "store-example", ...over }) }, ...more };
}

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

async function openDeployable(s: FakeSeed, session: Parameters<typeof withSession>[1] = {}, prepare?: (connection: FakeConnection) => void, organizations?: OrganizationCapability[]): Promise<{ connection: FakeConnection; page: HTMLElement }> {
  const connection = fakeConnection(s);
  prepare?.(connection);
  h.connection = connection;
  const content = withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />, { userId: "u-me", ...session });
  // Organization decisions are installed after the session's global set, as parts.test.tsx does.
  if (organizations !== undefined) setEffectiveCapabilities(session.capabilities ?? [], organizations);
  render(content);
  await waitFor(() => expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"));
  await click((await screen.findByText(HOST)).closest("button"));
  return { connection, page: await screen.findByRole("region", { name: `Deployable ${HOST}` }) };
}

async function openStoreStatistics(s: FakeSeed = seed(), session: Parameters<typeof withSession>[1] = {}, prepare?: (connection: FakeConnection) => void) {
  const { connection, page } = await openDeployable(s, session, prepare);
  await click(within(page).getByRole("button", { name: "Store statistics" }));
  return { connection, page: await screen.findByRole("region", { name: `Store statistics for ${HOST}` }) };
}

/** What each caption reads, its non-breaking spaces read as spaces. */
const captions = (page: HTMLElement) => rawCaptions(page).map((c) => c.replace(/\u00a0/g, " "));
const rawCaptions = (page: HTMLElement) => [...page.querySelectorAll(".os-caption")].map((c) => c.textContent ?? "");

/** Holds every `storeStatistics` read unanswered (each is still recorded as asked). */
function holdStoreReads(connection: FakeConnection): void {
  const execute = connection.query.executeNamed.bind(connection.query) as (...args: unknown[]) => Promise<unknown>;
  connection.query.executeNamed = ((name: string, ...rest: unknown[]) => {
    const answer = execute(name, ...rest);
    if (name !== "storeStatistics") return answer;
    answer.catch(() => {});
    return new Promise(() => {});
  }) as typeof connection.query.executeNamed;
}
const lastCall = (connection: FakeConnection) => connection.callsNamed("storeStatistics").at(-1) ?? "";

useFakeDate();

afterEach(() => {
  installSeededAccess("owner");
});

beforeEach(() => {
  vi.setSystemTime(Date.parse("2026-11-08T10:17:00Z"));
  localStorage.clear();
  sessionStorage.clear();
  h.connection = null;
});

describe("the Store statistics button", () => {
  it("draws the button after Statistics for a storefront with a store, to a holder of the sales part", async () => {
    const { page } = await openDeployable(seed());
    const buttons = within(page).getAllByRole("button");
    expect(buttons.indexOf(within(page).getByRole("button", { name: "Store statistics" }))).toBe(buttons.indexOf(within(page).getByRole("button", { name: "Statistics" })) + 1);
  });

  it("draws no button without the sales part", async () => {
    const { page } = await openDeployable(seed(), { role: "developer", capabilities: seededAccessWithout("developer", "app:deployables/sales") });
    expect(within(page).getByRole("button", { name: "Statistics" })).toBeTruthy();
    expect(within(page).queryByRole("button", { name: "Store statistics" })).toBeNull();
  });

  it("draws no button for a static site, for a system-owned one, and with no store on either destination", async () => {
    for (const site of [siteRow({ id: "site-shop", hostname: HOST, kind: "spa" }), { ...SHOP, systemOwned: true }, { ...SHOP, binding: {}, previewBinding: {} }]) {
      const { page } = await openDeployable({ sites: [site] });
      expect(within(page).queryByRole("button", { name: "Store statistics" })).toBeNull();
      cleanup();
    }
  });

  it("draws Store statistics and no Statistics on a draft storefront with a store, right after Logs", async () => {
    const { page } = await openDeployable({ sites: [{ ...SHOP, status: "draft" }] });
    expect(within(page).queryByRole("button", { name: "Statistics" })).toBeNull();
    const buttons = within(page).getAllByRole("button");
    const logs = buttons.findIndex((b) => (b.getAttribute("aria-label") ?? "").startsWith("Logs for"));
    expect(buttons.indexOf(within(page).getByRole("button", { name: "Store statistics" }))).toBe(logs + 1);
  });

  it("judges the sales part in the site's own organization", async () => {
    const allow = (accountId: string, verb: string, resource: string, effect: "allow" | "deny" = "allow"): OrganizationCapability => ({ accountId, verb, resource, effect });
    const organizations = [
      ...["acme", "beta"].flatMap((id) => [allow(id, "read", "data"), allow(id, "read", "app:deployables"), allow(id, "update", "data")]),
      allow("acme", "execute", "app:deployables/sales"),
      allow("beta", "execute", "app:deployables/sales", "deny"),
    ];
    // Not held globally: only an organization's decision can draw the button.
    const session = { role: "writer", capabilities: seededAccessWithout("developer", "app:deployables/sales") };
    const acme = await openDeployable({ sites: [{ ...SHOP, accountId: "acme" }] }, session, undefined, organizations);
    expect(within(acme.page).getByRole("button", { name: "Store statistics" })).toBeTruthy();
    cleanup();
    const beta = await openDeployable({ sites: [{ ...SHOP, accountId: "beta" }] }, session, undefined, organizations);
    expect(within(beta.page).queryByRole("button", { name: "Store statistics" })).toBeNull();
  });

  it("does not ask for a store's figures without the sales part", async () => {
    const { connection } = await openDeployable(seed(), { role: "developer", capabilities: seededAccessWithout("developer", "app:deployables/sales") });
    expect(connection.callsNamed("storeStatistics")).toEqual([]);
    cleanup();
    // Even a page opened straight on Store statistics asks nothing without it.
    const direct = fakeConnection(seed());
    h.connection = direct;
    render(withSession(<DeployablePage site={siteFromRow(SHOP)} pkg={null} credentials={[]} viewerUserId="u-me" nameOf={() => ""} can={partsWithout("sales")} clusterDomain="memql.example.com" onBack={() => {}} initialDetail="storeStatistics" />, { userId: "u-me" }));
    await act(async () => { await Promise.resolve(); });
    expect(direct.callsNamed("storeStatistics")).toEqual([]);
    expect(screen.queryByRole("region", { name: `Store statistics for ${HOST}` })).toBeNull();
    expect(screen.getByRole("region", { name: `Deployable ${HOST}` })).toBeTruthy();
  });

  it("draws the button when only Testing has a store, and opens on it", async () => {
    const site = { ...SHOP, binding: {}, previewBinding: { storeId: "store-example-dev" } };
    const { connection, page } = await openStoreStatistics({ sites: [site], storeStatistics: { "store-example-dev": storeStatisticsRow({ storeId: "store-example-dev", destination: "testing" }) } });
    expect(lastCall(connection)).toContain('storeId: "store-example-dev"');
    expect(within(page).queryByLabelText("Destination")).toBeNull();
  });
});

describe("the Store statistics page", () => {
  it("opens on Production under the deployable's trail, and offers Testing only when the two stores differ", async () => {
    const site = { ...SHOP, previewBinding: { storeId: "store-example-dev" } };
    const { connection, page } = await openStoreStatistics({ sites: [site], storeStatistics: { "store-example": storeStatisticsRow({ storeId: "store-example" }), "store-example-dev": storeStatisticsRow({ storeId: "store-example-dev", destination: "testing", storeName: "Example Shop (development)" }) } });
    expect(within(within(page).getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((item) => (item.textContent ?? "").trim())).toEqual(["Deployables", "Storefront", "Store statistics"]);
    expect(lastCall(connection)).toContain('storeId: "store-example"');
    chooseOption(within(page).getByLabelText("Destination"), "Testing");
    await waitFor(() => expect(lastCall(connection)).toContain('storeId: "store-example-dev"'));
    await waitFor(() => expect(captions(page).join(" ")).toContain("Example Shop (development)"));
  });

  it("says Production and Testing, with no control, when both name one store", async () => {
    const site = { ...SHOP, previewBinding: { storeId: "store-example" } };
    const { page } = await openStoreStatistics({ sites: [site], storeStatistics: { "store-example": storeStatisticsRow({ storeId: "store-example", destination: "both" }) } });
    await waitFor(() => expect(captions(page).join(" ")).toContain("Production and Testing"));
    expect(within(page).queryByLabelText("Destination")).toBeNull();
  });

  it("asks for the remembered period and asks again when it changes", async () => {
    const { connection, page } = await openStoreStatistics();
    expect(lastCall(connection)).toContain('period: "7d"');
    chooseOption(within(page).getByLabelText("Period"), "Last 30 days");
    await waitFor(() => expect(lastCall(connection)).toContain('period: "30d"'));
  });

  it("names the store, its currency, its days, since when and how fresh", async () => {
    const { page } = await openStoreStatistics(seed({ historySince: "2026-09-11T22:00:00Z", lastChangeAt: "2026-11-08T10:13:00Z" }));
    const line = await waitFor(() => { const all = captions(page).join(" | "); expect(all).toContain("Example Shop"); return all; });
    for (const part of ["Prices in EUR", "Days in Europe/Madrid", "Orders since Sep 12", "Last order change recorded 4m ago"]) expect(line).toContain(part);
  });

  it("says the days are UTC's when the answer names no zone", async () => {
    const { page } = await openStoreStatistics(seed({ timezone: null, start: "2026-11-02T00:00:00Z", historySince: "2026-09-12T00:00:00Z" }));
    await waitFor(() => expect(captions(page).join(" ")).toContain("Days in UTC"));
    expect(captions(page).join(" ")).toContain("Orders since Sep 12");
  });

  it("draws the development line, the tax line and the line about older orders, each only when it applies", async () => {
    const plain = await openStoreStatistics();
    await waitFor(() => expect(captions(plain.page).join(" ")).toContain("Prices in EUR"));
    for (const text of ["This is a development store", "Prices on this store include tax.", OLDER]) expect(captions(plain.page).join(" ")).not.toContain(text);
    cleanup();
    const all = await openStoreStatistics(seed({ development: true, taxesIncluded: true, olderOrderChangesNotRecorded: true }));
    await waitFor(() => expect(captions(all.page)).toContain(OLDER));
    expect(captions(all.page)).toContain("This is a development store: every order on it is a test order, and they are counted.");
    expect(captions(all.page)).toContain("Prices on this store include tax.");
  });

  it("shows a store with no row yet only the context it has, and says nothing of its state", async () => {
    const { page } = await openStoreStatistics(seed({
      storeName: null, storeDomain: "acme-widgets.myshopify.com", currency: null, timezone: null, development: null, taxesIncluded: null, olderOrderChangesNotRecorded: null,
      bucket: null, start: null, end: null, prevStart: null, historySince: null, lastChangeAt: null,
      recording: { state: "starting", reasons: [], since: null, blocked: false }, rebuilding: null, series: [],
    }));
    await waitFor(() => expect(captions(page).join(" ")).toContain("acme-widgets.myshopify.com"));
    expect(page.querySelector(".os-notice")).toBeNull();
    const all = captions(page).join(" ");
    for (const text of ["Prices in", "Days in", "Orders since", "Last order change"]) expect(all).not.toContain(text);
    expect(page.querySelector('[aria-busy="true"]')).toBeNull();
  });

  it("never breaks the context line inside one of its phrases", async () => {
    const { page } = await openStoreStatistics(seed({ historySince: "2026-09-11T22:00:00Z", lastChangeAt: "2026-11-08T10:13:00Z" }));
    const line = await waitFor(() => { const all = rawCaptions(page).join(" | "); expect(all).toContain("EUR"); return all; });
    for (const phrase of ["Prices\u00a0in\u00a0EUR", "Days\u00a0in\u00a0Europe/Madrid", "Orders\u00a0since\u00a0Sep\u00a012", "Last\u00a0order\u00a0change\u00a0recorded\u00a04m\u00a0ago"]) expect(line).toContain(phrase);
  });

  it("writes the year of the first day when it is not this year's, in the store's own days", async () => {
    const older = await openStoreStatistics(seed({ historySince: "2025-09-11T22:00:00Z" }));
    await waitFor(() => expect(captions(older.page).join(" ")).toContain("Orders since Sep 12, 2025"));
    cleanup();
    // 23:30 UTC on Dec 31 is already Jan 1 in Madrid: this year's day, so no year.
    const turned = await openStoreStatistics(seed({ historySince: "2025-12-31T23:30:00Z" }));
    await waitFor(() => expect(captions(turned.page).join(" ")).toContain("Orders since Jan 1"));
    expect(captions(turned.page).join(" ")).not.toContain("Jan 1, 2026");
  });

  it("offers both destinations from the site's own row before any answer", async () => {
    const site = { ...SHOP, previewBinding: { storeId: "store-example-dev" } };
    const { connection, page } = await openStoreStatistics({ sites: [site] }, {}, holdStoreReads);
    expect(lastCall(connection)).toContain('storeId: "store-example"');
    expect(within(page).getByLabelText("Destination")).toBeTruthy();
  });

  it("keeps the other destination within reach after a refusal", async () => {
    const site = { ...SHOP, previewBinding: { storeId: "store-example-dev" } };
    const { connection, page } = await openStoreStatistics({ sites: [site], storeStatisticsError: "This deployable does not serve that store for you." });
    await waitFor(() => expect(page.querySelector(".os-notice-line")?.textContent).toBe("This deployable does not serve that store for you."));
    chooseOption(within(page).getByLabelText("Destination"), "Testing");
    await waitFor(() => expect(lastCall(connection)).toContain('storeId: "store-example-dev"'));
  });

  it("lets the person try a refused read again", async () => {
    const { connection, page } = await openStoreStatistics({ sites: [SHOP], storeStatisticsError: "This deployable does not serve that store for you." });
    await waitFor(() => expect(page.querySelector(".os-notice-line")?.textContent).toBe("This deployable does not serve that store for you."));
    const asked = connection.callsNamed("storeStatistics").length;
    await click(within(page).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(connection.callsNamed("storeStatistics").length).toBe(asked + 1));
  });

  it("leaves a dropped connection to the shell's own notice", async () => {
    const { connection, page } = await openStoreStatistics(seed(), {}, (c) => {
      const execute = c.query.executeNamed.bind(c.query) as (...args: unknown[]) => Promise<unknown>;
      c.query.executeNamed = ((name: string, ...rest: unknown[]) => {
        if (name !== "storeStatistics") return execute(name, ...rest);
        void execute(name, ...rest);
        return Promise.reject(Object.assign(new Error("connection closed"), { reason: "transport" }));
      }) as typeof c.query.executeNamed;
    });
    await waitFor(() => expect(connection.callsNamed("storeStatistics").length).toBeGreaterThan(0));
    await act(async () => { await Promise.resolve(); });
    expect(page.querySelector(".os-notice")).toBeNull();
    expect(page.querySelector('[aria-busy="true"]')).toBeNull();
  });

  it("keeps a refusal standing while its retry is out, and draws no shape over it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { connection, page } = await openStoreStatistics({ sites: [SHOP], storeStatisticsError: "This deployable does not serve that store for you." });
    await waitFor(() => expect(page.querySelector(".os-notice-line")?.textContent).toBe("This deployable does not serve that store for you."));
    expect(page.querySelector('[aria-busy="true"]')).toBeNull();   // every kit skeleton is busy
    // The retry is held unanswered: the refusal is still the last word.
    const asked = connection.callsNamed("storeStatistics").length;
    holdStoreReads(connection);
    await act(async () => { await vi.advanceTimersByTimeAsync(statisticsRefreshMs("7d") + 1_000); });
    await waitFor(() => expect(connection.callsNamed("storeStatistics").length).toBeGreaterThan(asked));
    expect(page.querySelector(".os-notice-line")?.textContent).toBe("This deployable does not serve that store for you.");
    expect(page.querySelector('[aria-busy="true"]')).toBeNull();
  });
});
