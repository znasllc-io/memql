import { useEffect, useRef, useState } from "react";

import { AutoRefresh, Button, Caption, Head, Notice, useNow } from "../../../kit";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { formatCount, formatFreshness, formatMoment, formatMoney, formatMoneyAxis } from "../../../kit/format";
import { LocalTabs } from "../../../kit/LocalTabs";
import { absent, absentSentence, figureOf, type Figure } from "../../../kit/measure";
import { PageHelp } from "../../../kit/pageHelp";
import { RankedList, StatFigures, TimeChart, TimeChartSkeleton, bucketLabel, previousComparable, type RankedRow, type StatFigure } from "../../../kit/statistics";
import { useWide } from "../../../kit/useWide";
import { useOsConnection } from "../../../live/connection";
import type { SiteRow } from "../rows";
import { useDeployablesSettings } from "../settingsContext";
import { StatisticsPeriodControl } from "../statistics/SiteStatisticsView";
import { statisticsRefreshMs } from "../statistics/useSiteStatistics";
import { DestinationControl, type DestinationChoice } from "./DestinationControl";
import { PRODUCTS_SHOWN, ProductTabs, ProductsListPage, productFormat, productRows, productsFooter, type ProductsBy } from "./ProductsListPage";
import { destinationsOf, type StoreFigures, type StorePoint, type StoreStatistics } from "./read";
import { useStoreStatistics } from "./useStoreStatistics";
import "../statistics/statistics.css";

// A STOREFRONT'S STORE STATISTICS (the 2026-11-11 store statistics design,
// section 8): the store's sales, beside the website's Statistics. It takes the
// pane the way Reviews does, under the deployable's trail, and draws in
// Statistics' manner and with Statistics' pieces only: the Head with its quiet
// controls at the right, the store's context in captions, then a notice when
// one applies, the eight figures, the chart of the one chosen, and the lists.
// Spacing only between them, no dividers.
//
// THE PAGE OPENS ON PRODUCTION when Production has a store, else on Testing,
// and the choice is not remembered: whichever store a person looked at last
// is not a reason to show it next time. The Destination control is drawn from
// the site's own row whenever both destinations name a store -- before any
// answer and after a refusal, so the other store can always be tried and the
// Head does not move when an answer lands. It gives way only when an answer
// says the two name one store ("both"), because only the engine compares the
// two forms of an id; the context then says "Production and Testing".
//
// THE CONTEXT SAYS ONLY WHAT THE ANSWER HOLDS. A store the walk holds no row
// for yet has no currency, no zone and no period: nothing of those is said,
// and no figure is drawn as zero. Days are named in the zone the engine cut
// the period in (`periodZone`): the store's when it loads, UTC otherwise, so
// UTC days are never presented as the store's.
//
// ===========================================================================
// THE NOTICE SAYS WHAT STANDS IN THE WAY, AND THE ENGINE SAYS WHY
// ===========================================================================
// The page has a sentence of its own only for a state the mirror gives no
// reason for (paused, not connected), for a store still being brought in
// (starting, purging, rebuilding), and for a state that is not known. Every
// reason the answer lists is the engine's own sentence, shown in every state,
// `recording` included; the page writes none for a reason and never branches
// on a code. Then whether the figures are as of an earlier moment, and the
// refunds still being read.
//
// ===========================================================================
// AN ABSENT FIGURE SAYS WHY, A HOVER AWAY
// ===========================================================================
// The engine answers null for what it does not know, and the page says why
// from the answer's own facts, the first that applies (`absentReason`). The
// page computes nothing from an amount: every sum, the average and the floors
// are the engine's, and an amount is only written.

export const OLDER_ORDERS_LINE = "Refunds, edits and cancellations of orders placed more than 60 days ago are not recorded.";
export const DEVELOPMENT_LINE = "This is a development store: every order on it is a test order, and they are counted.";
export const TAX_LINE = "Prices on this store include tax.";

