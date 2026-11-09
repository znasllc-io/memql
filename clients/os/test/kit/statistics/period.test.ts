import { describe, expect, it } from "vitest";

import { EMAIL_PERIODS, SITE_PERIODS, bucketLabel, bucketMoment, isPeriod, periodLabel } from "../../../src/kit/statistics/period";

// The periods a statistics read offers, and the words a page says for them.
// Days are UTC everywhere in these statistics (spec D5), so a bucket's label
// is read in UTC too: a reader at UTC+2 sees the same "Nov 8" the rollup
// counted, not a day shifted by their own clock.

describe("the periods a statistics read offers", () => {
  it("are five for a website and four for email, shortest first", () => {
    expect(SITE_PERIODS).toEqual(["24h", "7d", "30d", "90d", "12m"]);
    // Email has no hourly period: its facts are counted by day.
    expect(EMAIL_PERIODS).toEqual(["7d", "30d", "90d", "12m"]);
  });

  it("read the way a person says them", () => {
    expect(SITE_PERIODS.map(periodLabel)).toEqual([
      "Last 24 hours",
      "Last 7 days",
      "Last 30 days",
      "Last 90 days",
      "Last 12 months",
    ]);
  });

  it("accept only a period the page offers, so a stored choice is never trusted as it is", () => {
    expect(isPeriod("30d", SITE_PERIODS)).toBe(true);
    expect(isPeriod("24h", SITE_PERIODS)).toBe(true);
    // A website period is not necessarily an email one.
    expect(isPeriod("24h", EMAIL_PERIODS)).toBe(false);
    expect(isPeriod("1y", SITE_PERIODS)).toBe(false);
    expect(isPeriod("", SITE_PERIODS)).toBe(false);
    expect(isPeriod(30, SITE_PERIODS)).toBe(false);
    expect(isPeriod(null, SITE_PERIODS)).toBe(false);
    expect(isPeriod(undefined, SITE_PERIODS)).toBe(false);
  });
});

describe("a bucket's label", () => {
  it("is the hour, the day or the month", () => {
    expect(bucketLabel("2026-11-08T14:00:00Z", "hour")).toBe("14:00");
    expect(bucketLabel("2026-11-08T00:00:00Z", "day")).toBe("Nov 8");
    expect(bucketLabel("2026-11-01T00:00:00Z", "month")).toBe("Nov 2026");
  });

  it("starts the day at 00:00, never 24:00", () => {
    expect(bucketLabel("2026-11-08T00:00:00Z", "hour")).toBe("00:00");
  });

  it("reads the instant in UTC, whatever offset it was written with", () => {
    // 01:30 at UTC+2 is 23:30 UTC on the day before, which is the bucket the
    // rollup counted it in.
    expect(bucketLabel("2026-11-09T01:30:00+02:00", "hour")).toBe("23:30");
    expect(bucketLabel("2026-11-09T01:30:00+02:00", "day")).toBe("Nov 8");
    expect(bucketLabel("2026-01-01T00:30:00+01:00", "month")).toBe("Dec 2025");
  });

  it("names every month in one short form", () => {
    const months = Array.from({ length: 12 }, (_, month) =>
      bucketLabel(new Date(Date.UTC(2026, month, 1)).toISOString(), "month"),
    );
    expect(months).toEqual([
      "Jan 2026", "Feb 2026", "Mar 2026", "Apr 2026", "May 2026", "Jun 2026",
      "Jul 2026", "Aug 2026", "Sep 2026", "Oct 2026", "Nov 2026", "Dec 2026",
    ]);
  });

  it("leaves an instant it cannot read as it was written, rather than inventing a date", () => {
    expect(bucketLabel("not a date", "day")).toBe("not a date");
  });
});

describe("a bucket named on its own", () => {
  it("carries the day with an hour, because nothing beside it says which day", () => {
    // Twenty-four hours start and end on the same hour.
    expect(bucketMoment("2026-11-07T15:00:00Z", "hour")).toBe("Nov 7, 15:00");
    expect(bucketMoment("2026-11-08T15:00:00Z", "hour")).toBe("Nov 8, 15:00");
    expect(bucketMoment("2026-11-09T01:30:00+02:00", "hour")).toBe("Nov 8, 23:30");
  });

  it("is the axis's own label for a day and a month", () => {
    expect(bucketMoment("2026-11-08T00:00:00Z", "day")).toBe("Nov 8");
    expect(bucketMoment("2026-11-01T00:00:00Z", "month")).toBe("Nov 2026");
    expect(bucketMoment("not a date", "hour")).toBe("not a date");
  });
});
