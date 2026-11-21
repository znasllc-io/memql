import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { borderlessArea, DEFAULT_PLACEMENT, DOCK_GAP, deskArea, placeWindows, type PlacementTokens } from "../../src/system/placement";
import { newWindow, type OsWindow } from "../../src/system/windows";

const T: PlacementTokens = { margin: 20, dockReserve: 80, maxSoloWidth: 900 };
const VIEW = { w: 1440, h: 900 };

function win(id: string, mode: OsWindow["mode"] = "normal"): OsWindow {
  return { ...newWindow(id, `app-${id}`, ""), mode };
}

describe("placement", () => {
  it("centers a solo window and clamps its width", () => {
    const rects = placeWindows([win("a")], VIEW, T);
    const area = deskArea(VIEW, T);
    const r = rects.a!;
    expect(r.w).toBe(900); // clamped below area width
    expect(r.h).toBe(area.h);
    expect(r.x - area.x).toBeCloseTo(area.x + area.w - (r.x + r.w), 5);
  });

  // Jesús, Nov 2026: two apps on one desk split the borderless area, edge to
  // edge, whatever mode each was in.
  it.each([
    ["normal", "normal"],
    ["maximized", "normal"],
    ["normal", "maximized"],
  ] as const)("splits the borderless area into two edge-to-edge halves (%s + %s)", (left, right) => {
    const rects = placeWindows([win("a", left), win("b", right)], VIEW, T);
    const h = VIEW.h - T.dockReserve;
    expect(rects.a).toEqual({ x: 0, y: 0, w: VIEW.w / 2, h });
    expect(rects.b).toEqual({ x: VIEW.w / 2, y: 0, w: VIEW.w / 2, h });
  });

  it("gives no rect to a minimized window, and its sibling goes solo", () => {
    const rects = placeWindows([win("a"), win("b", "minimized")], VIEW, T);
    expect(rects.b).toBeUndefined();
    expect(rects.a!.w).toBe(900);
  });

  it("maximizes above the dock", () => {
    const rects = placeWindows([win("a", "maximized")], VIEW, T);
    expect(rects.a).toEqual({ x: 0, y: 0, w: VIEW.w, h: VIEW.h - T.dockReserve });
  });

  it("fills the entire viewport in fullscreen, including after a resize", () => {
    for (const viewport of [VIEW, { w: 390, h: 844 }]) {
      const rects = placeWindows([win("a", "fullscreen")], viewport, T);
      expect(rects.a).toEqual({ x: 0, y: 0, w: viewport.w, h: viewport.h });
    }
  });

  it("keeps a solo window inside the desk area and a pair inside the borderless area", () => {
    for (const [wins, area] of [[[win("a")], deskArea(VIEW, T)], [[win("a"), win("b")], borderlessArea(VIEW, T)]] as const) {
      for (const r of Object.values(placeWindows([...wins], VIEW, T))) {
        if (!r) continue;
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
      if (!r) continue;
      expect(r.w).toBeGreaterThanOrEqual(0);
      expect(r.h).toBeGreaterThanOrEqual(0);
    }
  });

  // Draft B (Jesús, Nov 20 2026): every window reaches down to DOCK_GAP (4px since
  // Jesús closed the 16px gap the same evening)
  // above the dock's top edge; full screen still covers the dock.
  describe("down to the dock", () => {
    const css = (file: string) => readFileSync(join(dirname(fileURLToPath(import.meta.url)), "../../src/styles", file), "utf8");
    const dockH = Number(/--os-dock-h:\s*(\d+)px/.exec(css("tokens.css"))![1]);
    const dockBottom = Number(/\.os-dock \{[^}]*?\bbottom:\s*(\d+)px/.exec(css("index.css"))![1]);

    it("reserves exactly the dock's band plus the gap", () => {
      expect(DOCK_GAP).toBe(4);
      expect(DEFAULT_PLACEMENT.dockReserve).toBe(dockBottom + dockH + DOCK_GAP);
    });

    it.each([
      ["a window", [win("a")]],
      ["a borderless window", [win("a", "maximized")]],
      ["a split pair", [win("a"), win("b")]],
    ] as const)("%s stops 4px above the dock, on every screen", (_, wins) => {
      for (const view of [VIEW, { w: 1920, h: 1080 }, { w: 2560, h: 1440 }]) {
        const dockTop = view.h - dockBottom - dockH;
        for (const r of Object.values(placeWindows([...wins], view))) {
          expect(r!.y + r!.h).toBe(dockTop - 4);
        }
      }
    });

    it("leaves full screen over the dock", () => {
      expect(placeWindows([win("a", "fullscreen")], VIEW).a).toEqual({ x: 0, y: 0, w: VIEW.w, h: VIEW.h });
    });
  });
});
