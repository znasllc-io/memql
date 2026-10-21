import type { ExecutionDestination } from "../kit/ExecutionDestination";
// Inference routing remains independent of the goal's tool destination.
export type AskLevel = "" | "fast" | "strong" | "reasoning";

export interface AskRouting {
  destination?: ExecutionDestination;
  /** The source selector, sent as AiChatMsg.provider. "" is Auto. */
  source: string;
  /** The level, sent as AiChatMsg.level. "" is Auto. */
  level: AskLevel;
}

export const AUTO_ROUTING: AskRouting = Object.freeze({ source: "", level: "" }) as AskRouting;

// ---------------------------------------------------------------------------
// Routes (docs/superpowers/specs/2026-10-18-routes-design.md)
// ---------------------------------------------------------------------------
//
// A ROUTE is how a message's model calls are served: Auto (the cluster
// decides, the default), Local, Apps, or one the person made. The message
// field holds a route ID, and the wire carries it in the existing source
// field as `route:<id>` -- "" being Auto, so a conversation nobody routed
// sends exactly what it always did.

export const ROUTE_SOURCE_PREFIX = "route:";

/** Apps open sessions on the person's computer, outside a Workbench. Auto
 * leaves destination consent to the engine; it is not a grant of access. */
export function routeSupportsDestination(routeId = "", destination: ExecutionDestination = "auto"): boolean {
  return routeId !== "apps" || destination !== "workbench";
}

/** The source a route id travels as. Auto ("" or "auto") travels as "". */
export function routeSource(routeId: string): string {
  const id = routeId.trim();
  return id === "" || id === "auto" ? "" : ROUTE_SOURCE_PREFIX + id;
}

/** The route id a recorded source names, "" (Auto) for anything that is not a route. */
export function routeIdFromSource(source: string | undefined): string {
  const value = (source ?? "").trim();
  if (!value.startsWith(ROUTE_SOURCE_PREFIX)) return "";
  const id = value.slice(ROUTE_SOURCE_PREFIX.length);
  return id === "auto" ? "" : id;
}

/** One route the picker offers, as `routesForActor` reports it. */
export interface AskRouteOption {
  id: string;
  name: string;
  kind: "auto" | "local" | "apps" | "custom";
  scope: "cluster" | "personal";
  description: string;
  ready: boolean;
  /** Why it cannot serve now; "" when it can. Auto may carry one and stay
   *  choosable. */
  reason: string;
}

const KINDS = new Set(["auto", "local", "apps", "custom"]);

/**
 * The picker's options out of a `routesForActor` answer, built-in routes
 * first in the order the cluster lists them, the person's own after. A row
 * the picker cannot use (no id, an unknown kind) is dropped rather than drawn
 * as something it is not. Auto is always present and always choosable: it is
 * the absence of a choice, which the cluster never refuses, so a person whose
 * routes are all unready can still go back to it. When the cluster says Auto
 * cannot answer either, its reason is kept to show beside it.
 */
export function routeOptionsFrom(raw: unknown): AskRouteOption[] {
  const rows = Array.isArray((raw as { routes?: unknown } | null)?.routes) ? (raw as { routes: unknown[] }).routes : [];
  const options: AskRouteOption[] = [];
  for (const row of rows) {
    if (!row || typeof row !== "object") continue;
    const r = row as Record<string, unknown>;
    const id = typeof r.id === "string" ? r.id.trim() : "";
    const kind = typeof r.kind === "string" && KINDS.has(r.kind) ? (r.kind as AskRouteOption["kind"]) : null;
    if (id === "" || kind === null || options.some(option => option.id === id)) continue;
    const reason = r.ready === true ? "" : typeof r.reason === "string" ? r.reason.trim() : "";
    options.push({
      id,
      name: typeof r.name === "string" && r.name.trim() !== "" ? r.name.trim() : id,
      kind,
      scope: r.scope === "personal" ? "personal" : "cluster",
      description: typeof r.description === "string" ? r.description.trim() : "",
      ready: kind === "auto" ? true : r.ready === true,
      reason: kind === "auto" ? reason : r.ready === true ? "" : reason || "Not ready",
    });
  }
  if (!options.some(option => option.kind === "auto")) {
    options.unshift({ id: "auto", name: "Auto", kind: "auto", scope: "cluster", description: "", ready: true, reason: "" });
  }
  return [...options.filter(option => option.scope === "cluster"), ...options.filter(option => option.scope === "personal")];
}

/** The id the picker shows for a choice: Auto's own id when nothing is chosen. */
export function pickerValue(routeId: string, options: readonly AskRouteOption[]): string {
  if (routeId === "") return options.find(option => option.kind === "auto")?.id ?? "auto";
  return routeId;
}
