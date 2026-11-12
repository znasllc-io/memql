import { useState, type ComponentType, type ReactElement, type ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablePage } from "../src/apps/deployables/page/DeployablePage";
import type { WorkspaceDetail } from "../src/apps/deployables/page/DeployableWorkspace";
import { ALL_PARTS, partsWithout, type PartsHeld } from "../src/apps/deployables/parts";
import { siteFromRow } from "../src/apps/deployables/rows";
import { DEFAULT_DEPLOYABLES_SETTINGS, type DeployablesSettings } from "../src/apps/deployables/settings";
import { DeployablesSettingsProvider } from "../src/apps/deployables/settingsContext";
import { fakeConnection, siteRow, storeFigures, storeStatisticsRow, withSession, type FakeSeed } from "../test/deployables/harness";

// A storefront's Store statistics: the button beside Statistics, and the
// page in each state (the 2026-11-11 store statistics design, Tasks 21 and
// 22). Fixtures only: an invented bicycle shop. The figures are the engine's
// shape; the engine does not fill them yet, so every number here is invented.
//
//   view=storefront-with-store-statistics      the storefront's page: Store statistics after Statistics
//   view=storefront-without-sales              the same page to a person without the sales part: no button
//   view=storefront-draft-store-statistics     a draft storefront: no Statistics, Store statistics after Logs
//   view=store-statistics                      one store (binding: Production), a full week: figures, chart, every list
//   view=store-statistics-products             the same, with All products pressed: the whole list under the trail
//   view=store-statistics-two-stores           two stores, Production read: the Destination control
//   view=store-statistics-both                 both destinations name one store: "Production and Testing", no control
//   view=store-statistics-development          only Testing has a store (previewBinding): the development store
//   view=store-statistics-paused               paused: its own sentence, as of the last clear run
//   view=store-statistics-frozen               needs attention, a reason in the way and one that freezes nothing
//   view=store-statistics-blocked              blocked before any clear run: no figure can be given
//   view=store-statistics-current-reason       recording, with a reason the engine lists
//   view=store-statistics-unknown              the verdict could not be read
//   view=store-statistics-starting             the walk holds no row for the store yet
//   view=store-statistics-purging              the store's facts are being deleted
//   view=store-statistics-rebuilding           the store is being walked again
//   view=store-statistics-refunds-unread       two refunds still being read: four figures blank
//   view=store-statistics-no-currency          no currency recorded: every amount blank
//   view=store-statistics-no-zone              no zone recorded: "Days in UTC", every figure blank
//   view=store-statistics-zone-not-loaded      a zone recorded that did not load: "Days in UTC"
//   view=store-statistics-empty                no orders in the period: measured zeros, empty lists
//   view=store-statistics-below-zero           a day of refunds under the zero rule
//   view=store-statistics-pending              the read not answered yet: the page's shapes
//   view=store-statistics-refused              a refused read, in the engine's own sentence
//
// With `mode=light|dark`, `theme=`, `width=narrow` and `at=` as everywhere in
// the harness.

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;

const HOST = "bikes.memql.example.com";
const BIKES = siteRow({
  id: "site-bikes", hostname: HOST, kind: "shopify_storefront", status: "live", title: "City bikes",
  bundleRef: "blob://sites/site-bikes/v1/", binding: { storeId: "store-bikes" }, previewBinding: { storeId: "store-bikes-dev" },
});
const ONE_STORE = { ...BIKES, previewBinding: { storeId: "store-bikes" } };
const PRODUCTION_ONLY = { ...BIKES, previewBinding: {} };
const TESTING_ONLY = { ...BIKES, binding: {} };
const DRAFT = { ...BIKES, status: "draft" };

const STORES: Row[] = [
  { id: "store-bikes", domain: "acme-widgets.myshopify.com", name: "A city bike shop", status: "live", isDevelopment: false, developmentOfStoreId: "" } as unknown as Row,
  { id: "store-bikes-dev", domain: "acme-widgets-dev.myshopify.com", name: "A city bike shop (development)", status: "configured", isDevelopment: true, developmentOfStoreId: "store-bikes" } as unknown as Row,
];

const NAMES = { siteId: "site-bikes", storeName: "A city bike shop", storeDomain: "acme-widgets.myshopify.com" };
const DAYS = ["02", "03", "04", "05", "06", "07", "08"];
const at = (day: string) => `2026-11-${day}T00:00:00+01:00`;

