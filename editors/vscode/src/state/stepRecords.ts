// Folding executor events into step records: the part the install wizard and
// the uninstall run do identically.
//
// `AddClusterState` and `UninstallRunState` are separate machines on purpose --
// a failed install step and a failed removal ask for different next acts -- but
// what a step RECORD is, and how an event changes one, is the same on both
// sides: the plan arrives, steps start, report phases, write lines and settle.
// Two copies of that fold drift the way the old ones had begun to (one kept a
// remedy, the other did not need to), so the mechanical part lives here and
// each machine keeps only its own decisions about screens and failures.
//
// No `vscode` import (cmd/memql-lsp/vscodeimportrule_test.go).

import type { StepOutcome } from "../install/executor.js";
import type { StepProgress, StepState } from "./addCluster.js";

/** A step the plan names, as `runStarted` announces it. */
export interface PlannedStep {
  id: string;
  label: string;
  description: string;
}

export function freshStep(id: string, label: string, description: string): StepProgress {
  return {
    id,
    label,
    description,
    state: "pending",
    reason: "",
    exitCode: null,
    log: "",
    guided: false,
    remedy: "",
  };
}

/**
 * The records for a new attempt: exactly the plan, in plan order, every step
 * pending.
 *
 * EVERY STEP STARTS AGAIN, including the ones that passed last time. A retry
 * re-runs the whole graph, and a step that passed before will re-report within
 * a second as skipped -- but until it does, counting it as done would put the
 * bar ahead of the run, and the moment it did report the bar would have to
 * move backwards. What the step came to last time is kept as `previousState`,
 * for a screen that wants to show it.
 *
 * EXACTLY THE PLAN. A panel that runs a second graph through the same machine
 * (the deployment page rebuilds, then updates) must not carry the first graph's
 * steps into the second's total, where they would sit pending forever.
 *
 * `guided` survives: it is the operator's choice about a step, not a result.
 */
export function recordsForAttempt(previous: readonly StepProgress[], plan: readonly PlannedStep[]): StepProgress[] {
  return plan.map((planned) => {
    const fresh = freshStep(planned.id, planned.label, planned.description);
    const before = previous.find((row) => row.id === planned.id);
    if (before === undefined) return fresh;
    if (fresh.label === "") fresh.label = before.label;
    if (fresh.description === "") fresh.description = before.description;
    fresh.guided = before.guided;
    const last = before.state !== "pending" && before.state !== "running" ? before.state : before.previousState;
    if (last !== undefined) fresh.previousState = last;
    return fresh;
  });
}

/**
 * The record for a step an event names, created when the plan did not.
 *
 * A late label or description fills a blank one and never replaces a real one.
 */
export function upsertStep(
  records: StepProgress[],
  id: string,
  label: string,
  description: string,
): StepProgress {
  const existing = records.find((row) => row.id === id);
  if (existing !== undefined) {
    if (description !== "" && existing.description === "") existing.description = description;
    if (label !== "" && existing.label === "") existing.label = label;
    return existing;
  }
  const fresh = freshStep(id, label, description);
  records.push(fresh);
  return fresh;
}

/** The step began, at `now` (epoch milliseconds). */
export function markStarted(row: StepProgress, now: number): void {
  row.state = "running";
  row.startedAt = now;
  delete row.finishedAt;
  delete row.phase;
}

/** The running step reported a phase. A count is kept only when both halves arrived. */
export function markPhase(row: StepProgress, label: string, done?: number, total?: number): void {
  row.phase =
    typeof done === "number" && typeof total === "number" ? { label, done, total } : { label };
}

const STATUS_TO_STATE: Readonly<Record<string, StepState>> = {
  ok: "done",
  failed: "failed",
  skipped: "skipped",
  preserved: "preserved",
};

/**
 * The step settled. Its timings come from the outcome's own timestamps where
 * they parse, and from `now` where they do not; a step that settled without
 * ever starting (a skip) starts and finishes at the same moment.
 */
export function markFinished(row: StepProgress, outcome: StepOutcome, now: number): void {
  row.state = STATUS_TO_STATE[outcome.status] ?? "done";
  row.reason = outcome.reason ?? "";
  row.exitCode = outcome.exitCode;
  const finished = epochMs(outcome.finishedAt) ?? now;
  row.finishedAt = finished;
  if (row.startedAt === undefined) row.startedAt = epochMs(outcome.startedAt) ?? finished;
  delete row.phase;
}

/** Appends one line of the step's output. */
export function appendLog(row: StepProgress, line: string): void {
  row.log = row.log === "" ? line : `${row.log}\n${line}`;
}

/** A copy a caller may keep, sharing nothing mutable with the record. */
export function copyStep(row: StepProgress): StepProgress {
  return row.phase === undefined ? { ...row } : { ...row, phase: { ...row.phase } };
}

function epochMs(iso: string | undefined): number | undefined {
  if (iso === undefined || iso === "") return undefined;
  const ms = Date.parse(iso);
  return Number.isFinite(ms) ? ms : undefined;
}
