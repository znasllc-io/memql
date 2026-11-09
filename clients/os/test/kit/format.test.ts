import { describe, expect, it } from "vitest";

import { formatCount, formatRate } from "../../src/kit/format";

// A count as the statistics pieces write it by default: grouped, in one
// locale, the same one the chart's axis labels use, so a figure and the axis
// beside it never disagree about what a comma means.

describe("a count", () => {
  it("is grouped by thousands", () => {
    expect(formatCount(0)).toBe("0");
    expect(formatCount(1204)).toBe("1,204");
    expect(formatCount(12_345_678)).toBe("12,345,678");
  });

  it("keeps a fraction where there is one", () => {
    expect(formatCount(2.5)).toBe("2.5");
  });

  it("is a dash for a value that is not a number, never NaN on screen", () => {
    expect(formatCount(Number.NaN)).toBe("--");
    expect(formatCount(Number.POSITIVE_INFINITY)).toBe("--");
  });
});

describe("a rate", () => {
  it("is a whole percent from 10% up and at zero, one decimal below", () => {
    expect(formatRate(0.4)).toBe("40%");
    expect(formatRate(0.043)).toBe("4.3%");
    expect(formatRate(0)).toBe("0%");
  });

  it("is a dash when absent, never 0% and never NaN on screen", () => {
    expect(formatRate(null)).toBe("--");
    expect(formatRate(Number.NaN)).toBe("--");
  });
});
