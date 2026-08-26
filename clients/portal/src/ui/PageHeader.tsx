import type { ReactNode } from "react";

import { PageGuide } from "./PageGuide";

// The page header: subtitle / title / blurb on the left, ACTIONS IN A ROW on
// the right with the meta line beneath them. One component so seventeen
// screens cannot each invent a heading size -- and so the actions read as a
// toolbar instead of the stacked column the old frames produced (a column of
// buttons reads as a form; a row reads as "what you can do here").
//
// THE TYPE SCALE, stated once (the ui/README carries the same table):
//   h1     text-xl semibold tracking-tight -- this component, once per page.
//   band   text-xs semibold uppercase tracking-wide muted -- Band captions
//          (h2 on a page, h3 inside a composed section).
//   body   text-sm; supporting prose muted.
//   data   font-mono via DataText, never for chrome.
//   display font-display text-display -- Squada One, wordmark and big-number
//          moments only. No page reaches for it casually.
//
// ===========================================================================
// TWO SLOTS ABOVE THE TITLE, AND THE DIFFERENCE IS THE WHOLE POINT (#4657)
// ===========================================================================
// There used to be one, `eyebrow`, and almost every page put a concept id in
// it -- so the most prominent line above a page's name was a string in the
// engine's vocabulary, in monospace, on eleven screens. It was the single
// most useful fact for the person who was going to go and query the rows, and
// noise for everybody else.
//
//   subtitle  PLAIN LANGUAGE, and the default. Names WHERE you are in words:
//             "Fleet", "Library", "Everything that happened". Sentence case,
//             not monospace, because it is chrome rather than data.
//   eyebrow   THE MONO SLOT, kept for the case that motivated it and only
//             that case: where the value IS data -- a row's own id on its
//             detail page, which an operator does paste into a query.
//
// The concept id behind a page did not disappear; it moved into the page's
// guide, under Technical details, where its audience is (decision D5).
//
// `pageId` is what puts the Eye there. A page with no registered guide renders
// no button at all, and the repo-root coverage gate is what keeps that branch
// unreachable for a real destination.

export function PageHeader({
  eyebrow,
  subtitle,
  title,
  blurb,
  actions,
  meta,
  pageId,
}: {
  // Monospace, break-all. For a value that IS data.
  eyebrow?: ReactNode;
  // Plain language. Where you are, in the interface's own words.
  subtitle?: ReactNode;
  title: ReactNode;
  blurb?: ReactNode;
  actions?: ReactNode;
  meta?: ReactNode;
  // The guide registry key for this page. Absent means no guide button.
  pageId?: string;
}): ReactNode {
  return (
    <header className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3 border-b border-line pb-4">
      <div className="min-w-0">
        {/* SUBTITLE FIRST, then eyebrow. A page that carries both reads
            "Nexus" / the goal's id / the goal -- area, then address, then
            name, which is the order a person narrows in. The other way round
            put a monospace id above the word that says where you are. */}
        {subtitle === undefined ? null : (
          <div className="text-xs text-muted">{subtitle}</div>
        )}
        {eyebrow === undefined ? null : (
          <div className="font-mono text-xs break-all text-subtle">{eyebrow}</div>
        )}
        <div className="mt-1 flex items-center gap-1.5">
          <h1 className="min-w-0 text-xl font-semibold tracking-tight">{title}</h1>
          {pageId === undefined ? null : <PageGuide pageId={pageId} />}
        </div>
        {blurb === undefined ? null : (
          <p className="mt-1 max-w-2xl text-sm text-muted">{blurb}</p>
        )}
      </div>
      {actions === undefined && meta === undefined ? null : (
        <div className="flex shrink-0 flex-col items-end gap-2">
          {actions === undefined ? null : (
            <div className="flex flex-row flex-wrap items-center justify-end gap-2">
              {actions}
            </div>
          )}
          {meta === undefined ? null : <div>{meta}</div>}
        </div>
      )}
    </header>
  );
}
