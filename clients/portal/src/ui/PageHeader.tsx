import type { ReactNode } from "react";

// The page header: eyebrow / title / blurb on the left, ACTIONS IN A ROW on
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
// The eyebrow is a slot, not a string: a view puts the concept id there
// (mono, break-all -- it is the address an operator would paste into a
// query), the admin console puts its role-floor sentence. Both are the one
// most-useful fact about their page, which is what an eyebrow is for.

export function PageHeader({
  eyebrow,
  title,
  blurb,
  actions,
  meta,
}: {
  eyebrow?: ReactNode;
  title: ReactNode;
  blurb?: ReactNode;
  actions?: ReactNode;
  meta?: ReactNode;
}): ReactNode {
  return (
    <header className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3 border-b border-line pb-4">
      <div className="min-w-0">
        {eyebrow === undefined ? null : (
          <div className="font-mono text-xs break-all text-subtle">{eyebrow}</div>
        )}
        <h1 className="mt-1 text-xl font-semibold tracking-tight">{title}</h1>
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
