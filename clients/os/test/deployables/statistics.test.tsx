import { act, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Result } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted -- the
// shape deployables.test.tsx already uses.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LIST_TRAFFIC_WINDOW, MAX_SITES_PER_READ, fetchTrafficSummaries, windowBounds, windowSpec } from "../../src/apps/deployables/traffic";
import { SYSTEM_OWNED_TRAFFIC_NOTE, countingStartedSentence, formatMs, parseSiteStatistics, valueWord } from "../../src/apps/deployables/statistics/read";
import { statisticsRefreshMs } from "../../src/apps/deployables/statistics/useSiteStatistics";
import { chooseOption } from "../selectControl";
import {
  PLATFORM_SITE,
  SHOP,
  builtinReply,
  click,
  fakeConnection,
  siteRow,
  siteStatisticsRow,
  statisticsPoint,
  statisticsTotals,
  trafficRow,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// A deployable's Statistics page (the 2026-11-08 statistics design, section 9),
// which replaced its Traffic page.
//
// The pure half -- what the read MEANS, a null kept null, a value's words -- is
// asserted directly. The rendered half asserts what a person reads: the
// figures, the change against a period that can be compared, the chart of the
// figure they chose, and the sentences that say why a figure is not there.

const HOST = "blog.memql.example.com";
const BLOG = siteRow({ id: "site-blog", hostname: HOST, title: "Blog", status: "live", kind: "spa" });
const SETTINGS_KEY = "memql-os-deployables-v1";

function mount(connection: FakeConnection) {
  h.connection = connection;
  return render(
    withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} />, {
      role: "owner",
      userId: "u-me",
    }),
  );
}

/**
 * Open a deployable, then its Statistics, and return the STATISTICS PAGE.
 *
 * Scoped to the page region rather than the document: a hostname matches the
 * list row, the head and the rail's address alike.
 */
