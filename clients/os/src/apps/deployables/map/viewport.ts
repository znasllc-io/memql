// Pan and zoom, as arithmetic.
//
// Extracted from the component for the reason `layout.ts` is: a viewport
// asserted through a rendered SVG is asserted through React, the DOM and a
// transform string. These are four functions over four numbers, and they are
// where the one genuinely fiddly rule lives -- a wheel zoom has to keep the
// point under the cursor under the cursor, which is the difference between a
// map you can steer and one that runs away from you.

export interface Viewport {
  /** Translation in SCREEN pixels, applied before the scale. */
  x: number;
  y: number;
  scale: number;
}

export const MIN_SCALE = 0.35;
export const MAX_SCALE = 2.5;

/**
 * The step a key press takes.
 *
 * PAN is in screen pixels and deliberately does NOT divide by the scale: an
 * arrow key should move the view by the same visible distance whatever the
 * zoom, because the person is steering what they can see rather than what the
 * layout thinks it measures.
 */
export const KEY_PAN_STEP = 48;
export const KEY_ZOOM_STEP = 1.2;

export const IDENTITY: Viewport = { x: 0, y: 0, scale: 1 };

export function clampScale(scale: number): number {
  if (!Number.isFinite(scale)) return 1;
  return Math.min(MAX_SCALE, Math.max(MIN_SCALE, scale));
}

export function panBy(view: Viewport, dx: number, dy: number): Viewport {
  return { ...view, x: view.x + dx, y: view.y + dy };
}

/**
 * Zoom by `factor` about a point in the VIEWPORT's own coordinates (the
 * cursor, or the midpoint between two fingers).
 *
 * The invariant: the layout point currently under `(px, py)` is still under
 * `(px, py)` afterwards. Solve `p = (p - x) / s` for the new translation and
 * the whole thing is two lines -- which is exactly why it is written down once
 * here instead of inline in a pointer handler where it would be re-derived
 * every time somebody touched the file.
 *
 * The factor is applied to the CLAMPED result, so a wheel that keeps spinning
 * at the limit does not accumulate a debt the next scroll has to pay back.
 */
export function zoomAt(view: Viewport, factor: number, px: number, py: number): Viewport {
  const scale = clampScale(view.scale * factor);
  if (scale === view.scale) return view;
  const k = scale / view.scale;
  return {
    scale,
    x: px - (px - view.x) * k,
    y: py - (py - view.y) * k,
  };
}

/**
 * The view that fits a layout of `width` x `height` into a `vw` x `vh`
 * viewport, centred.
 *
 * Never zooms IN past 1: a two-site cluster blown up to fill a window reads as
 * a diagram of nothing, and the point of the map is the shape of the whole
 * deployment rather than the size of its boxes.
 *
 * A zero or negative dimension -- a window mid-open, a hidden desk, jsdom,
 * which measures everything as zero -- returns the identity rather than a
 * division by zero, so an unmeasured map is simply unscaled.
 */
export function fitTo(width: number, height: number, vw: number, vh: number): Viewport {
  if (width <= 0 || height <= 0 || vw <= 0 || vh <= 0) return IDENTITY;
  const scale = clampScale(Math.min(1, Math.min(vw / width, vh / height)));
  return {
    scale,
    x: (vw - width * scale) / 2,
    y: (vh - height * scale) / 2,
  };
}

/** The SVG transform a viewport becomes. Order matters: translate, then scale. */
export function transformOf(view: Viewport): string {
  return `translate(${round(view.x)} ${round(view.y)}) scale(${round(view.scale)})`;
}

/**
 * Three decimals.
 *
 * A transform string is compared by the browser and read by a test. Full
 * float precision makes both noisier for no visible difference: at this scale
 * a thousandth of a pixel is four orders of magnitude below anything anybody
 * can see.
 */
function round(n: number): number {
  return Math.round(n * 1000) / 1000;
}
