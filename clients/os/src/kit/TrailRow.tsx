import { useLayoutEffect, useRef, useState } from "react";
import { ArrowLeft, ChevronRight } from "lucide-react";

import type { Breadcrumb } from "./Breadcrumbs";
import { useWindowTrail, type PageTrail } from "./pageNavigation";

// THE TRAIL ROW -- one per window, drawn by the window frame directly UNDER
// the section tabs. Always present. The order is the hierarchy: the tabs
// choose the section, and the trail is depth within it.
//
// ===========================================================================
// BACK, A HAIRLINE, THEN WHERE YOU ARE
// ===========================================================================
// Back is FIRST and the trail follows it, with dead space and a hairline
// between the two so a cursor aimed at the first crumb cannot land on Back.
// Left of the hairline is history -- where you came from. Right of it is
// hierarchy -- where this page sits. They often agree and need not.
//
// ALWAYS PRESENT, BACK INCLUDED. At an app's root there is nowhere to go back
// to, and the button is still drawn, inert. Rule 12 says an act that is not
// legal is ABSENT rather than disabled, and that rule is about the ACTS of a
// lifecycle, where a disabled control promised something the server refused.
// This is navigation, and the cost runs the other way: a Back that appeared on
// the first drill-down would push the whole trail 51px to the right, and the
// crumb somebody was reaching for would move under their cursor. A fixed slot
// is what keeps the row a stable target; a browser's own greyed-out Back is
// the convention everybody already reads.
//
// THE CURRENT PAGE IS NEVER THE PART THAT IS CUT. When the trail is wider than
// its window the scroller is pinned to its END, so the beginning slides out of
// view and the page somebody is on stays readable. Going back shortens the
// trail and the earlier crumbs come back into view on their own.
//
// It is a real scroller (scrollbar hidden) rather than a clip, and that is
// deliberate: a crumb hidden by `overflow: hidden` still takes keyboard focus
// and cannot be scrolled to, so Tab would land on something invisible. Here
// the browser scrolls a focused crumb into view the way it would anywhere.

function compute(trail: readonly Breadcrumb[], page: PageTrail | null, fallback: string) {
  const local: readonly Breadcrumb[] = page
    ? page.breadcrumbs ?? (page.back ? [page.back, { label: page.title }] : [{ label: page.title }])
    : [{ label: fallback }];
  const items = [...trail, ...local].filter((item, index, all) => index === 0 || item.label !== all[index - 1]?.label);
  // A page's own `back` wins, then the window's origin -- what `Head` has
  // always done. Last, the nearest ancestor that goes anywhere: with Back
  // always on screen, a page that names a clickable parent and leaves Back
  // dead would read as broken.
  const ancestor = items.slice(0, -1).reverse().find(item => item.onSelect);
  const destination = page?.back ?? trail.at(-1) ?? ancestor;
  return { items, destination };
}

export function TrailRow({ fallback }: {
  /** What to call the place when no heading has said: the section's own name. */
  fallback: string;
}) {
  const nav = useWindowTrail();
  const scroller = useRef<HTMLElement>(null);
  const [clipped, setClipped] = useState(false);

  // `nav.page()` reads the heading's LATEST publication. The row re-renders
  // when the published signature changes, so what is drawn matches it; the
  // handlers below read it again at click time, so a closure is never stale.
  const read = () => compute(nav?.trail ?? [], nav?.page() ?? null, fallback);
  const { items, destination } = read();
  const drawn = items.map(item => item.label).join("\u001f");

  useLayoutEffect(() => {
    const element = scroller.current;
    if (!element) return;
    const pin = () => {
      element.scrollLeft = element.scrollWidth;
      setClipped(element.scrollLeft > 0);
    };
    pin();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(pin);
    observer.observe(element);
    return () => observer.disconnect();
  }, [drawn]);

  const target = destination?.onSelect ? destination.label : null;
  return (
    <div className="os-trail-row" data-os-trail-row>
      <button
        type="button"
        className="os-trail-back"
        aria-label={target ? `Back to ${target}` : "Back"}
        title={target ? `Back to ${target}` : "Nothing to go back to"}
        disabled={!target}
        onClick={() => read().destination?.onSelect?.()}
      >
        <ArrowLeft size={14} aria-hidden />
      </button>
      <span className="os-trail-divider" aria-hidden />
      <nav
        ref={scroller}
        className="os-trail"
        aria-label="Breadcrumbs"
        data-clipped={clipped || undefined}
        onScroll={event => setClipped(event.currentTarget.scrollLeft > 0)}
      >
        <ol>
          {items.map((item, index) => {
            const last = index === items.length - 1;
            return (
              <li key={`${index}:${item.label}`}>
                {index > 0 ? <ChevronRight className="os-trail-sep" size={12} aria-hidden /> : null}
                {!last && item.onSelect ? (
                  <button type="button" title={item.label} onClick={() => read().items[index]?.onSelect?.()}>{item.label}</button>
                ) : (
                  <span title={item.label} aria-current={last ? "page" : undefined}>{item.label}</span>
                )}
              </li>
            );
          })}
        </ol>
      </nav>
    </div>
  );
}
