import { useEffect, useRef, useState } from "react";

import { AutoRefresh, Button, Caption, Head, Notice, useNow } from "../../../kit";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { formatCount, formatFreshness, formatMoney, formatMoneyAxis } from "../../../kit/format";
import { LocalTabs } from "../../../kit/LocalTabs";
import { absent, figureOf, type Figure } from "../../../kit/measure";
import { PageHelp } from "../../../kit/pageHelp";
import { RankedList, StatFigures, TimeChart, TimeChartSkeleton, bucketLabel, previousComparable, type RankedRow, type StatFigure } from "../../../kit/statistics";
import { useWide } from "../../../kit/useWide";
import { useOsConnection } from "../../../live/connection";
import type { SiteRow } from "../rows";
import { useDeployablesSettings } from "../settingsContext";
import { StatisticsPeriodControl } from "../statistics/SiteStatisticsView";
import { statisticsRefreshMs } from "../statistics/useSiteStatistics";
import { DestinationControl, type DestinationChoice } from "./DestinationControl";
import { PRODUCTS_SHOWN, ProductsBlock, ProductsListPage, type ProductsBy } from "./ProductsListPage";
import { destinationsOf, type StoreFigures, type StorePoint, type StoreStatistics } from "./read";
import { StoreNotice, absentReason, broughtIn, listAnswer, rowFormat, rowValue, type FigureId } from "./storeState";
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
// What the answer says about itself -- the notice, why a figure is absent,
// what a list with no rows says -- is read in `storeState.tsx`, which the
// whole list of products reads too.
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
          <ProductsListPage statistics={stats} by={allProducts} trail={here} back={{ label: "Store statistics", onSelect: () => setAllProducts(null) }} />
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
  const money = rowFormat(formatMoney(stats?.currency ?? ""));
  const buyers = stats === null ? null : listAnswer(stats, buyerRows(stats), "orders");
  const sides = stats === null ? null : listAnswer(stats, sideRows(stats, sideBy), sideBy);

  return (
    <div ref={root} className="deployable-statistics">
      {stats !== null ? <StoreNotice stats={stats} /> : null}

      <div className="deployable-statistics-figures">
        {/* With no series there is no chart, and a figure that would choose
            nothing stands as a plain figure (DESIGN.md "Statistics"). */}
        <StatFigures label="Sales figures" figures={salesFigures(stats)} selected={selected} onSelect={stats === null || hasSeries(stats) ? (id) => setSelected(id as FigureId) : undefined} />
        <SalesChart stats={stats} selected={selected} />
      </div>

      <div className="deployable-statistics-lists" data-wide={wide || undefined}>
        <ProductsBlock stats={stats} by={productsBy} onBy={setProductsBy} title="Products" limit={PRODUCTS_SHOWN} onShowAll={() => onShowAll(productsBy)} />
        <RankedList title="Buyers" rows={buyers?.kind === "rows" ? buyers.rows : []} format={rowFormat(formatCount)} loading={stats === null} empty={buyers?.kind === "empty" ? buyers.empty : undefined} />
        {stats !== null && stats.wholesale !== null ? (
          <RankedList
            title="Wholesale and retail"
            rows={sides?.kind === "rows" ? sides.rows : []}
            format={sideBy === "netSales" ? money : rowFormat(formatCount)}
            empty={sides?.kind === "empty" ? sides.empty : undefined}
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
  if (!hasSeries(stats) || stats.bucket === null || spec.series === undefined) return null;
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

/** Whether the answer has buckets to chart: a store with no row yet has none. */
function hasSeries(stats: StoreStatistics): boolean {
  return stats.bucket !== null && stats.series.length > 0;
}

/** New, Returning, No customer recorded; null when the engine sent none, none when no order was counted. */
function buyerRows(stats: StoreStatistics): RankedRow[] | null {
  const buyers = stats.buyers;
  if (buyers === null) return null;
  if ((buyers.new ?? 0) + (buyers.returning ?? 0) + (buyers.unknown ?? 0) === 0) return [];
  return [
    { key: "new", label: "New", value: rowValue(buyers.new) },
    { key: "returning", label: "Returning", value: rowValue(buyers.returning) },
    { key: "unknown", label: "No customer recorded", value: rowValue(buyers.unknown) },
  ];
}

/**
 * Wholesale and Retail, by net sales or by orders; none when neither side has
 * an order. The block is drawn only when the answer has one (block 7).
 */
function sideRows(stats: StoreStatistics, by: "netSales" | "orders"): RankedRow[] | null {
  const sides = stats.wholesale;
  if (sides === null) return null;
  if ((sides.wholesale.orders ?? 0) + (sides.retail.orders ?? 0) === 0) return [];
  return [
    { key: "wholesale", label: "Wholesale", value: rowValue(sides.wholesale[by]) },
    { key: "retail", label: "Retail", value: rowValue(sides.retail[by]) },
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
  return phrase.replace(/ /g, "\u00a0");
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
