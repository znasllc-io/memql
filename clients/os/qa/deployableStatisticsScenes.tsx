import { useState, type ComponentType, type ReactElement, type ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../src/apps/deployables/DeployablesApp";
import { DeployablePage } from "../src/apps/deployables/page/DeployablePage";
import type { WorkspaceDetail } from "../src/apps/deployables/page/DeployableWorkspace";
import { ALL_PARTS } from "../src/apps/deployables/parts";
import { siteFromRow } from "../src/apps/deployables/rows";
import { DEFAULT_DEPLOYABLES_SETTINGS, LocalDeployablesSettingsStore, type DeployablesSettings } from "../src/apps/deployables/settings";
import { DeployablesSettingsProvider } from "../src/apps/deployables/settingsContext";
import type { Period } from "../src/kit/statistics/period";
import { fakeConnection, siteRow, siteStatisticsRow, statisticsPoint, statisticsTotals, withSession, type FakeSeed } from "../test/deployables/harness";

// A deployable's Statistics page and the Overview's Websites section, rendered
// (the 2026-11-08 statistics design, sections 9 and 10). Fixtures only.
//
//   view=deployable-statistics            thirty days, compared with the thirty before
//   view=deployable-statistics-counting   the week visitor counting started in
//   view=deployable-statistics-empty      a period with no row at all
//   view=deployable-statistics-pending    the read not answered yet: shapes only
//   view=deployable-statistics-refused    a refused read, in the engine's words
//   view=deployables-websites             the Overview, staff, every client
//   view=deployables-websites-member      the Overview, a member of one organization
//
// With `mode=light|dark`, `theme=` and `width=narrow` as everywhere in the
// harness; `&click=All pages` opens the whole Pages list, and `&click=Browser`
// switches the Devices list.

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;

const BLOG = siteRow({ id: "site-blog", hostname: "blog.memql.example.com", title: "Field notes", status: "live", kind: "spa" });
const SHOP = siteRow({ id: "site-shop", hostname: "shop.memql.example.com", title: "Storefront", status: "live", kind: "spa", accountId: "acme" });
const DOCS = siteRow({ id: "site-docs", hostname: "docs.memql.example.com", title: "", status: "live", kind: "static", accountId: "borden" });
const DAY = 86_400_000;

function at(ms: number): string {
  return new Date(ms).toISOString().replace(".000Z", "Z");
}

/** A month-shaped wave: quieter weekends and a slow climb. */
function wave(index: number, base: number): number {
  const weekend = index % 7 === 5 || index % 7 === 6 ? -base * 0.3 : 0;
  return Math.max(0, Math.round(base + base * 0.12 * Math.sin(index / 1.4) + weekend + index * base * 0.008));
}

function days(first: string, base: number): Row[] {
  const start = Date.parse(`${first}T00:00:00Z`);
  return Array.from({ length: 30 }, (_, index) => {
    const visitors = wave(index, base);
    const visits = Math.round(visitors * 1.27);
    return statisticsPoint(at(start + index * DAY), {
      visitors, visits, bounces: Math.round(visits * 0.36), pageViews: Math.round(visits * 2.7),
      requests: Math.round(visits * 14), errors: index % 9 === 4 ? 6 : 1, notFound: Math.round(visits * 0.4), p95Ms: 110 + (index % 5) * 14,
    });
  });
}

function sum(points: readonly Row[], key: string): number {
  return points.reduce((total, point) => total + Number(point[key] ?? 0), 0);
}

function totalsOf(points: readonly Row[], over: Partial<Row> = {}): Row {
  const visits = sum(points, "visits");
  return statisticsTotals({
    visitors: sum(points, "visitors"), visits, bounces: sum(points, "bounces"), pageViews: sum(points, "pageViews"),
    viewsPerVisit: sum(points, "pageViews") / visits, bounceRate: sum(points, "bounces") / visits,
    requests: sum(points, "requests"), errors: sum(points, "errors"), notFound: sum(points, "notFound"),
    bytes: sum(points, "requests") * 48_000, botRequests: Math.round(sum(points, "requests") * 0.06), medianMs: 46, p95Ms: 128,
    ...over,
  });
}

const PAGES = ["/", "/pricing", "/blog/dyeing-wool-with-plants-and-a-great-deal-of-patience", "/collections/winter", "/products/wool-scarf", "/about", "/contact", "/products/beanie", "/faq", "/shipping", "/returns", "/careers"];

const TOP = {
  page: [...PAGES.map((value, index) => ({ value, visitors: Math.round(4200 / (index + 1.3)), visits: Math.round(4600 / (index + 1.3)), pageViews: Math.round(9000 / (index + 1.3)) })), { value: "(other)", visitors: 310, visits: 330, pageViews: 620 }],
  source: [["direct", 5120], ["search.example", 3311], ["social.example", 1208], ["newsletter", 640], ["news.example", 92]].map(([value, visitors]) => ({ value: String(value), visitors: Number(visitors), visits: Math.round(Number(visitors) * 1.2), pageViews: Math.round(Number(visitors) * 3) })),
  country: [["DE", 3920], ["US", 2811], ["FR", 1402], ["NL", 988], ["GB", 640], ["(unknown)", 120]].map(([value, visitors]) => ({ value: String(value), visitors: Number(visitors), visits: Math.round(Number(visitors) * 1.2), pageViews: Math.round(Number(visitors) * 3) })),
  device: [["desktop", 6400], ["mobile", 3900], ["tablet", 410]].map(([value, visitors]) => ({ value: String(value), visitors: Number(visitors), visits: Math.round(Number(visitors) * 1.2), pageViews: Math.round(Number(visitors) * 3) })),
  browser: [["Chrome", 5200], ["Safari", 3300], ["Firefox", 1100], ["Edge", 900], ["Other", 210]].map(([value, visitors]) => ({ value: String(value), visitors: Number(visitors), visits: Math.round(Number(visitors) * 1.2), pageViews: Math.round(Number(visitors) * 3) })),
  os: [["macOS", 3800], ["Windows", 3500], ["iOS", 2100], ["Android", 1200], ["Linux", 220]].map(([value, visitors]) => ({ value: String(value), visitors: Number(visitors), visits: Math.round(Number(visitors) * 1.2), pageViews: Math.round(Number(visitors) * 3) })),
};

const CURRENT = days("2026-10-10", 420);
const PREVIOUS = days("2026-09-10", 380);

/** Thirty days, measured throughout, against the thirty before. */
const MONTH = siteStatisticsRow({
  period: "30d", start: "2026-10-10T00:00:00Z", end: "2026-11-09T00:00:00Z", prevStart: "2026-09-10T00:00:00Z", prevEnd: "2026-10-10T00:00:00Z",
  measuredSince: "2026-08-01", totals: totalsOf(CURRENT), previous: totalsOf(PREVIOUS), series: CURRENT, previousSeries: PREVIOUS, top: TOP,
  sites: [
    { siteId: "site-shop", visitors: 7400, visits: 9300, pageViews: 25100 },
    { siteId: "site-blog", visitors: 4100, visits: 5300, pageViews: 14000 },
    { siteId: "site-docs", visitors: 1900, visits: 2300, pageViews: 6100 },
  ],
});

/** The week visitor counting started in: six days of requests only, then the first counted day. */
const COUNTING_WEEK = Array.from({ length: 7 }, (_, index) => index < 6
  ? statisticsPoint(at(Date.parse("2026-11-02T00:00:00Z") + index * DAY), { requests: 2600 + index * 40, errors: 2, notFound: 170, p95Ms: 118 })
  : statisticsPoint("2026-11-08T00:00:00Z", { visitors: 231, visits: 290, bounces: 101, pageViews: 760, requests: 3100, errors: 1, notFound: 160, p95Ms: 121 }));
const COUNTING = siteStatisticsRow({
  measuredSince: "2026-11-08",
  totals: totalsOf(COUNTING_WEEK, { visitors: 231, visits: 290, bounces: 101, pageViews: 760, viewsPerVisit: 760 / 290, bounceRate: 101 / 290 }),
  previous: statisticsTotals({ requests: 17000, errors: 9, notFound: 1400, bytes: 800_000_000, botRequests: 1100, medianMs: 44, p95Ms: 117 }),
  series: COUNTING_WEEK,
  top: { page: TOP.page.slice(0, 4), source: TOP.source.slice(0, 2), country: TOP.country.slice(0, 3), device: TOP.device, browser: TOP.browser, os: TOP.os },
});

/** A period with no row at all. */
const EMPTY = siteStatisticsRow({
  measuredSince: null, totals: statisticsTotals(), previous: statisticsTotals(), top: {},
  series: Array.from({ length: 7 }, (_, index) => statisticsPoint(at(Date.parse("2026-11-02T00:00:00Z") + index * DAY))),
  previousSeries: Array.from({ length: 7 }, (_, index) => statisticsPoint(at(Date.parse("2026-10-26T00:00:00Z") + index * DAY))),
});

const ACCOUNTS = [
  { id: "acme", name: "Acme", status: "active" },
  { id: "borden", name: "Borden Ltd", status: "active" },
];

/** A connection whose statistics read never answers, so the page stays on its shapes. */
function pendingConnection(seed: FakeSeed) {
  const connection = fakeConnection(seed);
  const execute = connection.query.executeNamed.bind(connection.query);
  connection.query.executeNamed = ((name: string, ...rest: unknown[]) =>
    name === "siteStatistics" ? new Promise(() => {}) : (execute as (...args: unknown[]) => unknown)(name, ...rest)) as typeof connection.query.executeNamed;
  return connection;
}

/** A deployable's page opened on a detail, under the app's settings, so its period control works. */
function DeployableOn({ site, detail, period }: { site: Row; detail: WorkspaceDetail; period: Period }) {
  const [settings, setSettings] = useState<DeployablesSettings>({ ...DEFAULT_DEPLOYABLES_SETTINGS, statisticsPeriod: period });
  return (
    <DeployablesSettingsProvider value={{ settings, update: (patch) => setSettings((held) => ({ ...held, ...patch })), toggleSource: () => {} }}>
      <DeployablePage site={siteFromRow(site)} pkg={null} credentials={[]} viewerUserId="u-me" nameOf={() => ""} can={ALL_PARTS} clusterDomain="memql.example.com" onBack={() => {}} onOpenSource={() => {}} initialDetail={detail} />
    </DeployablesSettingsProvider>
  );
}

function settingsStore() {
  const data = new Map<string, string>([["memql-os-deployables-v1", JSON.stringify({ ...DEFAULT_DEPLOYABLES_SETTINGS, statisticsPeriod: "30d" })]]);
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

/** The scenes, framed as the harness frames every window body. */
export function deployableStatisticsScenes(Frame: Frame) {
  // The period the fixture was made for, so the control names what is drawn.
  const page = (seed: FakeSeed, period: Period = "7d", connect?: () => unknown) => ({
    connect: connect ?? (() => fakeConnection(seed)),
    wrap: (element: ReactElement, role: string) => withSession(element, { role, userId: "u-me" }),
    render: () => <Frame fallback="Deployables"><DeployableOn site={BLOG} detail="statistics" period={period} /></Frame>,
  });
  const overview = (session: Parameters<typeof withSession>[1]) => ({
    connect: () => fakeConnection({ sites: [SHOP, BLOG, DOCS], siteStatistics: MONTH, accounts: ACCOUNTS }),
    wrap: (element: ReactElement) => withSession(element, session),
    render: () => <Frame fallback="Overview"><DeployablesApp sectionId="map" navigate={() => {}} askContext={() => {}} store={settingsStore()} /></Frame>,
  });
  return {
    "deployable-statistics": page({ sites: [BLOG], siteStatistics: MONTH }, "30d"),
    "deployable-statistics-counting": page({ sites: [BLOG], siteStatistics: COUNTING }),
    "deployable-statistics-empty": page({ sites: [BLOG], siteStatistics: EMPTY }),
    "deployable-statistics-pending": page({ sites: [BLOG] }, "7d", () => pendingConnection({ sites: [BLOG] })),
    "deployable-statistics-refused": page({ sites: [BLOG], siteStatisticsError: 'siteStatistics: period must be one of 24h, 7d, 30d, 90d, 12m; got "week"' }),
    "deployables-websites": overview({ role: "owner", userId: "u-me" }),
    "deployables-websites-member": overview({ role: "user", userId: "u-member", everyAccount: false, accountIds: ["acme"] }),
  };
}
