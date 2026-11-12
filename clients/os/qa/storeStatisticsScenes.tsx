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
//   view=store-statistics-products-paused      All products of a paused store: its notice, as of the last clear run
//   view=store-statistics-products-frozen      All products of a store that needs attention, as of its last clear run
//   view=store-statistics-products-rebuilding  All products open when the next answer says the store is rebuilding
//   view=store-statistics-products-absent      All products open when the next answer sends no product list
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

// EVERY SCENE IS AN ANSWER THE ENGINE COULD SEND: the figures are the sums of
// the days the series holds, Net sales is Gross less Discounts and Refunds,
// the products add up to Net sales and the refunds that name none, the units
// to Units sold, and the two sides of wholesale to Net sales and Orders. The
// scenes add in cents, so the fixtures carry no rounding of their own.

/** A week of an invented bike shop, by day, in cents. */
const DAY = {
  gross: [13000, 28000, 10500, 0, 32000, 25500, 29800],
  discounts: [1200, 1950, 860, 0, 1880, 800, 1500],
  refunds: [0, 0, 0, 4000, 0, 3500, 2500],
  // Net plus shipping, tax and duties, less those refunded.
  total: [14280, 31810, 11760, -4620, 36440, 25650, 32920],
  orders: [2, 3, 1, 0, 3, 2, 3],
  units: [7, 11, 4, -1, 13, 8, 9],
};
const amount = (cents: number) => (cents / 100).toFixed(2);
const sum = (values: readonly number[]) => values.reduce((a, b) => a + b, 0);
const TITLES = ["City bike", "Folding bike", "Bell", "Lights set", "Pannier", "Helmet", "Lock", "Pump", "Inner tube", "Saddle", "Bottle cage", "Mudguards", "Kickstand", "Basket", "Chain oil", "Gloves", "Rain cape", "Mirror", "Tyre", "Brake pads", "Grips", "Pedals", "Child seat", "Phone mount", "Repair kit", "Bar tape", "Spokes", "Reflectors", "Cable lock", "Seat post", "Front rack"];

/** `total` split over `count` shares falling as 1, 1/2, 1/3 ...; whole units, the remainder on the first. */
function shares(total: number, count: number): number[] {
  const weights = Array.from({ length: count }, (_, i) => 1 / (i + 1));
  const whole = sum(weights);
  const parts = weights.map((w) => Math.floor((total * w) / whole));
  parts[0]! += total - sum(parts);
  return parts;
}

/**
 * The answer for the days before `upTo` (later days are null, as a frozen
 * store's are), with `otherOrders` of the orders in another currency (in
 * Orders, not in the average), and refunds of `withoutItem` cents naming no
 * product.
 */
