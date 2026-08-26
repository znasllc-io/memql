import type { PageManifest } from "../pages/manifest";
import { ARTIFACT_CONCEPT_ID } from "./concepts";

// Artifacts, as a manifest (epic memql#4661, task memql#4674).
export const ARTIFACTS_PAGE_ID = "library.artifacts";

export const ARTIFACTS_PAGE: PageManifest = {
  pageId: ARTIFACTS_PAGE_ID,
  title: "Artifacts",
  blurb:
    "Everything this cluster's Library has indexed for you -- files you uploaded, " +
    "generated outputs, and the notes, to-dos, calendar events, and memories your " +
    "agents have created. Put your own labels on one to say what it was for; a " +
    "label you or an agent added is a filter here too.",
  sections: [
    {
      conceptId: ARTIFACT_CONCEPT_ID,
      arrangement: {
        conceptId: ARTIFACT_CONCEPT_ID,
        elements: [
          { element: "statTile", band: "reading", bindings: { metric: [] } },
          {
            element: "widget",
            band: "roll",
            title: "Library",
            options: { widgetId: "artifacts" },
          },
        ],
      },
      required: [{ element: "widget", band: "roll", options: { widgetId: "artifacts" } }],
    },
  ],
};