/** A full week of an invented bike shop. */
const WEEK_FIGURES = { totalSales: "1482.40", netSales: "1206.10", orders: 14, averageOrderValue: "93.29", grossSales: "1388.00", discounts: "81.90", refunds: "100.00", unitsSold: 31 };
const WEEK_SERIES = DAYS.map((day, i) => {
  const net = ["118.00", "260.50", "96.40", "-40.00", "301.20", "212.00", "258.00"][i]!;
  const gross = ["130.00", "280.00", "105.00", "0.00", "320.00", "255.00", "298.00"][i]!;
  return {
    at: at(day), netSales: net, grossSales: gross,
    totalSales: ["142.80", "318.10", "117.60", "-46.20", "364.40", "256.50", "329.20"][i]!,
    discounts: ["12.00", "19.50", "8.60", "0.00", "18.80", "8.00", "15.00"][i]!,
    refunds: ["0.00", "0.00", "0.00", "40.00", "0.00", "35.00", "25.00"][i]!,
    orders: [2, 3, 1, 0, 3, 2, 3][i]!, unitsSold: [4, 7, 2, -1, 8, 5, 6][i]!,
  };
});
const TITLES = ["City bike", "Folding bike", "Bell", "Lights set", "Pannier", "Helmet", "Lock", "Pump", "Inner tube", "Saddle", "Bottle cage", "Mudguards", "Kickstand", "Basket", "Chain oil", "Gloves", "Rain cape", "Mirror", "Tyre", "Brake pads", "Grips", "Pedals", "Child seat", "Phone mount", "Repair kit"];
const BY_SALES = TITLES.map((title, i) => ({ key: `product-${i + 1}`, title, value: (Math.round((620 / (i + 1)) * 100) / 100).toFixed(2) }));
const BY_UNITS = TITLES.map((title, i) => ({ key: `product-${i + 1}`, title, value: Math.max(1, 12 - i) }));
const WEEK: Partial<Row> = {
  figures: WEEK_FIGURES,
  previous: { ...WEEK_FIGURES, totalSales: "1210.00", netSales: "1004.00", orders: 11, grossSales: "1150.00", discounts: "70.00", refunds: "76.00", unitsSold: 25 },
  series: WEEK_SERIES,
  productsBySales: BY_SALES,
  productsByUnits: BY_UNITS,
  productCount: 31,
  refundsWithoutItem: "15.00",
  buyers: { new: 8, returning: 5, unknown: 1 },
  wholesale: { wholesale: { orders: 3, netSales: "512.00" }, retail: { orders: 11, netSales: "694.10" } },
  otherCurrencies: [{ currency: "USD", orders: 2 }],
};
const CONTEXT = { ...NAMES, historySince: "2026-09-11T22:00:00Z", lastChangeAt: "2026-11-08T10:13:00+01:00" };
const BLANK = storeFigures();
const BLANK_SERIES = DAYS.map((day) => ({ at: at(day), totalSales: null, netSales: null, grossSales: null, discounts: null, refunds: null, orders: null, unitsSold: null }));

const CURRENT = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...WEEK, taxesIncluded: true, olderOrderChangesNotRecorded: true });
const DEV = storeStatisticsRow({
  storeId: "store-bikes-dev", siteId: "site-bikes", storeName: "A city bike shop (development)", storeDomain: "acme-widgets-dev.myshopify.com", destination: "previewBinding", development: true,
  historySince: "2026-10-20T00:00:00+02:00", figures: { totalSales: "64.00", netSales: "52.00", orders: 2, averageOrderValue: "26.00", grossSales: "56.00", discounts: "4.00", refunds: "0.00", unitsSold: 3 },
  series: DAYS.map((day, i) => ({ at: at(day), totalSales: i === 2 ? "64.00" : "0.00", netSales: i === 2 ? "52.00" : "0.00", grossSales: i === 2 ? "56.00" : "0.00", discounts: i === 2 ? "4.00" : "0.00", refunds: "0.00", orders: i === 2 ? 2 : 0, unitsSold: i === 2 ? 3 : 0 })),
  productsBySales: [{ key: "p1", title: "Bell", value: "40.00" }, { key: "p2", title: "Lock", value: "12.00" }], productsByUnits: [{ key: "p1", title: "Bell", value: 2 }, { key: "p2", title: "Lock", value: 1 }], productCount: 2,
  buyers: { new: 1, returning: 0, unknown: 1 },
});
const PAUSED = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...WEEK, asOf: "2026-11-06T09:00:00+01:00", recording: { state: "paused", reasons: [], since: "2026-11-06T09:00:00+01:00", blocked: true },
  series: WEEK_SERIES.map((point, i) => (i >= 4 ? { ...BLANK_SERIES[i]!, at: point.at } : point)) });
