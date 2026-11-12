import type { ReactNode } from "react";

import { Notice } from "../../../kit";
import { formatCount, formatMoment, formatMoney } from "../../../kit/format";
import { Measure } from "../../../kit/MeasureView";
import { absent, absentSentence } from "../../../kit/measure";
import type { RankedRow } from "../../../kit/statistics";
import type { StoreFigures, StoreStatistics } from "./read";

// WHAT A STORE'S ANSWER SAYS ABOUT ITSELF, read once for every page drawn from
// it: the Store statistics page and its whole list of products. Both tell the
// same truth from the same functions -- whether the store is still being
// brought in, why a figure is absent, what the notice says, and what a list
// with no rows says -- so a page opened from the other cannot draw a stale
// list as current.
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
// A LIST WITH NO ROWS SAYS WHICH KIND OF NOTHING IT IS
// ===========================================================================
// A list the engine did not send (null), or one that cannot be written (money
// with no currency), or any list while the store is being brought in, is
// ABSENT: the kit's dash with the reason the figures of its kind carry. A list
// sent empty says "No orders in this period." only when Orders is zero, and
// "Nothing to list for this period." otherwise, because an empty list beside
// fourteen orders is not a period without orders.

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
export const NOTHING_TO_LIST = "Nothing to list for this period.";

/** "1 refund is still being read from Shopify." / "N refunds are ..." */
export function refundsUnreadSentence(count: number): string {
  return count === 1 ? "1 refund is still being read from Shopify." : `${formatCount(count)} refunds are still being read from Shopify.`;
}

/** These refunds cannot be completed under the store's recorded order grant. */
export function refundsTooOldSentence(count: number): string {
  const subject = count === 1 ? "1 refund in this period is on an order" : `${formatCount(count)} refunds in this period are on orders`;
  return `${subject} too old for this store's connection to read, so these figures cannot be given for it.`;
}

function refundReasons(stats: StoreStatistics): string[] {
  const lines: string[] = [];
  if ((stats.refundsNotRead ?? 0) > 0) lines.push(refundsUnreadSentence(stats.refundsNotRead!));
  if ((stats.refundsTooOldToRead ?? 0) > 0) lines.push(refundsTooOldSentence(stats.refundsTooOldToRead!));
  return lines;
}

export type FigureId = keyof StoreFigures;

/** The figures that are money: with no currency they cannot be written. */
const MONEY: ReadonlySet<FigureId> = new Set(["totalSales", "netSales", "averageOrderValue", "grossSales", "discounts", "refunds"]);

/** The four figures a refund not yet read blanks (D22). */
const BLANKED_BY_UNREAD_REFUNDS: ReadonlySet<FigureId> = new Set(["refunds", "netSales", "totalSales", "unitsSold"]);

/** While the store's orders are being brought in (or deleted, or walked again), no figure is known. */
export function broughtIn(stats: StoreStatistics): boolean {
  const state = stats.recording.state;
  return state === "starting" || state === "purging" || state === "rebuilding" || stats.rebuilding === true;
}

/**
 * Why a figure the answer left null is absent: the first of these that
 * applies. "" when none does, and the kit then says nothing has reported it.
 */
export function absentReason(stats: StoreStatistics, id: FigureId): string {
  // A store with no row yet has recorded nothing at all; its zone is not missing, it is not read yet.
  if (stats.timezone === null && stats.start !== null) return NO_ZONE_REASON;
  if (broughtIn(stats)) return BROUGHT_IN_LINE;
  const zoneReason = stats.recording.reasons.find((reason) => reason.code === "statistics_timezone_unavailable");
  if (zoneReason !== undefined) return zoneReason.sentence;
  if (MONEY.has(id) && stats.currency === null) return NO_CURRENCY_REASON;
  if (BLANKED_BY_UNREAD_REFUNDS.has(id) && refundReasons(stats).length > 0) return refundReasons(stats).join(" ");
  if (id === "unitsSold" && (stats.incompleteRefunds ?? 0) > 0) return INCOMPLETE_REFUND_REASON;
  if (id === "averageOrderValue" && stats.figures.orders === 0) return NO_ORDERS;
  if (id === "averageOrderValue" && stats.figures.orders !== null && stats.figures.orders > 0 &&
      stats.otherCurrencies.every((other) => other.orders !== null) &&
      stats.otherCurrencies.reduce((sum, other) => sum + (other.orders ?? 0), 0) === stats.figures.orders) {
    return "No orders in this period are in the store's currency.";
  }
  if (stats.recording.blocked && stats.asOf === null) return NO_FIGURE_LINE;
  return "";
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
  lines.push(...refundReasons(stats));
  const fixable = state === "paused" || state === "not connected" || state === "needs attention";
  return fixable && !intake && lines.length > 0 ? { lines, next: STORE_PAGE_NEXT } : { lines };
}

