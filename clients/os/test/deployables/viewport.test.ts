import { describe, expect, it } from "vitest";

import {
  IDENTITY,
  MAX_SCALE,
  MIN_SCALE,
  clampScale,
  fitTo,
  panBy,
  transformOf,
  zoomAt,
} from "../../src/apps/deployables/map/viewport";

// Pan and zoom as arithmetic. The one rule worth writing down is the zoom
// invariant, and it is the one a hand-rolled wheel handler gets wrong.

describe("panning", () => {
  it("moves the view by screen pixels and leaves the scale alone", () => {
    expect(panBy({ x: 10, y: 20, scale: 2 }, 5, -5)).toEqual({ x: 15, y: 15, scale: 2 });
  });
});

describe("zooming about a point", () => {
  it("KEEPS THE POINT UNDER THE CURSOR UNDER THE CURSOR", () => {
    // The whole invariant, stated as the arithmetic it is: whatever layout
    // coordinate was under (px, py) before is under (px, py) after.
    const before = { x: 40, y: 25, scale: 1 };
    const px = 300;
    const py = 180;
    const layoutPointBefore = { x: (px - before.x) / before.scale, y: (py - before.y) / before.scale };

    const after = zoomAt(before, 1.6, px, py);
    const layoutPointAfter = { x: (px - after.x) / after.scale, y: (py - after.y) / after.scale };

    expect(layoutPointAfter.x).toBeCloseTo(layoutPointBefore.x, 6);
    expect(layoutPointAfter.y).toBeCloseTo(layoutPointBefore.y, 6);
    expect(after.scale).toBeCloseTo(1.6, 6);
  });

  it("holds the invariant when zooming OUT too", () => {
    const before = { x: -120, y: 60, scale: 1.8 };
    const [px, py] = [55, 410];
    const point = { x: (px - before.x) / before.scale, y: (py - before.y) / before.scale };
    const after = zoomAt(before, 1 / 1.6, px, py);
    expect((px - after.x) / after.scale).toBeCloseTo(point.x, 6);
    expect((py - after.y) / after.scale).toBeCloseTo(point.y, 6);
  });

  it("clamps, and stops moving once clamped", () => {
    // A wheel that keeps spinning at the limit must not accumulate a debt the
    // next scroll in the other direction has to pay back before anything moves.
    let view = { x: 0, y: 0, scale: MAX_SCALE };
    view = zoomAt(view, 4, 100, 100);
    expect(view.scale).toBe(MAX_SCALE);
    expect(view).toEqual({ x: 0, y: 0, scale: MAX_SCALE });

    let out = { x: 0, y: 0, scale: MIN_SCALE };
    out = zoomAt(out, 0.1, 100, 100);
    expect(out.scale).toBe(MIN_SCALE);
  });

  it("refuses a scale that is not a finite number, and clamps one that is", () => {
    // NaN and Infinity both fall back to 1 rather than to a bound: a value that
    // cannot be ranked is not a very large zoom, it is no answer at all, and
    // clamping it would turn a bug upstream into a map stuck at its limit.
    expect(clampScale(Number.NaN)).toBe(1);
    expect(clampScale(Number.POSITIVE_INFINITY)).toBe(1);
    expect(clampScale(1e6)).toBe(MAX_SCALE);
    expect(clampScale(1e-6)).toBe(MIN_SCALE);
  });
});

describe("fitting a layout to a viewport", () => {
  it("centres it", () => {
    const view = fitTo(400, 200, 800, 600);
    expect(view.scale).toBe(1);
    expect(view.x).toBe(200);
    expect(view.y).toBe(200);
  });

  it("shrinks to fit the tighter axis", () => {
    const view = fitTo(1600, 200, 800, 600);
    expect(view.scale).toBeCloseTo(0.5, 6);
  });

  it("never magnifies -- a two-site cluster blown up reads as a diagram of nothing", () => {
    expect(fitTo(100, 100, 900, 900).scale).toBe(1);
  });

  it("answers the identity for an unmeasured viewport rather than dividing by zero", () => {
    // jsdom measures everything as zero, and so does a window mid-open.
    expect(fitTo(400, 200, 0, 0)).toEqual(IDENTITY);
    expect(fitTo(0, 0, 800, 600)).toEqual(IDENTITY);
  });
});

describe("the transform string", () => {
  it("translates and then scales, in that order", () => {
    expect(transformOf({ x: 10, y: -4, scale: 1.5 })).toBe("translate(10 -4) scale(1.5)");
  });

  it("rounds, so a transform is readable and stable", () => {
    expect(transformOf({ x: 1 / 3, y: 0, scale: 2 / 3 })).toBe("translate(0.333 0) scale(0.667)");
  });
});
