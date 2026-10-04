import { useLayoutEffect, useRef, useState } from "react";

import { ROW_HEIGHT, logLayout, rowHeightAt, type LogDensity, type LogLayout } from "./LogLine";

export interface LogRowLayout {
  /** The list's root: hand it to the `.os-logs-list` element. */
  listRef: (el: HTMLElement | null) => void;
  /** How the list lays its rows out; the list carries it as `data-layout`. */
  layout: LogLayout;
  /** What the windowed list places every row by. */
  rowHeight: number;
}

interface Arrangement {
  layout: LogLayout;
  rowHeight: number;
}

/**
 * How a log list lays its rows out at its OWN width (R40, R40b, epic
 * memql#5478): WIDE, with the attributes an aligned column; MEDIUM, the
 * message first; or NARROW, each line on two rows (LogLine.tsx says where
 * each begins).
 *
 * MEASURED, NOT A CONTAINER QUERY, for kit/useWide's reason: the windowed list
 * places every row by arithmetic over one row height, so the arrangement is
 * geometry, not style. A container query could restyle a row onto two lines
 * and never tell the list it had grown, and the rows would overlap. One
 * decision, made here, is read by both halves: the list's row height and the
 * stylesheet's `.os-logs-list[data-layout]` -- one attribute on the list, and
 * nothing decided per row.
 *
 * THE MEASURES FOLLOW THE READER'S FONT. The cells are sized in characters of
 * rem-sized fonts, so a browser set to a larger font needs a wider list for
 * the same arrangement, and taller rows for its lines: the root font size is
 * read with the width, each time the list is measured -- when it mounts and
 * whenever it resizes. A font changed under a list that keeps its width is
 * read at its next resize.
 *
 * ONLY THE ANSWER IS STATE, as in kit/useWide: the layout and the row height,
 * set only when a measurement disagrees with them, so a resize that changes
 * neither renders nothing.
 *
 * Through a CALLBACK ref, because the list mounts only once there are rows,
 * after this hook has first run. Unmeasured -- a test's DOM, a list not yet
 * laid out -- is WIDE, the one line every row had before R40.
 */
export function useLogRowLayout(density: LogDensity): LogRowLayout {
  const [el, setEl] = useState<HTMLElement | null>(null);
  const [arrangement, setArrangement] = useState<Arrangement>(() => ({
    layout: "wide",
    rowHeight: ROW_HEIGHT[density],
  }));
  // What was last set, compared BEFORE setting: React skips an equal update
  // without calling the component only while nothing is pending, so handing
  // it the same answer again could still cost a render.
  const held = useRef(arrangement);

  useLayoutEffect(() => {
    if (el === null) return undefined;
    const read = (): void => {
      const rootPx = rootFontPx();
      const layout = logLayout(el.getBoundingClientRect().width, density, rootPx);
      const rowHeight = rowHeightAt(layout, density, rootPx);
      if (held.current.layout === layout && held.current.rowHeight === rowHeight) return;
      held.current = { layout, rowHeight };
      setArrangement(held.current);
    };
    read();
    if (typeof ResizeObserver === "undefined") return undefined;
    const observer = new ResizeObserver(read);
    observer.observe(el);
    return () => observer.disconnect();
  }, [el, density]);

  return { listRef: setEl, layout: arrangement.layout, rowHeight: arrangement.rowHeight };
}

/** The document's root font size in CSS pixels: the browser's own setting,
 *  since nothing in the OS sets one. 16, the browsers' default, where none
 *  can be read. */
function rootFontPx(): number {
  if (typeof document === "undefined") return 16;
  const px = parseFloat(getComputedStyle(document.documentElement).fontSize);
  return Number.isFinite(px) && px > 0 ? px : 16;
}
