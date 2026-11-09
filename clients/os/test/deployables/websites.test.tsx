import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { DEFAULT_DEPLOYABLES_SETTINGS, LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import type { OsAppProps } from "../../src/system/registry";
import { chooseOption, openSelect, selectedLabel } from "../selectControl";
import { PLATFORM_SITE, click, fakeConnection, siteRow, siteStatisticsRow, withSession, type FakeSeed } from "./harness";

// Every website a person may see, together, in the Deployables Overview (the
// 2026-11-08 statistics design, section 9): the figures summed over the
// websites in scope, the chart, each website ranked, and the sources and
// countries of all of them -- read under the client the person chose, where
// they have a choice (D13). The read narrows on the server (D11); what is
// asserted here is what the page asks for and what it draws.

const SHOP = siteRow({ id: "site-shop", hostname: "shop.memql.example.com", title: "Shop", status: "live" });
const BLOG = siteRow({ id: "site-blog", hostname: "blog.memql.example.com", title: "", status: "live" });
const ACCOUNTS = [
  { id: "acme", name: "Acme", status: "active" },
  { id: "borden", name: "Borden Ltd", status: "active" },
];
const PER_SITE = [
  { siteId: "site-shop", visitors: 800, visits: 1000, pageViews: 3000 },
  { siteId: "site-blog", visitors: 404, visits: 530, pageViews: 1210 },
];

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

/**
 * The app on its Overview, with a `navigate` that moves the window the way
 * the shell does, so a link to another section lands there.
 */
function mount(seed: FakeSeed, session: Parameters<typeof withSession>[1] = { role: "owner", userId: "u-me" }, store = memStore()) {
  const connection = fakeConnection(seed);
  h.connection = connection;
  let revision = 0;
  const app = (section: string, origin: NonNullable<OsAppProps["navigation"]>["origin"]) =>
    withSession(<DeployablesApp sectionId={section} navigation={{ origin, revision }} navigate={navigate} askContext={vi.fn()} store={store} />, session);
  const navigate = vi.fn((section: string, options?: { fromContent?: boolean }) => {
    revision += 1;
    result.rerender(app(section, options?.fromContent ? "content" : "peer"));
  });
  const result = render(app("map", "peer"));
  return { connection, navigate, store };
}

/** The Overview's Websites section, once its figures have landed. */
async function websites(): Promise<HTMLElement> {
  const section = await screen.findByRole("region", { name: "Website statistics" });
  await waitFor(() => expect(within(section).getByRole("radio", { name: /^Visitors/ }).querySelector(".os-figure, .os-figure-absent")).not.toBeNull());
  return section;
}

function figureValue(group: HTMLElement, label: string): string {
  const figure = [...group.querySelectorAll(".os-stat-figure")].find((el) => el.querySelector(".os-stat-figure-label")?.textContent === label);
  return figure?.querySelector(".os-stat-figure-value")?.textContent ?? "";
}

function rowsOf(list: HTMLElement): Array<[string, string]> {
  return [...list.querySelectorAll(".os-ranked-row")].map((row) => [row.querySelector(".os-ranked-label")?.textContent ?? "", row.querySelector(".os-ranked-value")?.textContent ?? ""]);
}

beforeEach(() => {
  h.connection = null;
});

describe("the Overview's websites", () => {
  it("adds up every website in scope, and ranks each of them", async () => {
    const { connection } = mount({ sites: [SHOP, BLOG, PLATFORM_SITE], siteStatistics: siteStatisticsRow({ sites: PER_SITE }) });
    const section = await websites();
    // The section's heading carries its controls: the period, and the client where there is a choice.
    const heading = section.querySelector(":scope > .os-head") as HTMLElement;
    expect(heading.querySelector(".os-subhead")?.textContent).toBe("Websites");
    expect(within(heading).getByLabelText("Period")).toBeTruthy();
    const figures = within(section).getByRole("radiogroup", { name: "Website figures" });
    expect([...figures.querySelectorAll(".os-stat-figure-label")].map((label) => label.textContent)).toEqual(["Visitors", "Visits", "Page views"]);
    expect(figureValue(figures, "Visitors")).toBe("1,204");
    expect(within(section).getByRole("application", { name: "Visitors" })).toBeTruthy();
    // ONE READ FOR EVERY WEBSITE, which names none: the server sums every
    // deployable the person may read, and never pages through ids for it.
    const call = connection.callsNamed("siteStatistics").at(-1)!;
    expect(call).not.toContain("siteIds");
    expect(call).toContain('period: "7d"');
    // Named by title, else hostname, and valued by visitors.
    const list = within(section).getByRole("region", { name: "Websites" });
    expect(rowsOf(list)).toEqual([["Shop", "800"], ["blog.memql.example.com", "404"]]);
    // Every website is on the list, so nothing says otherwise.
    expect(within(list).queryByText(/busiest of/)).toBeNull();
  });

  it("lists the eight busiest websites, and says how many there are", async () => {
    // The figures above add up every website in scope; a list that stops at
    // eight says so, or it reads as all of them.
    const many = Array.from({ length: 12 }, (_, index) => siteRow({ id: `site-${index + 1}`, hostname: `site-${index + 1}.memql.example.com`, title: `Website ${index + 1}`, status: "live" }));
    const perSite = many.map((site, index) => ({ siteId: String(site["id"]), visitors: 1200 - index * 50, visits: 1300 - index * 50, pageViews: 3000 - index * 100 }));
    mount({ sites: many, siteStatistics: siteStatisticsRow({ sites: perSite }) });
    const list = within(await websites()).getByRole("region", { name: "Websites" });
    expect(rowsOf(list).map(([label]) => label)).toEqual(["Website 1", "Website 2", "Website 3", "Website 4", "Website 5", "Website 6", "Website 7", "Website 8"]);
    expect(within(list).getByText("The 8 busiest of 12 websites.")).toBeTruthy();
  });

  it("leaves out a website nothing measured, and says which row counts page views", async () => {
    const docs = siteRow({ id: "site-docs", hostname: "docs.memql.example.com", title: "Docs", status: "live" });
    mount({
      sites: [SHOP, BLOG, docs],
      siteStatistics: siteStatisticsRow({
        sites: [
          { siteId: "site-shop", visitors: 800, visits: 1000, pageViews: 3000 },
          { siteId: "site-blog", visitors: null, visits: null, pageViews: 300 },
          { siteId: "site-docs", visitors: null, visits: null, pageViews: null },
        ],
      }),
    });
    const list = within(await websites()).getByRole("region", { name: "Websites" });
    // The figures above do not count a website neither of whose figures was
    // measured, so the list has nothing to rank it by either.
    expect(rowsOf(list)).toEqual([["Shop", "800"], ["blog.memql.example.com", "300"]]);
    expect(list.textContent).not.toContain("Docs");
    expect(list.textContent).not.toContain("--");
    // A row valued by its page views says so; one valued by visitors says nothing.
    const secondary = [...list.querySelectorAll(".os-ranked-row")].map((row) => row.querySelector(".os-ranked-secondary")?.textContent ?? "");
    expect(secondary).toEqual(["", "Page views"]);
  });

  it("stands between the status breakdown and the map", async () => {
    mount({ sites: [SHOP, BLOG], siteStatistics: siteStatisticsRow({ sites: PER_SITE }) });
    const section = await websites();
    const breakdown = screen.getByRole("region", { name: "Deployment status" });
    const map = await screen.findByRole("application", { name: "Deploy map" });
    expect(breakdown.compareDocumentPosition(section) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(section.compareDocumentPosition(map) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("opens a website's Statistics from its row", async () => {
    const { navigate } = mount({ sites: [SHOP, BLOG], siteStatistics: siteStatisticsRow({ sites: PER_SITE }) });
    const section = await websites();
    await click(within(within(section).getByRole("region", { name: "Websites" })).getByRole("button", { name: /^Shop/ }));
    expect(navigate).toHaveBeenCalledWith("deployables", { fromContent: true });
    expect(await screen.findByRole("region", { name: "Statistics for shop.memql.example.com" })).toBeTruthy();
  });

  it("offers staff every client and a member of one organization no control at all", async () => {
    mount({ sites: [SHOP, BLOG], siteStatistics: siteStatisticsRow({ sites: PER_SITE }), accounts: ACCOUNTS });
    let section = await websites();
    // Staff are offered every client, and All clients first.
    const client = await within(section).findByLabelText("Client");
    expect(within(openSelect(client)).getAllByRole("option").map((option) => option.textContent)).toEqual(["All clients", "Acme", "Borden Ltd"]);
    cleanup();

    const { connection } = mount(
      { sites: [SHOP], siteStatistics: siteStatisticsRow({ sites: PER_SITE.slice(0, 1) }), accounts: ACCOUNTS },
      { role: "user", userId: "u-member", everyAccount: false, accountIds: ["acme"] },
    );
    section = await websites();
    expect(within(section).queryByLabelText("Client")).toBeNull();
    // No other client is named anywhere, and the read names no organization:
    // a member's scope is their organization, decided on the server.
    expect(document.body.textContent).not.toContain("Borden Ltd");
    expect(connection.callsNamed("siteStatistics").at(-1)).not.toMatch(/accountIds: \["/);
  });

  it("reads under the client chosen, and remembers it", async () => {
    const { connection, store } = mount({ sites: [SHOP, BLOG], siteStatistics: siteStatisticsRow({ sites: PER_SITE }), accounts: ACCOUNTS });
    const section = await websites();
    // All clients sends no organization at all.
    expect(connection.callsNamed("siteStatistics").at(-1)).not.toMatch(/accountIds: \["/);
    chooseOption(await within(section).findByLabelText("Client"), "Acme");
    await waitFor(() => expect(connection.callsNamed("siteStatistics").at(-1)).toContain('accountIds: ["acme"]'));
    expect(connection.callsNamed("siteStatistics").at(-1)).not.toContain("siteIds");
    expect(store.load().statisticsScope).toBe("acme");
  });

  it("asks for the client it remembers from its very first read", async () => {
    // A staff member's stored client stands before the account list answers,
    // so the page never reads every client first and then theirs.
    const store = memStore();
    store.save({ ...DEFAULT_DEPLOYABLES_SETTINGS, statisticsScope: "acme" });
    const { connection } = mount({ sites: [SHOP, BLOG], siteStatistics: siteStatisticsRow({ sites: PER_SITE }), accounts: ACCOUNTS }, { role: "owner", userId: "u-me" }, store);
    const section = await websites();
    const calls = connection.callsNamed("siteStatistics");
    expect(calls[0]).toContain('accountIds: ["acme"]');
    expect(calls.every((call) => call.includes('accountIds: ["acme"]'))).toBe(true);
    await waitFor(() => expect(selectedLabel(within(section).getByLabelText("Client"))).toBe("Acme"));
  });

  it("shows the sources and countries of the websites in scope, crediting DB-IP", async () => {
    const seed: FakeSeed = { sites: [SHOP, BLOG], siteStatistics: siteStatisticsRow({ sites: PER_SITE }), accounts: ACCOUNTS };
    mount(seed);
    let section = await websites();
    expect(within(within(section).getByRole("region", { name: "Sources" })).getByText("Direct")).toBeTruthy();
    const countries = within(section).getByRole("region", { name: "Countries" });
    expect(within(countries).getByText("Germany")).toBeTruthy();
    expect(within(countries).getByRole("link", { name: "IP Geolocation by DB-IP" }).getAttribute("href")).toBe("https://db-ip.com");

    // One client's websites have their own sources: the lists are the read's,
    // already summed over what is in scope.
    seed.siteStatistics = siteStatisticsRow({
      sites: PER_SITE.slice(0, 1),
      top: {
        source: [{ value: "acme-newsletter", visitors: 90, visits: 95, pageViews: 300 }],
        country: [{ value: "FR", visitors: 70, visits: 75, pageViews: 200 }],
      },
    });
    chooseOption(await within(section).findByLabelText("Client"), "Acme");
    section = await screen.findByRole("region", { name: "Website statistics" });
    await waitFor(() => expect(within(within(section).getByRole("region", { name: "Sources" })).getByText("acme-newsletter")).toBeTruthy());
    expect(within(within(section).getByRole("region", { name: "Countries" })).getByText("France")).toBeTruthy();
    expect(within(within(section).getByRole("region", { name: "Sources" })).queryByText("Direct")).toBeNull();
  });

  it("reads the period chosen for every deployable's statistics", async () => {
    const { connection, store } = mount({ sites: [SHOP, BLOG], siteStatistics: siteStatisticsRow({ sites: PER_SITE }) });
    const section = await websites();
    chooseOption(within(section).getByLabelText("Period"), "Last 90 days");
    await waitFor(() => expect(connection.callsNamed("siteStatistics").at(-1)).toContain('period: "90d"'));
    expect(store.load().statisticsPeriod).toBe("90d");
  });

  it("is absent where there is no website but the cluster's own, and asks for nothing", async () => {
    const { connection } = mount({ sites: [PLATFORM_SITE], siteStatistics: siteStatisticsRow() });
    await screen.findByRole("region", { name: "Overview" });
    await screen.findByLabelText(/^Host os\.memql\.example\.com/);
    expect(screen.queryByRole("region", { name: "Website statistics" })).toBeNull();
    expect(connection.callsNamed("siteStatistics")).toHaveLength(0);
  });
});
