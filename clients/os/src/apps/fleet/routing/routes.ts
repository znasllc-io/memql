// A route, as this surface holds one.
//
// The engine's `routerListPolicies` answers the shipped and custom routes with
// BOTH their authored entries (`primary` + `fallbacks`) and their EXPANDED
// chain (`chain`, with any `policy:<name>` reference resolved). The composer
// edits the authored entries -- a route that includes another route keeps
// including it -- so that is what `routeEntries` answers, falling back to the
// chain only for a row that did not carry them.

import type { TaskPolicy } from "../taskPolicies";
import type { RouteLike } from "./sources";

export function routeEntries(route: TaskPolicy): string[] {
  if (route.primary) return [route.primary, ...(route.fallbacks ?? [])];
  return [...route.chain];
}

export function routesLike(routes: readonly TaskPolicy[]): RouteLike[] {
  return routes.map((r) => ({ name: r.name, entries: routeEntries(r) }));
}

/** The configuration revision a write must name; every route row carries it. */
export function catalogRevision(routes: readonly TaskPolicy[]): number {
  return routes.find((r) => typeof r.revision === "number")?.revision ?? 0;
}

/**
 * A route proposal from Ask: the same fields a manual save sends, and what
 * the person is asked to review before anything is written.
 */
export interface RouteProposal {
  name: string;
  description: string;
  primary: string;
  fallbacks: string[];
  revision: number;
  action?: "save" | "reset";
  explanation?: string;
  existing?: boolean;
}
