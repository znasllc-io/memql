import { afterEach, describe, expect, it, vi } from "vitest";

import { formatCount, formatMoney, formatMoneyAxis, formatRate } from "../../src/kit/format";

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

describe("a zero amount", () => {
  it("is never written with a sign", () => {
    expect(formatMoney("EUR")(-0)).toBe("\u20ac0.00");
    expect(formatMoney("EUR")(-0.004)).toBe("\u20ac0.00");
    expect(formatMoney("JPY")(-0.4)).toBe("\u00a50");
    expect(formatMoney("KWD")(-0.0004)).toBe("KWD\u00a00.000");
    expect(formatMoneyAxis("KWD")(-0.0004)).toBe("KWD\u00a00.000");
    expect(formatMoney("KWD")(-0.0005)).toBe("-KWD\u00a00.001");
    expect(formatMoneyAxis("EUR")(-0)).toBe("\u20ac0");
    expect(formatMoneyAxis("EUR")(-0.004)).toBe("\u20ac0.00");
    // A real negative keeps its sign.
    expect(formatMoney("EUR")(-0.005)).toBe("-\u20ac0.01");
    expect(formatMoney("EUR")(-36)).toBe("-\u20ac36.00");
  });
});

// The value axis of a money chart: round steps without the decimals a figure
// carries, so the gutter stays quiet. Anything with a fraction is written as
// the figure would write it.
describe("money on a value axis", () => {
  afterEach(() => vi.restoreAllMocks());

  it("writes a whole step without decimals", () => {
    expect(formatMoneyAxis("EUR")(1000)).toBe("\u20ac1,000");
    expect(formatMoneyAxis("EUR")(-200)).toBe("-\u20ac200");
    expect(formatMoneyAxis("JPY")(1200)).toBe("\u00a51,200");
    expect(formatMoneyAxis("KWD")(1)).toBe("KWD\u00a01");
  });

  it("writes a step with a fraction as the figure writes it", () => {
    expect(formatMoneyAxis("EUR")(0.5)).toBe(formatMoney("EUR")(0.5));
    expect(formatMoneyAxis("KWD")(2.5)).toBe("KWD\u00a02.500");
  });

  it("writes a value that is not finite as two dashes", () => expect(formatMoneyAxis("EUR")(Number.NaN)).toBe("--"));

  it("falls back as formatMoney does for a code Intl refuses, and never throws", () => {
    expect(formatMoneyAxis("EURO")(1000)).toBe(formatMoney("EURO")(1000));
    expect(formatMoneyAxis("EURO")(1204.5)).toBe("1,204.5 EURO");
    expect(() => formatMoneyAxis("")(1)).not.toThrow();
  });

  it("builds its formatters once per currency, not once per value", () => {
    const built = vi.spyOn(Intl, "NumberFormat");
    const euros = formatMoneyAxis("EUR");
    const before = built.mock.calls.length;
    euros(1000);
    euros(0.5);
    euros(-200);
    euros(-0);
    expect(built.mock.calls.length).toBe(before);
  });
});