function week(upTo = 7, { otherOrders = 0, withoutItem = 1500 } = {}): Partial<Row> {
  const kept = <T,>(values: readonly T[]) => values.slice(0, upTo);
  const gross = sum(kept(DAY.gross)), discounts = sum(kept(DAY.discounts)), refunds = sum(kept(DAY.refunds));
  const net = gross - discounts - refunds, orders = sum(kept(DAY.orders)), units = sum(kept(DAY.units));
  const noProduct = refunds >= withoutItem ? withoutItem : 0;
  const count = Math.min(TITLES.length, units);
  const bySales = shares(net + noProduct, count);
  // Every product listed sold at least one unit.
  const byUnits = shares(units - count, count).map((value) => value + 1);
  const listed = (values: number[], money: boolean) => values.slice(0, 25).map((value, i) => ({ key: `product-${i + 1}`, title: TITLES[i]!, value: money ? amount(value) : value }));
  const wholesaleOrders = Math.floor(orders / 4);
  const wholesaleNet = wholesaleOrders === 0 ? 0 : Math.round(net * 0.4);
  const fresh = Math.ceil(orders * 0.6), unknown = orders >= 3 ? 1 : 0;
  return {
    figures: {
      totalSales: amount(sum(kept(DAY.total))), netSales: amount(net), orders, grossSales: amount(gross), discounts: amount(discounts), refunds: amount(refunds), unitsSold: units,
      averageOrderValue: orders - otherOrders > 0 ? amount(Math.round((gross - discounts) / (orders - otherOrders))) : null,
    },
    previous: { totalSales: "1210.00", netSales: "1004.00", orders: 11, averageOrderValue: "98.18", grossSales: "1150.00", discounts: "70.00", refunds: "76.00", unitsSold: 25 },
    series: DAYS.map((day, i) => i < upTo
      ? { at: at(day), grossSales: amount(DAY.gross[i]!), discounts: amount(DAY.discounts[i]!), refunds: amount(DAY.refunds[i]!), netSales: amount(DAY.gross[i]! - DAY.discounts[i]! - DAY.refunds[i]!), totalSales: amount(DAY.total[i]!), orders: DAY.orders[i]!, unitsSold: DAY.units[i]! }
      : { at: at(day), grossSales: null, discounts: null, refunds: null, netSales: null, totalSales: null, orders: null, unitsSold: null }),
    productsBySales: listed(bySales, true),
    productsByUnits: listed(byUnits, false),
    productCount: count,
    refundsWithoutItem: noProduct === 0 ? null : amount(noProduct),
    buyers: { new: fresh, returning: orders - fresh - unknown, unknown },
    wholesale: { wholesale: { orders: wholesaleOrders, netSales: amount(wholesaleNet) }, retail: { orders: orders - wholesaleOrders, netSales: amount(net - wholesaleNet) } },
    otherCurrencies: otherOrders > 0 ? [{ currency: "USD", orders: otherOrders }] : [],
  };
}

const CONTEXT = { ...NAMES, historySince: "2026-09-11T22:00:00Z", lastChangeAt: "2026-11-08T10:13:00+01:00" };
const BLANK = storeFigures();
const FULL = week(7, { otherOrders: 2 });
const blankSeries = (series: unknown, keys: readonly string[]) => (series as Row[]).map((point) => ({ ...point, ...Object.fromEntries(keys.map((key) => [key, null])) }));

