import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useFakeDate } from "../readTheBar";
import { chooseOption } from "../selectControl";
import { seededAccessWithout } from "../seededAccess";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection hook is replaced, as in reviews.test.tsx; the fake beneath
// answers at `executeNamed`, so the call STRING is what is asserted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
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

async function openDeployable(s: FakeSeed, session: Parameters<typeof withSession>[1] = {}): Promise<{ connection: FakeConnection; page: HTMLElement }> {
  const connection = fakeConnection(s);
  h.connection = connection;
  render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />, { userId: "u-me", ...session }));
  await waitFor(() => expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"));
  await click((await screen.findByText(HOST)).closest("button"));
  return { connection, page: await screen.findByRole("region", { name: `Deployable ${HOST}` }) };
}

async function openStoreStatistics(s: FakeSeed = seed(), session: Parameters<typeof withSession>[1] = {}) {
  const { connection, page } = await openDeployable(s, session);
  await click(within(page).getByRole("button", { name: "Store statistics" }));
  return { connection, page: await screen.findByRole("region", { name: `Store statistics for ${HOST}` }) };
}

const captions = (page: HTMLElement) => [...page.querySelectorAll(".os-caption")].map((c) => c.textContent ?? "");
const lastCall = (connection: FakeConnection) => connection.callsNamed("storeStatistics").at(-1) ?? "";

useFakeDate();

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

  it("says a store with no row yet is still being brought in, and claims nothing it does not hold", async () => {
    const { page } = await openStoreStatistics(seed({
      storeName: null, storeDomain: null, currency: null, timezone: null, development: null, taxesIncluded: null, olderOrderChangesNotRecorded: null,
      bucket: null, start: null, end: null, prevStart: null, historySince: null, lastChangeAt: null,
      recording: { state: "starting", reasons: [], since: null, blocked: false }, rebuilding: null, series: [],
    }));
    await waitFor(() => expect(page.querySelector(".os-notice-line")?.textContent).toBe("The store's orders are still being brought in."));
    const all = captions(page).join(" ");
    for (const text of ["Prices in", "Days in", "Orders since", "Last order change"]) expect(all).not.toContain(text);
    expect(page.querySelector('[aria-busy="true"]')).toBeNull();
  });

  it("keeps a refusal standing while its retry is out, and draws no shape over it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { connection, page } = await openStoreStatistics({ sites: [SHOP], storeStatisticsError: "This deployable does not serve that store for you." });
    await waitFor(() => expect(page.querySelector(".os-notice-line")?.textContent).toBe("This deployable does not serve that store for you."));
    expect(page.querySelector('[aria-busy="true"]')).toBeNull();   // every kit skeleton is busy
    // The retry is held unanswered: the refusal is still the last word.
    const asked = connection.callsNamed("storeStatistics").length;
    const execute = connection.query.executeNamed.bind(connection.query) as (...args: unknown[]) => Promise<unknown>;
    connection.query.executeNamed = ((name: string, ...rest: unknown[]) => {
      // Recorded as asked, then never answered.
      const answer = execute(name, ...rest);
      if (name !== "storeStatistics") return answer;
      answer.catch(() => {});
      return new Promise(() => {});
    }) as typeof connection.query.executeNamed;
    await act(async () => { await vi.advanceTimersByTimeAsync(statisticsRefreshMs("7d") + 1_000); });
    await waitFor(() => expect(connection.callsNamed("storeStatistics").length).toBeGreaterThan(asked));
    expect(page.querySelector(".os-notice-line")?.textContent).toBe("This deployable does not serve that store for you.");
    expect(page.querySelector('[aria-busy="true"]')).toBeNull();
  });
});
