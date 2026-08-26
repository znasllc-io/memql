import type { ComponentType } from "react";

import { ADMIN_ROOT, adminPath } from "../admin/urls";
import { ARTIFACTS_ROOT } from "../artifacts/urls";
import { COMPOSE_ROOT } from "../compose/urls";
import { CONCEPTS_ROOT } from "../concepts/urls";
import { DATA_ORIGINS_ROOT } from "../dataorigins/urls";
import { FLEET_ROOT, fleetPath } from "../fleet/urls";
import { INTEGRATIONS_ROOT } from "../integrations/urls";
import { Archive, Boxes, Cpu, Gauge, LayoutGrid, Network, Orbit } from "../ui/icons";
import { VIEWS_ROOT } from "../views/urls";

// THE NAVIGATION, AS DATA. One definition; the rail, the tab strips, the
// command palette and two repo-root gates all walk this file.
//
// ===========================================================================
// SEVEN FLAT DESTINATIONS, NO CAPTIONS (decision D1)
// ===========================================================================
// The rail carried seventeen rows plus one per saved view, under six group
// captions, with Fleet reachable from two of them and four icons used twice.
// The count is the defect: a rail is a thing you scan, and past about seven
// items nobody scans it -- they search, and there was nothing to search with.
//
// So: seven areas, no captions (seven items need no grouping), and everything
// that used to be a row is either a TAB on the area that owns it or one
// keystroke away in the command palette. The palette is what makes the flat
// rail safe; without it this would be hiding things.
//
// ===========================================================================
// THE RAIL LISTS AREAS; A PAGE'S SUB-SURFACES ARE ITS TABS; NEVER BOTH (D2)
// ===========================================================================
// That is the whole placement rule, and it is what stopped Fleet appearing
// three times. If a thing is a facet of an area, it is a tab. If it is an
// area, it is a rail row. Nothing is both.
//
// ===========================================================================
// RAIL PLACEMENT IS NOT URL SHAPE
// ===========================================================================
// No route moved for this restructure and no redirect was needed. Library's
// two tabs are /artifacts and /deployables; Cluster's span /integrations,
// /data-origins, /stores and /admin/*. That is why a destination carries
// `match` -- a list of path prefixes it is ACTIVE for -- instead of relying on
// NavLink's own matching, which can only follow one path.
//
// ===========================================================================
// EVERY ID IS A GUIDE ID
// ===========================================================================
// A destination's id and a tab's id are the keys into src/guides/. The
// repo-root coverage gate walks this file against that registry, so adding a
// destination without writing its guide fails the build rather than shipping
// an Eye button that opens nothing.

export type NavIcon = ComponentType<{ size?: number | string; className?: string }>;

// WHO IS OFFERED A TAB. Absent means everyone.
//
// It mirrors what the rail rows gated on before the restructure, exactly: the
// engine refuses these reads below the named role, so a tab visible to
// everyone would be a door that does not open. `owner` is the narrower floor
// that arrived with AI providers (memql#4440).
export type NavAccess = "admin" | "owner";

export interface NavTab {
  readonly id: string;
  readonly label: string;
  readonly to: string;
  // NavLink `end` semantics for the tab strip's own matching.
  readonly end?: boolean;
  readonly access?: NavAccess;
}

export interface NavDestination {
  readonly id: string;
  readonly label: string;
  readonly icon: NavIcon;
  // Where the row goes when the destination has no tabs. With tabs, the row
  // goes to the first tab the caller may see -- so an admin lands on
  // Integrations and an owner lands on Integrations too, rather than on a
  // page they will be refused.
  readonly to?: string;
  // The path prefixes this destination owns. "/" matches only itself.
  readonly match: readonly string[];
  readonly tabs?: readonly NavTab[];
}

