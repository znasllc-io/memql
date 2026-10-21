// Routes as a person picks them in Ask: Auto, Local, Apps and their own
// (design record 2026-10-18-routes-design.md). A route decides which source
// serves each JOB, and a job is a level named for what it does.
//
// ===========================================================================
// TWO THINGS ARE CALLED "ROUTE" IN THE ENGINE, AND ONE ON THE SCREEN
// ===========================================================================
// The engine's ordered lists of sources (`dsl/policies`) are what the rules
// name; on the screen they are CHAINS, under Rules. A route here is the thing
// a person chooses for their own messages: it holds one ordered chain per
// job. Nothing on this page says "chain" -- a job lists its sources, in the
// order they are tried, and that is the whole idea a person needs.
//
// ===========================================================================
// SEARCH AND MEMORY IS THE CLUSTER'S
// ===========================================================================
// Every route serves embeddings from the cluster's bound embedder. The index
// holds vectors from exactly one model, and another embedder answers in a
// different vector space, so a route cannot choose one: the row is read-only
// everywhere, and offers the way to add one when the cluster has none.
//
// THE READS ARE FEATURE-DETECTED. A cluster without the routes builtins still
// opens this page: the three built-in routes are drawn from their shipped
// definitions, read-only, and say that saving needs a newer cluster.

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";
import { refusalWords } from "./vocabulary";

export type JobId = "fast" | "strong" | "reasoning" | "embeddings";
export type EditableJob = Exclude<JobId, "embeddings">;

/** Every job, in the order a page draws them. */
export const JOBS: readonly JobId[] = ["fast", "strong", "reasoning", "embeddings"];
/** The jobs a route chooses sources for; Search and memory is the cluster's. */
export const EDITABLE_JOBS: readonly EditableJob[] = ["fast", "strong", "reasoning"];

export const JOB_TITLES: Record<JobId, string> = {
  fast: "Quick tasks",
  strong: "Main work",
  reasoning: "Deep reasoning",
  embeddings: "Search and memory",
};

/** What each job is for, in one short line under its name. */
export const JOB_MEANING: Record<JobId, string> = {
  fast: "Triage, summaries and suggestions",
  strong: "Replies and most real work",
  reasoning: "Planning and hard problems",
  embeddings: "Finding things in your Library and memory",
};

/** A route holds at most this many sources per job (the engine's ceiling). */
export const MAX_JOB_SOURCES = 4;

export type RouteKind = "auto" | "local" | "apps" | "custom";

export interface RouteJob {
  job: JobId;
  /** Chain entries, tried in order. Embeddings: ["embedder:active"]. */
  sources: string[];
  editable: boolean;
  /** The engine's label for the first source that can serve now, "" when none. */
  servesNow: string;
  ready: boolean;
}

export interface RouteMode {
  /** "auto" | "local" | "apps" | a personal route's id. */
  id: string;
  name: string;
  kind: RouteKind;
  scope: "cluster" | "personal";
  description: string;
  ready: boolean;
  /** Why it is not ready, in a few words; "" when ready. */
  reason: string;
  /** A built-in with a cluster override. */
  customized: boolean;
  /**
   * The four jobs. For Auto, which decides each call itself, these are its
   * current picks when the engine reports them, read-only, and may be empty.
   */
  jobs: RouteJob[];
}

export type JobSources = Record<EditableJob, string[]>;

/** The shipped definitions, for "Start from" and for a cluster that predates routes. */
export const SHIPPED_JOBS: Record<"local" | "apps", JobSources> = {
  local: { fast: ["fleet:fastest"], strong: ["fleet:strongest"], reasoning: ["fleet:strongest"] },
  apps: { fast: ["app:*"], strong: ["app:*"], reasoning: ["app:*"] },
};

export const EMPTY_JOBS: JobSources = { fast: [], strong: [], reasoning: [] };

/** The built-ins in the order a person meets them. */
export const BUILT_IN_ORDER: readonly string[] = ["auto", "local", "apps"];

/** One line per built-in, used when the engine's own description is absent. */
export const BUILT_IN_DESCRIPTIONS: Record<string, string> = {
  auto: "Picks the best balance of speed, cost and quality for every call",
  local: "Local models for everything",
  apps: "Claude Code or Codex, with your embedding model",
};

const KINDS: readonly RouteKind[] = ["auto", "local", "apps", "custom"];

function str(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((m): m is string => typeof m === "string" && m.trim() !== "") : [];
}

