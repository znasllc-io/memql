import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { IconButton } from "./IconButton";

const GAP = 8;

/** Explicit paging shared by equipment and editable card collections. */
export function CardCarousel<T>({ items, itemKey, renderItem, label, listLabel = label, noun, plural = `${noun}s`, revealKey, minCardWidth = 176 }: {
  items: readonly T[];
  itemKey: (item: T) => string;
  renderItem: (item: T) => ReactNode;
  label: string;
  listLabel?: string;
  noun: string;
  plural?: string;
  minCardWidth?: number;
  /** A newly added item can reveal its page without selecting its neighbors. */
  revealKey?: string;
}) {
  const viewport = useRef<HTMLDivElement>(null);
  const revealed = useRef<string | undefined>(undefined);
  const id = useId();
  const [size, setSize] = useState(2);
  const [first, setFirst] = useState(0);
  const pageCount = Math.ceil(items.length / size);
  const page = Math.min(Math.floor(first / size), Math.max(0, pageCount - 1));
  const start = page * size;
  const end = Math.min(start + size, items.length);

  useLayoutEffect(() => {
    const element = viewport.current;
    if (!element) return;
    const resize = (width: number) => {
      // Retained hidden tabs have no width; preserve their current page.
      if (width <= 0) return;
      const focused = element.querySelector(":focus")?.closest<HTMLElement>("[data-card-index]");
      if (focused) setFirst(Number(focused.dataset.cardIndex));
      setSize(width < minCardWidth * 2 + GAP ? 1 : 2);
    };
    resize(element.getBoundingClientRect().width);
    const observer = new ResizeObserver(entries => resize(entries[0]?.contentRect.width ?? 0));
    observer.observe(element);
    return () => observer.disconnect();
  }, [items.length > 0, minCardWidth]);

  useEffect(() => { setFirst(start); }, [start]);
  useEffect(() => {
    if (revealKey === revealed.current) return;
    if (revealKey === undefined) { revealed.current = undefined; return; }
    const index = items.findIndex(item => itemKey(item) === revealKey);
    if (index >= 0) { revealed.current = revealKey; setFirst(index); }
  }, [revealKey, items, itemKey]);

  if (!items.length) return null;
  return <div className="os-card-carousel" role="group" aria-roledescription="carousel" aria-label={label}>
    <div className="os-card-viewport" ref={viewport} id={id}>
      <ul className="os-card-track" aria-label={listLabel} style={{
        gap: GAP,
        gridAutoColumns: `calc((100% - ${(size - 1) * GAP}px) / ${size})`,
        transform: `translateX(calc(-${page * 100}% - ${page * GAP}px))`,
      }}>
        {items.map((item, index) => <li key={itemKey(item)} data-card-index={index}
          aria-hidden={index < start || index >= end || undefined} inert={index < start || index >= end}>
          {renderItem(item)}
        </li>)}
      </ul>
    </div>
    <div className="os-card-pager">
      <IconButton label={`Previous ${plural}`} aria-controls={id} disabled={page === 0} onClick={() => setFirst(start - size)}><ChevronLeft size={17} aria-hidden /></IconButton>
      <span role="status" aria-atomic="true">{start + 1 === end ? end : `${start + 1}–${end}`} of {items.length} {items.length === 1 ? noun : plural}</span>
      <IconButton label={`Next ${plural}`} aria-controls={id} disabled={page === pageCount - 1} onClick={() => setFirst(start + size)}><ChevronRight size={17} aria-hidden /></IconButton>
    </div>
  </div>;
}
