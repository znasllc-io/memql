// Addresses inside the Fleet surface (epic memql#4349). Mirrors admin/urls.ts:
// a path is referenced from the route table, the tab strip, the nav rail and
// the tests, and four hand-written string literals is how a link ends up
// pointing one segment away from the route that serves it.
//
// Paths are written WITHOUT the /portal prefix -- the router's basename
// already carries it (see App / Vite's `base`).
//
// The route table (src/app/routes.tsx) mounts this module as a SPLAT, so
// nothing here is repeated there: adding a surface is a row in FLEET_SURFACES
// plus a Route in FleetRoutes.tsx, and no edit outside this directory.

export const FLEET_ROOT = "/fleet";

// A surface's slug, its tab label, the page title, and the sentence saying
// what an operator came here to do. The blurb is interface copy: it names the
// operator's job, not the schema.
export interface FleetSurface {
  readonly id: string;
  readonly label: string;
  readonly title: string;
  readonly blurb: string;
}

// TWO SURFACES, and the split is where the work RUNS rather than a tidy-up.
// A machine is somebody's own computer, reached over a stream it opened and
// revocable by its owner; a workbench is a sandbox this cluster provisions and
// throws away. They answer different questions ("is my laptop reachable" vs
// "why is this workbench node full") and the verbs on them are not the same
// verbs, so one combined table would have to caveat every column.
export const FLEET_SURFACES: readonly FleetSurface[] = [
  {
    id: "machines",
    label: "Machines",
    title: "Machines",
    blurb:
      "Every computer registered to this cluster as a worker -- what it can do, " +
      "how it is labelled, and which replica is holding its stream right now. " +
      "Routing decides which of them a piece of work lands on.",
  },
  {
    id: "apps",
    label: "Local apps",
    title: "Local apps",
    blurb:
      "Delegating work to an app you already pay for -- Claude Code or Codex -- " +
      "on a machine you own. Which task kinds may go there, which apps to try, " +
      "and the transcript of every run that did.",
  },
  {
    id: "workbenches",
    label: "Workbenches",
    title: "Workbenches",
    blurb:
      "The cluster's own sandboxed working directories: the replicas that host " +
      "them, and the per-plan workspaces living on each. Nothing here touches " +
      "anybody's machine.",
  },
];

export function fleetPath(surfaceId = ""): string {
  return surfaceId === "" ? FLEET_ROOT : `${FLEET_ROOT}/${surfaceId}`;
}

export function fleetSurfaceById(id: string): FleetSurface | undefined {
  return FLEET_SURFACES.find((surface) => surface.id === id);
}

// sessionPath addresses one delegated run's live transcript (memql#4363).
// Session ids are canonical (v1:worker:appSession:<shortId>) because the row
// is minted server-side and travels on the task, so the colons need encoding
// to survive the address bar intact.
export function sessionPath(sessionId: string): string {
  return `${FLEET_ROOT}/apps/sessions/${encodeURIComponent(sessionId)}`;
}

export const SESSION_ROUTE_PATTERN = "apps/sessions/:sessionId";
