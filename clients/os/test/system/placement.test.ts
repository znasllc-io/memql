import { describe, expect, it } from "vitest";

import { deskArea, placeWindows, type PlacementTokens } from "../../src/system/placement";
import { newWindow, type OsWindow } from "../../src/system/windows";

const T: PlacementTokens = { margin: 20, gutter: 10, dockReserve: 80, maxSoloWidth: 900 };
const VIEW = { w: 1440, h: 900 };

function win(id: string, mode: OsWindow["mode"] = "normal"): OsWindow {
  return { ...newWindow(id, `app-${id}`, ""), mode };
}

describe("placement", () => {
  it("centers a solo window and clamps its width", () => {
    const rects = placeWindows([win("a")], VIEW, T);
    const area = deskArea(VIEW, T);
    const r = rects.a;
    expect(r.w).toBe(900); // clamped below area width
    expect(r.h).toBe(area.h);
    expect(r.x - area.x).toBeCloseTo(area.x + area.w - (r.x + r.w), 5);
  });

  it("splits two windows into equal halves with the gutter between", () => {
    const rects = placeWindows([win("a"), win("b")], VIEW, T);
    const area = deskArea(VIEW, T);
    expect(rects.a.w).toBeCloseTo(rects.b.w, 5);
    expect(rects.a.x).toBe(area.x);
    expect(rects.b.x).toBeCloseTo(rects.a.x + rects.a.w + T.gutter, 5);
    expect(rects.b.x + rects.b.w).toBeCloseTo(area.x + area.w, 5);
  });

  it("gives no rect to a minimized window, and its sibling goes solo", () => {
    const rects = placeWindows([win("a"), win("b", "minimized")], VIEW, T);
    expect(rects.b).toBeUndefined();
    expect(rects.a.w).toBe(900);
  });

  it("fullscreen covers the desk area while the sibling keeps its half", () => {
    const rects = placeWindows([win("a", "fullscreen"), win("b")], VIEW, T);
    const area = deskArea(VIEW, T);
    expect(rects.a).toEqual(area);
    expect(rects.b.w).toBeLessThan(area.w);
  });

  it("keeps every rect inside the desk area", () => {
    for (const wins of [[win("a")], [win("a"), win("b")]]) {
      const area = deskArea(VIEW, T);
      for (const r of Object.values(placeWindows(wins, VIEW, T))) {
        expect(r.x).toBeGreaterThanOrEqual(area.x);
        expect(r.y).toBeGreaterThanOrEqual(area.y);
        expect(r.x + r.w).toBeLessThanOrEqual(area.x + area.w + 0.001);
        expect(r.y + r.h).toBeLessThanOrEqual(area.y + area.h + 0.001);
      }
    }
  });

  it("survives a tiny viewport without negative sizes", () => {
    const rects = placeWindows([win("a"), win("b")], { w: 30, h: 30 }, T);
    for (const r of Object.values(rects)) {
      expect(r.w).toBeGreaterThanOrEqual(0);
      expect(r.h).toBeGreaterThanOrEqual(0);
    }
  });
});
