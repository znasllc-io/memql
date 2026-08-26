import type { PageManifest } from "../pages/manifest";
import { WORKBENCH_WORKSPACE_CONCEPT_ID, WORKER_REGISTRATION_CONCEPT_ID } from "./concepts";
import { fleetSurfaceById } from "./urls";

// The Fleet pages, as manifests (epic memql#4661, task memql#4674).
//
// The title and blurb come from FLEET_SURFACES rather than being restated
// here: they are also the tab labels, and two copies of a page's own name is
// two places for it to drift.

const machines = fleetSurfaceById("machines");
const workbenches = fleetSurfaceById("workbenches");

export const MACHINES_PAGE_ID = "fleet.machines";

export const MACHINES_PAGE: PageManifest = {
  pageId: MACHINES_PAGE_ID,
  title: machines?.title ?? "Machines",
  blurb: machines?.blurb ?? "",
  sections: [
    {
      conceptId: WORKER_REGISTRATION_CONCEPT_ID,
      arrangement: {
        conceptId: WORKER_REGISTRATION_CONCEPT_ID,
        elements: [
          // The reading the page never had: how many machines, and how they
          // divide. Rendered by the element library over the same rows the
          // cards below read, so it improves when the library does.
          { element: "statTile", band: "reading", bindings: { metric: [] } },
          // The cards, the pairing panel and the routing editor. One widget,
          // for the reason MachinesPage's header states.
          {
            element: "widget",
            band: "roll",
            title: "Machines",
            options: { widgetId: "fleetMachines" },
          },
        ],
      },
      // A fleet page without the machines on it is not a rearrangement of the
      // fleet page -- it is a different page that happens to load the same
      // rows.
      required: [{ element: "widget", band: "roll", options: { widgetId: "fleetMachines" } }],
    },
  ],
};

export const WORKBENCHES_PAGE_ID = "fleet.workbenches";

export const WORKBENCHES_PAGE: PageManifest = {
  pageId: WORKBENCHES_PAGE_ID,
  title: workbenches?.title ?? "Workbenches",
  blurb: workbenches?.blurb ?? "",
  sections: [
    {
      conceptId: WORKBENCH_WORKSPACE_CONCEPT_ID,
      arrangement: {
        conceptId: WORKBENCH_WORKSPACE_CONCEPT_ID,
        elements: [
          { element: "statTile", band: "reading", bindings: { metric: [] } },
          {
            element: "widget",
            band: "roll",
            title: "Workspaces",
            options: { widgetId: "fleetWorkbenches" },
          },
        ],
      },
      required: [{ element: "widget", band: "roll", options: { widgetId: "fleetWorkbenches" } }],
    },
  ],
};
