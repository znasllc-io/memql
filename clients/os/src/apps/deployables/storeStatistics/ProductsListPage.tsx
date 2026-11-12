import { useState, type ReactNode } from "react";

import { Head } from "../../../kit";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { formatCount, formatMoney } from "../../../kit/format";
import { LocalTabs } from "../../../kit/LocalTabs";
import { RankedList, periodLabel, type RankedRow } from "../../../kit/statistics";
import { useDeployablesSettings } from "../settingsContext";
import type { StoreStatistics } from "./read";

// A STORE'S PRODUCTS, WHOLE: what "All products" opens, as a page in the Store
// statistics' place under the trail Deployables > <name> > Store statistics >
// Products, never a pop-up (DESIGN.md "Statistics"). It is the answer's whole
// top list -- the engine answers the leading 25 -- so when the period sold
// more products than that its footer says so ("The 25 best-selling of 31
// products.").
//
// IT READS NOTHING OF ITS OWN: it is drawn from the answer the Store
// statistics page holds, which that page keeps current, so the list and the
// figures one step back never disagree. As the website's whole lists do, its
// list is headed by the period, because the Head already says "Products".
//
// The rows, their footer and the tabs are this file's, and the Store
// statistics page draws its eight leading products with the same ones.

/** Which list of products: by net sales or by units. */
export type ProductsBy = "sales" | "units";

export const PRODUCT_TABS: readonly (readonly [ProductsBy, string])[] = [["sales", "Net sales"], ["units", "Units"]];

/** How many products the Store statistics page lists before "All products". */
export const PRODUCTS_SHOWN = 8;

/** One list of products as ranked rows: an amount by sales, a count by units. */
export function productRows(stats: StoreStatistics, by: ProductsBy): RankedRow[] {
  const products = by === "sales" ? stats.productsBySales : stats.productsByUnits;
  // A value the answer did not give is written as the formatters write a
  // value that is not a number ("--"), never as a zero.
  return products.map((product, index) => ({ key: product.key !== "" ? product.key : `${product.title}:${index}`, label: product.title, value: product.value ?? Number.NaN }));
}

/** How a product's value is written. */
export function productFormat(stats: StoreStatistics, by: ProductsBy): (value: number) => string {
  return by === "sales" ? formatMoney(stats.currency ?? "") : formatCount;
}

/**
 * The quiet lines under a list of products, each only when it applies: how
 * many products there are beside the `shown` listed, the refunds that name no
 * product, and the orders and refunds that have more lines than were recorded.
 */
export function productsFooter(stats: StoreStatistics, shown: number): ReactNode {
  const money = formatMoney(stats.currency ?? "");
  const lines: string[] = [];
  if (stats.productCount !== null && stats.productCount > shown) lines.push(`The ${formatCount(shown)} best-selling of ${formatCount(stats.productCount)} products.`);
  // An empty sum can arrive as "0" and a computed one as "0.00": both are zero.
  if (stats.refundsWithoutItem !== null && stats.refundsWithoutItem !== 0) lines.push(`Refunds of ${money(stats.refundsWithoutItem)} name no product, so they are in Net sales and not here.`);
  const orders = stats.incompleteOrders ?? 0;
  if (orders > 0) lines.push(orders === 1 ? "1 order has more lines than were recorded, so these are floors." : `${formatCount(orders)} orders have more lines than were recorded, so these are floors.`);
  const refunds = stats.incompleteRefunds ?? 0;
  if (refunds > 0) lines.push(refunds === 1 ? "1 refund has more lines than were recorded, so units are not exact." : `${formatCount(refunds)} refunds have more lines than were recorded, so units are not exact.`);
  if (lines.length === 0) return undefined;
  return lines.map((line) => <p key={line} className="store-statistics-footer-line">{line}</p>);
}

/**
 * The tabs that choose which list of products, under the list's heading. They
 * switch one list in place, so the shared tabs' return to the top of the page
 * does not apply.
 */
export function ProductTabs({ value, onChange }: { value: ProductsBy; onChange: (by: ProductsBy) => void }) {
  return (
    <div className="deployable-statistics-device-tabs" data-os-scroll-ignore>
      <LocalTabs label="Products by" value={value} onChange={onChange} options={PRODUCT_TABS} />
    </div>
  );
}

export function ProductsListPage({ statistics, by, trail, back, empty }: {
  /** The answer the Store statistics page holds; null while its read is out. */
  statistics: StoreStatistics | null;
  /** The list that was showing when "All products" was pressed. */
  by: ProductsBy;
  /** The trail down to Store statistics: Products is added after it. */
  trail: readonly Breadcrumb[];
  /** Back to Store statistics. */
  back: { label: string; onSelect: () => void };
  /** What an empty list says. */
  empty: string;
}) {
  const { settings } = useDeployablesSettings();
  const [shown, setShown] = useState<ProductsBy>(by);
  const rows = statistics === null ? [] : productRows(statistics, shown);
  return (
    <>
      <Head navigationKey="storeStatistics:products" title="Products" breadcrumbs={[...trail, { label: "Products" }]} back={back} />
      <RankedList
        title={periodLabel(statistics?.period ?? settings.statisticsPeriod)}
        rows={rows}
        limit={Number.POSITIVE_INFINITY}
        format={statistics === null ? formatCount : productFormat(statistics, shown)}
        loading={statistics === null}
        empty={empty}
        footer={statistics === null ? undefined : productsFooter(statistics, rows.length)}
        control={<ProductTabs value={shown} onChange={setShown} />}
      />
    </>
  );
}
