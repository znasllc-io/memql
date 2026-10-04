import type { Compute, Delivery, RunRow, StageRow, StageStatus, StepRow } from "./rows";

// THE ONE VOCABULARY for pipelines in this app (epic memql#5479), the way
// `../words.ts` is the one vocabulary for a deployable. The Runs list, the run
// page's stops and bar, the source page's Checks and its Pipeline fact all read
// their words from here, so the word a person learns on one surface is the word
// they meet on the next -- and it is the word the check run on GitHub uses for
// the same state (component/pipelines/summary.go): Passed, Failed, Cancelled,
// Running, Waiting, Not run, Skipped. A run that says "Failed at tests" here
// and "Failed at tests: tests.unit" on GitHub is one reading in two places.
//
// NO CODE IS A WORD. A `pipeline_*` code never reaches a person as text; a
// refusal renders through `ProblemNotice` with `packages/refusals.ts` copy, and
// the short phrases below are for a row too narrow for a sentence.

export type RunTone = "accent" | "warn" | "muted";

/** The mark a run, a stage or a step draws: the kit rail's states. */
export type RunMark = "done" | "stopped" | "current" | "ahead" | "skipped" | "pending";

export interface Outcome {
  /** One word: Passed, Failed, Running, Queued, Cancelling, Cancelled, Refused. */
  word: string;
  /** Where and how long, in words: "at tests, 7m 40s". Empty when nothing is known. */
  detail: string;
  tone: RunTone;
  mark: RunMark;
}

