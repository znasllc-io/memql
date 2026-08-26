import type { PageManifest } from "../pages/manifest";
import { SITE_CONCEPT_ID } from "./concepts";

// Deployables, as a manifest (epic memql#4661, task memql#4674).
//
// The blurb is the one every reader gets. The cluster-owner variant stays in
// the body, because it is conditional on WHO is reading and a manifest is data
// that does not know -- telling somebody "across every user" when they are
// seeing only their own would be a page lying about its own scope.
export const DEPLOYABLES_PAGE_ID = "platform.deployables";

export const DEPLOYABLES_PAGE: PageManifest = {
  pageId: DEPLOYABLES_PAGE_ID,
  title: "Deployables",
  blurb:
    "The things this cluster hosts. A deployable is data, not infrastructure: " +
    "deploying and rolling back point its row at a bundle version, and the edge " +
    "picks the change up on its next resolve for that hostname.",
  sections: [
    {
      conceptId: SITE_CONCEPT_ID,
      arrangement: {
        conceptId: SITE_CONCEPT_ID,
        elements: [
          { element: "statTile", band: "reading", bindings: { metric: [] } },
          {
            element: "widget",
            band: "roll",
            title: "Deployables",
            options: { widgetId: "deployables" },
          },
        ],
      },
      required: [{ element: "widget", band: "roll", options: { widgetId: "deployables" } }],
    },
  ],
};
