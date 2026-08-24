// The uninstall's own run state: where the removal has got to, and what broke.
//
// WHY IT IS NOT AddClusterState. That machine folds an INSTALL: a failed step
// takes it to the `failedStep` screen, which offers Retry and Switch-to-Guided.
// Neither recovery exists here. An uninstall step that fails has not left a
// half-built artifact to resume -- it has left the artifact it was told to
// remove, still there, still recorded in the receipt -- so the honest answer is
// to name the step that refused and stop, with the receipt untouched and the
// whole run repeatable. Folding this into the install machine would put two
// different meanings of "failed" behind one screen.
//
// WHAT IT DELIBERATELY REUSES. `StepProgress` and, through it, `toStepViews` /
// `failureGuidance` from state/installProgress.ts. The rows an uninstall draws
// are the same rows an install draws -- a step, its state, what it said -- and
// a parallel projection would be a second place for "how a step reads" to be
// decided.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go),
// which is what lets an operator's whole path through a removal -- approve,
// run, fail, retry -- be driven under bare `node --test`.
//
// Refs: #3476 #3474 #3469 #3463

import type { ExecEvent } from "../install/executor.js";
import type { StepProgress } from "./addCluster.js";

/**
 * Where the removal has got to.
 *
 * `stopped` is its own phase rather than a flavour of `failed`, for the reason
 * ExecutionReport keeps `cancelled` separate from `ok`: a run the operator
 * stopped has broken nothing, and telling them a step failed when they were the
 * one who intervened would send them looking for a fault that is not there.
 */
export type UninstallPhase =
  /** The itemized list is on screen and nothing has run. */
  | "preview"
  /** The operator approved the list; steps are executing. */
  | "running"
  /** Every step that ran, ran. The artifacts are off the machine. */
  | "removed"
  /** A step failed, or the run could not start at all. */
  | "failed"
  /** The operator stopped it. What went, went; the rest is still there. */
  | "stopped";

export class UninstallRunState {
  private currentPhase: UninstallPhase = "preview";
  private rows: StepProgress[] = [];
  private failedStepId: string | undefined;
  private problemMessage = "";
  private followUpMessage = "";
  // The log disclosure, held here for the same reason AddClusterState holds its
  // own (memql#4455): the panel re-renders wholesale on every `stepLog`, so a
  // `<details>` an operator opened would close itself a second later.
  //
  // A SECOND PAIR OF FIELDS RATHER THAN A SHARED ONE, because these are two
  // different runs. An operator who opened the log on a failed install and then
  // started an uninstall should meet a closed pane: the disclosure being open is
  // a fact about the run they were reading, not a preference.
  private logsShown = false;
  private logsFollowTail = true;

  get phase(): UninstallPhase {
    return this.currentPhase;
  }

  get steps(): StepProgress[] {
    return this.rows.map((row) => ({ ...row }));
  }

  /**
   * The step that failed, by name.
   *
   * THE FIRST one, when several did. The graph stops dependents of a failure
   * but runs everything independent of it to completion, so a run can report
   * more than one -- and the first is the one the operator acts on, with the
   * rest visible in the list below it.
   */
  get failure(): StepProgress | undefined {
    const found = this.rows.find((row) => row.id === this.failedStepId);
    return found === undefined ? undefined : { ...found };
  }

  /** Whether the removal's output is disclosed. */
  get logsOpen(): boolean {
    return this.logsShown;
  }

  /** Whether the pane should still be pinned to the tail on the next render. */
  get logsFollow(): boolean {
    return this.logsFollowTail;
  }

  /** The operator pressed the disclosure. See `AddClusterState.toggleLogs`. */
  toggleLogs(): void {
    this.logsShown = !this.logsShown;
    if (this.logsShown) this.logsFollowTail = true;
  }

  /** Recorded, never repainted. See `AddClusterState.setLogsFollow`. */
  setLogsFollow(follow: boolean): void {
    this.logsFollowTail = follow;
  }

  /** Why the run could not start, when no step ever reported. */
  get problem(): string {
    return this.problemMessage;
  }

  /**
   * What went wrong AFTER the machine was cleaned.
   *
   * Held apart from `problem` because it is a different piece of news: the
   * artifacts are gone and the uninstall succeeded, and what failed is the
   * editor's own bookkeeping. Reporting it as a failed uninstall would send the
   * operator to re-run a removal that has nothing left to remove.
   */
  get followUpProblem(): string {
    return this.followUpMessage;
  }