/** The notice of block 2, or nothing when nothing stands in the way. It holds no act. */
export function StoreNotice({ stats }: { stats: StoreStatistics }) {
  const notice = noticeLines(stats);
  if (notice.lines.length === 0) return null;
  return (
    <Notice tone={notice.next !== undefined ? "warn" : "info"} sentence={notice.lines[0]} next={notice.next}>
      {notice.lines.slice(1).map((line, index) => <p key={`${index}:${line}`} className="os-notice-line">{line}</p>)}
    </Notice>
  );
}

/** What a list holds: its rows, or why it has none. */
export type ListAnswer = { kind: "rows"; rows: RankedRow[] } | { kind: "empty"; empty: ReactNode };

/**
 * Why an absent list of products is absent: the reason of the figure it is
 * ranked by, and otherwise, when a refund in the period has more lines than
 * were recorded -- the engine then sends neither list -- the sentence Units
 * sold carries for that case, word for word.
 */
export function productsAbsentReason(stats: StoreStatistics, kind: FigureId): string {
  const reason = absentReason(stats, kind);
  if (reason !== "" || (stats.incompleteRefunds ?? 0) === 0) return reason;
  return absentReason(stats, "unitsSold");
}

/**
 * A list as the page draws it. `rows` is null when the list is absent (the
 * engine sent none, or it cannot be written); `kind` names the figure whose
 * reason an absent list carries, unless `reason` is given.
 */
export function listAnswer(stats: StoreStatistics, rows: RankedRow[] | null, kind: FigureId, reason: string = absentReason(stats, kind)): ListAnswer {
  if (broughtIn(stats) || rows === null) {
    const figure = absent("unmeasured", reason);
    return { kind: "empty", empty: <><Measure figure={figure} /> {figure.kind === "absent" ? absentSentence(figure) : ""}</> };
  }
  if (rows.length > 0) return { kind: "rows", rows };
  return { kind: "empty", empty: stats.figures.orders === 0 ? NO_ORDERS : NOTHING_TO_LIST };
}

/**
 * How a row's value is written: the formatter's, or the kit's dash for a
 * value the answer left out, never "--".
 */
export function rowFormat(format: (value: number) => string): (value: number) => string {
  return (value) => (Number.isFinite(value) ? format(value) : "—");
}

/** A row value the answer left out, kept as "not a number" so it is never drawn as zero. */
export function rowValue(value: number | null): number {
  return value ?? Number.NaN;
}

/**
 * "Refunds of €12.00 name no product, so they are in Net sales and not
 * here.", when the answer carries such refunds and the store's currency is
 * known; null otherwise, and while the store is being brought in. Without a
 * currency the amount would be a bare number. An empty sum can arrive as "0"
 * and a computed one as "0.00": both are zero.
 */
export function refundsWithoutItemSentence(stats: StoreStatistics): string | null {
  if (broughtIn(stats) || stats.currency === null || stats.refundsWithoutItem === null || stats.refundsWithoutItem === 0) return null;
  if (stats.refundsWithoutItem < 0) return `Fees of ${formatMoney(stats.currency)(-stats.refundsWithoutItem)} kept from refunds are in Net sales and not here.`;
  return `Refunds of ${formatMoney(stats.currency)(stats.refundsWithoutItem)} name no product, so they are in Net sales and not here.`;
}
