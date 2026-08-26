import type { PageManifest } from "../pages/manifest";
import { PLAN_CONCEPT_ID } from "./concepts";

// The Nexus goal page, AS AN ARRANGEMENT (epic memql#4661, task memql#4673).
//
// ===========================================================================
// THE PROOF, AND WHY THIS PAGE IS THE ONE THAT PROVES IT
// ===========================================================================
// The claim of spec D6 is that the arrangement system is the PAGE system --
// every portal page is a layout plus elements plus registered widgets, and is
// therefore regenerable, versioned and consistent. The obvious objection is
// that some pages are too rich for a grammar, and the richest page in the
// console is this one: a WebGL scene with hover, click-to-detail,
// materialization, a demand frame loop and a no-WebGL fallback.
//
// It is a focus layout with one hero. That is the proof.
//
// ===========================================================================
// WHAT THE ARRANGEMENT DOES NOT COVER, STATED PLAINLY
// ===========================================================================
// The goal PICKER and the Map/Constructs/Replay tabs stay in GoalLayout, and
// they are not an omission: they are ROUTE-LEVEL NAVIGATION -- which goal, and
// which of its three surfaces -- and navigation is not a reading of a
// population. An arrangement that placed the tab bar would be an arrangement a
// regeneration could remove, and a page you cannot navigate away from is not a
// rearrangement of this page.
//
// The same rule the excluded-surfaces list follows (sign-in, the composer
// editor, dialogs): the arrangement covers what the page SHOWS, not how a
// person got here or where they go next.

export const GOAL_PAGE_ID = "nexus.goal";

export const GOAL_PAGE: PageManifest = {
  pageId: GOAL_PAGE_ID,
  title: "Map",
  blurb: "A goal's world, as the system works on it.",
  sections: [
    {
      conceptId: PLAN_CONCEPT_ID,
      arrangement: {
        conceptId: PLAN_CONCEPT_ID,
        // FOCUS, with the map as the one thing the page is about. The
        // supporting column is where a reading of the same goal goes -- the
        // phase counts the map's fallback already carries in text.
        layout: "focus",
        elements: [
          {
            element: "scene",
            band: "shape",
            role: "hero",
            options: { sceneId: "goalMap" },
          },
          {
            element: "statTile",
            band: "reading",
            role: "supporting",
            bindings: { metric: [] },
          },
        ],
      },
      // A goal page without its map is not a rearrangement of the goal page.
      // The guardrail is what stops a regeneration from producing a valid
      // arrangement of a page that no longer does its job.
      required: [{ element: "scene", band: "shape", options: { sceneId: "goalMap" } }],
    },
  ],
};