async function openStatistics(seed: FakeSeed, hostname = HOST, prepare?: (connection: FakeConnection) => void): Promise<{ connection: FakeConnection; page: HTMLElement }> {
  const connection = fakeConnection(seed);
  prepare?.(connection);
  mount(connection);
  await waitFor(() => expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"));
  await click((await screen.findByText(hostname)).closest("button"));
  const deployable = await screen.findByRole("region", { name: `Deployable ${hostname}` });
  await click(within(deployable).getByRole("button", { name: "Statistics" }));
  return { connection, page: await screen.findByRole("region", { name: `Statistics for ${hostname}` }) };
}

/** A `siteStatistics` read held until the test answers it, or lets it through to the fake. */
interface HeldRead {
  call: string;
  answer: (reply: Result) => void;
  through: () => Promise<void>;
}

/**
 * Hold the `siteStatistics` reads `which` picks. The fake records only the
 * calls it is handed, so a held call is in the returned list, not in
 * `connection.calls`, until it is let through.
 */
function holdStatisticsReads(connection: FakeConnection, which: (call: string) => boolean): HeldRead[] {
  const execute = connection.query.executeNamed.bind(connection.query) as (name: string, call: string, options?: unknown) => Promise<Result>;
  const held: HeldRead[] = [];
  connection.query.executeNamed = ((name: string, call: string, options?: unknown) => {
    if (name !== "siteStatistics" || !which(call)) return execute(name, call, options);
    return new Promise<Result>((resolve, reject) => {
      held.push({ call, answer: resolve, through: () => execute(name, call, options).then(resolve, reject) });
    });
  }) as typeof connection.query.executeNamed;
  return held;
}

/** The trail a page's Head names, crumb by crumb. */
function crumbs(page: HTMLElement): string[] {
  return within(within(page).getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((item) => (item.textContent ?? "").trim());
}

/** Every figure's change line on the page, in order. */
function changes(page: HTMLElement): string[] {
  return [...page.querySelectorAll(".os-stat-figure-change")].map((line) => line.textContent ?? "");
}

/** The figure row a page's website figures stand in. */
async function websiteFigures(page: HTMLElement): Promise<HTMLElement> {
  const figures = await within(page).findByRole("radiogroup", { name: "Website figures" });
  await waitFor(() => expect(within(figures).getByRole("radio", { name: /^Visitors/ }).querySelector(".os-figure, .os-figure-absent")).not.toBeNull());
  return figures;
}

/** The value a figure shows, by its label. */
function figureValue(group: HTMLElement, label: string): string {
  const figure = [...group.querySelectorAll(".os-stat-figure")].find((el) => el.querySelector(".os-stat-figure-label")?.textContent === label);
  return figure?.querySelector(".os-stat-figure-value")?.textContent ?? "";
}

/** Seven daily buckets from Nov 2, 2026: six before visitor counting, the seventh counted. */
function shipWeek(): ReturnType<typeof statisticsPoint>[] {
  return Array.from({ length: 7 }, (_, index) => {
    const at = `2026-11-0${index + 2}T00:00:00Z`;
    return index < 6
      ? statisticsPoint(at, { requests: 2600, errors: 2, notFound: 170, p95Ms: 110 })
      : statisticsPoint(at, { visitors: 31, visits: 40, bounces: 12, pageViews: 96, requests: 3200, errors: 2, notFound: 183, p95Ms: 120 });
  });
}

beforeEach(() => {
  // THE PERIOD IS REMEMBERED, and vitest isolates per FILE rather than per
  // test: without this a test that picks thirty days leaves the next one there.
  globalThis.localStorage?.clear();
});

// ---------------------------------------------------------------------------
// The read
// ---------------------------------------------------------------------------

describe("the statistics read", () => {
  it("keeps a JSON null as null and a zero as zero, at every level", () => {
    const stats = parseSiteStatistics(siteStatisticsRow({
      totals: statisticsTotals({ visitors: null, requests: 0, bytes: "13000" }),
      series: [statisticsPoint("2026-11-02T00:00:00Z", { visitors: null, requests: 0 })],
      sites: [{ siteId: "site-blog", visitors: null, visits: 3, pageViews: 0 }],
    }));
    // NOT rowNumber(), which reads null as 0: an unmeasured figure and a
    // measured zero are different answers.
    expect(stats.totals.visitors).toBeNull();
    expect(stats.totals.requests).toBe(0);
    // The wire may carry a large count as a string.
    expect(stats.totals.bytes).toBe(13000);
    expect(stats.series[0]?.visitors).toBeNull();
    expect(stats.series[0]?.requests).toBe(0);
    expect(stats.sites).toEqual([{ siteId: "site-blog", visitors: null, visits: 3, pageViews: 0 }]);
  });

  it("reads the period, its bounds and the day counting began", () => {
    const stats = parseSiteStatistics(siteStatisticsRow());
    expect(stats.period).toBe("7d");
    expect(stats.bucket).toBe("day");
    expect(stats.start).toBe("2026-11-02T00:00:00Z");
    expect(stats.prevStart).toBe("2026-10-26T00:00:00Z");
    expect(stats.measuredSince).toBe("2026-10-01");
    expect(stats.series).toHaveLength(7);
    expect(stats.previousSeries).toHaveLength(7);
    expect(stats.previous?.visitors).toBe(1075);
  });

  it("answers no previous period for twelve months, and every dimension's list", () => {
    const stats = parseSiteStatistics(siteStatisticsRow({ period: "12m", bucket: "month", hasPrevious: false, prevStart: null, prevEnd: null, previous: null, previousSeries: null, top: { page: [{ value: "/", visitors: 3, visits: 4, pageViews: 9 }] } }));
    expect(stats.previous).toBeNull();
    expect(stats.previousSeries).toBeNull();
    expect(stats.prevStart).toBeNull();
    expect(stats.top.page).toEqual([{ value: "/", visitors: 3, visits: 4, pageViews: 9 }]);
    for (const dimension of ["source", "country", "device", "browser", "os"] as const) expect(stats.top[dimension]).toEqual([]);
  });

  it("drops a list entry with no count rather than counting it as nothing", () => {
    const stats = parseSiteStatistics(siteStatisticsRow({ top: { source: [{ value: "direct", visitors: null, visits: 1, pageViews: 1 }, { value: "search.example", visitors: 4, visits: 5, pageViews: 6 }] } }));
    expect(stats.top.source.map((entry) => entry.value)).toEqual(["search.example"]);
  });

  it("writes a response time in milliseconds, then seconds", () => {
    expect(formatMs(120)).toBe("120 ms");
    expect(formatMs(1400)).toBe("1.4 s");
    expect(formatMs(0)).toBe("0 ms");
    // Rounding up to a thousand milliseconds is a second.
    expect(formatMs(999.7)).toBe("1.0 s");
  });

  it("names the values a person reads rather than the ones the rollup keeps", () => {
    expect(valueWord("page", "(other)")).toBe("Other");
    expect(valueWord("country", "(unknown)")).toBe("Unknown");
    expect(valueWord("source", "direct")).toBe("Direct");
    expect(valueWord("source", "search.example")).toBe("search.example");
    expect(valueWord("page", "/pricing")).toBe("/pricing");
    // A country is stored as its ISO code and read as its name.
    expect(valueWord("country", "DE")).toBe("Germany");
    expect(valueWord("device", "mobile")).toBe("Mobile");
    expect(valueWord("browser", "Chrome")).toBe("Chrome");
  });

  it("says when visitor counting began, in UTC", () => {
    expect(countingStartedSentence("2026-11-08")).toBe("Visitor counting started on Nov 8, 2026.");
  });
});

// ---------------------------------------------------------------------------
// On the page
// ---------------------------------------------------------------------------

describe("a deployable's Statistics", () => {
  it("opens as a page under the deployable, with no dialog", async () => {
    const { page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow() });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("region", { name: `Deployable ${HOST}` })).toBeNull();
    expect(crumbs(page)).toEqual(["Deployables", "Blog", "Statistics"]);
    expect(within(page).getByRole("heading", { name: "Statistics" })).toBeTruthy();
  });

  it("draws the figures and the chart from the read", async () => {
    const { connection, page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow() });
    const figures = await websiteFigures(page);
    expect([...figures.querySelectorAll(".os-stat-figure-label")].map((label) => label.textContent)).toEqual(["Visitors", "Visits", "Page views", "Views per visit", "Bounce rate"]);
    expect(figureValue(figures, "Visitors")).toBe("1,204");
    expect(within(figures).getByRole("radio", { name: /^Visitors/ }).querySelector(".os-stat-figure-change")?.textContent).toBe("+12%");
    expect(within(figures).getByRole("radio", { name: /^Visits/ }).querySelector(".os-stat-figure-change")?.textContent).toBe("No change");
    expect(figureValue(figures, "Views per visit")).toBe("2.8");
    expect(figureValue(figures, "Bounce rate")).toBe("30%");
    // Every figure is chartable, the derived ones too.
    expect(within(figures).getAllByRole("radio")).toHaveLength(5);
    // The chart draws the checked figure, this period over the one before it.
    const plot = within(page).getByRole("application", { name: "Visitors" });
    expect(plot.querySelector('path[data-series="current"]')).not.toBeNull();
    expect(plot.querySelector('path[data-series="previous"]')).not.toBeNull();
    // The read named THIS deployable, for the remembered period.
    const call = connection.callsNamed("siteStatistics").at(-1)!;
    expect(call).toContain('siteIds: ["site-blog"]');
    expect(call).toContain('period: "7d"');
  });

  it("charts the figure that is pressed, a rate worked out per bucket included", async () => {
    const { page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow() });
    const figures = await websiteFigures(page);
    await click(within(figures).getByRole("radio", { name: /^Page views/ }));
    expect(within(figures).getByRole("radio", { name: /^Page views/ }).getAttribute("aria-checked")).toBe("true");
    expect(within(page).getByRole("application", { name: "Page views" })).toBeTruthy();
    expect(within(page).queryByRole("application", { name: "Visitors" })).toBeNull();

    // A bucket's bounce rate is its bounces over its visits: 65 of 215 on Nov 6.
    await click(within(figures).getByRole("radio", { name: /^Bounce rate/ }));
    const plot = within(page).getByRole("application", { name: "Bounce rate" });
    const summary = document.getElementById(plot.getAttribute("aria-describedby") ?? "")?.textContent ?? "";
    expect(summary).toContain("Bounce rate: 7 points from Nov 2 to Nov 8");
    expect(summary).toContain("highest 30% on Nov 6");
  });

  it("remembers the period chosen, and reads it", async () => {
    const { connection, page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow() });
    await websiteFigures(page);
    chooseOption(within(page).getByLabelText("Period"), "Last 30 days");
    await waitFor(() => expect(JSON.parse(globalThis.localStorage.getItem(SETTINGS_KEY) ?? "{}").statisticsPeriod).toBe("30d"));
    await waitFor(() => expect(connection.callsNamed("siteStatistics").at(-1)).toContain('period: "30d"'));
  });

  it("says when visitor counting began instead of drawing zeroes before it, and compares with nothing partly counted", async () => {
    const { page } = await openStatistics({
      sites: [BLOG],
      siteStatistics: siteStatisticsRow({
        measuredSince: "2026-11-08",
        totals: statisticsTotals({ visitors: 31, visits: 40, bounces: 12, pageViews: 96, viewsPerVisit: 2.4, bounceRate: 0.3, requests: 18800, errors: 14, notFound: 1203, bytes: 13_000_000, botRequests: 230, medianMs: 48, p95Ms: 120 }),
        previous: statisticsTotals({ requests: 17000, errors: 0, notFound: 1410, bytes: 12_000_000, botRequests: 210, medianMs: 45, p95Ms: 110 }),
        series: shipWeek(),
      }),
    });
    const figures = await websiteFigures(page);
    expect(within(page).getByText("Visitor counting started on Nov 8, 2026.")).toBeTruthy();
    expect(figureValue(figures, "Visitors")).toBe("31");
    // The six days before counting are GAPS, not zero visitors.
    const plot = within(page).getByRole("application", { name: "Visitors" });
    expect(document.getElementById(plot.getAttribute("aria-describedby") ?? "")?.textContent).toContain("6 not measured");
    // The week before began before counting, so nothing is compared with it --
    // not even requests, which it measured: a partly counted period overstates
    // every change against it.
    expect(changes(page).every((line) => line === "")).toBe(true);
    expect(page.querySelector('path[data-series="previous"]')).toBeNull();
  });

  it("shows a figure nothing measured as a dash, never a zero", async () => {
    const { page } = await openStatistics({
      sites: [BLOG],
      siteStatistics: siteStatisticsRow({
        measuredSince: null,
        totals: statisticsTotals({ requests: 18800, errors: 14, notFound: 1203, bytes: 13_000_000, botRequests: 230, medianMs: 48, p95Ms: 120 }),
        previous: statisticsTotals(),
        series: shipWeek().map((point) => ({ ...point, visitors: null, visits: null, bounces: null, pageViews: null })),
      }),
    });
    const figures = await websiteFigures(page);
    for (const label of ["Visitors", "Visits", "Page views", "Views per visit", "Bounce rate"]) {
      expect(figureValue(figures, label)).toBe("—");
    }
    expect(figures.textContent).not.toMatch(/\b0\b/);
  });

  it("says nothing was recorded for a period with no rows, and draws no figure", async () => {
    const { page } = await openStatistics({
      sites: [BLOG],
      siteStatistics: siteStatisticsRow({
        measuredSince: null,
        totals: statisticsTotals(),
        previous: statisticsTotals(),
        series: shipWeek().map((point) => statisticsPoint(String(point["at"]))),
        top: {},
      }),
    });
    expect(await within(page).findByText("Nothing was recorded for last 7 days.")).toBeTruthy();
    expect(within(page).queryByRole("radiogroup")).toBeNull();
    expect(within(page).queryByRole("application")).toBeNull();
  });

  it("opens the whole list as a page from All pages, and Back returns to Statistics", async () => {
    const pages = Array.from({ length: 12 }, (_, index) => ({ value: `/page-${index + 1}`, visitors: 120 - index, visits: 130 - index, pageViews: 200 - index }));
    const { page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow({ top: { page: pages } }) });
    await websiteFigures(page);
    const list = within(page).getByRole("region", { name: "Pages" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(8);
    await click(within(list).getByRole("button", { name: "All pages" }));

    const full = await screen.findByRole("region", { name: `Pages for ${HOST}` });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(crumbs(full)).toEqual(["Deployables", "Blog", "Statistics", "Pages"]);
    // Every page, under the period it covers.
    const rows = await within(full).findByRole("region", { name: "Last 7 days" });
    await waitFor(() => expect(within(rows).getAllByRole("listitem")).toHaveLength(12));
    expect(within(rows).getByText("/page-12")).toBeTruthy();

    await click(within(full).getByRole("button", { name: "Back to Statistics" }));
    expect(await screen.findByRole("region", { name: `Statistics for ${HOST}` })).toBeTruthy();
  });

  it("names sources, countries and devices in words, and switches devices to browsers and systems", async () => {
    const { page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow() });
    await websiteFigures(page);
    expect(within(within(page).getByRole("region", { name: "Sources" })).getByText("Direct")).toBeTruthy();
    const countries = within(page).getByRole("region", { name: "Countries" });
    expect(within(countries).getByText("Germany")).toBeTruthy();
    expect(within(countries).getByText("Unknown")).toBeTruthy();
    const devices = within(page).getByRole("region", { name: "Devices" });
    expect(within(devices).getByText("Desktop")).toBeTruthy();
    // THE TABS SWITCH A LIST IN PLACE, halfway down the page, so they stand
    // outside the shared tabs' return to the top (kit/pageScroll ignores a
    // press inside `data-os-scroll-ignore`). jsdom lays nothing out, so the
    // scroll itself was checked in a browser; this keeps the marker in place.
    expect(within(page).getByRole("button", { name: "Browser" }).closest("[data-os-scroll-ignore]")).not.toBeNull();
    await click(within(page).getByRole("button", { name: "Browser" }));
    expect(within(within(page).getByRole("region", { name: "Devices" })).getByText("Safari")).toBeTruthy();
    await click(within(page).getByRole("button", { name: "OS" }));
    expect(within(within(page).getByRole("region", { name: "Devices" })).getByText("macOS")).toBeTruthy();
  });

  it("credits DB-IP under the countries", async () => {
    const { page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow() });
    await websiteFigures(page);
    const credit = within(within(page).getByRole("region", { name: "Countries" })).getByRole("link", { name: "IP Geolocation by DB-IP" });
    expect(credit.getAttribute("href")).toBe("https://db-ip.com");
  });

  it("reads the site's health in its own figures, chart and facts", async () => {
    const { page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow() });
    await websiteFigures(page);
    const health = within(page).getByRole("region", { name: "Health" });
    const figures = within(health).getByRole("radiogroup", { name: "Health figures" });
    expect([...figures.querySelectorAll(".os-stat-figure-label")].map((label) => label.textContent)).toEqual(["Requests", "Errors", "Not found", "Response time, p95"]);
    expect(figureValue(figures, "Requests")).toBe("18,800");
    expect(figureValue(figures, "Response time, p95")).toBe("120 ms");
    // Errors from none to some: "New", never "+Infinity%".
    expect(within(figures).getByRole("radio", { name: /^Errors/ }).querySelector(".os-stat-figure-change")?.textContent).toBe("New");
    expect(within(health).getByRole("application", { name: "Requests" })).toBeTruthy();
    expect(within(health).getByText("Data served").nextElementSibling?.textContent).toBe("12.4 MiB");
    expect(within(health).getByText("Median response").nextElementSibling?.textContent).toBe("48 ms");
    expect(within(health).getByText("Bot requests").nextElementSibling?.textContent).toBe("230");
  });

  it("says what a visitor, a visit and a bounce are, once, in the page's help", async () => {
    const { page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow() });
    await websiteFigures(page);
    const help = page.querySelector(".os-page-help-inline")?.textContent ?? "";
    for (const said of ["without cookies", "30 minutes", "one page", "UTC", "page change", "Health", "DB-IP"]) {
      expect(help).toContain(said);
    }
  });

  it("shows the engine's sentence for a refused read, and tries again by itself", async () => {
    const sentence = 'siteStatistics: period must be one of 24h, 7d, 30d, 90d, 12m; got "week"';
    const { connection, page } = await openStatistics({ sites: [BLOG], siteStatisticsError: sentence });
    await waitFor(() => expect(within(page).getByText(sentence)).toBeTruthy());
    expect(within(page).getByText("Retrying automatically.")).toBeTruthy();
    const before = connection.callsNamed("siteStatistics");
    await act(async () => { window.dispatchEvent(new Event("focus")); });
    await waitFor(() => expect(connection.callsNamed("siteStatistics").length).toBe(before.length + 1));
    expect(connection.callsNamed("siteStatistics").at(-1)).toBe(before.at(-1));
  });

  it("keeps a refusal standing while its retry is out, and draws no loading shapes over it", async () => {
    // DESIGN.md: a skeleton must never conceal an error. The retry asks the
    // same question again; until something answers it, the refusal is still
    // the last word, so it stays, and shapes would read as progress.
    const sentence = 'siteStatistics: period must be one of 24h, 7d, 30d, 90d, 12m; got "week"';
    const seed: FakeSeed = { sites: [BLOG], siteStatisticsError: sentence };
    let held: HeldRead[] = [];
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { page } = await openStatistics(seed, HOST, (connection) => {
        // Only the retry is held; the first read is refused at once.
        held = holdStatisticsReads(connection, () => seed.siteStatisticsError === undefined);
      });
      await waitFor(() => expect(within(page).getByText(sentence)).toBeTruthy());
      delete seed.siteStatisticsError;
      seed.siteStatistics = siteStatisticsRow();
      // The retry that "Retrying automatically." promises: the page's own cadence.
      await act(async () => { await vi.advanceTimersByTimeAsync(statisticsRefreshMs("7d") + 1_000); });
      await waitFor(() => expect(held).toHaveLength(1));
      expect(page.isConnected).toBe(true);
      expect(within(page).getByText(sentence)).toBeTruthy();
      expect(within(page).getByText("Retrying automatically.")).toBeTruthy();
      expect(page.querySelector('[aria-busy="true"]')).toBeNull();
      expect(within(page).queryByRole("radiogroup")).toBeNull();
      // The retry answers: the figures replace the refusal.
      await act(async () => { await held[0]!.through(); });
      expect(figureValue(await websiteFigures(page), "Visitors")).toBe("1,204");
      expect(within(page).queryByText(sentence)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("discards a read still out for the period it left", async () => {
    let held: HeldRead[] = [];
    const { page } = await openStatistics(
      { sites: [BLOG], siteStatistics: siteStatisticsRow({ period: "30d", totals: statisticsTotals({ visitors: 4321, requests: 9000 }) }) },
      HOST,
      // The week's read is held; the month's is answered at once.
      (connection) => { held = holdStatisticsReads(connection, (call) => call.includes('period: "7d"')); },
    );
    await waitFor(() => expect(held).toHaveLength(1));
    chooseOption(within(page).getByLabelText("Period"), "Last 30 days");
    expect(figureValue(await websiteFigures(page), "Visitors")).toBe("4,321");
    // The week's answer lands after the month's. It is about a period this page
    // no longer shows, and is thrown away.
    await act(async () => {
      held[0]!.answer(builtinReply("siteStatistics", [siteStatisticsRow({ totals: statisticsTotals({ visitors: 7, requests: 10 }) })]));
    });
    await act(async () => {});
    expect(figureValue(within(page).getByRole("radiogroup", { name: "Website figures" }), "Visitors")).toBe("4,321");
  });

  it("reads again every fifteen minutes, and every five for the last 24 hours", async () => {
    expect(statisticsRefreshMs("24h")).toBe(300_000);
    for (const period of ["7d", "30d", "90d", "12m"] as const) expect(statisticsRefreshMs(period)).toBe(900_000);

    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { connection, page } = await openStatistics({ sites: [BLOG], siteStatistics: siteStatisticsRow() });
      await websiteFigures(page);
      expect(connection.callsNamed("siteStatistics")).toHaveLength(1);
      await act(async () => { await vi.advanceTimersByTimeAsync(600_000); });
      expect(connection.callsNamed("siteStatistics")).toHaveLength(1);
      await act(async () => { await vi.advanceTimersByTimeAsync(320_000); });
      await waitFor(() => expect(connection.callsNamed("siteStatistics")).toHaveLength(2));

      // The last 24 hours fill by the hour, so they are read three times as often.
      chooseOption(within(page).getByLabelText("Period"), "Last 24 hours");
      await waitFor(() => expect(connection.callsNamed("siteStatistics")).toHaveLength(3));
      await act(async () => { await vi.advanceTimersByTimeAsync(320_000); });
      await waitFor(() => expect(connection.callsNamed("siteStatistics")).toHaveLength(4));
      expect(connection.callsNamed("siteStatistics").at(-1)).toContain('period: "24h"');
    } finally {
      vi.useRealTimers();
    }
  });

  it("shows a system-owned deployable's note and asks for nothing", async () => {
    const { connection, page } = await openStatistics({ sites: [PLATFORM_SITE] }, String(PLATFORM_SITE["hostname"]));
    await waitFor(() => expect(within(page).getByText(SYSTEM_OWNED_TRAFFIC_NOTE)).toBeTruthy());
    expect(within(page).queryByLabelText("Period")).toBeNull();
    expect(within(page).queryByRole("radiogroup")).toBeNull();
    expect(connection.callsNamed("siteStatistics")).toHaveLength(0);
  });

  it("offers no Statistics on a draft, which has never served", async () => {
    // ABSENT, NOT A PAGE OF NOTHING: the way Access codes is left out where it
    // would have nothing to open.
    const draft = siteRow({ id: "site-draft", hostname: "draft.memql.example.com", status: "draft" });
    const connection = fakeConnection({ sites: [draft], siteStatistics: siteStatisticsRow() });
    mount(connection);
    await waitFor(() => expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"));
    await click((await screen.findByText("draft.memql.example.com")).closest("button"));
    const page = await screen.findByRole("region", { name: "Deployable draft.memql.example.com" });
    expect(within(page).queryByRole("button", { name: "Statistics" })).toBeNull();
    expect(within(page).getByRole("button", { name: /^Logs for / }).nextElementSibling).toBeNull();
    expect(connection.callsNamed("siteStatistics")).toHaveLength(0);
  });

  it("shows no comparison for twelve months", async () => {
    globalThis.localStorage.setItem(SETTINGS_KEY, JSON.stringify({ version: 1, statisticsPeriod: "12m", statisticsScope: "", expandedSources: [] }));
    const months = Array.from({ length: 13 }, (_, index) => statisticsPoint(new Date(Date.UTC(2025, 10 + index, 1)).toISOString().replace(".000Z", "Z"), { visitors: 900 + index * 30, visits: 1000 + index * 30, bounces: 300, pageViews: 3000 + index * 90, requests: 16000, errors: 3, notFound: 200, p95Ms: 120 }));
    const { connection, page } = await openStatistics({
      sites: [BLOG],
      siteStatistics: siteStatisticsRow({ period: "12m", bucket: "month", start: "2025-11-01T00:00:00Z", end: "2026-12-01T00:00:00Z", hasPrevious: false, prevStart: null, prevEnd: null, previous: null, previousSeries: null, series: months }),
    });
    await websiteFigures(page);
    expect(connection.callsNamed("siteStatistics").at(-1)).toContain('period: "12m"');
    expect(changes(page).every((line) => line === "")).toBe(true);
    expect(page.querySelector('path[data-series="previous"]')).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The list row's figure, which still reads the request aggregates
// ---------------------------------------------------------------------------

describe("the list row's figure", () => {
  it("says when a deployable was last served, and nothing for one the week did not cover", async () => {
    const summary = trafficRow({
      windowStart: "2026-08-27T12:00:00Z",
      siteId: "site-shop",
      requestCount: 128,
      lastServedAt: "2026-09-03T11:00:00Z",
    });
    const connection = fakeConnection({
      sites: [siteRow({ ...SHOP, id: "site-shop", status: "live" }), siteRow({ id: "site-quiet", hostname: "quiet.memql.example.com" })],
      traffic: { summary: [summary] },
    });
    mount(connection);

    // Scoped to the LIST rather than to a page: nothing is open here, and the
    // map beside the list draws the same deployables.
    const list = await screen.findByRole("list", { name: /deployables/i });
    await waitFor(() => expect(within(list).getByText(/^served /)).toBeTruthy());
    // ONE CALL for the whole list, in the summary mode built for it.
    const calls = connection.callsNamed("siteTrafficInWindow");
    expect(calls.filter((c) => c.includes("summary: true"))).toHaveLength(1);
    expect(calls[0]).toContain('bucket: "1h"');

    // The deployable the answer did not cover is unmeasured, and its row says
    // NOTHING rather than "never" -- which would be a claim.
    expect(within(list).getAllByText(/^served /)).toHaveLength(1);
  });

  it("reads a week, in hour buckets, aligned both ends and half-open", () => {
    // A list row answers "is anybody using this at all", and a deployable that
    // served nobody yesterday but two hundred people on Monday is one somebody
    // is using.
    expect(LIST_TRAFFIC_WINDOW).toBe("week");
    const spec = windowSpec(LIST_TRAFFIC_WINDOW);
    expect(spec.bucket).toBe("1h");
    const now = new Date("2026-09-03T12:34:56.789Z");
    const { start, end } = windowBounds(LIST_TRAFFIC_WINDOW, now);
    expect(Date.parse(start) % spec.bucketMs).toBe(0);
    expect(Date.parse(end) % spec.bucketMs).toBe(0);
    expect(Date.parse(end) - Date.parse(start)).toBe(spec.spanMs);
    // The end is aligned UP, so the bucket in progress is included.
    expect(Date.parse(end)).toBeGreaterThanOrEqual(now.getTime());
    // Never a hot loop: a list left open on a desk reads at most once a minute.
    expect(spec.refreshMs).toBeGreaterThanOrEqual(60_000);
  });

  it("pages the list read at the server's own cap", async () => {
    // The server REFUSES a call past its cap rather than truncating it, so a
    // cluster with more deployables than that must produce several calls --
    // one refusal would leave the whole list with no figures, which reads as
    // "nobody is using any of these".
    const ids = Array.from({ length: MAX_SITES_PER_READ + 3 }, (_, i) => `site-${i}`);
    const calls: string[] = [];
    const query = {
      siteTrafficInWindow: async (args: { siteIds: string[] }) => {
        calls.push(args.siteIds.join(","));
        return { rows: () => [] };
      },
    } as unknown as Parameters<typeof fetchTrafficSummaries>[0];

    await fetchTrafficSummaries(query, ids, "week", new Date("2026-09-03T12:34:00Z"));
    expect(calls).toHaveLength(2);
    expect(calls[0]!.split(",")).toHaveLength(MAX_SITES_PER_READ);
    expect(calls[1]!.split(",")).toHaveLength(3);
    // Every id is asked about exactly once: a page boundary that dropped or
    // repeated one would be invisible in the rendered list.
    expect(new Set(calls.join(",").split(","))).toEqual(new Set(ids));
  });
});
