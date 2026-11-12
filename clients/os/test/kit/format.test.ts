import { afterEach, describe, expect, it, vi } from "vitest";

import { formatCount, formatMoney, formatRate } from "../../src/kit/format";

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

// Money as a store's statistics write it: in the locale `formatCount` writes
// in, with the currency's own decimals, so a yen total has none and a dinar
// total three.
describe("money", () => {
  afterEach(() => vi.restoreAllMocks());

  it("writes a currency with no decimals, two and three", () => {
    expect(formatMoney("JPY")(1200)).toBe("\u00a51,200"); // the yen sign
    expect(formatMoney("EUR")(1204.5)).toBe("\u20ac1,204.50"); // the euro sign
    expect(formatMoney("KWD")(1.255)).toBe("KWD\u00a01.255"); // Intl puts a no-break space after a code
  });

  it("writes an amount below zero with its sign", () => expect(formatMoney("EUR")(-36)).toBe("-\u20ac36.00"));

  it("writes a value that is not finite as two dashes", () => {
    expect(formatMoney("EUR")(Number.NaN)).toBe("--");
    expect(formatMoney("EUR")(Number.POSITIVE_INFINITY)).toBe("--");
    expect(formatMoney("EURO")(Number.NaN)).toBe("--");
  });

  it("writes the count and the code for a code Intl refuses", () => {
    expect(formatMoney("EURO")(1204.5)).toBe("1,204.5 EURO");
    expect(() => formatMoney("")(1)).not.toThrow();
    expect(formatMoney("")(1)).toBe("1");
  });

  it("builds its formatter once per currency, not once per value", () => {
    const built = vi.spyOn(Intl, "NumberFormat");
    const euros = formatMoney("EUR");
    euros(1);
    euros(2);
    euros(3);
    expect(built).toHaveBeenCalledTimes(1);
  });
});
