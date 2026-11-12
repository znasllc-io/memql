import { describe, expect, it } from "vitest";

import { destinationsOf, fetchStoreStatistics, parseStoreStatistics } from "../../src/apps/deployables/storeStatistics/read";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { SHOP, fakeConnection, siteRow, storeStatisticsRow } from "./harness";

// THE STORE STATISTICS READ, as this app parses it (apps/deployables/
// storeStatistics/read.ts). A null figure is "not measured" and must stay
// null; a money figure arrives as a decimal string.

describe("the store statistics read", () => {
  it("keeps a null figure null and reads a money string as its number", () => {
    const s = parseStoreStatistics(storeStatisticsRow({ storeId: "store-example", figures: { totalSales: null, netSales: "65.99", orders: 2, averageOrderValue: null, grossSales: "109.99", discounts: null, refunds: null, unitsSold: null } }), "7d");
    expect(s.figures.netSales).toBe(65.99);
    expect(s.figures.orders).toBe(2);
    expect(s.figures.totalSales).toBeNull();
    expect(s.figures.averageOrderValue).toBeNull();
  });

  it("answers null for a reply with no row", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    expect(await fetchStoreStatistics(connection.query, { siteId: "site-shop", storeId: "store-example", period: "7d" })).toBeNull();
  });

  it("reads each destination's store as the site holds it", () => {
    expect(destinationsOf(siteFromRow({ ...SHOP, previewBinding: { storeId: "store-example-dev" } }))).toEqual({ production: "store-example", testing: "store-example-dev" });
    expect(destinationsOf(siteFromRow(siteRow({ id: "site-plain", kind: "spa" })))).toEqual({ production: "", testing: "" });
  });

  it("reads a store with no row yet as its period and nothing else", () => {
    const s = parseStoreStatistics(storeStatisticsRow({
      storeId: "store-example", storeName: null, storeDomain: null, currency: null, timezone: null, development: null, taxesIncluded: null,
      olderOrderChangesNotRecorded: null, bucket: null, start: null, end: null, prevStart: null, historySince: null, lastChangeAt: null,
      recording: { state: "starting", reasons: [], since: null, blocked: false }, rebuilding: null, series: [],
    }), "30d");
    expect(s.period).toBe("7d");
    expect(s.start).toBeNull();
    expect(s.timezone).toBeNull();
    expect(s.recording.state).toBe("starting");
    expect(s.development).toBeNull();
  });
});