function jobFrom(raw: unknown): RouteJob | null {
  if (raw === null || typeof raw !== "object") return null;
  const row = raw as Row;
  const job = str(row, "job") as JobId;
  if (!JOBS.includes(job)) return null;
  return {
    job,
    sources: job === "embeddings" ? ["embedder:active"] : strings(row["sources"]),
    editable: job !== "embeddings" && row["editable"] !== false,
    servesNow: str(row, "servesNow"),
    ready: row["ready"] === true,
  };
}

/** One route as the engine answers it, or null for a row that is not one. */
export function routeModeFrom(raw: Row): RouteMode | null {
  const row = flatten(raw);
  const id = str(row, "id").trim();
  if (id === "") return null;
  const declared = str(row, "kind") as RouteKind;
  const kind: RouteKind = KINDS.includes(declared) ? declared : id === "auto" || id === "local" || id === "apps" ? (id as RouteKind) : "custom";
  const jobs = (Array.isArray(row["jobs"]) ? (row["jobs"] as unknown[]) : []).map(jobFrom).filter((j): j is RouteJob => j !== null);
  const scope = str(row, "scope") === "personal" || (kind === "custom" && str(row, "scope") !== "cluster") ? "personal" : "cluster";
  return {
    id,
    name: str(row, "name").trim() || (kind === "custom" ? id : kind.charAt(0).toUpperCase() + kind.slice(1)),
    kind,
    scope,
    description: str(row, "description").trim() || BUILT_IN_DESCRIPTIONS[kind] || "",
    ready: row["ready"] === true,
    reason: str(row, "reason"),
    customized: row["customized"] === true,
    // AUTO'S JOBS ARE ITS PICKS, read-only and possibly absent: what it would
    // serve each job with right now, when the engine says.
    jobs: kind === "auto" ? jobs.map((j) => ({ ...j, editable: false })) : completeJobs(jobs),
  };
}

/** The four jobs in order, with any job the engine left out drawn empty. */
function completeJobs(jobs: readonly RouteJob[]): RouteJob[] {
  return JOBS.map(
    (job) =>
      jobs.find((j) => j.job === job) ?? {
        job,
        sources: job === "embeddings" ? ["embedder:active"] : [],
        editable: job !== "embeddings",
        servesNow: "",
        ready: false,
      },
  );
}

function revisionOf(row: Row): number {
  const v = flatten(row)["revision"];
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

/**
 * The routes a `routesForActor` reply carries.
 *
 * TWO WIRE SHAPES ARE TAKEN, on purpose: the builtin answers one object
 * (`{ routes, revision }`), which arrives as ONE row carrying a `routes` list,
 * and a reply that lists the routes as rows (each with the revision) reads the
 * same. Either way the order is the page's, not the wire's.
 */
export function routeModesFrom(rows: readonly Row[]): { routes: RouteMode[]; revision: number } {
  const first = rows[0] === undefined ? null : flatten(rows[0]);
  if (first !== null && Array.isArray(first["routes"])) {
    const routes = (first["routes"] as unknown[])
      .filter((r): r is Row => r !== null && typeof r === "object")
      .map(routeModeFrom)
      .filter((r): r is RouteMode => r !== null);
    return { routes: orderRouteModes(routes), revision: revisionOf(first) };
  }
  const routes = rows.map(routeModeFrom).filter((r): r is RouteMode => r !== null);
  return { routes: orderRouteModes(routes), revision: rows.reduce((max, r) => Math.max(max, revisionOf(r)), 0) };
}

/** Auto, Local, Apps, then the person's own routes by name. */
export function orderRouteModes(routes: readonly RouteMode[]): RouteMode[] {
  const rank = (r: RouteMode) => (BUILT_IN_ORDER.includes(r.id) && r.scope === "cluster" ? BUILT_IN_ORDER.indexOf(r.id) : 99);
  return [...routes].sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name));
}

/** The built-ins as shipped, for a cluster whose engine has no routes builtins yet. */
export function shippedRouteModes(): RouteMode[] {
  const shipped = (id: "local" | "apps"): RouteMode => ({
    id,
    name: id === "local" ? "Local" : "Apps",
    kind: id,
    scope: "cluster",
    description: BUILT_IN_DESCRIPTIONS[id]!,
    ready: false,
    reason: "",
    customized: false,
    jobs: completeJobs(EDITABLE_JOBS.map((job) => ({ job, sources: [...SHIPPED_JOBS[id][job]], editable: true, servesNow: "", ready: false }))),
  });
  return [
    { id: "auto", name: "Auto", kind: "auto", scope: "cluster", description: BUILT_IN_DESCRIPTIONS.auto!, ready: false, reason: "", customized: false, jobs: [] },
    shipped("local"),
    shipped("apps"),
  ];
}

