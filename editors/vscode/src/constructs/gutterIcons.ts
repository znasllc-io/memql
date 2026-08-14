// The three gutter glyphs, as SVG data URIs.
//
// PURE AND SEPARATE FROM decorations.ts FOR ONE REASON: the requirement is that
// the three marks are "distinguishable without relying on colour alone", and
// that is only assertable if the GEOMETRY is reachable from a test. A
// TextEditorDecorationType hands VS Code a URI and never lets anyone look
// inside it again, so an adapter-only implementation could satisfy the
// requirement by accident and stop satisfying it silently.
//
// A DATA URI RATHER THAN A SHIPPED .svg. Three files in `media/` would need
// packaging into the .vsix, a path resolved against the extension root at
// runtime, and a test that the files exist -- to deliver 200 bytes each. The
// data URI has no packaging step and no path to get wrong.
//
// SHAPE CARRIES THE MEANING, COLOUR ONLY REINFORCES IT:
//
//   untrained  a hollow ring     -- an outline where something solid could be
//   drifted    a triangle        -- the universal "this diverged" silhouette
//   live       a filled disc     -- solid, present, nothing to do
//
// A ring, a triangle and a disc are separable at 16 pixels, in greyscale, and
// by anyone who cannot distinguish the three colours. The colours are VS Code
// theme-independent literals because an SVG in a gutter icon gets no CSS
// variables -- so they are chosen to read on both a light and a dark gutter
// rather than to match any one theme.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3761 #3745

import type { GutterMark } from "../state/training.js";

/** The glyph geometry, with no colour in it at all. */
const SHAPE: Readonly<Record<GutterMark, string>> = {
  // Ring: a stroked circle, deliberately NOT filled -- the hollow centre is
  // what a reader sees first and is the whole distinction from `live`.
  untrained: '<circle cx="8" cy="8" r="4.5" fill="none" stroke="COLOR" stroke-width="1.6"/>',
  // Triangle, apex up.
  drifted: '<path d="M8 3.2 L13.4 12.6 L2.6 12.6 Z" fill="COLOR"/>',
  // Filled disc.
  live: '<circle cx="8" cy="8" r="4.5" fill="COLOR"/>',
};

/**
 * The colour per mark. Reinforcement only -- strip every one of these and the
 * three glyphs remain distinguishable, which is what `gutterShape` exists to
 * let a test assert.
 */
const COLOR: Readonly<Record<GutterMark, string>> = {
  untrained: "#9aa0a6",
  drifted: "#d98c1f",
  live: "#3f9142",
};

/** The colourless geometry for a mark. Exported for the distinguishability test. */
export function gutterShape(mark: GutterMark): string {
  return SHAPE[mark];
}

/** The full SVG document for a mark. */
export function gutterSvg(mark: GutterMark): string {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 16 16">${SHAPE[
    mark
  ].replace(/COLOR/g, COLOR[mark])}</svg>`;
}

/**
 * The `data:` URI a gutter icon path takes.
 *
 * base64 rather than percent-encoding: an SVG contains `#`, `"` and `<`, and a
 * URI parser that treats `#` as a fragment separator truncates the document at
 * the first colour literal. Encoding it away removes the class of bug rather
 * than escaping around it.
 */
export function gutterIconUri(mark: GutterMark): string {
  const base64 = Buffer.from(gutterSvg(mark), "utf8").toString("base64");
  return `data:image/svg+xml;base64,${base64}`;
}

// -----------------------------------------------------------------------------
// The signature-range decoration
// -----------------------------------------------------------------------------

/**
 * The visual half of a `DecorationRenderOptions`, as a plain object.
 *
 * PLAIN AND PURE SO THE ONE RULE CAN BE ASSERTED. "Does not fight syntax
 * colouring" has exactly one mechanical meaning for a decoration: it must not
 * set `color`, and it must not set `textDecoration` (which can carry an
 * arbitrary CSS declaration, `color` included, and is the loophole an assertion
 * on `color` alone would miss). A test reads these objects and checks both keys
 * are absent -- which is the acceptance criterion "syntax colouring is
 * unchanged, asserted not eyeballed", stated as something a machine can check.
 *
 * The adapter adds `rangeBehavior` and the overview-ruler lane, both of which
 * are `vscode` enums and neither of which can affect a token's colour.
 */
export interface MarkDecoration {
  gutterIconPath: string;
  gutterIconSize: string;
  overviewRulerColor: string;
  /** A faint wash behind the signature. Absent for `live`. */
  backgroundColor?: string;
  /** A dotted underline under the signature. Absent for `live`. */
  border?: string;
  borderRadius?: string;
}

/**
 * How a mark decorates the signature range.
 *
 * `live` GETS NO RANGE DECORATION -- gutter only. In a healthy file every
 * construct is live, and washing every signature in the file would be a
 * permanent tint over the code that conveys nothing a clean gutter does not.
 * The text area stays untouched until something needs attention, which is what
 * makes the attention states legible when they appear.
 */
export function markDecoration(mark: GutterMark): MarkDecoration {
  const base: MarkDecoration = {
    gutterIconPath: gutterIconUri(mark),
    gutterIconSize: "contain",
    overviewRulerColor: COLOR[mark],
  };
  if (mark === "live") return base;
  return {
    ...base,
    // Alpha-blended so it sits UNDER the syntax colours rather than competing
    // with them; at 12% it reads as a tint on any theme without altering the
    // foreground of a single token.
    backgroundColor: `${COLOR[mark]}1f`,
    border: `1px dotted ${COLOR[mark]}`,
    borderRadius: "2px",
  };
}
