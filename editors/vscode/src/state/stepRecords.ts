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
import type { LogLine } from "../webview/ui/protocol.js";
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

/**
 * The step began, at `now` (epoch milliseconds). `timeoutSeconds` is the
 * step's own ceiling from the graph, kept so a timeout can say how long the
 * step was allowed rather than guessing.
 */
export function markStarted(row: StepProgress, now: number, timeoutSeconds?: number): void {
  row.state = "running";
  row.startedAt = now;
  delete row.finishedAt;
  delete row.phase;
  if (timeoutSeconds !== undefined && Number.isFinite(timeoutSeconds) && timeoutSeconds > 0) {
    row.timeoutSeconds = timeoutSeconds;
  }
}

/**
 * The running step reported a phase, at `now` (epoch milliseconds). A count is
 * kept only when both halves arrived.
 *
 * `since` is when THIS phase began: the first report carrying its label. A
 * repeat of the same phase with a higher count keeps it, and a new phase starts
 * its own -- which is what lets runProgress spread a count over the part of the
 * step the clock had not already credited.
 */
export function markPhase(
  row: StepProgress,
  label: string,
  done: number | undefined,
  total: number | undefined,
  now: number,
): void {
  const since = row.phase?.label === label && row.phase.since !== undefined ? row.phase.since : now;
  row.phase =
    typeof done === "number" && typeof total === "number" ? { label, done, total, since } : { label, since };
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
  // THE SCRIPT'S OWN WORDS AND ITS OWN FIX, read off the envelope on BOTH
  // machines. The remedy used to be read by the install machine alone, so an
  // uninstall step that failed for want of a password -- the hosts block --
  // named no command and got the wrong advice (memql#5118 audit).
  row.message = row.state === "failed" ? messageFrom(outcome.envelope) : "";
  row.remedy = remedyFrom(outcome.envelope);
  const finished = epochMs(outcome.finishedAt) ?? now;
  row.finishedAt = finished;
  if (row.startedAt === undefined) row.startedAt = epochMs(outcome.startedAt) ?? finished;
  delete row.phase;
}

/**
 * The remedy a capability declared, or "" (memql#3551).
 *
 * DEFENSIVE ABOUT ITS OWN INPUT. The envelope is JSON a script produced, so
 * `result` can be anything at all; anything that is not a non-empty string is
 * no remedy. It is about to be offered to an operator as a command to run with
 * root, so "probably a string" is not the standard.
 */
export function remedyFrom(envelope: { result?: unknown } | null | undefined): string {
  const result = envelope?.result;
  if (result === null || typeof result !== "object") return "";
  const value = (result as Record<string, unknown>).remedy;
  return typeof value === "string" ? value.trim() : "";
}

/**
 * What a failed script said about itself, for a person: `error.message`, or
 * `result.reason`, or "".
 *
 * The same two sources the executor reads for its record, WITHOUT the exit
 * code and verify detail it wraps them in -- those are the log's (see
 * `StepProgress.message`).
 */
export function messageFrom(envelope: { result?: unknown; error?: unknown } | null | undefined): string {
  const error = envelope?.error;
  if (error !== null && typeof error === "object") {
    const message = (error as Record<string, unknown>).message;
    if (typeof message === "string" && message.trim() !== "") return message.trim();
  }
  const result = envelope?.result;
  if (result !== null && typeof result === "object") {
    const reason = (result as Record<string, unknown>).reason;
    if (typeof reason === "string" && reason.trim() !== "") return reason.trim();
  }
  return "";
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

// ---------------------------------------------------------------------------
// the run's log, as the page shows it: one chronological stream
// ---------------------------------------------------------------------------

/** One line of the run's log, with the step it came from. */
export interface RunLogEntry {
  stepId: string;
  line: LogLine;
}

/** A line that reads as an error in a script's own output. */
const ERROR_LINE = /^(?:ERROR\b|ERRO\[|error:|fatal:)/i;

/**
 * The run's output in the order it happened, with each step's label drawn
 * ONCE, where that step's lines begin.
 *
 * WHY ONE STREAM. The per-step blocks this replaces put a wave's concurrent
 * steps one after another, so the log never read like the output it was: a
 * person watching an install sees lines arrive, not sections fill. Labelling
 * every line would repeat "Creating the cluster" four hundred times; labelling
 * the first line of each run of lines from one step marks exactly where the
 * output changes hands, and nowhere else.
 *
 * BOUNDED like the page's own pane (the oldest lines go first), and it keeps
 * the step each line came from, which is what lets a failure open the log AT
 * the failed step (`anchoredAt`) rather than at the tail.
 *
 * Lines arrive here already redacted; this module does not know the home
 * directory, and a second redaction pass would be a second place to get it
 * wrong.
 */
export class RunLogLines {
  private entries: RunLogEntry[] = [];
  private lastStep: string | undefined;

  constructor(private readonly limit = 5000) {}

  /** Records one line and returns it as the page should draw it. */
  add(stepId: string, label: string, text: string, tone?: LogLine["tone"]): LogLine {
    const line: LogLine = { text };
    if (stepId !== this.lastStep && label !== "") line.label = label;
    this.lastStep = stepId;
    const shade = tone ?? (ERROR_LINE.test(text.trim()) ? "error" : undefined);
    if (shade !== undefined) line.tone = shade;
    this.entries.push({ stepId, line });
    if (this.entries.length > this.limit) this.entries.splice(0, this.entries.length - this.limit);
    return { ...line };
  }

  /** Forgets everything, for a new attempt. */
  reset(): void {
    this.entries = [];
    this.lastStep = undefined;
  }

  get length(): number {
    return this.entries.length;
  }

  /** Every line, oldest first. */
  lines(): LogLine[] {
    return this.entries.map((entry) => ({ ...entry.line }));
  }

  /**
   * Every line, with the FIRST line of `stepId` marked as the place to open
   * the log at. That line always carries the step's label: it is the first of
   * its step, so the step changed hands there.
   */
  anchoredAt(stepId: string): LogLine[] {
    let marked = false;
    return this.entries.map((entry) => {
      const line: LogLine = { ...entry.line };
      if (!marked && entry.stepId === stepId) {
        marked = true;
        line.anchor = true;
      }
      return line;
    });
  }

  /** The log as plain text, for Copy: each step's label on its own line, its output indented. */
  text(): string {
    const out: string[] = [];
    for (const entry of this.entries) {
      if (entry.line.label !== undefined && entry.line.label !== "") out.push(entry.line.label);
      out.push(`  ${entry.line.text}`);
    }
    return out.join("\n");
  }
}
