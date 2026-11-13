import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { ProductsBlock } from "../../src/apps/deployables/storeStatistics/ProductsListPage";
import { parseStoreStatistics } from "../../src/apps/deployables/storeStatistics/read";
import { absentReason, StoreNotice } from "../../src/apps/deployables/storeStatistics/storeState";
import { storeStatisticsRow } from "./harness";

afterEach(cleanup);

const statistics = (over: Record<string, unknown> = {}) => parseStoreStatistics(storeStatisticsRow({
  storeId: "store-example", currency: "EUR", timezone: "Europe/Madrid", periodZone: "Europe/Madrid",
  recording: { state: "recording", reasons: [], blocked: false }, ...over,
}), "7d");

describe("statistics completeness", () => {
  it.each([1, 3])("distinguishes %i refunds too old to read from a pending refund", (count) => {
    const stats = statistics({ refundsTooOldToRead: count, refundsNotRead: 1 });
    render(<StoreNotice stats={stats} />);
    expect(screen.getByText("1 refund is still being read from Shopify.")).toBeTruthy();
    const old = count === 1
      ? "1 refund in this period is on an order too old for this store's connection to read, so these figures cannot be given for it."
      : "3 refunds in this period are on orders too old for this store's connection to read, so these figures cannot be given for it.";
    expect(screen.getByText(old)).toBeTruthy();
    for (const id of ["refunds", "netSales", "totalSales", "unitsSold"] as const) expect(absentReason(stats, id)).toContain(old);
    expect(absentReason(stats, "grossSales")).not.toContain(old);
  });

  it("explains an unusable recorded time zone with the engine's reason", () => {
    const sentence = "The store's time zone could not be read, so its figures cannot be given.";
    const stats = statistics({ timezone: "Mars/Olympus_Mons", periodZone: "UTC", recording: { state: "recording", reasons: [{ code: "statistics_timezone_unavailable", sentence }], blocked: false } });
    expect(absentReason(stats, "orders")).toBe(sentence);
    render(<StoreNotice stats={stats} />);
    expect(screen.getByText(sentence)).toBeTruthy();
  });

  it("explains an absent average when all orders use other currencies", () => {
    const stats = statistics({ figures: { orders: 3, averageOrderValue: null }, otherCurrencies: [{ currency: "USD", orders: 2 }, { currency: "CAD", orders: 1 }] });
    expect(absentReason(stats, "averageOrderValue")).toBe("No orders in this period are in the store's currency.");
    const partlyUnknown = statistics({ figures: { orders: 3, averageOrderValue: null }, otherCurrencies: [{ currency: "USD", orders: null }] });
    expect(absentReason(partlyUnknown, "averageOrderValue")).not.toContain("No orders");
  });

  it("counts the selected product list and describes fees kept from refunds", () => {
    const products = [{ key: "a", title: "Bell", value: 5 }];
    const stats = statistics({ productsBySales: products, productsByUnits: products, productCount: 19, productSalesCount: 7, productUnitsCount: 19, refundsWithoutItem: "-9.00" });
    const { rerender } = render(<ProductsBlock stats={stats} by="sales" onBy={() => {}} title="Products" limit={1} />);
    expect(screen.getByText("The 1 best-selling of 7 products.")).toBeTruthy();
    expect(screen.getByText("Fees of €9.00 kept from refunds are in Net sales and not here.")).toBeTruthy();
    rerender(<ProductsBlock stats={stats} by="units" onBy={() => {}} title="Products" limit={1} />);
    expect(screen.getByText("The 1 best-selling of 19 products.")).toBeTruthy();
    expect(screen.queryByText("The 1 best-selling of 7 products.")).toBeNull();
  });
});
