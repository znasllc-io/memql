import type { QueryClient, Row } from "@znasllc-io/memql-sdk-core/client";

import { SITE_PERIODS, isPeriod, type Bucket, type Period } from "../../../kit/statistics/period";
import { boundStoreId, previewStoreId, type SiteRow } from "../rows";

// A store's statistics, as this app reads them (the 2026-11-11 store
// statistics design): the `storeStatistics` read for the Store statistics
// page of a storefront deployable, one store of one of its destinations.
//
// PURE, and separate from every component, for `statistics/read.ts`' reason:
// what an answer MEANS is asserted on functions.
//
// A JSON NULL STAYS NULL. The engine writes everything it does not know
// present and null (component/storestats, `Statistics.Payload()`): a store
// the walk holds no row for yet, a figure nothing measured, a zone that is
// not recorded. A money figure arrives as a decimal string and is read with
// `Number()`; a null is never read as 0, because a zero is a measurement and
// null is the absence of one.

/** Which of the site's destinations names the store; "both" when the two name one. */
export type StoreDestination = "production" | "testing" | "both";

/** A period's eight figures. Money is a number in the store's currency; null is "not measured". */
export interface StoreFigures {
  totalSales: number | null;
  netSales: number | null;
  averageOrderValue: number | null;
  grossSales: number | null;
  discounts: number | null;
  refunds: number | null;
  orders: number | null;
  unitsSold: number | null;
}

/** One bucket of the series, starting `at` (RFC3339 with the store's offset). */
export interface StorePoint {
  at: string;
  totalSales: number | null;
  netSales: number | null;
  grossSales: number | null;
  discounts: number | null;
  refunds: number | null;
  orders: number | null;
  unitsSold: number | null;
}

/** One reason of the store's recording verdict, in the engine's own sentence. */
export interface StoreReason {
  code: string;
  sentence: string;
}

/** The store's recording verdict, as the walk kept it. */
export interface StoreRecording {
  /** recording, starting, paused, needs attention, not connected, or "" when the walk could not read it. */
  state: string;
  reasons: StoreReason[];
  since: string | null;
  /** True while the figures are as of `asOf`, or none can be given. */
  blocked: boolean;
}

/** One row of a product list: an amount for the list by sales, a count for the list by units. */
export interface StoreProduct {
  key: string;
  title: string;
  value: number | null;
}

export interface StoreBuyers {
  new: number | null;
  returning: number | null;
  unknown: number | null;
}

export interface StoreSide {
  orders: number | null;
  netSales: number | null;
}

/** One answer of `storeStatistics`, key for key. */
export interface StoreStatistics {
  storeId: string;
  siteId: string;
  storeName: string | null;
  storeDomain: string | null;
  /** Null when the answer names none of the three. */
  destination: StoreDestination | null;
  currency: string | null;
  /**
   * The store's zone. Null when the store has none OR one the engine could not
   * load: the answer does not tell the two apart, and in both the period was
   * cut in UTC.
   */
  timezone: string | null;
  development: boolean | null;
  taxesIncluded: boolean | null;
  olderOrderChangesNotRecorded: boolean | null;
  period: Period;
  bucket: Bucket | null;
  /** Null, with `end` and `bucket`, for a store the walk holds no row for yet. */
  start: string | null;
  end: string | null;
  prevStart: string | null;
  historySince: string | null;
  lastChangeAt: string | null;
  /** Set only while something stands in the way: the walk's last clear run. */
  asOf: string | null;
  recording: StoreRecording;
  rebuilding: boolean | null;
  figures: StoreFigures;
  previous: StoreFigures | null;
  /** The figures that are a floor ("or more"), by key. */
  floors: Readonly<Record<string, boolean>>;
  series: StorePoint[];
  productsBySales: StoreProduct[];
  productsByUnits: StoreProduct[];
  productCount: number | null;
  refundsWithoutItem: number | null;
  incompleteOrders: number | null;
  incompleteRefunds: number | null;
  refundsNotRead: number | null;
  buyers: StoreBuyers | null;
  wholesale: { wholesale: StoreSide; retail: StoreSide } | null;
  otherCurrencies: { currency: string; orders: number | null }[];
}

/** A number off the wire: a finite number, or a decimal string; anything else, null included, is null. */
function figure(raw: unknown): number | null {
  if (typeof raw === "number") return Number.isFinite(raw) ? raw : null;
  if (typeof raw === "string" && raw.trim() !== "") {
    const parsed = Number(raw);
    return Number.isFinite(parsed) ? parsed : null;
  }
  return null;
}

function text(raw: unknown): string {
  return typeof raw === "string" ? raw : "";
}

function textOrNull(raw: unknown): string | null {
  return typeof raw === "string" && raw.trim() !== "" ? raw : null;
}

function flag(raw: unknown): boolean | null {
  return typeof raw === "boolean" ? raw : null;
}

function object(raw: unknown): Record<string, unknown> | null {
  return raw !== null && typeof raw === "object" && !Array.isArray(raw) ? (raw as Record<string, unknown>) : null;
}

function list(raw: unknown): unknown[] {
  return Array.isArray(raw) ? raw : [];
}

function figuresFrom(raw: Record<string, unknown>): StoreFigures {
  return {
    totalSales: figure(raw["totalSales"]),
    netSales: figure(raw["netSales"]),
    averageOrderValue: figure(raw["averageOrderValue"]),
    grossSales: figure(raw["grossSales"]),
    discounts: figure(raw["discounts"]),
    refunds: figure(raw["refunds"]),
    orders: figure(raw["orders"]),
    unitsSold: figure(raw["unitsSold"]),
  };
}