export const BROUGHT_IN_LINE = "The store's orders are still being brought in.";
export const PAUSED_LINE = "Recording is paused for this store.";
export const NOT_CONNECTED_LINE = "This store is not connected.";
export const NOT_KNOWN_LINE = "Whether this store is recording is not known.";
export const NO_FIGURE_LINE = "No figure can be given until this is fixed.";
export const STORE_PAGE_NEXT = "The Store page of this deployable says what fixes it.";
export const NO_ZONE_REASON = "The store's time zone has not been recorded yet.";
export const NO_CURRENCY_REASON = "The store's currency has not been recorded yet.";
export const INCOMPLETE_REFUND_REASON = "A refund in this period has more lines than were recorded.";
export const NO_ORDERS = "No orders in this period.";

/** "1 refund is still being read from Shopify." / "N refunds are ..." */
export function refundsUnreadSentence(count: number): string {
  return count === 1 ? "1 refund is still being read from Shopify." : `${formatCount(count)} refunds are still being read from Shopify.`;
}

type FigureId = keyof StoreFigures;
type SeriesId = Exclude<keyof StorePoint, "at">;

interface SalesFigure {
  id: FigureId;
  label: string;
  money: boolean;
  /** The series' key for it; absent for the average, which has no per-bucket value. */
  series?: SeriesId;
}

/** The eight figures, in D12's order. */
export const SALES_FIGURES: readonly SalesFigure[] = [
  { id: "totalSales", label: "Total sales", money: true, series: "totalSales" },
  { id: "netSales", label: "Net sales", money: true, series: "netSales" },
  { id: "orders", label: "Orders", money: false, series: "orders" },
  { id: "averageOrderValue", label: "Average order value", money: true },
  { id: "grossSales", label: "Gross sales", money: true, series: "grossSales" },
  { id: "discounts", label: "Discounts", money: true, series: "discounts" },
  { id: "refunds", label: "Refunds", money: true, series: "refunds" },
  { id: "unitsSold", label: "Units sold", money: false, series: "unitsSold" },
];

/** The four figures a refund not yet read blanks (D22). */
const BLANKED_BY_UNREAD_REFUNDS: ReadonlySet<FigureId> = new Set(["refunds", "netSales", "totalSales", "unitsSold"]);

/** While the store's orders are being brought in (or deleted, or walked again), no figure is known. */
function broughtIn(stats: StoreStatistics): boolean {
  const state = stats.recording.state;
  return state === "starting" || state === "purging" || state === "rebuilding" || stats.rebuilding === true;
}

/**
 * Why a figure the answer left null is absent: the first of these that
 * applies. "" when none does, and the kit then says nothing has reported it.
 */
export function absentReason(stats: StoreStatistics, id: FigureId): string {
  const money = SALES_FIGURES.find((spec) => spec.id === id)?.money ?? false;
  // A store with no row yet has recorded nothing at all; its zone is not missing, it is not read yet.
  if (stats.timezone === null && stats.start !== null) return NO_ZONE_REASON;
  if (broughtIn(stats)) return BROUGHT_IN_LINE;
  if (money && stats.currency === null) return NO_CURRENCY_REASON;
  if ((stats.refundsNotRead ?? 0) > 0 && BLANKED_BY_UNREAD_REFUNDS.has(id)) return refundsUnreadSentence(stats.refundsNotRead ?? 0);
  if (id === "unitsSold" && (stats.incompleteRefunds ?? 0) > 0) return INCOMPLETE_REFUND_REASON;
  if (id === "averageOrderValue" && stats.figures.orders === 0) return NO_ORDERS;
  if (stats.recording.blocked && stats.asOf === null) return NO_FIGURE_LINE;
  return "";
}

function figureFor(stats: StoreStatistics, id: FigureId, values: StoreFigures): Figure {
  const value = broughtIn(stats) ? null : values[id];
  return value === null ? absent("unmeasured", absentReason(stats, id)) : figureOf(value, stats.floors[id] === true);
}

