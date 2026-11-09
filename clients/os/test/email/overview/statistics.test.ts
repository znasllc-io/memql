import { describe, expect, it } from "vitest";

import { parseEmailStatistics } from "../../../src/apps/email/overview/statistics";
import { STATISTICS_TOTALS, statisticsRow, twelveMonthsStatisticsRow, unmeasuredStatisticsRow } from "../harness";

// THE EMAIL STATISTICS READ'S ANSWER, AS THE PAGE HOLDS IT (spec 7;
// component/emailstats/statistics.go, `Payload`).
//
// What these keep honest is the one rule a parse can break without anybody
// seeing it: a figure nothing measured arrives as a JSON null and must stay a
// null. A helper that reads null as 0 turns "not measured" into "nothing
// happened", which is the mistake the whole statistics design exists to refuse.

describe("the email statistics read's answer", () => {
  it("reads the period, its bounds and every figure under the server's own names", () => {
    const stats = parseEmailStatistics(statisticsRow());

    expect(stats).toMatchObject({
      period: "30d",
      bucket: "day",
      start: "2026-10-10T00:00:00Z",
      end: "2026-11-09T00:00:00Z",
      prevStart: "2026-09-10T00:00:00Z",
      measuredSince: "2026-09-01",
    });
    expect(stats.totals).toEqual(STATISTICS_TOTALS);
    expect(stats.previous).toMatchObject({ sent: 1075, subscribes: 0, openRate: 0.4 });
    expect(stats.series).toHaveLength(30);
    expect(stats.series[5]).toEqual({
      at: "2026-10-15T00:00:00Z",
      sent: 64, uniqueOpens: 16, uniqueClicks: 2, unsubscribes: 1, subscribes: 7,
      bounces: 0, complaints: 0, failed: 0, suppressed: 0, openRate: 0.4, clickRate: 0.03,
    });
    expect(stats.previousSeries).toHaveLength(30);
    expect(stats.campaigns).toEqual([
      { id: "c-winter", sent: 600, uniqueOpens: 300, uniqueClicks: 34, openRate: 0.5, clickRate: 0.057 },
      { id: "c-restock", sent: 604, uniqueOpens: 200, uniqueClicks: 19, openRate: 0.33, clickRate: 0.032 },
    ]);
    expect(stats.automations).toEqual([
      { id: "r-welcome", kind: "rule", sent: 120, failed: 2, suppressed: 1 },
      { id: "j-series", kind: "journey", sent: 40, failed: 0, suppressed: 0 },
    ]);
    expect(stats.audiences).toEqual([
      { id: "a-news", subscribes: 12, unsubscribes: 3, net: 9 },
      { id: "a-signups", subscribes: 2, unsubscribes: 5, net: -3 },
    ]);
    expect(stats.accounts).toEqual([]);
  });

  it("keeps a figure nothing measured a null, and a measured zero a zero", () => {
    const stats = parseEmailStatistics(unmeasuredStatisticsRow());

    expect(stats.measuredSince).toBeNull();
    expect(Object.values(stats.totals).every((value) => value === null)).toBe(true);
    expect(Object.values(stats.previous ?? {}).every((value) => value === null)).toBe(true);
    for (const point of stats.series) {
      const { at, ...figures } = point;
      expect(at).not.toBe("");
      expect(Object.values(figures).every((value) => value === null)).toBe(true);
    }

    // Zero is an answer: the provider reported no outcome, and that is not "not measured".
    const counted = parseEmailStatistics(statisticsRow({ totals: { delivered: 0, openRate: null } }));
    expect(counted.totals.delivered).toBe(0);
    expect(counted.totals.openRate).toBeNull();
  });

  it("has no previous period for twelve months, and buckets them by the month", () => {
    const stats = parseEmailStatistics(twelveMonthsStatisticsRow());

    expect(stats).toMatchObject({ period: "12m", bucket: "month", prevStart: null, previous: null, previousSeries: null });
    // Only the start of the previous period is kept: it is what decides a comparison.
    expect(stats).not.toHaveProperty("hasPrevious");
    expect(stats).not.toHaveProperty("prevEnd");
    expect(stats.series.map((point) => point.at.slice(0, 7))).toEqual([
      "2025-11", "2025-12", "2026-01", "2026-02", "2026-03", "2026-04", "2026-05",
      "2026-06", "2026-07", "2026-08", "2026-09", "2026-10", "2026-11",
    ]);
  });

  it("leaves out a list entry it can neither name nor rank, and keeps the rest in the read's order", () => {
    const stats = parseEmailStatistics(statisticsRow({
      campaigns: [
        { id: "c-first", sent: 10, uniqueOpens: 9, uniqueClicks: 1, openRate: 0.9, clickRate: 0.1 },
        { sent: 10, uniqueOpens: 8, uniqueClicks: 1, openRate: 0.8, clickRate: 0.1 },
        { id: "c-counted-nothing", sent: null, uniqueOpens: 7, uniqueClicks: 1, openRate: 0.7, clickRate: 0.1 },
        { id: "c-last", sent: 10, uniqueOpens: 6, uniqueClicks: 0, openRate: 0.6, clickRate: null },
      ],
      automations: [
        { id: "r-1", kind: "rule", sent: 3, failed: 0, suppressed: 0 },
        { id: "w-1", kind: "webhook", sent: 3, failed: 0, suppressed: 0 },
        { id: "j-1", kind: "journey", sent: 2, failed: 1, suppressed: 0 },
      ],
      audiences: [{ id: "a-1", subscribes: 4, unsubscribes: 1, net: 3 }, { id: "", subscribes: 1, unsubscribes: 0, net: 1 }],
      accounts: "every organization",
    }));

    expect(stats.campaigns.map((row) => row.id)).toEqual(["c-first", "c-last"]);
    // A rate the read could not state stays unstated.
    expect(stats.campaigns[1]?.clickRate).toBeNull();
    expect(stats.automations.map((row) => `${row.kind}:${row.id}`)).toEqual(["rule:r-1", "journey:j-1"]);
    expect(stats.audiences.map((row) => row.id)).toEqual(["a-1"]);
    expect(stats.accounts).toEqual([]);
  });
});