/** A duration the way the check run spells one: 52s, 7m 40s, 1h 3m. "" for unknown. */
export function durationWords(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  const seconds = Math.max(1, Math.round(ms / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    const rest = seconds % 60;
    return rest === 0 ? `${minutes}m` : `${minutes}m ${rest}s`;
  }
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  return rest === 0 ? `${hours}h` : `${hours}h ${rest}m`;
}

/** Comma-joined, blanks dropped: the house separator for a run's facts. */
export function joinWords(...parts: readonly string[]): string {
  return parts.filter((p) => p.trim() !== "").join(", ");
}

/**
 * The short phrase a refusal reads as on a row. The page renders the full
 * copy; a row has room for this. A manifest's refusals share one phrase,
 * because on a row the fact is "the pipeline block is wrong" and the page says
 * which part.
 */
const REFUSAL_PHRASES: Record<string, string> = {
  pipeline_fork_refused: "pull request from a fork",
  pipeline_disconnected: "pipeline disconnected",
  pipeline_not_declared: "no pipeline block",
  pipeline_secret_not_allowed: "a secret is not allowed",
  pipeline_fleet_not_consented: "a step needs the fleet",
  package_manifest_invalid: "memql-package.yaml is not valid",
  package_manifest_missing: "no memql-package.yaml",
  source_too_large: "the repository is too large",
};

const MANIFEST_REFUSALS: ReadonlySet<string> = new Set([
  "pipeline_stage_invalid", "pipeline_step_invalid", "pipeline_select_invalid", "pipeline_select_missing",
  "pipeline_event_unknown", "pipeline_bucket_unknown", "pipeline_service_unknown", "pipeline_need_unknown",
  "pipeline_secret_invalid",
]);

export function refusalPhrase(code: string): string {
  if (code === "") return "";
  if (REFUSAL_PHRASES[code] !== undefined) return REFUSAL_PHRASES[code]!;
  if (MANIFEST_REFUSALS.has(code)) return "the pipeline block is not valid";
  return "the repository could not be read";
}

/** The stage a run is at: the first one running, else the first still waiting after one that ran. */
export function currentStage(stages: readonly StageRow[]): StageRow | null {
  return stages.find((s) => s.status === "running") ?? stages.find((s) => s.status === "waiting") ?? null;
}

/**
 * How a run ended, or where it is -- the one reading the Runs list's row, the
 * run page's bar and the source page's Checks rows all show.
 *
 * A REFUSED FORK RUN AND A DEPLOY-PLUS-NOTIFY RUN READ AS OUTCOMES IN WORDS
 * (issue memql#5499): "Refused, pull request from a fork" rather than a code,
 * and a push that ran every stage but could not notify says "notify skipped"
 * beside Passed -- a stage named in the manifest's own words -- so Passed never
 * implies a message went out that did not.
 */
export function runOutcome(run: RunRow): Outcome {
  const took = durationWords(run.durationMs);
  if (run.status === "queued") {
    return run.cancelRequested
      ? { word: "Cancelling", detail: "", tone: "muted", mark: "current" }
      : { word: "Queued", detail: "", tone: "muted", mark: "ahead" };
  }
  if (run.status === "in_progress") {
    const at = currentStage(run.stages);
    if (run.cancelRequested) return { word: "Cancelling", detail: at ? `at ${at.name}` : "", tone: "muted", mark: "current" };
    return { word: "Running", detail: at ? at.name : "", tone: "accent", mark: "current" };
  }
  switch (run.conclusion) {
    case "success": {
      const ran = run.stages.filter((s) => s.status === "passed").length;
      const skipped = run.stages.filter((s) => s.status === "skipped").map((s) => `${s.name} skipped`);
      const stages = run.stages.length === 0 ? "nothing to run" : `${ran} stage${ran === 1 ? "" : "s"}`;
      return { word: "Passed", detail: joinWords(stages, ...skipped, took), tone: "accent", mark: "done" };
    }
    case "cancelled": {
      const at = run.stages.find((s) => s.status === "cancelled") ?? currentStage(run.stages);
      return { word: "Cancelled", detail: joinWords(at ? `at ${at.name}` : "", took), tone: "muted", mark: "skipped" };
    }
    case "refused":
      return { word: "Refused", detail: refusalPhrase(run.refusalCode), tone: "warn", mark: "stopped" };
    case "failure": {
      if (run.refusalCode !== "") {
        return { word: "Failed", detail: joinWords(refusalPhrase(run.refusalCode), took), tone: "warn", mark: "stopped" };
      }
      const at = run.stages.find((s) => s.status === "failed");
      return { word: "Failed", detail: joinWords(at ? `at ${at.name}` : "", took), tone: "warn", mark: "stopped" };
    }
  }
  return { word: "Finished", detail: took, tone: "muted", mark: "done" };
}

/** What opened the run, in words. */
export function triggerWords(run: RunRow): string {
  switch (run.event) {
    case "pull_request":
      return run.pullRequest > 0 ? `Pull request #${run.pullRequest}` : "Pull request";
    case "merge_group":
      return "Merge queue";
    case "release":
      return run.version !== "" && run.version !== run.sha ? `Release ${run.version}` : "Release";
  }
  return "Push";
}

/** Which attempt this is, and what it re-ran: "" for a first attempt. */
export function attemptWords(run: RunRow): string {
  if (run.attempt <= 1) return "";
  return run.rerunFailedOnly ? `Attempt ${run.attempt}, failed steps only` : `Attempt ${run.attempt}`;
}

export function modeWord(mode: RunRow["mode"]): string {
  return mode === "full" ? "Full suite" : "Affected";
}

export function shortSha(sha: string): string {
  return sha.trim().slice(0, 7);
}

/** A commit message's first line, or the commit when GitHub gave none. */
export function runTitle(run: RunRow): string {
  const first = run.title.split("\n")[0]?.trim() ?? "";
  return first !== "" ? first : `Commit ${shortSha(run.sha)}`;
}

/** The branch a run is about; a merge queue's and a release's are named by their event. */
export function branchWords(run: RunRow): string {
  if (run.headBranch !== "") return run.headBranch;
  return run.event === "merge_group" ? "merge queue" : "";
}

const STAGE_WORDS: Record<StageStatus, string> = {
  waiting: "Waiting",
  running: "Running",
  passed: "Passed",
  failed: "Failed",
  cancelled: "Cancelled",
  skipped: "Skipped",
  blocked: "Not run",
};

export function stageWord(status: StageStatus): string {
  return STAGE_WORDS[status];
}

/** A step's state in one word. A step a failed-only re-run carried over passed in an earlier attempt. */
export function stepWord(step: StepRow): string {
  switch (step.status) {
    case "done":
      return "Passed";
    case "failed":
      return step.refused ? "Refused" : "Failed";
    case "cancelled":
      return "Cancelled";
    case "running":
      return "Running";
    case "skipped":
      if (step.errorCode === "pipeline_passed_earlier") return "Passed earlier";
      if (step.errorCode === "pipeline_stage_blocked") return "Not run";
      return "Skipped";
  }
  return "Waiting";
}

export function stepMark(step: StepRow): RunMark {
  switch (step.status) {
    case "done":
      return "done";
    case "failed":
      return "stopped";
    case "cancelled":
      return "skipped";
    case "running":
      return "current";
    case "skipped":
      return step.errorCode === "pipeline_passed_earlier" ? "done" : "skipped";
  }
  return "ahead";
}

/** "go-tests 2 of 4" for a shard of four; the name alone otherwise. */
export function stepLabel(step: StepRow, shardsOf: number): string {
  return step.shard > 0 && shardsOf > 1 ? `${step.name} ${step.shard} of ${shardsOf}` : step.name;
}

/** Where a step ran: "Cluster", "Fleet: studio-mac", or "" when no runner answered. */
export function whereWords(step: StepRow, machineName?: (workerId: string) => string): string {
  if (step.where.surface === "cluster") return "Cluster";
  if (step.where.surface === "fleet") {
    const name = step.where.workerId !== "" && machineName ? machineName(step.where.workerId) : "";
    return name !== "" ? `Fleet: ${name}` : "Fleet";
  }
  return "";
}

export function deliveryWords(delivery: Delivery): string {
  return delivery === "poll" ? "This cluster asks GitHub every minute" : "GitHub sends each push and pull request";
}

/** The short form the Pipeline fact line reads. */
export function deliveryShort(delivery: Delivery): string {
  return delivery === "poll" ? "polling" : "webhook";
}

export function computeWords(compute: Compute): string {
  return compute === "cluster_and_fleet" ? "Cluster and your fleet" : "Cluster only";
}

/** The short form the Pipeline fact line reads. */
export function computeShort(compute: Compute): string {
  return compute === "cluster_and_fleet" ? "cluster and fleet" : "cluster";
}

// ---------------------------------------------------------------------------
// Time
// ---------------------------------------------------------------------------

function startOfDay(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
}

const DAY_MS = 86_400_000;

/**
 * The day a run belongs to, as the Runs list's group heading says it: Today,
 * Yesterday, a weekday within the week, a date within the year, a date with
 * its year otherwise. Local time, because "today" is the reader's.
 */
export function dayLabel(iso: string, now: Date): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "Earlier";
  const days = Math.round((startOfDay(now) - startOfDay(at)) / DAY_MS);
  if (days <= 0) return "Today";
  if (days === 1) return "Yesterday";
  if (days < 7) return at.toLocaleDateString(undefined, { weekday: "long", month: "long", day: "numeric" });
  if (at.getFullYear() === now.getFullYear()) return at.toLocaleDateString(undefined, { month: "long", day: "numeric" });
  return at.toLocaleDateString(undefined, { year: "numeric", month: "long", day: "numeric" });
}

/** The key a run's day groups under: its local calendar day. */
export function dayKey(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  return `${at.getFullYear()}-${String(at.getMonth() + 1).padStart(2, "0")}-${String(at.getDate()).padStart(2, "0")}`;
}

/** The time of day a run was opened, for a row already under its day's heading. */
export function timeOfDay(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  return at.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}
