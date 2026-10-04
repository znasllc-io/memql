import { idTail, sameId, type PipelineRow, type RunRow } from "./rows";
import { dayKey, dayLabel, runTitle } from "./words";

// Readings over a set of runs, pure and DOM-free (epic memql#5479): the Runs
// list's filter and its days, the newest attempt of a key, the source page's
// last run per branch, and what Latest upstream's checks say. Each is a
// function of rows, so the table is what gets asserted.

/** A run's outcome as a Refine facet. */
export type OutcomeKey = "passed" | "failed" | "running" | "queued" | "cancelled" | "refused";

export const OUTCOME_FACETS: readonly { value: OutcomeKey; label: string }[] = [
  { value: "passed", label: "Passed" },
  { value: "failed", label: "Failed" },
  { value: "running", label: "Running" },
  { value: "queued", label: "Queued" },
  { value: "cancelled", label: "Cancelled" },
  { value: "refused", label: "Refused" },
];

export function outcomeKey(run: RunRow): OutcomeKey {
  if (run.status === "queued") return "queued";
  if (run.status === "in_progress") return "running";
  switch (run.conclusion) {
    case "success":
      return "passed";
    case "cancelled":
      return "cancelled";
    case "refused":
      return "refused";
  }
  return "failed";
}

export interface RunFilter {
  /** A case-insensitive substring of the commit message, or the start of its SHA. */
  search: string;
  /** A pipeline's id: the source the Refine facet names. "" is every source. */
  pipelineId: string;
  /** A branch, exactly. "" is every branch. */
  branch: string;
  outcome: OutcomeKey | "";
}

export const NO_RUN_FILTER: RunFilter = { search: "", pipelineId: "", branch: "", outcome: "" };

export function filterIsNarrowing(f: RunFilter): boolean {
  return f.search.trim() !== "" || f.pipelineId !== "" || f.branch !== "" || f.outcome !== "";
}

/** Newest first: by when the run was opened, then by attempt, then by id so a tie never reshuffles. */
export function newestFirst(a: RunRow, b: RunRow): number {
  const at = Date.parse(a.queuedAt) || 0;
  const bt = Date.parse(b.queuedAt) || 0;
  if (at !== bt) return bt - at;
  if (a.attempt !== b.attempt) return b.attempt - a.attempt;
  return a.id < b.id ? 1 : a.id > b.id ? -1 : 0;
}

export function filterRuns(runs: readonly RunRow[], f: RunFilter): RunRow[] {
  const q = f.search.trim().toLowerCase();
  return runs
    .filter((r) => f.pipelineId === "" || sameId(r.pipelineId, f.pipelineId))
    .filter((r) => f.branch === "" || r.headBranch === f.branch)
    .filter((r) => f.outcome === "" || outcomeKey(r) === f.outcome)
    .filter((r) => q === "" || runTitle(r).toLowerCase().includes(q) || r.sha.toLowerCase().startsWith(q))
    .sort(newestFirst);
}

export interface RunDay {
  /** The local calendar day, `YYYY-MM-DD`. */
  key: string;
  /** Today, Yesterday, a weekday, a date. */
  label: string;
  runs: RunRow[];
}

/** Runs grouped by the local day they were opened, newest day first, each day newest first. */
export function groupByDay(runs: readonly RunRow[], now: Date): RunDay[] {
  const days: RunDay[] = [];
  const at = new Map<string, RunDay>();
  for (const run of [...runs].sort(newestFirst)) {
    const key = dayKey(run.queuedAt);
    let day = at.get(key);
    if (!day) {
      day = { key, label: dayLabel(run.queuedAt, now), runs: [] };
      at.set(key, day);
      days.push(day);
    }
    day.runs.push(run);
  }
  return days;
}

/** The branches the runs name, for the Branch facet: each once, sorted. */
export function branchesOf(runs: readonly RunRow[]): string[] {
  return [...new Set(runs.map((r) => r.headBranch).filter((b) => b !== ""))].sort();
}