const FROZEN = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...WEEK, asOf: "2026-11-04T09:59:15+01:00", recording: { state: "needs attention", reasons: [
  { code: "privacy_request_failing", sentence: "A privacy request for this store could not be completed." },
  { code: "check_failing", sentence: "The last check of orders failed." },
], since: "2026-11-02T08:00:00+01:00", blocked: true }, series: WEEK_SERIES.map((point, i) => (i >= 3 ? { ...BLANK_SERIES[i]!, at: point.at } : point)) });
const BLOCKED = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, asOf: null, recording: { state: "needs attention", reasons: [{ code: "check_failing", sentence: "The last check of orders failed." }], since: "2026-11-07T09:00:00+01:00", blocked: true } });
const CURRENT_REASON = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...WEEK, recording: { state: "recording", reasons: [{ code: "statistics_rows_unreadable", sentence: "Some of this store's orders could not be read and are left out of these figures." }], since: "2026-11-02T08:00:00+01:00", blocked: false } });
const UNKNOWN = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...WEEK, recording: { state: "", reasons: [], since: null, blocked: false } });
const intake = (state: string) => storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, rebuilding: state === "rebuilding", recording: { state, reasons: [], since: "2026-11-08T09:00:00+01:00", blocked: true } });
const REFUNDS_UNREAD = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...WEEK, refundsNotRead: 2,
  figures: { ...WEEK_FIGURES, refunds: null, netSales: null, totalSales: null, unitsSold: null }, previous: null });
const NO_CURRENCY = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...WEEK, currency: null,
  figures: { ...BLANK, orders: 14, unitsSold: 31 }, previous: null, wholesale: null, otherCurrencies: [],
  productsBySales: [], series: WEEK_SERIES.map((point) => ({ ...point, totalSales: null, netSales: null, grossSales: null, discounts: null, refunds: null })) });
const NO_ZONE = storeStatisticsRow({ storeId: "store-bikes", ...NAMES, timezone: null, periodZone: "UTC", start: "2026-11-02T00:00:00Z", end: "2026-11-09T00:00:00Z", historySince: "2026-09-12T00:00:00Z",
  series: DAYS.map((day) => ({ ...BLANK_SERIES[0]!, at: `2026-11-${day}T00:00:00Z` })) });
const ZONE_NOT_LOADED = storeStatisticsRow({ storeId: "store-bikes", ...NAMES, ...WEEK, timezone: "Europe/Atlantis", periodZone: "UTC", start: "2026-11-02T00:00:00Z", end: "2026-11-09T00:00:00Z", historySince: "2026-09-12T00:00:00Z",
  series: WEEK_SERIES.map((point, i) => ({ ...point, at: `2026-11-${DAYS[i]}T00:00:00Z` })) });
const ZERO = { totalSales: "0.00", netSales: "0", orders: 0, averageOrderValue: null, grossSales: "0.00", discounts: "0.00", refunds: "0.00", unitsSold: 0 };
const EMPTY = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, figures: ZERO, previous: { ...ZERO }, buyers: { new: 0, returning: 0, unknown: 0 },
  series: DAYS.map((day) => ({ at: at(day), totalSales: "0.00", netSales: "0.00", grossSales: "0.00", discounts: "0.00", refunds: "0.00", orders: 0, unitsSold: 0 })) });
const BELOW_ZERO = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...WEEK,
  figures: { ...WEEK_FIGURES, totalSales: "-58.20", netSales: "-52.00", unitsSold: -2 },
  series: WEEK_SERIES.map((point, i) => ({ ...point, totalSales: ["12.00", "0.00", "-120.00", "0.00", "36.40", "0.00", "13.40"][i]! })) });
const STARTING = storeStatisticsRow({
  storeId: "store-bikes", siteId: "site-bikes", storeName: null, storeDomain: null, currency: null, timezone: null, periodZone: null, development: null, taxesIncluded: null,
  olderOrderChangesNotRecorded: null, bucket: null, start: null, end: null, prevStart: null, historySince: null, lastChangeAt: null,
  recording: { state: "starting", reasons: [], since: null, blocked: false }, rebuilding: null, series: [],
});

