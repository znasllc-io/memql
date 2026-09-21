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
