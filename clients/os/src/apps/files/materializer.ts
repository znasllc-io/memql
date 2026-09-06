// The seam between Files and the Materializer (epic memql#4981, #4983).
//
// ONE DIRECTION, AND IT IS THE ONLY ONE. Files reads the composition record to
// answer "which of my files came out of the Materializer" and offers one act
// that hands the person over to the app that owns it. Nothing here writes a
// composition, and nothing here restates what one is ABOUT -- the sources, the
// template, the models that contributed, the provenance. Those are the
// record's, and the record is the Materializer's.
//
// WHICH QUESTION EACH SURFACE ANSWERS, agreed with epic memql#4977 so neither
// drifts into the other: their Materialized section answers WHAT WAS MADE, one
// row per composition; the Files rail's Materializer place answers WHERE THE
// FILE IS, one row per output file, an ordinary Library artifact that opens,
// downloads, moves and archives like every other one.

/**
 * The Materializer's app id.
 *
 * It is `materializer` even though the DSL namespace is `compose`, and that
 * split is theirs rather than an inconsistency: `materializer` already names
 * the engine's boot seeder, so the ROWS took a different word while the app
 * kept the name a person knows -- `v1:work:goal.requestedVia` was already
 * carrying this string before either epic.
 */
export const MATERIALIZER_APP = "materializer";

/** The section that opens the composer on one composition. */
export const MATERIALIZER_COMPOSER = "composer";
