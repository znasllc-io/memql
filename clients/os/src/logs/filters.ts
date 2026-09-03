import type { LogsSearchArgs, LogsTailArgs } from "@znasllc-io/memql-sdk-core/client";

import type { LogLevel, LogRow } from "./rows";

// The facet state every logs surface holds, and how it becomes a call
// (epic memql#4895). Pure: the sections hold the state, this file turns it
// into `logsTail` / `logsSearch` arguments and the chips that describe it.

/** The level floor: everything at this level and above. */
export type LevelFloor = "all" | "info" | "warn" | "error";

export const LEVEL_FLOORS: readonly LevelFloor[] = ["all", "info", "warn", "error"];

export type WindowPreset = "15m" | "1h" | "6h" | "24h" | "7d" | "30d" | "custom";

export const WINDOW_PRESETS: readonly WindowPreset[] = ["15m", "1h", "6h", "24h", "7d", "30d", "custom"];

/** The windows a per-app Logs section offers. Shorter than Search's: the
 *  section follows a stream and a month-long stream is a search. */
export const TAIL_WINDOWS: readonly WindowPreset[] = ["15m", "1h", "6h", "24h"];

export interface LogFilters {
  levelFloor: LevelFloor;
  components: string[];
  nodes: string[];
  apps: string[];
  subject: string;
  subjectConcept: string;
  text: string;
  window: WindowPreset;
  /** `datetime-local` values, read only when `window` is `custom`. */
  from: string;
  to: string;
}

export const DEFAULT_FILTERS: LogFilters = {
  levelFloor: "all",
  components: [],
  nodes: [],
  apps: [],
  subject: "",
  subjectConcept: "",
  text: "",
  window: "1h",
  from: "",
  to: "",
};

/** What a surface is ABOUT before any facet: an app's slice (its id plus
 *  the concepts it owns), or nothing for the whole store. */
export interface LogScope {
  apps?: readonly string[];
  subjectConcepts?: readonly string[];
}

const WINDOW_MS: Record<Exclude<WindowPreset, "custom">, number> = {
  "15m": 15 * 60_000,
  "1h": 60 * 60_000,
  "6h": 6 * 60 * 60_000,
  "24h": 24 * 60 * 60_000,
  "7d": 7 * 24 * 60 * 60_000,
  "30d": 30 * 24 * 60 * 60_000,
};

/** The window as a name: "Last 15 minutes", "Last hour", "Custom window". */
export function windowLabel(preset: WindowPreset): string {
  switch (preset) {
    case "15m":
      return "Last 15 minutes";
    case "1h":
      return "Last hour";
    case "6h":
      return "Last 6 hours";
    case "24h":
      return "Last 24 hours";
    case "7d":
      return "Last 7 days";
    case "30d":
      return "Last 30 days";
    default:
      return "Custom window";
  }
}

/** The window as the tail of a sentence: "in the last hour". */
export function windowPhrase(preset: WindowPreset): string {
  if (preset === "custom") return "in this window";
  return `in the ${windowLabel(preset).toLowerCase()}`;
}

/** The choice pill's short form. */
export function windowShort(preset: WindowPreset): string {
  switch (preset) {
    case "15m":
      return "15 min";
    case "1h":
      return "1 h";
    case "6h":
      return "6 h";
    case "24h":
      return "24 h";
    case "7d":
      return "7 d";
    case "30d":
      return "30 d";
    default:
      return "Custom";
  }
}

/**
 * The levels a floor admits, or undefined for no constraint -- an absent
 * `levels` argument is "every level", and sending all four would put a
 * predicate on the wire that changes nothing.
 */
export function levelsFor(floor: LevelFloor): LogLevel[] | undefined {
  switch (floor) {
    case "info":
      return ["info", "warn", "error"];
    case "warn":
      return ["warn", "error"];
    case "error":
      return ["error"];
    default:
      return undefined;
  }
}

/** A `datetime-local` value as an instant, or null when it does not parse.
 *  The control hands back local wall time with no zone; `Date` reads it as
 *  local, which is what the person meant. */
export function parseLocal(value: string): Date | null {
  const trimmed = value.trim();
  if (trimmed === "") return null;
  const parsed = new Date(trimmed);
  return Number.isNaN(parsed.getTime()) ? null : parsed;
}

/**
 * The half-open [start, end) a window names, against `now`. Null for a
 * custom window that is not yet a window -- a blank or unparseable end, or
 * an end before its start.
 */
export function windowBounds(
  filters: Pick<LogFilters, "window" | "from" | "to">,
  now: Date,
): { start: Date; end: Date } | null {
  if (filters.window !== "custom") {
    return { start: new Date(now.getTime() - WINDOW_MS[filters.window]), end: now };
  }
  const start = parseLocal(filters.from);
  const end = parseLocal(filters.to);
  if (start === null || end === null || end.getTime() <= start.getTime()) return null;
  return { start, end };
}

