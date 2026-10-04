import { useLayoutEffect, useState } from "react";

import { ROW_HEIGHT, stackBelow, stackedRowHeight, type LogDensity } from "./LogLine";

export interface LogRowLayout {
  /** The list's root: hand it to the `.os-logs-list` element. */
  listRef: (el: HTMLElement | null) => void;
  /** Whether rows take two lines; the list carries it as `data-stacked`. */
  stacked: boolean;
  /** What the windowed list places every row by. */
  rowHeight: number;
}

/**
 * How a log list lays its rows out at its OWN width (R40, epic memql#5478):
 * one line, or, narrower than STACK_BELOW, two.
 *
 * MEASURED, NOT A CONTAINER QUERY, for kit/useWide's reason: the windowed list
 * places every row by arithmetic over one row height, so the arrangement is
 * geometry, not style. A container query could restyle a row and never tell
 * the list it had grown, and the rows would overlap. One decision, made here,
 * is read by both halves: the list's row height and the stylesheet's
 * `.os-logs-list[data-stacked]`.
 *
 * THE MEASURE FOLLOWS THE READER'S FONT. The cells are sized in characters of
 * rem-sized fonts, so a browser set to a larger font needs a wider list for
 * the same line, and a taller row for the two stacked lines: the root font
 * size is read with the width, each time the list is measured -- when it
 * mounts and whenever it resizes. A font changed under a list that keeps its
 * width is read at its next resize.
 *
 * ONLY THE ANSWER IS STATE, as in kit/useWide: one number, zero for one line
 * and otherwise the stacked rows' height, so a resize that leaves the list on
 * the same side of its measure at the same font renders nothing.
 *
 * Through a CALLBACK ref, because the list mounts only once there are rows,
 * after this hook has first run. Unmeasured -- a test's DOM, a list not yet
 * laid out -- is one line, the arrangement every row had before this existed.
 */
export function useLogRowLayout(density: LogDensity): LogRowLayout {
  const [el, setEl] = useState<HTMLElement | null>(null);
  const [stackedHeight, setStackedHeight] = useState(0);

  useLayoutEffect(() => {
    if (el === null) return undefined;
    const read = (): void => {
      const width = el.getBoundingClientRect().width;
      const rootPx = rootFontPx();
      setStackedHeight(width > 0 && width < stackBelow(density, rootPx) ? stackedRowHeight(density, rootPx) : 0);
    };
    read();
    if (typeof ResizeObserver === "undefined") return undefined;
    const observer = new ResizeObserver(read);
    observer.observe(el);
    return () => observer.disconnect();
  }, [el, density]);

  const stacked = stackedHeight > 0;
  return { listRef: setEl, stacked, rowHeight: stacked ? stackedHeight : ROW_HEIGHT[density] };
}

/** The document's root font size in CSS pixels: the browser's own setting,
 *  since nothing in the OS sets one. 16, the browsers' default, where none
 *  can be read. */
function rootFontPx(): number {
  if (typeof document === "undefined") return 16;
  const px = parseFloat(getComputedStyle(document.documentElement).fontSize);
  return Number.isFinite(px) && px > 0 ? px : 16;
}
