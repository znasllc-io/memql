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
import { formatMoment } from "../../src/kit/format";
import { SHOP, click, fakeConnection, siteRow, storeStatisticsRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

// A STOREFRONT'S STORE STATISTICS: the button beside Statistics, for a holder
// of the sales part, on a storefront that has a store, and the page
// (apps/deployables/storeStatistics): its head and context, its notice, its
// eight figures and chart, its lists, and every state.

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
    const { connection, page } = await openStoreStatistics({ sites: [site], storeStatistics: { "store-example-dev": storeStatisticsRow({ storeId: "store-example-dev", destination: "previewBinding" }) } });
    expect(lastCall(connection)).toContain('storeId: "store-example-dev"');
    expect(within(page).queryByLabelText("Destination")).toBeNull();
  });
});

describe("the Store statistics page", () => {
  it("opens on Production under the deployable's trail, and offers Testing only when the two stores differ", async () => {
    const site = { ...SHOP, previewBinding: { storeId: "store-example-dev" } };
    const { connection, page } = await openStoreStatistics({ sites: [site], storeStatistics: { "store-example": storeStatisticsRow({ storeId: "store-example" }), "store-example-dev": storeStatisticsRow({ storeId: "store-example-dev", destination: "previewBinding", storeName: "Example Shop (development)" }) } });
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
    const { page } = await openStoreStatistics(seed({ timezone: null, periodZone: "UTC", start: "2026-11-02T00:00:00Z", historySince: "2026-09-12T00:00:00Z" }));
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

  it("shows a store with no row yet only the context it has, and that its orders are still being brought in", async () => {
    const { page } = await openStoreStatistics(seed({
      storeName: null, storeDomain: "acme-widgets.myshopify.com", currency: null, timezone: null, periodZone: null, development: null, taxesIncluded: null, olderOrderChangesNotRecorded: null,
      bucket: null, start: null, end: null, prevStart: null, historySince: null, lastChangeAt: null,
      recording: { state: "starting", reasons: [], since: null, blocked: false }, rebuilding: null, series: [],
    }));
    await waitFor(() => expect(captions(page).join(" ")).toContain("acme-widgets.myshopify.com"));
    // What is said of its state is the notice's (block 2), and nothing else.
    expect(page.querySelector(".os-notice")?.textContent).toBe("The store's orders are still being brought in.");
    const all = captions(page).join(" ");
    for (const text of ["Prices in", "Days in", "Orders since", "Last order change"]) expect(all).not.toContain(text);
    expect(page.querySelector('[aria-busy="true"]')).toBeNull();
    // No series, so no chart, and no figure offered as a choice that changes nothing.
    expect(page.querySelector(".os-time-chart")).toBeNull();
    expect(within(page).queryByRole("radio")).toBeNull();
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

// ---------------------------------------------------------------------------
// The figures, the chart, the lists and the states (blocks 2 to 8)
// ---------------------------------------------------------------------------

/** The kit's spelling of a figure nothing measured (kit `Measure`). */
const DASH = "—";
const BROUGHT_IN = "The store's orders are still being brought in.";

const figures = (page: HTMLElement) => within(page).findByRole("radiogroup", { name: "Sales figures" });
function figure(group: HTMLElement, label: string) {
  const el = [...group.querySelectorAll(".os-stat-figure")].find((f) => f.querySelector(".os-stat-figure-label")?.textContent === label);
  return { value: el?.querySelector(".os-stat-figure-value")?.textContent ?? "", reason: el?.querySelector(".os-figure-absent")?.getAttribute("title") ?? "", change: el?.querySelector(".os-stat-figure-change")?.textContent ?? "" };
}
const list = (page: HTMLElement, title: string) => within(page).getByRole("region", { name: title });
const WEEK = { // the week Task 18's first test reads
  figures: { totalSales: "81.53", netSales: "65.99", orders: 2, averageOrderValue: "51.00", grossSales: "109.99", discounts: "8.00", refunds: "36.00", unitsSold: 3 },
  series: ["02", "03", "04", "05", "06", "07", "08"].map((day, i) => ({ at: `2026-11-${day}T00:00:00+01:00`, totalSales: null, orders: i === 3 || i === 4 ? 1 : 0, grossSales: null, discounts: null, refunds: null, unitsSold: i === 3 ? 3 : i === 4 ? 1 : i === 5 ? -1 : 0, netSales: i === 3 ? "91.99" : i === 4 ? "10.00" : i === 5 ? "-36.00" : "0.00" })),
  buyers: { new: 1, returning: 1, unknown: 0 },
};

describe("the Store statistics figures, chart and lists", () => {
  it("draws eight figures in order, money in the store's currency", async () => {
    const group = await figures((await openStoreStatistics(seed(WEEK))).page);
    expect([...group.querySelectorAll(".os-stat-figure-label")].map((l) => l.textContent)).toEqual(["Total sales", "Net sales", "Orders", "Average order value", "Gross sales", "Discounts", "Refunds", "Units sold"]);
    expect(figure(group, "Total sales").value).toBe("€81.53");
    expect(figure(group, "Average order value").value).toBe("€51.00");
    expect(figure(group, "Orders").value).toBe("2");
    expect(within(group).queryByRole("radio", { name: /^Average order value/ })).toBeNull();   // the average is not chartable
  });

  it("says the store is still being brought in, and draws every figure absent with that reason", async () => {
    const { page } = await openStoreStatistics(seed({ recording: { state: "starting", reasons: [], since: null, blocked: false } }));
    const group = await figures(page);
    expect(page.querySelector(".os-notice-line")?.textContent).toBe(BROUGHT_IN);
    for (const label of ["Total sales", "Orders", "Units sold"]) expect(figure(group, label)).toMatchObject({ value: DASH, reason: BROUGHT_IN });
  });

  it("says a purging or rebuilding store is still being brought in, and draws no figure it holds", async () => {
    for (const state of ["purging", "rebuilding"]) {
      const { page } = await openStoreStatistics(seed({ ...WEEK, asOf: null, rebuilding: state === "rebuilding", recording: { state, reasons: [], since: "2026-11-07T09:00:00Z", blocked: true } }));
      const group = await figures(page);
      expect(page.querySelector(".os-notice-line")?.textContent).toBe(BROUGHT_IN);
      expect(page.querySelector(".os-notice")?.textContent ?? "").not.toContain("No figure can be given");
      for (const label of ["Total sales", "Net sales", "Orders", "Average order value", "Units sold"]) expect(figure(group, label)).toMatchObject({ value: DASH, reason: BROUGHT_IN });
      expect(list(page, "Buyers").textContent).toContain(BROUGHT_IN);
      cleanup();
    }
  });

  it("says a paused store's state in its own sentence, then the moment the figures are as of, and names the Store page", async () => {
    // The real shape: a paused store carries no reason.
    const { page } = await openStoreStatistics(seed({ ...WEEK, asOf: "2026-11-07T09:00:00Z", recording: { state: "paused", reasons: [], since: "2026-11-07T09:00:00Z", blocked: true } }));
    await figures(page);
    const notice = page.querySelector(".os-notice")?.textContent ?? "";
    expect(notice).toContain("Recording is paused for this store.");
    expect(notice).toContain(`These figures are as of ${formatMoment("2026-11-07T09:00:00Z")}.`);
    expect(notice).toContain("The Store page of this deployable says what fixes it.");
    expect(within(page.querySelector(".os-notice") as HTMLElement).queryByRole("button")).toBeNull();
  });

  it("says a store is not connected before the one reason it lists", async () => {
    // A not-connected store lists no reason but a failing privacy request.
    const privacy = "A privacy request for this store could not be completed.";
    const { page } = await openStoreStatistics(seed({ ...WEEK, asOf: "2026-11-06T12:00:00Z", recording: { state: "not connected", reasons: [{ code: "privacy_request_failing", sentence: privacy }], since: "2026-11-06T12:00:00Z", blocked: true } }));
    await figures(page);
    const notice = page.querySelector(".os-notice")?.textContent ?? "";
    expect(notice.indexOf("This store is not connected.")).toBeGreaterThanOrEqual(0);
    expect(notice.indexOf(privacy)).toBeGreaterThan(notice.indexOf("This store is not connected."));
    expect(notice).toContain(`These figures are as of ${formatMoment("2026-11-06T12:00:00Z")}.`);
  });

  it("lists a reason that freezes nothing and draws current figures with no as-of", async () => {
    for (const reason of [
      { code: "privacy_request_failing", sentence: "A privacy request for this store could not be completed." },
      { code: "approval_outdated", sentence: "This store approved an older list of permissions." },   // its concepts are products', which the figures do not read
    ]) {
      const { page } = await openStoreStatistics(seed({ ...WEEK, asOf: null, recording: { state: "needs attention", reasons: [reason], since: "2026-11-02T08:00:00Z", blocked: false } }));
      const group = await figures(page);
      const notice = page.querySelector(".os-notice")?.textContent ?? "";
      expect(notice).toContain(reason.sentence);
      expect(notice).not.toContain("These figures are as of");
      expect(figure(group, "Net sales").value).toBe("€65.99");
      cleanup();
    }
  });

  it("lists the reasons of a recording store too, in the engine's own sentences and with nothing of the page's", async () => {
    const sentence = "Some of this store's orders could not be read and are left out of these figures.";
    const { page } = await openStoreStatistics(seed({ ...WEEK, recording: { state: "recording", reasons: [{ code: "statistics_rows_unreadable", sentence }], since: "2026-11-02T08:00:00Z", blocked: false } }));
    const group = await figures(page);
    expect(page.querySelector(".os-notice")?.textContent).toBe(sentence);
    expect(figure(group, "Net sales").value).toBe("€65.99");
  });

  it("draws no notice for a recording store with nothing in the way", async () => {
    const { page } = await openStoreStatistics(seed(WEEK));
    await figures(page);
    expect(page.querySelector(".os-notice")).toBeNull();
  });

  it("draws a failing order check as of the walk's last clear run, not the verdict's since", async () => {
    // Needs attention since Monday for a privacy request; the order check failed on Wednesday.
    const { page } = await openStoreStatistics(seed({ ...WEEK, asOf: "2026-11-04T09:59:15Z", recording: { state: "needs attention", reasons: [
      { code: "privacy_request_failing", sentence: "A privacy request for this store could not be completed." },
      { code: "check_failing", sentence: "The last check of orders failed" },
    ], since: "2026-11-02T08:00:00Z", blocked: true } }));
    await figures(page);
    const notice = page.querySelector(".os-notice")?.textContent ?? "";
    expect(notice).toContain(`These figures are as of ${formatMoment("2026-11-04T09:59:15Z")}.`);
    expect(notice).not.toContain(formatMoment("2026-11-02T08:00:00Z"));
    expect(notice).toContain("A privacy request for this store could not be completed.");
  });

  it("says no figure can be given for a store blocked before any clear run", async () => {
    const blank = { totalSales: null, netSales: null, orders: null, averageOrderValue: null, grossSales: null, discounts: null, refunds: null, unitsSold: null };
    const { page } = await openStoreStatistics(seed({ figures: blank, asOf: null, recording: { state: "needs attention", reasons: [{ code: "check_failing", sentence: "The last check of orders failed" }], since: "2026-11-07T09:00:00Z", blocked: true } }));
    await figures(page);
    expect(page.querySelector(".os-notice")?.textContent ?? "").toContain("No figure can be given until this is fixed.");
  });

  it("says whether the store is recording is not known, and still draws the figures", async () => {
    const { page } = await openStoreStatistics(seed({ ...WEEK, recording: { state: "", reasons: [], since: null, blocked: false } }));
    const group = await figures(page);
    expect(page.querySelector(".os-notice-line")?.textContent).toBe("Whether this store is recording is not known.");
    expect(figure(group, "Net sales").value).toBe("€65.99");
  });

  it("draws the four figures a refund not yet read blanks as dashes with the reason", async () => {
    const { page } = await openStoreStatistics(seed({ ...WEEK, refundsNotRead: 1, figures: { ...WEEK.figures, refunds: null, netSales: null, totalSales: null, unitsSold: null } }));
    const group = await figures(page);
    for (const label of ["Refunds", "Net sales", "Total sales", "Units sold"]) expect(figure(group, label)).toMatchObject({ value: DASH, reason: "1 refund is still being read from Shopify." });
    expect(figure(group, "Gross sales").value).toBe("€109.99");
    expect(page.querySelector(".os-notice-line")?.textContent).toBe("1 refund is still being read from Shopify.");
  });

  it("says the store's state before the refunds still being read", async () => {
    const { page } = await openStoreStatistics(seed({ ...WEEK, refundsNotRead: 2, asOf: "2026-11-07T09:00:00Z", recording: { state: "paused", reasons: [], since: "2026-11-07T09:00:00Z", blocked: true } }));
    await figures(page);
    const notice = page.querySelector(".os-notice")?.textContent ?? "";
    expect(page.querySelector(".os-notice-line")?.textContent).toBe("Recording is paused for this store.");
    expect(notice.indexOf("2 refunds are still being read from Shopify.")).toBeGreaterThan(notice.indexOf("Recording is paused for this store."));
  });

  it("draws units as a dash with its own reason when a refund has more lines than were recorded", async () => {
    const group = await figures((await openStoreStatistics(seed({ ...WEEK, incompleteRefunds: 1, figures: { ...WEEK.figures, unitsSold: null } }))).page);
    expect(figure(group, "Units sold")).toMatchObject({ value: DASH, reason: "A refund in this period has more lines than were recorded." });
  });

  it("draws money as dashes with the reason when the currency is not known, and every figure when the zone is not", async () => {
    const noCurrency = await figures((await openStoreStatistics(seed({ currency: null, figures: { ...WEEK.figures, totalSales: null, netSales: null, averageOrderValue: null, grossSales: null, discounts: null, refunds: null } }))).page);
    expect(figure(noCurrency, "Net sales")).toMatchObject({ value: DASH, reason: "The store's currency has not been recorded yet." });
    expect(figure(noCurrency, "Orders").value).toBe("2");
    cleanup();
    const noZone = await figures((await openStoreStatistics(seed({ timezone: null, periodZone: "UTC" }))).page);
    expect(figure(noZone, "Orders")).toMatchObject({ value: DASH, reason: "The store's time zone has not been recorded yet." });
  });

  it("says the days are UTC's when the zone the store recorded did not load", async () => {
    const { page } = await openStoreStatistics(seed({ ...WEEK, timezone: "Mars/Olympus_Mons", periodZone: "UTC", start: "2026-11-02T00:00:00Z", series: WEEK.series.map((point, i) => ({ ...point, at: `2026-11-0${i + 2}T00:00:00Z` })) }));
    await figures(page);
    const line = captions(page).join(" ");
    expect(line).toContain("Days in UTC");
    expect(line).not.toContain("Mars/Olympus_Mons");
  });

  it("draws a floor as or more, with no change", async () => {
    const group = await figures((await openStoreStatistics(seed({ ...WEEK, floors: { grossSales: true }, incompleteOrders: 1, previous: { ...WEEK.figures, grossSales: "50.00" }, historySince: "2026-09-01T00:00:00Z" }))).page);
    expect(figure(group, "Gross sales").value).toBe("€109.99 or more");
    expect(figure(group, "Gross sales").change).toBe("");
  });

  it("draws a change against a period that can be compared, and none when it began before the orders did", async () => {
    const comparable = await figures((await openStoreStatistics(seed({ ...WEEK, previous: { ...WEEK.figures, orders: 1 }, historySince: "2026-09-01T00:00:00Z" }))).page);
    expect(figure(comparable, "Orders").change).toBe("+100%");
    cleanup();
    const tooEarly = await figures((await openStoreStatistics(seed({ ...WEEK, previous: { ...WEEK.figures, orders: 1 }, historySince: "2026-10-30T00:00:00Z" }))).page);
    expect(figure(tooEarly, "Orders").change).toBe("");
  });

  it("labels the chart's buckets in the store's time zone, draws the day below zero, and leaves gaps before the orders and after as-of", async () => {
    const series = WEEK.series.map((point, i) => (i === 0 || i === 6 ? { ...point, netSales: null } : point));
    const { page } = await openStoreStatistics(seed({ ...WEEK, series }));
    await click(within(await figures(page)).getByRole("radio", { name: /^Net sales/ }));
    const chart = page.querySelector(".os-time-chart") as HTMLElement;
    // "2026-11-02T00:00:00+01:00" is Nov 1 in UTC: the first label is the store's day.
    expect(chart.querySelector(".os-time-chart-times span")?.textContent).toBe("Nov 2");
    expect(chart.querySelector('.os-time-chart-grid[data-zero="true"]')).not.toBeNull();
    expect((chart.querySelector('path[data-series="current"]')?.getAttribute("d") ?? "").split("M").length - 1).toBe(1);   // one run of five days, a gap either side
    expect(chart.querySelector("figcaption")?.textContent).toContain("2 not measured");
  });

  it("draws the shapes of the figures, the chart and the lists while the first read is out", async () => {
    const { page } = await openStoreStatistics(seed(WEEK), {}, holdStoreReads);
    const group = await figures(page);
    expect(group.querySelectorAll('[aria-busy="true"]').length).toBe(8);
    expect(page.querySelector('.os-time-chart[aria-busy="true"]')).not.toBeNull();
    expect(list(page, "Products").querySelector('[aria-busy="true"]')).not.toBeNull();
    expect(list(page, "Buyers").querySelector('[aria-busy="true"]')).not.toBeNull();
  });

  it("says how many products there are and opens all of them as a page under the trail", async () => {
    const products = Array.from({ length: 25 }, (_, i) => ({ key: `gid://shopify/Product/${i + 1}`, title: `Inner tube ${i + 1}`, value: String(100 - i) }));
    const { page } = await openStoreStatistics(seed({ ...WEEK, productsBySales: products, productsByUnits: products, productCount: 31 }));
    await figures(page);
    expect(list(page, "Products").querySelectorAll(".os-ranked-label")).toHaveLength(8);
    expect(list(page, "Products").querySelector(".os-ranked-footer")?.textContent).toContain("The 8 best-selling of 31 products.");
    await click(within(list(page, "Products")).getByRole("button", { name: "All products" }));
    const all = await screen.findByRole("region", { name: `Store statistics for ${HOST}` });
    expect(within(within(all).getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((item) => (item.textContent ?? "").trim()).slice(-2)).toEqual(["Store statistics", "Products"]);
    expect(all.querySelectorAll(".os-ranked-label")).toHaveLength(25);
    expect(all.querySelector(".os-ranked-footer")?.textContent).toContain("The 25 best-selling of 31 products.");
  });

  it("ranks products by units when Units is chosen", async () => {
    const { page } = await openStoreStatistics(seed({ ...WEEK, productsBySales: [{ key: "a", title: "City bike", value: "80.00" }, { key: "b", title: "Bell", value: "19.99" }], productsByUnits: [{ key: "b", title: "Bell", value: 5 }, { key: "a", title: "City bike", value: 2 }], productCount: 2 }));
    await figures(page);
    expect(list(page, "Products").querySelector(".os-ranked-label")?.textContent).toBe("City bike");
    await click(within(within(page).getByRole("navigation", { name: "Products by" })).getByRole("button", { name: "Units" }));
    expect(list(page, "Products").querySelector(".os-ranked-label")?.textContent).toBe("Bell");
    expect(list(page, "Products").querySelector(".os-ranked-value")?.textContent).toBe("5");
  });

  it("writes the refunds that name no product as money, and says how many orders and refunds are short of lines", async () => {
    const { page } = await openStoreStatistics(seed({ ...WEEK, productsBySales: [{ key: "a", title: "City bike", value: "80.00" }], productCount: 1, refundsWithoutItem: "12.00", incompleteOrders: 3, incompleteRefunds: 1, floors: { grossSales: true } }));
    await figures(page);
    const footer = list(page, "Products").querySelector(".os-ranked-footer")?.textContent ?? "";
    expect(footer).toContain("Refunds of €12.00 name no product, so they are in Net sales and not here.");
    expect(footer).toContain("3 orders have more lines than were recorded, so these are floors.");
    expect(footer).toContain("1 refund has more lines than were recorded, so units are not exact.");
    expect(footer).not.toContain("12.00 EUR");
  });

  it("says nothing of refunds that name no product when they come to zero", async () => {
    for (const zero of ["0", "0.00"]) {
      const { page } = await openStoreStatistics(seed({ ...WEEK, productsBySales: [{ key: "a", title: "City bike", value: "80.00" }], productCount: 1, refundsWithoutItem: zero }));
      await figures(page);
      expect(list(page, "Products").querySelector(".os-ranked-footer")).toBeNull();
      cleanup();
    }
  });

  it("counts buyers as new, returning and with no customer recorded", async () => {
    const { page } = await openStoreStatistics(seed({ ...WEEK, buyers: { new: 4, returning: 2, unknown: 1 } }));
    await figures(page);
    expect([...list(page, "Buyers").querySelectorAll(".os-ranked-label")].map((l) => l.textContent)).toEqual(["New", "Returning", "No customer recorded"]);
  });

  it("draws the wholesale block only when the answer has one", async () => {
    const without = await openStoreStatistics(seed(WEEK));
    await figures(without.page);
    expect(within(without.page).queryByRole("region", { name: "Wholesale and retail" })).toBeNull();
    cleanup();
    const { page } = await openStoreStatistics(seed({ ...WEEK, wholesale: { wholesale: { orders: 1, netSales: "55.99" }, retail: { orders: 1, netSales: "10.00" } } }));
    await figures(page);
    expect([...list(page, "Wholesale and retail").querySelectorAll(".os-ranked-label")].map((l) => l.textContent)).toEqual(["Wholesale", "Retail"]);
    expect(list(page, "Wholesale and retail").textContent).toContain("€55.99");
    await click(within(within(page).getByRole("navigation", { name: "Wholesale and retail by" })).getByRole("button", { name: "Orders" }));
    expect([...list(page, "Wholesale and retail").querySelectorAll(".os-ranked-value")].map((v) => v.textContent)).toEqual(["1", "1"]);
  });

  it("says how many orders in another currency are not in the sums", async () => {
    const { page } = await openStoreStatistics(seed({ ...WEEK, otherCurrencies: [{ currency: "USD", orders: 2 }] }));
    await figures(page);
    expect(captions(page)).toContain("2 orders in USD are not in these sums.");
  });

  it("says no orders in this period, with a measured zero and a dash for the average", async () => {
    const zero = { totalSales: "0.00", netSales: "0", orders: 0, averageOrderValue: null, grossSales: "0.00", discounts: "0.00", refunds: "0.00", unitsSold: 0 };
    const { page } = await openStoreStatistics(seed({ figures: zero, buyers: { new: 0, returning: 0, unknown: 0 } }));
    const group = await figures(page);
    expect(figure(group, "Orders").value).toBe("0");
    expect(figure(group, "Net sales").value).toBe("€0.00");
    expect(figure(group, "Total sales").value).toBe("€0.00");
    expect(figure(group, "Average order value")).toMatchObject({ value: DASH, reason: "No orders in this period." });
    expect(list(page, "Products").textContent).toContain("No orders in this period.");
    expect(list(page, "Buyers").textContent).toContain("No orders in this period.");
  });

  it("draws no divider between rows and no pill, badge or toast", async () => {
    const { page } = await openStoreStatistics(seed({ ...WEEK, productsBySales: [{ key: "a", title: "City bike", value: "80.00" }], productCount: 1 }));
    await figures(page);
    expect(page.querySelector("hr, .os-divider, .os-pill, .os-badge, .os-chip, .os-toast")).toBeNull();
  });

  it("does not offer Try again while its retry is out", async () => {
    const { connection, page } = await openStoreStatistics({ sites: [SHOP], storeStatisticsError: "This deployable does not serve that store for you." });
    await waitFor(() => expect(within(page).getByRole("button", { name: "Try again" })).toBeTruthy());
    holdStoreReads(connection);
    await click(within(page).getByRole("button", { name: "Try again" }));
    expect(page.querySelector(".os-notice-line")?.textContent).toBe("This deployable does not serve that store for you.");
    expect(within(page).queryByRole("button", { name: "Try again" })).toBeNull();
  });
});
