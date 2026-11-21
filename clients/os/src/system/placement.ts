// Window placement is computed, never freehand (spec D2). Given the desk's
// visible windows and the viewport, return a rect per window: a solo window
// centers at a generous clamp, or takes its half when it rests on one (a side
// drop, Jesús Nov 20 2026), a solo borderless (maximized) one covers the
// area above the dock, a solo fullscreen one covers the entire viewport, and
// two windows split the borderless area edge to edge, whatever mode each was
// in (Jesús, Nov 2026). The chrome animates BETWEEN these rects (FLIP); tests
// assert the rects.

import type { OsWindow } from "./windows";
import type { WindowId } from "./windows";

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface Viewport {
  w: number;
  h: number;
}

export interface PlacementTokens {
  /** Outer margin around the desk area. */
  margin: number;
  /** Height reserved for the dock (and pager) at the bottom. */
  dockReserve: number;
  /** A solo window never grows wider than this. */
  maxSoloWidth: number;
}

export const DEFAULT_PLACEMENT: PlacementTokens = {
  margin: 28,
  dockReserve: 96,
  maxSoloWidth: 1280,
};

/** The desk area: viewport minus margins and the dock reserve. */
export function deskArea(viewport: Viewport, t: PlacementTokens = DEFAULT_PLACEMENT): Rect {
  return {
    x: t.margin,
    y: t.margin,
    w: Math.max(0, viewport.w - t.margin * 2),
    h: Math.max(0, viewport.h - t.margin * 2 - t.dockReserve),
  };
}

/** The borderless area: the whole width above the dock's band. */
export function borderlessArea(viewport: Viewport, t: PlacementTokens = DEFAULT_PLACEMENT): Rect {
  return { x: 0, y: 0, w: viewport.w, h: Math.max(0, viewport.h - t.dockReserve) };
}

/** The borderless area cut in two, edge to edge: [left, right]. */
export function halvesOf(viewport: Viewport, t: PlacementTokens = DEFAULT_PLACEMENT): [Rect, Rect] {
  const area = borderlessArea(viewport, t);
  const w = area.w / 2;
  return [{ ...area, w }, { ...area, x: w, w }];
}

/** Rects for a desk's windows. Minimized windows get no rect. */
export function placeWindows(
  windows: OsWindow[],
  viewport: Viewport,
  t: PlacementTokens = DEFAULT_PLACEMENT,
): Record<WindowId, Rect> {
  const [first, second] = windows.filter((w) => w.mode !== "minimized");
  if (!first) return {};
  if (second) {
    const [left, right] = halvesOf(viewport, t);
    return { [first.id]: left, [second.id]: right };
  }
  if (first.mode === "maximized") return { [first.id]: borderlessArea(viewport, t) };
  if (first.mode === "fullscreen") return { [first.id]: { x: 0, y: 0, w: viewport.w, h: viewport.h } };
  if (first.half !== undefined) return { [first.id]: halvesOf(viewport, t)[first.half] };
  const area = deskArea(viewport, t);
  const w = Math.min(area.w, t.maxSoloWidth);
  return { [first.id]: { x: area.x + (area.w - w) / 2, y: area.y, w, h: area.h } };
}
