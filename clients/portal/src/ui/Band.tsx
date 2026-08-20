import type { ReactNode } from "react";

// Band is one horizon of a page: a small-caps caption on a hairline rule
// that runs to the pane's edge. A rule with a caption, not a card header --
// a band is a HORIZON in one continuous page, and boxing each one would make
// three readings of a single population look like three unrelated widgets.
//
// This is the single implementation that used to exist twice (the views'
// Band and the composer's ComposeBand, copied because the primitives lived
// inside the guarded views directory). src/ui/ sits outside that directory,
// so the copy's reason evaporated; headingLevel carries the one real
// difference (a view band is an h2, a composed section's band an h3).
//
// `title` omitted with no meta makes it the opening reading -- the numbers
// ARE the label, and captioning a stat strip "Summary" adds a word and no
// information. The caption row still appears for a title-less band WITH
// meta, because the composer hangs move/remove controls there.

export function Band({
  title,
  meta,
  panel = false,
  headingLevel = "h2",
  children,
}: {
  title?: string;
  meta?: ReactNode;
  // Wrap the contents in a surface. On for an enumerable body (a table needs
  // an edge and its own horizontal scroll); off for a reading or a rail,
  // which sit directly on the page.
  panel?: boolean;
  headingLevel?: "h2" | "h3";
  children: ReactNode;
}): ReactNode {
  const Heading = headingLevel;
  return (
    <section className="min-w-0">
      {title === undefined && meta === undefined ? null : (
        <div className="mb-3 flex items-baseline gap-3">
          <Heading className="shrink-0 text-xs font-semibold tracking-wide text-muted uppercase">
            {title ?? ""}
          </Heading>
          <span aria-hidden="true" className="h-px min-w-4 flex-1 bg-line" />
          {meta === undefined ? null : (
            <span className="shrink-0 text-xs text-subtle">{meta}</span>
          )}
        </div>
      )}
      {panel ? (
        <div className="overflow-x-auto rounded-lg border border-line bg-surface p-1">
          {children}
        </div>
      ) : (
        children
      )}
    </section>
  );
}

// Panel is the bare surface card, for content that needs an edge without a
// band caption -- detail panes, dialogs' bodies, empty-state wells.
export function Panel({
  padded = true,
  children,
}: {
  padded?: boolean;
  children: ReactNode;
}): ReactNode {
  return (
    <div
      className={
        "overflow-x-auto rounded-lg border border-line bg-surface" +
        (padded ? " p-3" : "")
      }
    >
      {children}
    </div>
  );
}
