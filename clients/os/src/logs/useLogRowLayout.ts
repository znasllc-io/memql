import { useLayoutEffect, useState } from "react";

import { ROW_HEIGHT, STACKED_ROW_HEIGHT, STACK_BELOW } from "./LogLine";

/** A log list's density, the key of every row-geometry table. */
export type LogDensity = keyof typeof ROW_HEIGHT;

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
 * Through a CALLBACK ref, because the list mounts only once there are rows,
 * after this hook has first run. Unmeasured -- a test's DOM, a list not yet
 * laid out -- is one line, the arrangement every row had before this existed.
 */
export function useLogRowLayout(density: LogDensity): LogRowLayout {
  const [el, setEl] = useState<HTMLElement | null>(null);
  const [width, setWidth] = useState(0);

  useLayoutEffect(() => {
    if (el === null) return undefined;
    const read = (): void => setWidth(el.getBoundingClientRect().width);
    read();
    if (typeof ResizeObserver === "undefined") return undefined;
    const observer = new ResizeObserver(read);
    observer.observe(el);
    return () => observer.disconnect();
  }, [el]);

  const stacked = width > 0 && width < STACK_BELOW[density];
  return { listRef: setEl, stacked, rowHeight: stacked ? STACKED_ROW_HEIGHT[density] : ROW_HEIGHT[density] };
}
