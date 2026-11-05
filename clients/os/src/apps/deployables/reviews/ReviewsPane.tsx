import type { ReactNode } from "react";

// THE ONE PANE every Reviews page stands in: its heading, its content, and
// the window's action bar on the bottom edge (DESIGN.md rule 12).
//
// NO HORIZONTAL DIVIDERS HERE (the owner, 2026-11-04: they "look pretty
// terrible" and crowd the page). The rows of a list and the sections of a
// page stand apart by their spacing; `data-os-dividers="none"` is the kit
// stylesheet's switch for a surface that has moved to that rule.

export function ReviewsPane({ heading, bar, context, children }: {
  /** The page's `Head`, and the review views' tabs where the page has them. */
  heading: ReactNode;
  /** The page's `ActionBar`. */
  bar: ReactNode;
  /** What Ask is told about the page. */
  context: Record<string, unknown>;
  children: ReactNode;
}) {
  return (
    <section className="os-action-pane os-reviews" data-os-dividers="none" data-os-page-context={JSON.stringify(context)}>
      <div className="os-action-body os-app-stack">
        {heading}
        {children}
      </div>
      {bar}
    </section>
  );
}