/** A route's editable jobs as a draft. */
export function jobSourcesOf(route: Pick<RouteMode, "jobs">): JobSources {
  const of = (job: EditableJob) => [...(route.jobs.find((j) => j.job === job)?.sources ?? [])];
  return { fast: of("fast"), strong: of("strong"), reasoning: of("reasoning") };
}

export function sameJobSources(a: JobSources, b: JobSources): boolean {
  return EDITABLE_JOBS.every((job) => a[job].length === b[job].length && a[job].every((entry, i) => entry === b[job][i]));
}

/** Every editable job has at least one source: the engine refuses an empty job. */
export function jobsComplete(jobs: JobSources): boolean {
  return EDITABLE_JOBS.every((job) => jobs[job].length > 0);
}

/** Who may change a route: the person's own, or a built-in for an owner or developer. */
export function canEditRoute(route: Pick<RouteMode, "kind" | "scope">, composes: boolean): boolean {
  if (route.kind === "auto") return false;
  if (route.scope === "personal") return true;
  return composes;
}

/** A refusal from a routes write, in the screen's words. */
export function routeRefusal(err: unknown): { stale: boolean; text: string } {
  const message = err instanceof Error ? err.message : String(err);
  if (/route_not_ready/i.test(message)) return { stale: false, text: message.replace(/^.*route_not_ready:?\s*/i, "") || "That route is not ready." };
  return refusalWords(message);
}

// ---------------------------------------------------------------------------
// Auto's balance and the person's budget
// ---------------------------------------------------------------------------

export interface Balance {
  speed: number;
  cost: number;
  quality: number;
}

export interface AutoBalance extends Balance {
  recommended: Balance;
  customized: boolean;
  revision: number;
}

export const RECOMMENDED_BALANCE: Balance = { speed: 50, cost: 50, quality: 50 };

function clampWeight(v: unknown, fallback: number): number {
  const n = typeof v === "number" ? v : typeof v === "string" && v.trim() !== "" ? Number(v) : NaN;
  if (!Number.isFinite(n)) return fallback;
  return Math.max(0, Math.min(100, Math.round(n)));
}

function balanceFrom(raw: unknown, fallback: Balance): Balance {
  const row = raw !== null && typeof raw === "object" ? (raw as Row) : {};
  return {
    speed: clampWeight(row["speed"], fallback.speed),
    cost: clampWeight(row["cost"], fallback.cost),
    quality: clampWeight(row["quality"], fallback.quality),
  };
}

export function autoBalanceFrom(rows: readonly Row[]): AutoBalance | null {
  if (rows[0] === undefined) return null;
  const row = flatten(rows[0]);
  const recommended = balanceFrom(row["recommended"], RECOMMENDED_BALANCE);
  const own = balanceFrom(row, recommended);
  return { ...own, recommended, customized: row["customized"] === true, revision: revisionOf(row) };
}

export function sameBalance(a: Balance, b: Balance): boolean {
  return a.speed === b.speed && a.cost === b.cost && a.quality === b.quality;
}

export interface Budget {
  /** Null when the person has no budget. */
  monthlyLimitUSD: number | null;
  spentThisMonthUSD: number;
  resetsAt: string;
  /** Where the spend stands: "near" from 80% of the budget, "over" from all of it; "" when not said. */
  state: "none" | "under" | "near" | "over" | "";
}

const BUDGET_STATES: readonly Budget["state"][] = ["none", "under", "near", "over"];

export function budgetFrom(rows: readonly Row[]): Budget | null {
  if (rows[0] === undefined) return null;
  const row = flatten(rows[0]);
  const limit = row["monthlyLimitUSD"];
  const spent = row["spentThisMonthUSD"];
  return {
    monthlyLimitUSD: typeof limit === "number" && Number.isFinite(limit) && limit > 0 ? limit : null,
    spentThisMonthUSD: typeof spent === "number" && Number.isFinite(spent) ? Math.max(0, spent) : 0,
    resetsAt: str(row, "resetsAt"),
    state: BUDGET_STATES.find((s) => s === str(row, "state")) ?? "",
  };
}

