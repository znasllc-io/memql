// What a page's guide is made of.
//
// ===========================================================================
// THE GUIDE IS WHERE THE INTERNALS WENT
// ===========================================================================
// The copy sweep (memql#4657) took concept ids out of page eyebrows, env vars
// out of body copy and engine vocabulary out of blurbs. None of it was WRONG
// -- it was in the wrong place, on screen for everybody, all the time, when
// its audience is an operator who occasionally needs it. This is that place:
// one entry per page, opened deliberately from the Eye button, with the
// technical half gated to owner and admin (decision D5).
//
// So the shape has to comfortably carry concept ids, env keys and doc paths.
// If a guide entry cannot hold something the sweep removed, the sweep will
// quietly drop it instead.
//
// PLAIN TS OBJECTS, DELIBERATELY. No markdown, no MDX, no loader. Guide
// content is prose plus a handful of short lists, and a rendering pipeline
// for that buys a build step, a sanitiser and a class of injection bug in
// exchange for italics.

export interface GuideTechnical {
  // The concept id(s) whose rows this page reads or writes. The one place in
  // the portal where a concept id is user-facing text on purpose -- it is the
  // address an operator pastes into a query, which is exactly why the copy
  // gate allowlists this directory and nowhere else.
  readonly concepts?: readonly string[];
  // The environment keys that decide how this page behaves.
  readonly env?: readonly string[];
  // Repo-relative documentation paths. Paths rather than links: the portal
  // serves no docs, and a link that 404s is worse than a path somebody can
  // search for.
  readonly docs?: readonly string[];
  // Anything else an operator would want and a person would not -- a
  // retention window, a refresh interval, which node answers the read.
  readonly notes?: readonly string[];
}

export interface GuideEntry {
  // The page id. Matches the destination or tab id in src/app/nav.ts, and the
  // guide-coverage gate walks BOTH -- so a new destination without a guide
  // fails the build rather than shipping an Eye button that opens nothing.
  readonly id: string;
  // The dialog's heading. Usually the page's own title.
  readonly title: string;
  // What you are looking at: two to four sentences, operator voice. Names
  // what the person controls or sees, never how the engine is built.
  readonly body: string;
  // How it works: short bullets in product terms -- where the data comes
  // from, what the actions do, what to expect.
  readonly how: readonly string[];
  // Owner/admin only, and collapsed even for them.
  readonly technical?: GuideTechnical;
  // A guide video for this page. ABSENT is the normal state and renders the
  // placeholder; a video lands later keyed by page id with no code change,
  // which is the whole reason this is a registry rather than JSX per page.
  readonly videoRef?: string;
}
