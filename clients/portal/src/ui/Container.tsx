import type { ReactNode } from "react";

// The page frame: ONE content width, and it is the shell's.
//
// Every routed page renders its body inside this. A page, pane or frame never
// sets max-w-*, mx-auto or a fixed width on its own root -- measure belongs to
// CONTENT, not to the frame: a paragraph caps its own line length, a form caps
// its own field width, an EmptyState centres itself.
//
// # Why this exists at all, rather than the shell just doing it
//
// AppShell's <main> could set the width and pages could carry nothing. It does
// not, because then the rule lives in one file nobody opens while editing a
// page, and the only thing standing between the portal and a second `max-w-4xl`
// is that nobody thought of it. A wrapper in the page's own file is the rule
// where the rule gets broken, and it is greppable.
//
// # What was here before
//
// A `variant` prop with two widths, `data` (full) and `reading`
// (mx-auto max-w-3xl). `reading` had ZERO usages in its lifetime and the two
// widths were not the problem anyway: pages predating this component hand-rolled
// their own max-w-*, so the column jumped as an operator moved between sections
// -- Concepts and Integrations at 5xl, campaign editing at 3xl, Sites full width
// but its own detail page at 3xl. One width is what stops that, and a variant
// nobody chose was just a second way to reintroduce it.
//
// portal_page_frame_test.go at the repo root enforces this.

export function Container({ children }: { children: ReactNode }): ReactNode {
  return <div className="w-full">{children}</div>;
}
