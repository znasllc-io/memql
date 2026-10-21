import { useLayoutEffect, type RefObject } from "react";

/** The share of the Ask surface the message text may take before it scrolls. */
export const FIELD_MAX_SHARE = 0.4;

/**
 * The message field grows with its text and the controls under it stay put.
 *
 * The text area is measured, not left to CSS: a field-sizing rule would grow
 * it without the ceiling, and the ceiling is a share of the SURFACE (the sheet
 * can be maximized, the widget is small), which only a measurement knows. The
 * floor -- two rows -- is the field's own CSS min-height, so a layout that
 * reports no size (a hidden sheet, jsdom) leaves the floor standing rather
 * than collapsing the box.
 *
 * Growing UPWARD needs nothing here: the field sits at the bottom of a column
 * whose conversation log takes the remaining height, so a taller field takes
 * its rows from the log above it.
 */
export function useGrowingField(field: RefObject<HTMLTextAreaElement | null>, surface: RefObject<HTMLElement | null>, value: string) {
  useLayoutEffect(() => {
    const element = field.current;
    if (!element) return;
    const size = () => {
      const ceiling = (surface.current?.clientHeight ?? 0) * FIELD_MAX_SHARE;
      element.style.height = "auto";
      const wanted = element.scrollHeight;
      if (wanted <= 0 || ceiling <= 0) { element.style.removeProperty("height"); return; }
      element.style.height = `${Math.min(wanted, ceiling)}px`;
      element.style.overflowY = wanted > ceiling ? "auto" : "hidden";
    };
    size();
    const host = surface.current;
    if (!host || typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(size);
    observer.observe(host);
    return () => observer.disconnect();
  }, [field, surface, value]);
}