/** A connection whose store statistics read never answers. */
function pendingConnection(seed: FakeSeed) {
  const connection = fakeConnection(seed);
  const execute = connection.query.executeNamed.bind(connection.query) as (...args: unknown[]) => unknown;
  connection.query.executeNamed = ((name: string, ...rest: unknown[]) =>
    name === "storeStatistics" ? new Promise(() => {}) : execute(name, ...rest)) as typeof connection.query.executeNamed;
  return connection;
}

/** A deployable's page, under the app's settings so its period control works. */
function DeployableOn({ site, detail, can }: { site: Row; detail?: WorkspaceDetail; can: PartsHeld }) {
  const [settings, setSettings] = useState<DeployablesSettings>({ ...DEFAULT_DEPLOYABLES_SETTINGS, statisticsPeriod: "7d" });
  return (
    <DeployablesSettingsProvider value={{ settings, update: (patch) => setSettings((held) => ({ ...held, ...patch })), toggleSource: () => {} }}>
      <DeployablePage site={siteFromRow(site)} pkg={null} credentials={[]} viewerUserId="u-me" nameOf={() => ""} can={can} clusterDomain="memql.example.com" onBack={() => {}} onOpenSource={() => {}} initialDetail={detail} />
    </DeployablesSettingsProvider>
  );
}

export function storeStatisticsScenes(Frame: Frame) {
  const scene = (site: Row, seed: Omit<FakeSeed, "sites" | "stores">, detail?: WorkspaceDetail, can: PartsHeld = ALL_PARTS, connect?: () => unknown) => ({
    connect: connect ?? (() => fakeConnection({ sites: [site], stores: STORES, ...seed })),
    wrap: (element: ReactElement, role: string) => withSession(element, { role, userId: "u-me" }),
    render: () => <Frame fallback="Deployables"><DeployableOn site={site} detail={detail} can={can} /></Frame>,
  });
  const both = { storeStatistics: { "store-bikes": CURRENT, "store-bikes-dev": DEV } };
  const page = (row: Row) => scene(PRODUCTION_ONLY, { storeStatistics: { "store-bikes": row } }, "storeStatistics");
  return {
    "storefront-with-store-statistics": scene(BIKES, both),
    "storefront-without-sales": scene(BIKES, both, undefined, partsWithout("sales")),
    "storefront-draft-store-statistics": scene(DRAFT, both),
    "store-statistics": page(CURRENT),
    "store-statistics-products": page(CURRENT),
    "store-statistics-two-stores": scene(BIKES, both, "storeStatistics"),
    "store-statistics-both": scene(ONE_STORE, { storeStatistics: { "store-bikes": { ...CURRENT, destination: "both" } } }, "storeStatistics"),
    "store-statistics-development": scene(TESTING_ONLY, { storeStatistics: { "store-bikes-dev": DEV } }, "storeStatistics"),
    "store-statistics-paused": page(PAUSED),
    "store-statistics-frozen": page(FROZEN),
    "store-statistics-blocked": page(BLOCKED),
    "store-statistics-current-reason": page(CURRENT_REASON),
    "store-statistics-unknown": page(UNKNOWN),
    "store-statistics-starting": page(STARTING),
    "store-statistics-purging": page(intake("purging")),
    "store-statistics-rebuilding": page(intake("rebuilding")),
    "store-statistics-refunds-unread": page(REFUNDS_UNREAD),
    "store-statistics-no-currency": page(NO_CURRENCY),
    "store-statistics-no-zone": page(NO_ZONE),
    "store-statistics-zone-not-loaded": page(ZONE_NOT_LOADED),
    "store-statistics-empty": page(EMPTY),
    "store-statistics-below-zero": page(BELOW_ZERO),
    "store-statistics-pending": scene(PRODUCTION_ONLY, {}, "storeStatistics", ALL_PARTS, () => pendingConnection({ sites: [PRODUCTION_ONLY], stores: STORES })),
    "store-statistics-refused": scene(PRODUCTION_ONLY, { storeStatisticsError: "This deployable does not serve that store for you." }, "storeStatistics"),
  };
}

/** Views reached by pressing: `click=` is set for them, so a one-shot capture opens them. */
export const STORE_STATISTICS_CLICKS: Record<string, string> = {
  "store-statistics-products": "All products",
};