function pointsFrom(raw: unknown): StorePoint[] {
  return list(raw).flatMap((entry) => {
    const point = object(entry);
    if (point === null || text(point["at"]) === "") return [];
    return [{
      at: text(point["at"]),
      totalSales: figure(point["totalSales"]),
      netSales: figure(point["netSales"]),
      grossSales: figure(point["grossSales"]),
      discounts: figure(point["discounts"]),
      refunds: figure(point["refunds"]),
      orders: figure(point["orders"]),
      unitsSold: figure(point["unitsSold"]),
    }];
  });
}

function productsFrom(raw: unknown): StoreProduct[] {
  return list(raw).flatMap((entry) => {
    const product = object(entry);
    if (product === null) return [];
    return [{ key: text(product["key"]), title: text(product["title"]), value: figure(product["value"]) }];
  });
}

function sideFrom(raw: unknown): StoreSide {
  const side = object(raw);
  return { orders: figure(side?.["orders"]), netSales: figure(side?.["netSales"]) };
}

function destinationFrom(raw: unknown): StoreDestination | null {
  return raw === "production" || raw === "testing" || raw === "both" ? raw : null;
}

/**
 * Parse the read's one row: the payload `Statistics.Payload()` writes,
 * flattened onto the row. `requested` labels an answer that names no period
 * the page offers.
 */
export function parseStoreStatistics(row: Row, requested: Period): StoreStatistics {
  const recording = object(row["recording"]);
  const previous = object(row["previous"]);
  const floors = object(row["floors"]) ?? {};
  const buyers = object(row["buyers"]);
  const wholesale = object(row["wholesale"]);
  const bucket = row["bucket"];
  return {
    storeId: text(row["storeId"]),
    siteId: text(row["siteId"]),
    storeName: textOrNull(row["storeName"]),
    storeDomain: textOrNull(row["storeDomain"]),
    destination: destinationFrom(row["destination"]),
    currency: textOrNull(row["currency"]),
    timezone: textOrNull(row["timezone"]),
    development: flag(row["development"]),
    taxesIncluded: flag(row["taxesIncluded"]),
    olderOrderChangesNotRecorded: flag(row["olderOrderChangesNotRecorded"]),
    period: isPeriod(row["period"], SITE_PERIODS) ? row["period"] : requested,
    bucket: bucket === "hour" || bucket === "day" || bucket === "month" ? bucket : null,
    start: textOrNull(row["start"]),
    end: textOrNull(row["end"]),
    prevStart: textOrNull(row["prevStart"]),
    historySince: textOrNull(row["historySince"]),
    lastChangeAt: textOrNull(row["lastChangeAt"]),
    asOf: textOrNull(row["asOf"]),
    recording: {
      state: text(recording?.["state"]),
      reasons: list(recording?.["reasons"]).flatMap((entry) => {
        const reason = object(entry);
        const sentence = text(reason?.["sentence"]);
        return reason === null || sentence === "" ? [] : [{ code: text(reason["code"]), sentence }];
      }),
      since: textOrNull(recording?.["since"]),
      blocked: recording?.["blocked"] === true,
    },
    rebuilding: flag(row["rebuilding"]),
    figures: figuresFrom(object(row["figures"]) ?? {}),
    previous: previous === null ? null : figuresFrom(previous),
    floors: Object.fromEntries(Object.entries(floors).filter(([, held]) => held === true)) as Record<string, boolean>,
    series: pointsFrom(row["series"]),
    productsBySales: productsFrom(row["productsBySales"]),
    productsByUnits: productsFrom(row["productsByUnits"]),
    productCount: figure(row["productCount"]),
    refundsWithoutItem: figure(row["refundsWithoutItem"]),
    incompleteOrders: figure(row["incompleteOrders"]),
    incompleteRefunds: figure(row["incompleteRefunds"]),
    refundsNotRead: figure(row["refundsNotRead"]),
    buyers: buyers === null ? null : { new: figure(buyers["new"]), returning: figure(buyers["returning"]), unknown: figure(buyers["unknown"]) },
    wholesale: wholesale === null ? null : { wholesale: sideFrom(wholesale["wholesale"]), retail: sideFrom(wholesale["retail"]) },
    otherCurrencies: list(row["otherCurrencies"]).flatMap((entry) => {
      const other = object(entry);
      const currency = text(other?.["currency"]);
      return currency === "" ? [] : [{ currency, orders: figure(other?.["orders"]) }];
    }),
  };
}

/**
 * Read one store's statistics for a period, through the generated builder,
 * so the call string is the one the engine parses. Null when the read
 * answered no row. A refusal is thrown with the engine's own sentence.
 */
export async function fetchStoreStatistics(
  query: QueryClient,
  args: { siteId: string; storeId: string; period: Period },
  signal?: AbortSignal,
): Promise<StoreStatistics | null> {
  const result = await query.storeStatistics({ siteId: args.siteId, storeId: args.storeId, period: args.period }, { signal });
  const row = result.rows()[0];
  return row === undefined ? null : parseStoreStatistics(row, args.period);
}

/**
 * The store each destination names, as the site holds it, "" when unbound.
 * The ids are passed to the read untouched: the engine compares both forms of
 * an id, so the client parses none.
 */
export function destinationsOf(site: SiteRow): { production: string; testing: string } {
  return { production: boundStoreId(site), testing: previewStoreId(site) };
}