/** The figure row from an answer, or its shapes while there is none. */
export function salesFigures(stats: StoreStatistics | null): StatFigure[] {
  if (stats === null) return SALES_FIGURES.map((spec) => ({ id: spec.id, label: spec.label, figure: absent("unread"), chartable: spec.series !== undefined, loading: true }));
  const money = formatMoney(stats.currency ?? "");
  const comparable = previousComparable(stats.prevStart, stats.historySince);
  return SALES_FIGURES.map((spec) => {
    const previous = comparable && stats.previous !== null ? stats.previous[spec.id] : null;
    return {
      id: spec.id,
      label: spec.label,
      figure: figureFor(stats, spec.id, stats.figures),
      ...(previous !== null ? { previous: figureOf(previous) } : {}),
      format: spec.money ? money : formatCount,
      chartable: spec.series !== undefined,
    };
  });
}

/** What a list with no rows says: no orders when the orders were counted, else why they were not. */
function emptyList(stats: StoreStatistics | null): string {
  if (stats === null) return NO_ORDERS;
  const orders = figureFor(stats, "orders", stats.figures);
  return orders.kind === "measured" ? NO_ORDERS : absentSentence(orders);
}

/** Block 2: what stands in the way, in the order the design gives; empty when nothing does. */
export function noticeLines(stats: StoreStatistics): { lines: string[]; next?: string } {
  const state = stats.recording.state;
  const lines: string[] = [];
  const intake = broughtIn(stats);
  if (intake) lines.push(BROUGHT_IN_LINE);
  else if (state === "paused") lines.push(PAUSED_LINE);
  else if (state === "not connected") lines.push(NOT_CONNECTED_LINE);
  else if (state === "") lines.push(NOT_KNOWN_LINE);
  for (const reason of stats.recording.reasons) lines.push(reason.sentence);
  if (!intake && stats.asOf !== null) lines.push(`These figures are as of ${formatMoment(stats.asOf)}.`);
  else if (!intake && stats.recording.blocked) lines.push(NO_FIGURE_LINE);
  if ((stats.refundsNotRead ?? 0) > 0) lines.push(refundsUnreadSentence(stats.refundsNotRead ?? 0));
  const fixable = state === "paused" || state === "not connected" || state === "needs attention";
  return fixable && !intake && lines.length > 0 ? { lines, next: STORE_PAGE_NEXT } : { lines };
}

export interface StoreStatisticsSectionProps {
  site: SiteRow;
  /** The deployable's name, as its page's trail says it. */
  name: string;
  /** The trail down to the deployable: Store statistics is added after it. */
  trail: readonly Breadcrumb[];
  /** Back out of Store statistics, to the deployable. */
  back: { label: string; onSelect: () => void };
}

