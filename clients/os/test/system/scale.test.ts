import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, it } from "vitest";

import { cellAt, snapZoneOf } from "../../src/chrome/Desktop";
import { desktopScale, dragTranslate, osScale, SCALE_BASE_WIDTH, SCALE_CAP, setOsScale } from "../../src/system/scale";

// Whole-desktop scaling (Jesús, Nov 20 2026; draft pages 18-19). The page is
// zoomed and the browser keeps reporting SCREEN pixels (pointer, rects) while
// styles are CSS pixels; these are the conversions the desktop relies on. The
// zoom semantics themselves (rects and clientX in screen pixels, lengths and
// vw/vh multiplied) were checked in Chromium, which jsdom cannot do.

afterEach(() => setOsScale(1));

describe("the scale", () => {
  it("is screen width / 1440, never below x1, capped at x1.5", () => {
    expect(SCALE_BASE_WIDTH).toBe(1440);
    expect(SCALE_CAP).toBe(1.5);
    expect(desktopScale(1280)).toBe(1);
    expect(desktopScale(1440)).toBe(1);
    expect(desktopScale(1920)).toBeCloseTo(4 / 3, 5);
    expect(desktopScale(2160)).toBe(1.5);
    expect(desktopScale(2560)).toBe(1.5);
    expect(desktopScale(3840)).toBe(1.5);
  });

  it("zooms the page and exposes --os-scale, and takes both off at x1", () => {
    const root = document.documentElement.style;
    setOsScale(1.5);
    expect(osScale()).toBe(1.5);
    expect(root.getPropertyValue("--os-scale")).toBe("1.5");
    setOsScale(1);
    expect(osScale()).toBe(1);
    expect(root.getPropertyValue("--os-scale")).toBe("");
    expect(root.getPropertyValue("zoom")).toBe("");
  });

  it("draws a drag's screen-pixel delta as the CSS translate that stays under the pointer", () => {
    expect(dragTranslate(null)).toBeUndefined();
    setOsScale(1.5);
    expect(dragTranslate({ x: 150, y: -30 })).toBe("translate(100px, -20px)");
  });
});

describe("desktop pointer maths on a 2560-wide screen (x1.5, desk 1706.67 CSS px wide)", () => {
  const width = 2560 / 1.5;
  const desk = { getBoundingClientRect: () => new DOMRect(0, 0, 2560, 1440) } as HTMLElement;
  const drag = (x: number, y: number) => ({ activatorEvent: { clientX: x, clientY: y }, delta: { x: 0, y: 0 } }) as never;

  it("snaps at the real edges of the screen, 24 CSS px (36 screen px) deep", () => {
    setOsScale(1.5);
    expect(snapZoneOf(drag(2550, 700), desk, width)).toBe("right");
    expect(snapZoneOf(drag(2560 - 37, 700), desk, width)).toBeNull();
    expect(snapZoneOf(drag(30, 700), desk, width)).toBe("left");
    expect(snapZoneOf(drag(1280, 30), desk, width)).toBe("top");
    expect(snapZoneOf(drag(1280, 700), desk, width)).toBeNull();
  });

  it("finds the icon cell under the pointer in grid (CSS) units", () => {
    setOsScale(1.5);
    const grid = { cols: 20, rows: 10 };
    // Cell (2, 1) starts at 20 + 2 * 96 = 212, 20 + 104 = 124 CSS px: 318, 186 on screen.
    expect(cellAt(318 + 1, 186 + 1, grid)).toEqual({ col: 2, row: 1 });
    expect(cellAt(318 - 1, 186 - 1, grid)).toEqual({ col: 1, row: 0 });
  });
});

// Viewport units are zoomed with everything else, so 100vw on a x1.5 desktop
// would be 1.5 screens wide. Every one is divided by --os-scale.
describe("the stylesheets", () => {
  const src = join(dirname(fileURLToPath(import.meta.url)), "../../src");
  const css = (readdirSync(src, { recursive: true }) as string[]).filter((f) => f.endsWith(".css"));
  it.each(css)("%s divides every viewport unit by --os-scale", (file) => {
    const text = readFileSync(join(src, file), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
    const bare = text.match(/(?<![\w.-])\d*\.?\d+(vw|vh|dvh|svh|lvh|dvw|svw|lvw|vmin|vmax)\b(?! \/ var\(--os-scale, 1\))/g);
    expect(bare).toBeNull();
  });
});
