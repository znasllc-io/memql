import { useLayoutEffect, useState, type RefObject } from "react";

/**
 * Whether an element is at least `min` CSS pixels wide, kept current.
 *
 * FOR A LAYOUT THAT MOVES CONTENT, NOT ONE THAT RESTYLES IT. A container query
 * is the right tool when the same DOM only needs different rules. It cannot put
 * a step's body in a second pane: that is a different PLACE in the tree, and
 * drawing it in both and hiding one would mount every field, every read and
 * every `id` twice.
 *
 * MEASURED BEFORE THE FIRST PAINT (`useLayoutEffect`), so a wide window never
 * flashes the narrow arrangement. Where nothing can be measured -- a test's
 * DOM, a pane that is not laid out yet -- the answer is `false`, which is the
 * arrangement that needs no room.
 */
export function useWide(ref: RefObject<HTMLElement | null>, min: number): boolean {
  const [wide, setWide] = useState(false);
  useLayoutEffect(() => {
    const el = ref.current;
    if (el === null) return;
    const read = () => setWide(el.getBoundingClientRect().width >= min);
    read();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(read);
    observer.observe(el);
    return () => observer.disconnect();
  }, [ref, min]);
  return wide;
}

/**
 * An element's width in CSS pixels, kept current: `useWide`'s measurement for
 * a layout that needs the number rather than a threshold (how many time labels
 * a chart's axis has room for).
 *
 * NULL UNTIL SOMETHING HAS BEEN LAID OUT. A width of zero is a test's DOM or a
 * pane nobody can see yet, not a measurement, so it reads as null too and the
 * caller falls back to the size it would draw at.
 */
export function useWidth(ref: RefObject<HTMLElement | null>): number | null {
  const [width, setWidth] = useState<number | null>(null);
  useLayoutEffect(() => {
    const el = ref.current;
    if (el === null) return;
    const read = () => {
      const measured = el.getBoundingClientRect().width;
      setWidth(measured > 0 ? measured : null);
    };
    read();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(read);
    observer.observe(el);
    return () => observer.disconnect();
  }, [ref]);
  return width;
}