export function StoreStatisticsSection({ site, name, trail, back }: StoreStatisticsSectionProps) {
  const connection = useOsConnection();
  const { settings } = useDeployablesSettings();
  const period = settings.statisticsPeriod;
  const destinations = destinationsOf(site);
  const [chosen, setDestination] = useState<DestinationChoice>(destinations.production !== "" ? "production" : "testing");
  // A destination whose store was unbound while the page stood reads the other.
  const destination: DestinationChoice = destinations[chosen] !== "" ? chosen : chosen === "production" ? "testing" : "production";
  const storeId = destinations[destination];
  const reading = useStoreStatistics({ siteId: site.id, storeId, period, enabled: true });
  const stats = reading.value;
  // The whole list of products, as a page under this one's trail, when open.
  const [allProducts, setAllProducts] = useState<ProductsBy | null>(null);

  // WHETHER THE TWO DESTINATIONS NAME ONE STORE is the last answer's word,
  // held across a change of destination or period so the control does not
  // come and go while the next read is out.
  const [oneStore, setOneStore] = useState(false);
  useEffect(() => {
    if (stats !== null && stats.destination !== null) setOneStore(stats.destination === "both");
  }, [stats]);
  const offerDestination = destinations.production !== "" && destinations.testing !== "" && !oneStore;

  const pending = stats === null && (reading.state === "idle" || reading.state === "loading");
  const here: Breadcrumb[] = [...trail, { label: "Store statistics", onSelect: () => setAllProducts(null) }];

  return (
    <section
      className="os-action-pane"
      aria-label={`Store statistics for ${site.hostname}`}
      data-os-dividers="none"
      data-os-page-context={JSON.stringify({ page: allProducts === null ? "Store statistics" : "Store statistics: products", siteId: site.id, hostname: site.hostname, name, destination, storeId, period })}
    >
      <div className="os-action-body os-app-stack">
        {allProducts !== null ? (
          <ProductsListPage statistics={stats} by={allProducts} trail={here} back={{ label: "Store statistics", onSelect: () => setAllProducts(null) }} empty={emptyList(stats)} />
        ) : (
          <Head navigationKey={`storeStatistics:${site.id}`} title="Store statistics" breadcrumbs={[...trail, { label: "Store statistics" }]} back={back}>
            {offerDestination ? <DestinationControl value={destination} onChange={setDestination} /> : null}
            <StatisticsPeriodControl />
          </Head>
        )}
        <StoreStatisticsHelp />
        <AutoRefresh onRefresh={reading.reload} busy={reading.state === "loading"} enabled={connection !== null} intervalMs={statisticsRefreshMs(period)} />
        {reading.state === "error" && reading.error !== "" ? (
          // VERBATIM: the engine's own sentence, kept while the read is tried
          // again. A dropped connection has no sentence here: the shell's own
          // notice reports it, and the read is tried again when it is back.
          // Try again is not offered while a retry is already out.
          <Notice tone="error" sentence={reading.error} next="Retrying automatically.">
            {reading.inFlight ? null : <Button onClick={() => void reading.reload()}>Try again</Button>}
          </Notice>
        ) : null}
        {allProducts !== null ? null : (
          <>
            {pending ? <Caption><InlineSkeleton label="Loading the store" /></Caption> : stats !== null ? <StoreContext stats={stats} /> : null}
            {/* A refusal with nothing held draws no shape under it. */}
            {pending || stats !== null ? <StoreBody stats={stats} onShowAll={setAllProducts} /> : null}
          </>
        )}
      </div>
    </section>
  );
}

/** Blocks 2 to 8: the notice, the figures and their chart, the lists, and the other currencies. */
function StoreBody({ stats, onShowAll }: { stats: StoreStatistics | null; onShowAll: (by: ProductsBy) => void }) {
  const root = useRef<HTMLDivElement | null>(null);
  // TWO COLUMNS FROM 760px OF PANE, as the website's lists stand: Products
  // beside Buyers, then Wholesale and retail.
  const wide = useWide(root, 760);
  const [selected, setSelected] = useState<FigureId>("totalSales");
  const [productsBy, setProductsBy] = useState<ProductsBy>("sales");
  const [sideBy, setSideBy] = useState<"netSales" | "orders">("netSales");
  const notice = stats === null ? { lines: [] } : noticeLines(stats);
  const empty = emptyList(stats);
  const intake = stats !== null && broughtIn(stats);
  const money = formatMoney(stats?.currency ?? "");
  const products = stats === null || intake ? [] : productRows(stats, productsBy);

  return (
    <div ref={root} className="deployable-statistics">
      {notice.lines.length > 0 ? (
        <Notice tone={notice.next !== undefined ? "warn" : "info"} sentence={notice.lines[0]} next={notice.next}>
          {notice.lines.slice(1).map((line, index) => <p key={`${index}:${line}`} className="os-notice-line">{line}</p>)}
        </Notice>
      ) : null}

      <div className="deployable-statistics-figures">
        <StatFigures label="Sales figures" figures={salesFigures(stats)} selected={selected} onSelect={(id) => setSelected(id as FigureId)} />
        <SalesChart stats={stats} selected={selected} />
      </div>

      <div className="deployable-statistics-lists" data-wide={wide || undefined}>
        <RankedList
          title="Products"
          rows={products}
          limit={PRODUCTS_SHOWN}
          format={stats === null ? formatCount : productFormat(stats, productsBy)}
          onShowAll={() => onShowAll(productsBy)}
          showAllLabel="All products"
          footer={stats === null ? undefined : productsFooter(stats, Math.min(PRODUCTS_SHOWN, products.length))}
          control={<ProductTabs value={productsBy} onChange={setProductsBy} />}
          loading={stats === null}
          empty={empty}
        />
        <RankedList title="Buyers" rows={stats === null || intake ? [] : buyerRows(stats)} loading={stats === null} empty={empty} />
        {stats !== null && stats.wholesale !== null ? (
          <RankedList
            title="Wholesale and retail"
            rows={intake ? [] : sideRows(stats, sideBy)}
            format={sideBy === "netSales" ? money : formatCount}
            empty={empty}
            control={
              <div className="deployable-statistics-device-tabs" data-os-scroll-ignore>
                <LocalTabs label="Wholesale and retail by" value={sideBy} onChange={setSideBy} options={[["netSales", "Net sales"], ["orders", "Orders"]]} />
              </div>
            }
          />
        ) : null}
      </div>

      {stats !== null && stats.otherCurrencies.length > 0 ? <Caption>{otherCurrenciesSentence(stats.otherCurrencies)}</Caption> : null}
    </div>
  );
}