  /** The operator approved the list. */
  begin(): void {
    this.currentPhase = "running";
    this.rows = [];
    this.failedStepId = undefined;
    this.problemMessage = "";
    this.followUpMessage = "";
    // A new removal starts closed, for the reason `AddClusterState.beginRun`
    // states: an open pane would be showing the previous run's output.
    this.logsShown = false;
    this.logsFollowTail = true;
  }

  /**
   * Folds one executor event into the rows.
   *
   * NEVER THROWS ON AN EVENT IT DOES NOT KNOW, for the reason AddClusterState
   * does not: the union belongs to the executor and is free to grow, and a
   * removal that crashed on an unfamiliar event would abandon a machine
   * half-cleaned.
   *
   * A failed step does NOT change the phase. The run is still going -- the
   * executor finishes every wave that does not depend on the failure -- and
   * moving to a terminal phase here would report a stopped run while steps were
   * still executing. `finish` is where the phase settles, off the report.
   */
  apply(event: ExecEvent): void {
    switch (event?.type) {
      case "runStarted": {
        // The removals ahead, in graph order, before any of them runs
        // (memql#3474). It matters more here than on the install side: the
        // operator has just consented to an itemized list, and a progress
        // display that showed only what had already gone would not let them
        // check the run against what they agreed to.
        for (const step of event.steps) this.upsert(step.id, step.description);
        return;
      }
      case "stepStarted": {
        const row = this.upsert(event.step.id, event.step.description);
        row.state = "running";
        return;
      }
      case "stepLog": {
        const row = this.upsert(event.step.id, event.step.description);
        row.log = row.log === "" ? event.line : `${row.log}\n${event.line}`;
        return;
      }
      case "stepFinished": {
        const row = this.upsert(event.step.id, event.step.description);
        row.state = STATUS_TO_STATE[event.outcome.status] ?? "done";
        row.reason = event.outcome.reason ?? "";
        row.exitCode = event.outcome.exitCode;
        // The FIRST failure is kept. A later one does not replace it: the
        // operator is being told which step to act on, and the earliest failure
        // is the one the rest may be consequences of.
        if (row.state === "failed" && this.failedStepId === undefined) this.failedStepId = row.id;
        // FAILURE OPENS THE LOG (memql#4455). A removal that refused has left an
        // artifact on the machine, and the output is what says which and why;
        // making the operator find a toggle first would be design spite at the
        // one moment the log IS the product.
        if (row.state === "failed") this.logsShown = true;
        return;
      }
      default:
        // waveStarted, and anything added later. Nothing to show.
        return;
    }
  }

  /** The run ended on its own terms. */
  finish(report: { ok: boolean; cancelled?: boolean }): void {
    if (report.cancelled === true) {
      this.currentPhase = "stopped";
      return;
    }
    this.currentPhase = report.ok ? "removed" : "failed";
  }

  /**
   * The run never started.
   *
   * A missing receipt is the case that matters: `runUninstall` refuses rather
   * than falling back to the graph's idea of what an install creates, and the
   * refusal has to reach the operator as a sentence rather than as a preview
   * that sits there having quietly done nothing.
   */
  fail(message: string): void {
    this.currentPhase = "failed";
    this.problemMessage = message;
  }

  /** The machine is clean; something the editor keeps about it is not. */
  noteFollowUpProblem(message: string): void {
    this.followUpMessage = message;
  }

  /** Back to the list, with nothing carried over from a previous attempt. */
  reset(): void {
    this.currentPhase = "preview";
    this.rows = [];
    this.failedStepId = undefined;
    this.problemMessage = "";
    this.followUpMessage = "";
  }

  private upsert(id: string, description: string): StepProgress {
    const existing = this.rows.find((row) => row.id === id);
    if (existing !== undefined) {
      if (description !== "" && existing.description === "") existing.description = description;
      return existing;
    }
    const fresh: StepProgress = {
      id,
      description,
      state: "pending",
      reason: "",
      exitCode: null,
      log: "",
      // Never true here. Guided is an INSTALL affordance -- the operator runs
      // one command by hand and the same verify decides when it is done -- and
      // an uninstall offers no such per-step hand-over. The field is carried
      // because the row type is shared with the install run, not because this
      // path can set it.
      guided: false,
      // A removal step has no remedy to offer: what it needs is not something
      // an operator supplies, it is an artifact the receipt already names.
      remedy: "",
    };
    this.rows.push(fresh);
    return fresh;
  }
}

const STATUS_TO_STATE: Record<string, StepProgress["state"]> = {
  ok: "done",
  failed: "failed",
  skipped: "skipped",
  preserved: "preserved",
};