/** USD for a person: "$12.40", "$1,200". */
export function usd(amount: number): string {
  const whole = Number.isInteger(amount) && amount >= 100;
  return new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", minimumFractionDigits: whole ? 0 : 2, maximumFractionDigits: whole ? 0 : 2 }).format(amount);
}

// ---------------------------------------------------------------------------
// A route page's bar (DESIGN.md rule 12): the state in words, then only the
// acts legal from that state -- absent, never disabled.
// ---------------------------------------------------------------------------

export type RouteModeAct = "discard" | "save" | "restore" | "delete" | "keep" | "confirmRestore" | "confirmDelete";

export interface RouteModeBar {
  state: string;
  detail: string;
  tone: "live" | "paused" | "busy" | "none";
  acts: RouteModeAct[];
}

export function routeModeBar(input: {
  route: Pick<RouteMode, "scope" | "customized">;
  editable: boolean;
  /** Why a route the reader could change cannot be saved here; absent, it is who can. */
  readOnlyDetail?: string;
  dirty: boolean;
  complete: boolean;
  busy: "" | "saving" | "restoring" | "deleting";
  asking: "" | "restore" | "delete";
}): RouteModeBar {
  const personal = input.route.scope === "personal";
  const scope = personal ? "Only you use this route" : "Applies to everyone on this cluster";
  if (!input.editable) {
    return { state: input.route.customized ? "Changed from shipped" : "Shipped", detail: input.readOnlyDetail ?? "An owner or developer can change it", tone: "none", acts: [] };
  }
  if (input.busy !== "") {
    return { state: input.busy === "saving" ? "Saving" : input.busy === "restoring" ? "Restoring" : "Deleting", detail: "", tone: "busy", acts: [] };
  }
  if (input.asking === "restore" && !personal && input.route.customized) {
    return { state: "Restore the shipped sources?", detail: "This cluster's changes to the route are removed", tone: "paused", acts: ["keep", "confirmRestore"] };
  }
  if (input.asking === "delete" && personal) {
    return { state: "Delete this route?", detail: "It is removed for good", tone: "paused", acts: ["keep", "confirmDelete"] };
  }
  if (input.dirty) {
    return {
      state: "Unsaved changes",
      detail: input.complete ? scope : "Every job needs a source",
      tone: "paused",
      acts: input.complete ? ["discard", "save"] : ["discard"],
    };
  }
  if (personal) return { state: "Saved", detail: scope, tone: "live", acts: ["delete"] };
  if (input.route.customized) return { state: "Changed from shipped", detail: scope, tone: "live", acts: ["restore"] };
  return { state: "Shipped", detail: scope, tone: "live", acts: [] };
}

/** A personal route's name: 1 to 40 characters, not one of yours already. */
export function routeNameProblem(name: string, taken: readonly string[], own = ""): string {
  const trimmed = name.trim();
  if (trimmed === "") return "Name the route";
  if (trimmed.length > 40) return "Forty characters at most";
  const lower = trimmed.toLowerCase();
  if (lower !== own.trim().toLowerCase() && taken.some((t) => t.trim().toLowerCase() === lower)) return "You have a route with that name";
  if (["auto", "local", "apps"].includes(lower)) return "That name is a built-in route";
  return "";
}

/** The sources Auto ranks for each job when the engine does not report its picks: the shipped general chains. */
export const AUTO_CANDIDATES: JobSources = {
  fast: ["fleet:fastest", "app:*", "federation:cheapest"],
  strong: ["fleet:strongest", "app:*", "federation:cheapest"],
  reasoning: ["fleet:strongest", "app:*", "federation:strongest"],
};

/**
 * The cluster's bound embedding model, as a page reads it. Four answers, not
 * two: "none bound" is a fact a page acts on (pull one), and "could not read
 * it" is not that fact -- it must never render as "No embedding model".
 */
export type EmbedderReading = { state: "loading" } | { state: "none" } | { state: "bound"; name: string } | { state: "unread"; error: string };

export function embedderReading(v: { value: string | null; loading: boolean; error: string; available: boolean }): EmbedderReading {
  if (!v.available) return { state: "unread", error: "" };
  if (v.error !== "") return { state: "unread", error: v.error };
  if (v.value === null) return v.loading ? { state: "loading" } : { state: "unread", error: "" };
  return v.value === "" ? { state: "none" } : { state: "bound", name: v.value };
}