const CURRENT = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...FULL, taxesIncluded: true, olderOrderChangesNotRecorded: true });
const DEV = storeStatisticsRow({
  storeId: "store-bikes-dev", siteId: "site-bikes", storeName: "A city bike shop (development)", storeDomain: "acme-widgets-dev.myshopify.com", destination: "previewBinding", development: true,
  historySince: "2026-10-20T00:00:00+02:00", figures: { totalSales: "64.00", netSales: "52.00", orders: 2, averageOrderValue: "26.00", grossSales: "56.00", discounts: "4.00", refunds: "0.00", unitsSold: 3 },
  series: DAYS.map((day, i) => ({ at: at(day), totalSales: i === 2 ? "64.00" : "0.00", netSales: i === 2 ? "52.00" : "0.00", grossSales: i === 2 ? "56.00" : "0.00", discounts: i === 2 ? "4.00" : "0.00", refunds: "0.00", orders: i === 2 ? 2 : 0, unitsSold: i === 2 ? 3 : 0 })),
  productsBySales: [{ key: "p1", title: "Bell", value: "40.00" }, { key: "p2", title: "Lock", value: "12.00" }], productsByUnits: [{ key: "p1", title: "Bell", value: 2 }, { key: "p2", title: "Lock", value: 1 }], productCount: 2,
  buyers: { new: 1, returning: 0, unknown: 1 },
});
// Paused on Nov 6 at 09:00: Nov 6's bucket began before then and is kept, the days after are null.
const PAUSED = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...week(5), asOf: "2026-11-06T09:00:00+01:00", recording: { state: "paused", reasons: [], since: "2026-11-06T09:00:00+01:00", blocked: true } });
// The order check failed on Nov 4 at 09:59: the figures are as of then.
const FROZEN = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...week(3), asOf: "2026-11-04T09:59:15+01:00", recording: { state: "needs attention", reasons: [
  { code: "privacy_request_failing", sentence: "A privacy request for this store could not be completed." },
  { code: "check_failing", sentence: "The last check of orders failed." },
], since: "2026-11-02T08:00:00+01:00", blocked: true } });
// Blocked before any clear run, and no zone recorded: no figure and no list is known.
const UNKNOWN_LISTS = { productsBySales: null, productsByUnits: null, productCount: null };
const BLOCKED = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...UNKNOWN_LISTS, asOf: null, recording: { state: "needs attention", reasons: [{ code: "check_failing", sentence: "The last check of orders failed." }], since: "2026-11-07T09:00:00+01:00", blocked: true } });
const CURRENT_REASON = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...FULL, recording: { state: "recording", reasons: [{ code: "statistics_rows_unreadable", sentence: "Some of this store's orders could not be read and are left out of these figures." }], since: "2026-11-02T08:00:00+01:00", blocked: false } });
const UNKNOWN = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...FULL, recording: { state: "", reasons: [], since: null, blocked: false } });
const intake = (state: string, over: Partial<Row> = {}) => storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...over, rebuilding: state === "rebuilding", recording: { state, reasons: [], since: "2026-11-08T09:00:00+01:00", blocked: true } });
// Two refunds not read yet: the four figures they blank, their buckets, and
// the lists that subtract refunds are not known.
const REFUNDS_UNREAD = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...FULL, refundsNotRead: 2, previous: null,
  figures: { ...(FULL.figures as Row), refunds: null, netSales: null, totalSales: null, unitsSold: null },
  series: blankSeries(FULL.series, ["refunds", "netSales", "totalSales", "unitsSold"]),
  productsBySales: null, productsByUnits: null, productCount: null, refundsWithoutItem: null,
  wholesale: { wholesale: { orders: 3, netSales: null }, retail: { orders: 11, netSales: null } } });
// No currency: every amount is null, counts stand.
const NO_CURRENCY = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, ...week(7), currency: null, previous: null,
  figures: { ...BLANK, orders: 14, unitsSold: 51 },
  series: blankSeries(week(7).series, ["grossSales", "discounts", "refunds", "netSales", "totalSales"]),
  productsBySales: null, refundsWithoutItem: null, wholesale: null });
const NO_ZONE = storeStatisticsRow({ storeId: "store-bikes", ...NAMES, ...UNKNOWN_LISTS, timezone: null, periodZone: "UTC", start: "2026-11-02T00:00:00Z", end: "2026-11-09T00:00:00Z", historySince: "2026-09-12T00:00:00Z",
  series: DAYS.map((day) => ({ at: `2026-11-${day}T00:00:00Z`, totalSales: null, netSales: null, grossSales: null, discounts: null, refunds: null, orders: null, unitsSold: null })) });
const ZONE_NOT_LOADED = storeStatisticsRow({ storeId: "store-bikes", ...NAMES, ...FULL, timezone: "Europe/Atlantis", periodZone: "UTC", start: "2026-11-02T00:00:00Z", end: "2026-11-09T00:00:00Z", historySince: "2026-09-12T00:00:00Z",
  series: (FULL.series as Row[]).map((point, i) => ({ ...point, at: `2026-11-${DAYS[i]}T00:00:00Z` })) });
const ZERO = { totalSales: "0.00", netSales: "0", orders: 0, averageOrderValue: null, grossSales: "0.00", discounts: "0.00", refunds: "0.00", unitsSold: 0 };
const EMPTY = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT, figures: ZERO, previous: { ...ZERO }, buyers: { new: 0, returning: 0, unknown: 0 }, productCount: 0,
  series: DAYS.map((day) => ({ at: at(day), totalSales: "0.00", netSales: "0.00", grossSales: "0.00", discounts: "0.00", refunds: "0.00", orders: 0, unitsSold: 0 })) });