/**
 * The newest attempt of `run`'s key that is NEWER than it, or null. A re-run
 * of an attempt that has a newer one is still legal -- it opens the key's next
 * attempt -- but not while the newest is unfinished: the engine refuses that
 * (ErrRunInProgress), so the page must not offer it.
 */
export function newerAttemptOf(run: RunRow, all: readonly RunRow[]): RunRow | null {
  let newest: RunRow | null = null;
  for (const other of all) {
    if (other.runKey !== run.runKey || !sameId(other.pipelineId, run.pipelineId) || other.attempt <= run.attempt) continue;
    if (newest === null || other.attempt > newest.attempt) newest = other;
  }
  return newest;
}

/** The runs of one pipeline. */
export function runsOfPipeline(runs: readonly RunRow[], pipelineId: string): RunRow[] {
  return runs.filter((r) => sameId(r.pipelineId, pipelineId));
}

/** A run's branch as the Checks rows group it: the merge queue's runs are one row. */
export function checksBranchOf(run: RunRow): string {
  if (run.event === "merge_group") return "merge queue";
  return run.headBranch;
}

/**
 * The newest run per branch -- the source page's Checks rows (D14). The
 * default branch first, then the rest newest first. A run naming no branch (a
 * release's tag) is left out: its row would have no name to read.
 */
export function lastRunPerBranch(runs: readonly RunRow[], defaultBranch: string): RunRow[] {
  const newest = new Map<string, RunRow>();
  for (const run of [...runs].sort(newestFirst)) {
    const branch = checksBranchOf(run);
    if (branch === "" || newest.has(branch)) continue;
    newest.set(branch, run);
  }
  return [...newest.values()].sort((a, b) => {
    const ad = checksBranchOf(a) === defaultBranch ? 0 : 1;
    const bd = checksBranchOf(b) === defaultBranch ? 0 : 1;
    return ad !== bd ? ad - bd : newestFirst(a, b);
  });
}

/** What Latest upstream's checks say, in words, or "" when nothing honest can be said. */
export type UpstreamChecks = "" | "checks passed, not yet deployed" | "checks passed" | "checks failed" | "checks running";

/**
 * Latest upstream's checks (D14: "checks passed, not yet deployed"): the
 * newest run of the pipeline at the latest upstream commit. A SHA is compared
 * by prefix either way, because the source row may carry a short one.
 */
export function upstreamChecks(runs: readonly RunRow[], pipeline: PipelineRow | null, latestSha: string, deployedSha: string): UpstreamChecks {
  if (pipeline === null || latestSha.trim() === "") return "";
  const latest = latestSha.trim().toLowerCase();
  const matches = (sha: string) => {
    const s = sha.trim().toLowerCase();
    return s !== "" && (s.startsWith(latest) || latest.startsWith(s));
  };
  const at = runsOfPipeline(runs, pipeline.id).filter((r) => matches(r.sha) && r.conclusion !== "refused").sort(newestFirst)[0];
  if (!at) return "";
  if (at.status !== "completed") return "checks running";
  if (at.conclusion === "success") {
    const deployed = deployedSha.trim().toLowerCase();
    const isDeployed = deployed !== "" && (deployed.startsWith(latest) || latest.startsWith(deployed));
    return isDeployed ? "checks passed" : "checks passed, not yet deployed";
  }
  if (at.conclusion === "failure") return "checks failed";
  return "";
}

/** The pipeline of a source, by package id: one per source (D12). */
export function pipelineForPackage(pipelines: readonly PipelineRow[], packageId: string): PipelineRow | null {
  return pipelines.find((p) => sameId(p.packageId, packageId)) ?? null;
}

/** A run by id, under either id spelling. */
export function runById(runs: readonly RunRow[], runId: string): RunRow | null {
  const tail = idTail(runId);
  return runs.find((r) => r.id === tail) ?? null;
}