/** The chart of the chosen figure, its shape while there is no answer, and nothing without a series. */
function SalesChart({ stats, selected }: { stats: StoreStatistics | null; selected: FigureId }) {
  const spec = SALES_FIGURES.find((candidate) => candidate.id === selected && candidate.series !== undefined) ?? SALES_FIGURES[0]!;
  if (stats === null) return <TimeChartSkeleton label={`Loading ${spec.label.toLowerCase()} over time`} />;
  if (stats.bucket === null || stats.series.length === 0 || spec.series === undefined) return null;
  const key = spec.series;
  const intake = broughtIn(stats);
  return (
    <TimeChart
      label={spec.label}
      bucket={stats.bucket}
      points={stats.series.map((point) => ({ at: point.at, value: intake ? null : point[key] }))}
      format={spec.money ? formatMoney(stats.currency ?? "") : formatCount}
      axisFormat={spec.money ? formatMoneyAxis(stats.currency ?? "") : formatCount}
      timeZone={stats.periodZone ?? undefined}
    />
  );
}

/** New, Returning, No customer recorded; none when no order was counted. */
function buyerRows(stats: StoreStatistics): RankedRow[] {
  const buyers = stats.buyers;
  if (buyers === null || (buyers.new ?? 0) + (buyers.returning ?? 0) + (buyers.unknown ?? 0) === 0) return [];
  return [
    { key: "new", label: "New", value: buyers.new ?? Number.NaN },
    { key: "returning", label: "Returning", value: buyers.returning ?? Number.NaN },
    { key: "unknown", label: "No customer recorded", value: buyers.unknown ?? Number.NaN },
  ];
}

/** Wholesale and Retail, by net sales or by orders; none when neither side has an order. */
function sideRows(stats: StoreStatistics, by: "netSales" | "orders"): RankedRow[] {
  const sides = stats.wholesale;
  if (sides === null || (sides.wholesale.orders ?? 0) + (sides.retail.orders ?? 0) === 0) return [];
  return [
    { key: "wholesale", label: "Wholesale", value: sides.wholesale[by] ?? Number.NaN },
    { key: "retail", label: "Retail", value: sides.retail[by] ?? Number.NaN },
  ];
}

/** "2 orders in USD are not in these sums.", or several currencies in one sentence. */
export function otherCurrenciesSentence(others: readonly { currency: string; orders: number | null }[]): string {
  const parts = others.map(({ currency, orders }) => (orders === null ? `orders in ${currency}` : `${formatCount(orders)} ${orders === 1 ? "order" : "orders"} in ${currency}`));
  const joined = parts.length <= 1 ? (parts[0] ?? "") : `${parts.slice(0, -1).join(", ")} and ${parts.at(-1)}`;
  const one = others.length === 1 && others[0]?.orders === 1;
  return `${joined.charAt(0).toUpperCase()}${joined.slice(1)} ${one ? "is" : "are"} not in these sums.`;
}

