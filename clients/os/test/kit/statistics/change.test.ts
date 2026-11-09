import { describe, expect, it } from "vitest";

import { absent, changeLabel, changeOf, figureOf } from "../../../src/kit/measure";

// A figure's change from the previous period. An absent figure is not a zero
// (kit/measure), so a change against one is not a number either: a website
// whose visitors were not counted last week did not grow from nothing.

describe("the change from the previous period", () => {
  it("is nothing when either period was not measured", () => {
    expect(changeOf(figureOf(5), absent("unmeasured"))).toEqual({ kind: "none" });
    expect(changeOf(absent("unmeasured"), figureOf(5))).toEqual({ kind: "none" });
    expect(changeOf(absent("failed"), absent("refused"))).toEqual({ kind: "none" });
    // A period with no previous one (twelve months) has nothing to compare with.
    expect(changeOf(figureOf(5), undefined)).toEqual({ kind: "none" });
  });

  it("is new when the previous period counted none and this one counted some", () => {
    expect(changeOf(figureOf(3), figureOf(0))).toEqual({ kind: "new" });
  });

  it("is the difference as a share of the previous period otherwise", () => {
    expect(changeOf(figureOf(112), figureOf(100))).toEqual({ kind: "ratio", ratio: 0.12 });
    expect(changeOf(figureOf(92), figureOf(100))).toEqual({ kind: "ratio", ratio: -0.08 });
    expect(changeOf(figureOf(0), figureOf(40))).toEqual({ kind: "ratio", ratio: -1 });
    expect(changeOf(figureOf(0), figureOf(0))).toEqual({ kind: "ratio", ratio: 0 });
  });

  it("is nothing against a floor, which is not an exact count", () => {
    // "5,000 or more" against 4,000 is at least a quarter up, and saying
    // "+25%" would state the least it could be as what it is.
    expect(changeOf(figureOf(5000, true), figureOf(4000))).toEqual({ kind: "none" });
    expect(changeOf(figureOf(4000), figureOf(5000, true))).toEqual({ kind: "none" });
  });
});

describe("a change, as a person reads it", () => {
  it("is a signed whole percentage, with a true minus sign", () => {
    expect(changeLabel({ kind: "ratio", ratio: 0.12 })).toBe("+12%");
    expect(changeLabel({ kind: "ratio", ratio: -0.08 })).toBe("−8%");
    expect(changeLabel({ kind: "ratio", ratio: 0.126 })).toBe("+13%");
    expect(changeLabel({ kind: "ratio", ratio: -1 })).toBe("−100%");
    expect(changeLabel({ kind: "ratio", ratio: 15 })).toBe("+1,500%");
  });

  it("is no change below half a percent either way, and never a signed zero", () => {
    expect(changeLabel({ kind: "ratio", ratio: 0 })).toBe("No change");
    expect(changeLabel({ kind: "ratio", ratio: 0.004 })).toBe("No change");
    expect(changeLabel({ kind: "ratio", ratio: -0.0049 })).toBe("No change");
    expect(changeLabel({ kind: "ratio", ratio: 0.005 })).toBe("+1%");
    expect(changeLabel({ kind: "ratio", ratio: -0.005 })).toBe("−1%");
  });

  it("says new, or nothing at all", () => {
    expect(changeLabel({ kind: "new" })).toBe("New");
    expect(changeLabel({ kind: "none" })).toBe("");
  });

  it("reads straight from two figures", () => {
    expect(changeLabel(changeOf(figureOf(1204), figureOf(1075)))).toBe("+12%");
    expect(changeLabel(changeOf(figureOf(1204), absent("unmeasured")))).toBe("");
  });
});
