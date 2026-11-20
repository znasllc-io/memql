// THE WHOLE DESKTOP GROWS WITH THE SCREEN (Jesús, Nov 20 2026; draft pages
// 18-19, item 16): windows, dock, icons and text, by screen width / 1440,
// never below x1 and capped at x1.5. A 13-inch laptop (1440 wide) is x1,
// Full HD x1.33, a 1440p monitor x1.5; a wider screen then only adds room.
//
// HOW: CSS `zoom` on <html> while the desktop shell is mounted, and the shell
// lays out in a viewport of screen / scale. That is what the browser's own
// zoom does (the previews were made with it), with one difference the code
// has to carry: the browser keeps reporting SCREEN pixels -- event.clientX,
// getBoundingClientRect(), elementFromPoint() -- while style lengths (left,
// top, width, translate) and offsetWidth, clientWidth, scrollTop and
// ResizeObserver are CSS pixels, which the zoom multiplies. So every place that
// carries a measured screen value into a style, a scroll position or layout
// maths divides it by `osScale()`; dnd-kit keeps working in screen pixels and
// only the drawn translate is divided (`dragTranslate`). Viewport units (vw,
// vh) are zoomed too, so the stylesheets divide them by `--os-scale`
// (test/system/scale.test.ts fails on one that does not).
//
// WHY <html> AND NOT .os-root: menus, select lists, pickers and toasts portal
// to <body>; zoom on the root of the app would leave them small. WHY NOT
// transform: scale(): it makes the root the containing block of every
// position:fixed descendant and scales the rendered bitmap, where zoom lays
// out again at the larger size and stays sharp.
//
// Verified in Chromium (standard zoom, Chrome 128 and later; Firefox 126
// and later implement the same). Safari is not verified.

/** The screen width drawn at x1. */
export const SCALE_BASE_WIDTH = 1440;
/** The largest scale; past it a bigger screen only adds room. */
export const SCALE_CAP = 1.5;

/** The desktop's scale for a screen this wide (CSS pixels at browser zoom 100%). */
export function desktopScale(screenWidth: number): number {
  return Math.min(SCALE_CAP, Math.max(1, screenWidth / SCALE_BASE_WIDTH));
}

let current = 1;

/** Screen pixels per CSS pixel right now: 1 unless the desktop is scaled. */
export function osScale(): number {
  return current;
}

/** Zoom the whole page by `scale`; 1 takes the zoom off. */
export function setOsScale(scale: number): void {
  current = scale;
  const style = document.documentElement.style;
  if (scale === 1) {
    style.removeProperty("zoom");
    style.removeProperty("--os-scale");
  } else {
    style.setProperty("zoom", String(scale));
    style.setProperty("--os-scale", String(scale));
  }
}

/** A dnd-kit transform (screen pixels) as the CSS translate that follows the pointer. */
export function dragTranslate(transform: { x: number; y: number } | null): string | undefined {
  return transform ? `translate(${transform.x / current}px, ${transform.y / current}px)` : undefined;
}
