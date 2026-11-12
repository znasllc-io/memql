import { useState, type ReactNode } from "react";

import { Caption, Head } from "../../../kit";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { formatCount, formatMoney } from "../../../kit/format";
import { LocalTabs } from "../../../kit/LocalTabs";
import { RankedList, periodLabel, type RankedRow } from "../../../kit/statistics";
import { useDeployablesSettings } from "../settingsContext";
import type { StoreStatistics } from "./read";
import { StoreNotice, listAnswer, productsAbsentReason, refundsWithoutItemSentence, rowFormat, rowValue } from "./storeState";

// A STORE'S PRODUCTS, WHOLE: what "All products" opens, as a page in the Store
// statistics' place under the trail Deployables > <name> > Store statistics >
// Products, never a pop-up (DESIGN.md "Statistics"). It is the answer's whole
// top list -- the engine answers the leading 25 -- so when the period sold
// more products than that its footer says so ("The 25 best-selling of 31
// products.").
//
// IT READS NOTHING OF ITS OWN, AND TELLS THE SAME TRUTH: it is drawn from the
// answer the Store statistics page holds, which that page keeps current, under
// the same notice from the same functions (`storeState.tsx`). A paused or
// frozen store's list says it is as of an earlier moment, and while the store
// is being brought in it lists nothing. As the website's whole lists do, its
// list is headed by the period, because the Head already says "Products".
//
// The block -- the tabs, the list, its footer and the line about refunds that
// name no product -- is this file's, and the Store statistics page draws its
// eight leading products with the same one.

/** Which list of products: by net sales or by units. */
export type ProductsBy = "sales" | "units";

export const PRODUCT_TABS: readonly (readonly [ProductsBy, string])[] = [["sales", "Net sales"], ["units", "Units"]];

/** How many products the Store statistics page lists before "All products". */
export const PRODUCTS_SHOWN = 8;

/**
 * One list of products as ranked rows, or null when it is absent: the engine
 * sent none, or it is money and the store has no currency to write it in.
 */
export function productRows(stats: StoreStatistics, by: ProductsBy): RankedRow[] | null {
  const products = by === "sales" ? (stats.currency === null ? null : stats.productsBySales) : stats.productsByUnits;
  if (products === null) return null;
  return products.map((product, index) => ({ key: product.key !== "" ? product.key : `${product.title}:${index}`, label: product.title, value: rowValue(product.value) }));
}

/** How a product's value is written. */
function productFormat(stats: StoreStatistics, by: ProductsBy): (value: number) => string {
  return rowFormat(by === "sales" ? formatMoney(stats.currency ?? "") : formatCount);
}

/**
 * The quiet lines under a list of products, each only when it applies: how
 * many products there are beside the `shown` listed, the refunds that name no
 * product, and the orders that have more lines than were recorded. A refund
 * with more lines than were recorded has no line here: the engine then sends
 * neither list, so there are no rows for a footer to stand under, and the
 * absent list says it (productsAbsentReason).
 */
function productsFooter(stats: StoreStatistics, by: ProductsBy, shown: number): ReactNode {
  const lines: string[] = [];
  const count = by === "sales" ? stats.productSalesCount : stats.productUnitsCount;
  if (count !== null && count > shown) lines.push(`The ${formatCount(shown)} best-selling of ${formatCount(count)} products.`);
  const refunds = refundsWithoutItemSentence(stats);
  if (refunds !== null) lines.push(refunds);
  const orders = stats.incompleteOrders ?? 0;
  if (orders > 0) lines.push(orders === 1 ? "1 order has more lines than were recorded, so these are floors." : `${formatCount(orders)} orders have more lines than were recorded, so these are floors.`);
  if (lines.length === 0) return undefined;
  return lines.map((line) => <p key={line} className="store-statistics-footer-line">{line}</p>);
}

/**
 * The tabs, the list and its footer. The kit draws no footer under a list
 * with no rows, so the line about refunds that name no product is drawn
 * beneath it then: those refunds are in Net sales whether or not a product
 * sold.
 */
export function ProductsBlock({ stats, by, onBy, title, limit, onShowAll }: {
  stats: StoreStatistics | null;
  by: ProductsBy;
  onBy: (by: ProductsBy) => void;
  title: string;
  limit: number;
  onShowAll?: () => void;
}) {
  const kind = by === "sales" ? "netSales" : "unitsSold";
  const answer = stats === null ? null : listAnswer(stats, productRows(stats, by), kind, productsAbsentReason(stats, kind));
  const rows = answer?.kind === "rows" ? answer.rows : [];
  const refunds = stats !== null && answer?.kind === "empty" ? refundsWithoutItemSentence(stats) : null;
  return (
    <div className="store-statistics-products">
      <RankedList
        title={title}
        rows={rows}
        limit={limit}
        format={stats === null ? formatCount : productFormat(stats, by)}
        onShowAll={onShowAll}
        showAllLabel="All products"
        footer={stats === null ? undefined : productsFooter(stats, by, Math.min(limit, rows.length))}
        control={
          // The tabs switch one list in place, so the shared tabs' return to
          // the top of the page does not apply.
          <div className="deployable-statistics-device-tabs" data-os-scroll-ignore>
            <LocalTabs label="Products by" value={by} onChange={onBy} options={PRODUCT_TABS} />
          </div>
        }
        loading={stats === null}
        empty={answer?.kind === "empty" ? answer.empty : undefined}
      />
      {refunds !== null ? <Caption>{refunds}</Caption> : null}
    </div>
  );
}

export function ProductsListPage({ statistics, by, trail, back }: {
  /** The answer the Store statistics page holds; null while its read is out. */
  statistics: StoreStatistics | null;
  /** The list that was showing when "All products" was pressed. */
  by: ProductsBy;
  /** The trail down to Store statistics: Products is added after it. */
  trail: readonly Breadcrumb[];
  /** Back to Store statistics. */
  back: { label: string; onSelect: () => void };
}) {
  const { settings } = useDeployablesSettings();
  const [shown, setShown] = useState<ProductsBy>(by);
  return (
    <>
      <Head navigationKey="storeStatistics:products" title="Products" breadcrumbs={[...trail, { label: "Products" }]} back={back} />
      {statistics !== null ? <StoreNotice stats={statistics} /> : null}
      <ProductsBlock stats={statistics} by={shown} onBy={setShown} title={periodLabel(statistics?.period ?? settings.statisticsPeriod)} limit={Number.POSITIVE_INFINITY} />
    </>
  );
}