function facets(scope: LogScope, filters: LogFilters): LogsTailArgs {
  const args: LogsTailArgs = {};
  // The scope's two halves are ORed by the engine into one predicate: an
  // app's slice is "tagged with me OR about my things". A facet naming apps
  // narrows the same predicate, which is what a person picking an app in the
  // Logs app means.
  const apps = filters.apps.length > 0 ? filters.apps : [...(scope.apps ?? [])];
  if (apps.length > 0) args.apps = apps;
  if ((scope.subjectConcepts?.length ?? 0) > 0) args.subjectConcepts = [...(scope.subjectConcepts ?? [])];
  const levels = levelsFor(filters.levelFloor);
  if (levels !== undefined) args.levels = levels;
  if (filters.components.length > 0) args.components = [...filters.components];
  if (filters.nodes.length > 0) args.nodes = [...filters.nodes];
  const subject = filters.subject.trim();
  if (subject !== "") {
    args.subject = subject;
    const concept = filters.subjectConcept.trim();
    if (concept !== "") args.subjectConcept = concept;
  }
  const text = filters.text.trim();
  if (text !== "") args.text = text;
  return args;
}

/** The tail's arguments: the facets and no cursor. The hook adds the cursor. */
export function toTailArgs(scope: LogScope, filters: LogFilters): LogsTailArgs {
  return facets(scope, filters);
}

/** The search's arguments, or null when the window is not yet a window. */
export function toSearchArgs(scope: LogScope, filters: LogFilters, now: Date): LogsSearchArgs | null {
  const bounds = windowBounds(filters, now);
  if (bounds === null) return null;
  return {
    windowStart: bounds.start.toISOString(),
    windowEnd: bounds.end.toISOString(),
    ...facets(scope, filters),
  };
}

/**
 * The rows inside the window, for a tail. `logsTail` has no window argument
 * -- a baseline is the newest lines whatever their age -- so the window is a
 * fold over what arrived, re-read against the clock so a line ages out.
 */
export function withinWindow(rows: readonly LogRow[], preset: WindowPreset, now: Date): LogRow[] {
  if (preset === "custom") return [...rows];
  const floor = now.getTime() - WINDOW_MS[preset];
  return rows.filter((row) => row.at.getTime() >= floor);
}

/** Whether any facet beyond the window narrows the reading -- the line
 *  between "nothing recorded" and "no lines match". */
export function isNarrowed(filters: LogFilters): boolean {
  return (
    filters.levelFloor !== "all" ||
    filters.components.length > 0 ||
    filters.nodes.length > 0 ||
    filters.apps.length > 0 ||
    filters.subject.trim() !== "" ||
    filters.text.trim() !== ""
  );
}

export function levelFloorLabel(floor: LevelFloor): string {
  switch (floor) {
    case "info":
      return "Info and above";
    case "warn":
      return "Warnings and above";
    case "error":
      return "Errors only";
    default:
      return "All levels";
  }
}

/** One active constraint, and the patch that clears it. The sections turn
 *  these into removable Refine chips (DESIGN.md rule 2). The search text is
 *  not one: the Refine affordance already carries it. */
export interface Constraint {
  id: string;
  label: string;
  clear: Partial<LogFilters>;
}

export function constraintsOf(filters: LogFilters): Constraint[] {
  const out: Constraint[] = [];
  if (filters.levelFloor !== "all") {
    out.push({ id: "level", label: levelFloorLabel(filters.levelFloor), clear: { levelFloor: "all" } });
  }
  for (const component of filters.components) {
    out.push({
      id: `component:${component}`,
      label: component,
      clear: { components: filters.components.filter((c) => c !== component) },
    });
  }
  for (const node of filters.nodes) {
    out.push({ id: `node:${node}`, label: node, clear: { nodes: filters.nodes.filter((n) => n !== node) } });
  }
  for (const app of filters.apps) {
    out.push({ id: `app:${app}`, label: `app ${app}`, clear: { apps: filters.apps.filter((a) => a !== app) } });
  }
  if (filters.subject.trim() !== "") {
    out.push({ id: "subject", label: `subject ${filters.subject.trim()}`, clear: { subject: "", subjectConcept: "" } });
  }
  return out;
}

/** "1 line" / "212 lines". Tabular, and never blank. */
export function lineCount(n: number): string {
  return `${n.toLocaleString()} ${n === 1 ? "line" : "lines"}`;
}

/**
 * The one intent payload a logs surface acts on: `{ subject, subjectConcept }`
 * (spec H). Null for any other payload -- that is another surface's
 * instruction and is left standing for it, never consumed here.
 */
export function subjectIntentOf(payload: Record<string, unknown>): { subject: string; subjectConcept: string } | null {
  const subject = payload.subject;
  if (typeof subject !== "string" || subject.trim() === "") return null;
  const concept = payload.subjectConcept;
  return { subject: subject.trim(), subjectConcept: typeof concept === "string" ? concept.trim() : "" };
}