// A week whose refunds outweighed its sales: a City bike from an earlier
// order came back on Nov 4, and two small orders sold.
const BELOW_ZERO = storeStatisticsRow({ storeId: "store-bikes", ...CONTEXT,
  figures: { totalSales: "-58.20", netSales: "-52.00", orders: 2, averageOrderValue: "60.00", grossSales: "120.00", discounts: "0.00", refunds: "172.00", unitsSold: 2 },
  previous: { totalSales: "210.00", netSales: "180.00", orders: 3, averageOrderValue: "60.00", grossSales: "180.00", discounts: "0.00", refunds: "0.00", unitsSold: 4 },
  series: DAYS.map((day, i) => ({ at: at(day),
    grossSales: ["40.00", "0.00", "0.00", "0.00", "80.00", "0.00", "0.00"][i]!, discounts: "0.00", refunds: i === 2 ? "172.00" : "0.00",
    netSales: ["40.00", "0.00", "-172.00", "0.00", "80.00", "0.00", "0.00"][i]!, totalSales: ["44.00", "0.00", "-190.00", "0.00", "87.80", "0.00", "0.00"][i]!,
    orders: [1, 0, 0, 0, 1, 0, 0][i]!, unitsSold: [2, 0, -1, 0, 1, 0, 0][i]! })),
  productsBySales: [{ key: "p2", title: "Lock", value: "80.00" }, { key: "p1", title: "Bell", value: "40.00" }, { key: "p3", title: "City bike", value: "-172.00" }],
  productsByUnits: [{ key: "p1", title: "Bell", value: 2 }, { key: "p2", title: "Lock", value: 1 }, { key: "p3", title: "City bike", value: -1 }], productCount: 3,
  buyers: { new: 1, returning: 1, unknown: 0 },
  wholesale: { wholesale: { orders: 0, netSales: "0.00" }, retail: { orders: 2, netSales: "-52.00" } } });
const STARTING = storeStatisticsRow({
  storeId: "store-bikes", siteId: "site-bikes", storeName: null, storeDomain: null, currency: null, timezone: null, periodZone: null, development: null, taxesIncluded: null,
  olderOrderChangesNotRecorded: null, bucket: null, start: null, end: null, prevStart: null, historySince: null, lastChangeAt: null,
  recording: { state: "starting", reasons: [], since: null, blocked: false }, rebuilding: null, series: [],
});

/**
 * A connection that answers `first`, then `later` once the window is focused
 * again 5 seconds in, after "All products" is pressed: how a page that is
 * open meets the next answer.
 */
function changingConnection(site: Row, first: Row, later: Row) {
  const answers: Record<string, Row> = { "store-bikes": first };
  const connection = fakeConnection({ sites: [site], stores: STORES, storeStatistics: answers });
  window.setTimeout(() => {
    answers["store-bikes"] = later;
    window.dispatchEvent(new Event("focus"));
  }, 5000);
  return connection;
}

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
    "store-statistics-products-paused": page(PAUSED),
    "store-statistics-products-frozen": page(FROZEN),
    "store-statistics-products-rebuilding": scene(PRODUCTION_ONLY, {}, "storeStatistics", ALL_PARTS, () => changingConnection(PRODUCTION_ONLY, CURRENT, intake("rebuilding", { ...FULL }))),
    "store-statistics-products-absent": scene(PRODUCTION_ONLY, {}, "storeStatistics", ALL_PARTS, () => changingConnection(PRODUCTION_ONLY, CURRENT, { ...CURRENT, productsBySales: null, productsByUnits: null, productCount: null })),
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
  "store-statistics-products-paused": "All products",
  "store-statistics-products-frozen": "All products",
  "store-statistics-products-rebuilding": "All products",
  "store-statistics-products-absent": "All products",
};
