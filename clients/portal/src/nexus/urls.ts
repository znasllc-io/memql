// Addresses inside Nexus.
//
// Design D5 -- EVERY NODE AND EVERY MOMENT IS A URL; THE CAMERA IS NOT.
//
// The portal's standing rule is that every destination is a URL rather than a
// piece of component state (#3316), and Nexus keeps it: which goal, which
// page, which node is open and which moment the scrubber sits at are all in
// the address bar, so all four survive a refresh and can be sent to a
// colleague.
//
// The camera is the deliberate exception, and it is the one piece of state
// this portal has never had to accommodate before, so it is worth saying why
// rather than leaving it as an omission. A camera position is CONTINUOUS and
// changes on every frame of a drag: putting it in the URL means either a
// history entry per frame or a debounce that makes the address lag the view.
// It is also not what a person means by "look at this" -- they mean the node,
// and the node re-frames the camera on arrival. So the URL names the subject
// and the view follows it.
//
// Paths are written WITHOUT the /portal prefix; the router's basename already
// carries it (see App / Vite's `base`).

export const NEXUS_ROOT = "/nexus";

// The moment the scrubber sits at, as an RFC3339 timestamp. A named export
// because Replay writes it and its test reads it, and the two have to agree
// on the spelling.
export const AT_PARAM = "at";

// A plan id is a bare id at every wire seam (docs/public/concepts/identifiers.md:
// clients never compose or parse the canonical `{concept}:{shortId}` form), so
// there is no colon to preserve -- but it is still encoded, because "bare"
// is a contract about COMPOSITION and says nothing about which characters a
// short id may contain.
export function nexusPath(planId?: string): string {
  return planId === undefined || planId === "" ? NEXUS_ROOT : `${NEXUS_ROOT}/${encodeURIComponent(planId)}`;
}

export function constructsPath(planId: string): string {
  return `${nexusPath(planId)}/constructs`;
}

export function replayPath(planId: string, at?: string): string {
  const base = `${nexusPath(planId)}/replay`;
  return at === undefined || at === "" ? base : `${base}?${AT_PARAM}=${encodeURIComponent(at)}`;
}

// nodePath addresses one node's detail.
//
// TWO ADDRESSES, one per page that can open a node, and that is not
// duplication. The Replay page's event list is the map's keyboard index
// (design 4.4) and pressing Enter on an entry has to open that node WITHOUT
// throwing away the moment the scrubber is parked at -- a single address
// under the Map would navigate away from the replay and lose `?at=`. So the
// node is a child of whichever page opened it, and both are deep-linkable.
export function nodePath(planId: string, nodeId: string): string {
  return `${nexusPath(planId)}/node/${encodeURIComponent(nodeId)}`;
}

export function replayNodePath(planId: string, nodeId: string, at?: string): string {
  const base = `${nexusPath(planId)}/replay/node/${encodeURIComponent(nodeId)}`;
  return at === undefined || at === "" ? base : `${base}?${AT_PARAM}=${encodeURIComponent(at)}`;
}

// The three pages, in tab order. Exported as data because the tab strip, the
// route table and the test all enumerate them, and three copies of a list
// this small is how the fourth page ends up missing from one of them.
export type NexusSurfaceId = "map" | "constructs" | "replay";

export interface NexusSurface {
  id: NexusSurfaceId;
  label: string;
  // What this page answers. Rendered as the page blurb, so it is written as a
  // sentence rather than a noun.
  blurb: string;
}

export const NEXUS_SURFACES: readonly NexusSurface[] = [
  {
    id: "map",
    label: "Map",
    blurb: "What is happening: the goal's world as the system works on it.",
  },
  {
    id: "constructs",
    label: "Constructs",
    blurb: "What it built: the queries, mutations and automations this goal authored.",
  },
  {
    id: "replay",
    label: "Replay",
    blurb: "How it got here: the goal's own history, scrubbable.",
  },
];

export function surfacePath(id: NexusSurfaceId, planId: string): string {
  switch (id) {
    case "map":
      return nexusPath(planId);
    case "constructs":
      return constructsPath(planId);
    case "replay":
      return replayPath(planId);
  }
}
