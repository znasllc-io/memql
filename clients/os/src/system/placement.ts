// Window placement is computed, never freehand (spec D2). Given the desk's
// visible windows and the viewport, return a rect per window: solo windows
// center at a generous clamp, two windows split the desk, fullscreen covers
// the desk area above the dock. The chrome animates BETWEEN these rects
// (FLIP); tests assert the rects.

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
  /** Gap between two side-by-side windows. */
  gutter: number;
  /** Height reserved for the dock (and pager) at the bottom. */
  dockReserve: number;
  /** A solo window never grows wider than this. */
  maxSoloWidth: number;
}

export const DEFAULT_PLACEMENT: PlacementTokens = {
  margin: 28,
  gutter: 16,
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

/**
 * Rects for a desk's windows. Minimized windows get no rect. A fullscreen
 * window covers the desk area and sits above its sibling (z handled by the
 * chrome; the sibling keeps its split rect so leaving fullscreen restores).
 */
export function placeWindows(
  windows: OsWindow[],
  viewport: Viewport,
  t: PlacementTokens = DEFAULT_PLACEMENT,
): Record<WindowId, Rect> {
  const area = deskArea(viewport, t);
  const visible = windows.filter((w) => w.mode !== "minimized");
  const rects: Record<WindowId, Rect> = {};

  const split = visible.filter((w) => w.mode !== "fullscreen");
  if (split.length === 1) {
    const solo = split[0];
    const w = Math.min(area.w, t.maxSoloWidth);
    rects[solo.id] = { x: area.x + (area.w - w) / 2, y: area.y, w, h: area.h };
  } else if (split.length === 2) {
    const w = Math.max(0, (area.w - t.gutter) / 2);
    rects[split[0].id] = { x: area.x, y: area.y, w, h: area.h };
    rects[split[1].id] = { x: area.x + w + t.gutter, y: area.y, w, h: area.h };
  }

  for (const win of visible) {
    if (win.mode === "fullscreen") rects[win.id] = area;
  }
  return rects;
}
