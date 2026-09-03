import {
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";

// A windowed list (epic memql#4895, spec H): only the rows in view plus an
// overscan are in the DOM, at a fixed row height, so ten thousand lines cost
// what forty do.
//
// FIRST USE, HERE. Nothing else in the OS is virtualized -- every other list
// renders every row, and every other list is a few hundred rows at most. It
// lives beside the surface that needed it and is promoted to `kit/` on the
// second use (the kit's own rule), rather than being an abstraction invented
// from one example.
//
// The geometry is arithmetic over the scroll position and the container
// height: a spacer holds the full height so the scrollbar is honest, and the
// rendered slice is translated to where it belongs. The container height
// comes from a ResizeObserver, with a fallback for the environments that lay
// nothing out (jsdom measures every box at zero, and a list that rendered
// zero rows there would be untestable).
//
// `follow` pins the view to the bottom as rows arrive. It is the PARENT's
// state: this list reports when the reader has scrolled away from the bottom
// (and back), and the parent decides what that means -- a tail pauses its
// following, a search does nothing.

export interface WindowedListProps<T> {
  rows: readonly T[];
  rowHeight: number;
  overscan?: number;
  renderRow: (row: T, index: number) => ReactNode;
  rowId: (row: T) => string;
  selectedId?: string;
  onSelect?: (id: string) => void;
  follow: boolean;
  onFollowChange: (follow: boolean) => void;
  /** Names the grid for assistive tech. */
  label: string;
  /** The DOM id prefix rows are named under; two lists on one surface
   *  need two. */
  id?: string;
}

/** The height assumed until the container has been measured, or where it
 *  never can be. Sixteen comfortable rows. */
export const FALLBACK_HEIGHT = 480;
const DEFAULT_OVERSCAN = 8;

export function WindowedList<T>({
  rows,
  rowHeight,
  overscan = DEFAULT_OVERSCAN,
  renderRow,
  rowId,
  selectedId = "",
  onSelect,
  follow,
  onFollowChange,
  label,
  id = "os-vlist",
}: WindowedListProps<T>) {
  const rootRef = useRef<HTMLDivElement | null>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [height, setHeight] = useState(FALLBACK_HEIGHT);
  const [activeIndex, setActiveIndex] = useState(-1);

  const total = rows.length * rowHeight;
  // The cursor can be left pointing past the end when rows are trimmed or a
  // reading changes; what renders is always the clamped answer.
  const active = activeIndex >= 0 && activeIndex < rows.length ? activeIndex : -1;

  useLayoutEffect(() => {
    const el = rootRef.current;
    if (el === null) return undefined;
    const measure = (): void => {
      const measured = el.clientHeight;
      if (measured > 0) setHeight(measured);
    };
    measure();
    if (typeof ResizeObserver === "undefined") return undefined;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // Pin to the bottom while following. A layout effect, so the pinned
  // position is what paints -- an effect after paint would show the old
  // bottom for a frame and then jump.
  useLayoutEffect(() => {
    if (!follow) return;
    const target = Math.max(0, total - height);
    const el = rootRef.current;
    if (el !== null) el.scrollTop = target;
    setScrollTop(target);
  }, [follow, total, height]);

  function scrollTo(next: number): void {
    const el = rootRef.current;
    if (el !== null) el.scrollTop = next;
    setScrollTop(next);
  }

  function onScroll(): void {
    const el = rootRef.current;
    if (el === null) return;
    const top = el.scrollTop;
    setScrollTop(top);
    const viewport = el.clientHeight > 0 ? el.clientHeight : height;
    const fromBottom = total - (top + viewport);
    // More than one row away from the bottom is "scrolled up"; back within a
    // row of it is "back". The pin itself lands at zero and changes nothing.
    if (follow && fromBottom > rowHeight) onFollowChange(false);
    else if (!follow && fromBottom <= rowHeight) onFollowChange(true);
  }

  function reveal(index: number): void {
    const top = index * rowHeight;
    const bottom = top + rowHeight;
    if (top < scrollTop) scrollTo(top);
    else if (bottom > scrollTop + height) scrollTo(bottom - height);
  }

  function move(next: number): void {
    if (rows.length === 0) return;
    const clamped = Math.max(0, Math.min(rows.length - 1, next));
    setActiveIndex(clamped);
    reveal(clamped);
    // Arrowing away from the last row while following would be undone by the
    // next arrival; arrowing to it (End) is asking to follow again.
    const atEnd = clamped === rows.length - 1;
    if (follow && !atEnd) onFollowChange(false);
    else if (!follow && atEnd) onFollowChange(true);
  }

  function onKeyDown(event: ReactKeyboardEvent<HTMLDivElement>): void {
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        move(active < 0 ? 0 : active + 1);
        break;
      case "ArrowUp":
        event.preventDefault();
        move(active < 0 ? rows.length - 1 : active - 1);
        break;
      case "Home":
        event.preventDefault();
        move(0);
        break;
      case "End":
        event.preventDefault();
        move(rows.length - 1);
        break;
      case "Enter": {
        const row = active >= 0 ? rows[active] : undefined;
        if (row === undefined) return;
        event.preventDefault();
        onSelect?.(rowId(row));
        break;
      }
      case "Escape":
        if (selectedId === "" && active < 0) return;
        event.preventDefault();
        setActiveIndex(-1);
        onSelect?.("");
        break;
      default:
        break;
    }
  }

  const start = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const end = Math.min(rows.length, Math.ceil((scrollTop + height) / rowHeight) + overscan);
  const slice = rows.slice(start, end);

  return (
    <div
      ref={rootRef}
      id={id}
      className="os-vlist"
      role="grid"
      aria-label={label}
      aria-rowcount={rows.length}
      aria-activedescendant={active >= 0 ? `${id}-row-${active}` : undefined}
      tabIndex={0}
      onScroll={onScroll}
      onKeyDown={onKeyDown}
    >
      <div className="os-vlist-space" style={{ height: total }}>
        <div className="os-vlist-slice" style={{ transform: `translateY(${start * rowHeight}px)` }}>
          {slice.map((row, offset) => {
            const index = start + offset;
            const key = rowId(row);
            return (
              <div
                key={key}
                id={`${id}-row-${index}`}
                role="row"
                aria-rowindex={index + 1}
                aria-selected={key === selectedId}
                data-active={index === active || undefined}
                className="os-vlist-row"
                style={{ height: rowHeight }}
                onClick={() => {
                  setActiveIndex(index);
                  onSelect?.(key);
                }}
              >
                {renderRow(row, index)}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