/**
 * What the figures mean and how they differ from Shopify's own reports (the
 * design's section 5), said once in the window's one information control,
 * never as standing copy on the page.
 */
function StoreStatisticsHelp() {
  return (
    <PageHelp title="Store statistics">
      <p>Orders are the orders placed in the period, on the day they were processed in the store&rsquo;s time zone. A cancelled order leaves every figure, its refunds with it, in every period. Test orders are left out, except on a development store.</p>
      <p>Gross sales is each line&rsquo;s price before discounts times the units sold; gift cards are left out. Discounts are the discounts on those units. Refunds is the money refunded for items, on the day of the refund, without shipping, tax and duties.</p>
      <p>Net sales is Gross sales less Discounts and Refunds. Total sales is Net sales plus the shipping, tax and duties charged, less those refunded; tips and additional fees are left out.</p>
      <p>Average order value is Gross sales less Discounts, over the orders in the store&rsquo;s currency. Refunds do not enter it. Units sold is the units sold less the units refunded, and can be below zero.</p>
      <p>A buyer is new when their first order with the store falls in the period, and returning when they ordered before it. Orders are wholesale when their buyer holds wholesale access now, so a buyer whose access ends moves to retail.</p>
      <p>Only orders in the store&rsquo;s currency are summed; orders in another currency are counted and said apart.</p>
      <p>Shopify&rsquo;s own reports can differ: an edit that removes a unit changes the order&rsquo;s own day here, and the edit&rsquo;s day there; a cancellation removes the order here, where Shopify shows a sale and a return; a return fee lowers Refunds here, where Shopify shows it as its own line.</p>
    </PageHelp>
  );
}

/** A short phrase that a narrow pane must not break: its spaces do not wrap. */
function unbroken(phrase: string): string {
  return phrase.replace(/ /g, " ");
}

/** A day as the store's clock read it: "Sep 12", with its year when that is not this year's ("Sep 12, 2025"). */
function storeDay(at: string, now: Date, zone: string | undefined): string {
  const year = (instant: string) => bucketLabel(instant, "month", zone).split(" ").at(-1);
  const day = bucketLabel(at, "day", zone);
  return year(at) === year(now.toISOString()) ? day : `${day}, ${year(at)}`;
}

/**
 * Block 1: which store, in what currency and days, since when, how fresh;
 * then each line that applies. A store with no row yet shows only what it has
 * (what is said of its state is the notice's).
 */
function StoreContext({ stats }: { stats: StoreStatistics }) {
  const now = useNow(60_000);
  // The zone the engine cut the days in, never the one the store recorded
  // when that one did not load.
  const zone = stats.periodZone ?? undefined;
  const line = [
    // The name may be long, so it alone wraps where it must.
    stats.storeName ?? stats.storeDomain,
    stats.destination === "both" ? unbroken("Production and Testing") : null,
    stats.currency !== null ? unbroken(`Prices in ${stats.currency}`) : null,
    // No period was cut for a store with no row, so no days are named.
    stats.start !== null ? unbroken(`Days in ${stats.periodZone ?? "UTC"}`) : null,
    stats.historySince !== null ? unbroken(`Orders since ${storeDay(stats.historySince, now, zone)}`) : null,
    stats.lastChangeAt !== null ? unbroken(`Last order change recorded ${formatFreshness(stats.lastChangeAt, now)}`) : null,
  ].filter((part): part is string => part !== null && part !== "");
  return (
    <>
      {line.length > 0 ? <Caption>{line.join(" · ")}</Caption> : null}
      {stats.development === true ? <Caption>{DEVELOPMENT_LINE}</Caption> : null}
      {stats.taxesIncluded === true ? <Caption>{TAX_LINE}</Caption> : null}
      {stats.olderOrderChangesNotRecorded === true ? <Caption>{OLDER_ORDERS_LINE}</Caption> : null}
    </>
  );
}