export const DESTINATIONS: readonly NavDestination[] = [
  {
    id: "console",
    label: "Console",
    icon: Gauge,
    to: "/",
    match: ["/"],
  },
  {
    // Nexus keeps its own per-goal tabs (Map / Constructs / Replay) INSIDE a
    // goal, which is a different altitude from an area's facets -- so it has
    // no tab strip here, and Goals is simply the landing.
    id: "nexus",
    label: "Nexus",
    icon: Orbit,
    to: "/nexus",
    match: ["/nexus"],
  },
  {
    // ONE Views destination, and the gallery is the index. The five built-in
    // views, the caller's composed ones and the door to composing another
    // were three separate things in the rail; they are one page now, which is
    // what they always were to the person reading them.
    id: "views",
    label: "Views",
    icon: LayoutGrid,
    to: VIEWS_ROOT,
    match: [VIEWS_ROOT, COMPOSE_ROOT],
  },
  {
    id: "concepts",
    label: "Concepts",
    icon: Boxes,
    match: [CONCEPTS_ROOT, "/modules"],
    tabs: [
      { id: "concepts", label: "Concepts", to: CONCEPTS_ROOT, end: true },
      // Owner/admin, as the rail row was: the engine refuses the reads behind
      // it outright, so this is not a permission being advertised.
      { id: "concepts.modules", label: "Modules", to: "/modules", access: "admin" },
    ],
  },
  {
    id: "fleet",
    label: "Fleet",
    // Cpu rather than Monitor: a machine is a computer doing work, and it
    // frees Monitor for the theme toggle, which is the only other thing in
    // the product that means "your screen".
    icon: Cpu,
    match: [FLEET_ROOT],
    // SPELLED OUT rather than mapped from FLEET_SURFACES, and the derivation
    // it replaced was the tempting version. Two reasons it is worse here:
    //
    //   * an id is a GUIDE KEY, and the repo-root coverage gate reads these
    //     ids out of this file. A template literal is not a value a gate can
    //     read, and a gate that skipped what it could not parse would go
    //     quiet exactly when somebody added something.
    //   * a new area surface should be a deliberate decision anyway. It needs
    //     a guide entry, and deriving the tab silently gives it one without.
    //
    // The two lists cannot drift: nav.test.ts asserts these ids and labels
    // against FLEET_SURFACES.
    tabs: [
      { id: "fleet.machines", label: "Machines", to: fleetPath("machines"), end: true },
      { id: "fleet.apps", label: "Local apps", to: fleetPath("apps"), end: true },
      { id: "fleet.workbenches", label: "Workbenches", to: fleetPath("workbenches"), end: true },
    ],
  },
  {
    id: "library",
    label: "Library",
    icon: Archive,
    match: [ARTIFACTS_ROOT, "/deployables"],
    tabs: [
      { id: "library.artifacts", label: "Artifacts", to: ARTIFACTS_ROOT, end: true },
      { id: "library.deployables", label: "Deployables", to: "/deployables" },
    ],
  },
  {
    id: "cluster",
    label: "Cluster",
    icon: Network,
    match: [INTEGRATIONS_ROOT, DATA_ORIGINS_ROOT, "/stores", ADMIN_ROOT],
    tabs: [
      { id: "cluster.integrations", label: "Integrations", to: INTEGRATIONS_ROOT, end: true },
      { id: "cluster.data-origins", label: "Data origins", to: DATA_ORIGINS_ROOT, access: "admin" },
      { id: "cluster.stores", label: "Stores", to: "/stores", access: "admin" },
      // Reachable by tab at last: /admin/providers was typed-URL only.
      { id: "cluster.providers", label: "AI providers", to: adminPath("providers"), access: "owner" },
      { id: "cluster.tokens", label: "Tokens", to: adminPath("tokens"), access: "admin" },
      { id: "cluster.keys", label: "Signing keys", to: adminPath("keys"), access: "admin" },
      { id: "cluster.settings", label: "Settings", to: adminPath("settings"), access: "admin" },
    ],
  },
];

// maySee answers whether a caller holding `role` is offered a tab.
//
// An UNRESOLVED role (empty, because the connection is still handshaking) is
// treated as below every floor. The safe direction: the tab appears when the
// role arrives, and nothing offers a surface to somebody it will refuse.
export function maySee(access: NavAccess | undefined, role: string): boolean {
  if (access === undefined) return true;
  if (access === "owner") return role === "owner";
  return role === "owner" || role === "admin";
}

export function visibleTabs(destination: NavDestination, role: string): readonly NavTab[] {
  return (destination.tabs ?? []).filter((tab) => maySee(tab.access, role));
}

// Where the rail row points. With tabs, the first one the caller may see --
// which is why /cluster is not a route: there is nothing an operator would
// read on a landing page above a tab strip that the tab strip does not
// already say (the lesson /admin's index learned in memql#4264).
export function destinationPath(destination: NavDestination, role: string): string {
  const tabs = visibleTabs(destination, role);
  const first = tabs[0];
  if (first !== undefined) return first.to;
  return destination.to ?? "/";
}

// Whether a destination owns the address currently open.
//
// Prefix matching, with "/" exact -- the console prefixes every other path and
// would otherwise be permanently active.
export function destinationIsActive(destination: NavDestination, pathname: string): boolean {
  return destination.match.some((prefix) =>
    prefix === "/" ? pathname === "/" : pathname === prefix || pathname.startsWith(`${prefix}/`),
  );
}

export function destinationById(id: string): NavDestination | undefined {
  return DESTINATIONS.find((destination) => destination.id === id);
}

// Every id the nav declares: the destinations plus every tab, deduplicated.
// The guide-coverage gate reads this list, and so does the palette.
export function navPageIds(): readonly string[] {
  const ids = new Set<string>();
  for (const destination of DESTINATIONS) {
    ids.add(destination.id);
    for (const tab of destination.tabs ?? []) ids.add(tab.id);
  }
  return [...ids];
}
