import { describe, expect, it } from "vitest";

import { CELL_H } from "../../src/chrome/Desktop";
import { gridForViewport } from "../../src/chrome/state";
import { DEFAULT_PLACEMENT } from "../../src/system/placement";

// Desk icons go down to the line windows stop at (Jesús, Nov 20 2026: "an
// invisible barrier ... it doesn't let me drag stuff beneath that line"). The
// grid's last row ends above that line, and one more row would cross it.

const TOP_PAD = 20;
const windowLine = (height: number) => height - DEFAULT_PLACEMENT.dockReserve;
const gridBottom = (rows: number) => TOP_PAD + rows * CELL_H;

describe("the desk icon grid's rows", () => {
  it("reach a seventh row on Jesús's Safari window (1385x881), where a flat 200px reserve stopped at six", () => {
    const { rows } = gridForViewport(1385, 881);
    expect(rows).toBe(7);
    expect(gridBottom(rows)).toBeLessThanOrEqual(windowLine(881));
  });

  // Laptop, 1440x900 MacBook Air, a short browser window, Full HD at x1.33
  // (1440x810 in the desk's CSS pixels), a 1440p monitor at x1.5 (1707x960),
  // and a tall portrait screen.
  it.each([
    [1280, 720],
    [1440, 900],
    [1200, 640],
    [1440, 810],
    [1707, 960],
    [1080, 1920],
  ])("never sit under the dock and never stop a row early at %ix%i", (w, h) => {
    const { rows } = gridForViewport(w, h);
    expect(gridBottom(rows)).toBeLessThanOrEqual(windowLine(h));
    expect(gridBottom(rows + 1)).toBeGreaterThan(windowLine(h));
  });

  it("keep two rows on a window too short for them, as before", () => {
    expect(gridForViewport(800, 200).rows).toBe(2);
  });
});
