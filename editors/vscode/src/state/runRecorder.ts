// Turning a run's events into the record on disk.
//
// `session.ts` emits an `ExecEvent` stream and `state/runLog.ts` owns the file
// format; this is the one place that maps between them. It exists so that
// EVERY local run writes a record -- the Deployments page's create and upgrade,
// and the add-cluster wizard's install, repair and uninstall alike. A recorder
// per caller would be several answers to "what counts as a step outcome", and
// the answer has to be one, because the Deployments tree reads them all as one
// history.
//
// THE MAPPING IS ONE-TO-ONE, DELIBERATELY. The executor's four step statuses
// (`ok` / `failed` / `skipped` / `preserved`) are the run log's four terminal
// item statuses, spelled identically. `preserved` in particular survives
// untranslated: it means the uninstall KEPT something the operator already had,
// and the run log is where that fact has to still be true when somebody reads
// the history a week later.
//
// EVENTS ARE FOLDED, NOT QUEUED. Each one rewrites the whole document through
// runLog's atomic write, so a run killed at any point leaves a record naming
// exactly the steps that completed. That is the property the file format exists
// for, and it only holds if the write happens per event rather than at the end.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3739 #3736 #3733

import type { ExecEvent } from "../install/executor.js";
import {
  newLocalRun,
  settleRunStatus,
  type Run,
  type RunItem,
  type RunItemStatus,
  type RunKind,
} from "./deployments.js";
import { finishRun, mintRunId, recordRunItem, startRun } from "./runLog.js";

export interface RunRecorderOptions {
  /** Where the records live -- runLog.defaultRunsDir() in production. */
  dir: string;
  instance: string;
  kind: RunKind;
  fromVersion?: string;
  toVersion?: string;
  /** Injectable clock; every timestamp on the record comes through it. */
  now?: () => string;
  /** Injectable entropy for the run id, so a test can pin the filename. */
  entropy?: string;
}

/**
 * Records one run as it happens.
 *
 * A WRITE FAILURE NEVER STOPS THE RUN. Every method swallows its own error:
 * the record is an account of an install, and losing the account is a smaller
 * harm than aborting the install that is half-way through changing the
 * machine. `lastError` is kept so a caller that wants to say so can.
 */
export class RunRecorder {
  private run: Run;
  private lastError_ = "";

  private constructor(
    private readonly dir: string,
    run: Run,
    private readonly clock: () => string,
  ) {
    this.run = run;
  }

  static async begin(options: RunRecorderOptions): Promise<RunRecorder> {
    const clock = options.now ?? ((): string => new Date().toISOString());
    const startedAt = clock();
    const run = newLocalRun({
      id: mintRunId(options.kind, startedAt, options.entropy),
      instance: options.instance,
      kind: options.kind,
      startedAt,
      ...(options.fromVersion !== undefined ? { fromVersion: options.fromVersion } : {}),
      ...(options.toVersion !== undefined ? { toVersion: options.toVersion } : {}),
    });
    const recorder = new RunRecorder(options.dir, run, clock);
    await recorder.guard(async () => {
      recorder.run = await startRun(options.dir, run);
    });
    return recorder;
  }

  /** The record as it currently stands. */
  get current(): Run {
    return this.run;
  }

  /** The last write failure, or "" when every write has landed. */
  get lastError(): string {
    return this.lastError_;
  }

  /**
   * Folds one executor event into the record.
   *
   * `runStarted` seeds EVERY step as `pending`, which is what makes a killed
   * run legible: the record then names both what completed and what never got
   * its turn. Growing the list from the steps that happen to start would leave
   * an abandoned run indistinguishable from a short one.
   *
   * `stepLog` and `waveStarted` are deliberately not recorded. A log line is
   * the panel's business while the run is on screen, and a wave index is the
   * executor's schedule rather than anything about the install -- neither is a
   * fact a history should have to carry.
   */
  async apply(event: ExecEvent): Promise<void> {
    switch (event.type) {
      case "runStarted":
        await this.guard(async () => {
          for (const step of event.steps) {
            this.run = await recordRunItem(this.dir, this.run, {
              label: step.id,
              status: "pending",
              ...(step.description !== "" ? { detail: step.description } : {}),
            });
          }
        });
        return;
      case "stepStarted":
        await this.guard(async () => {
          this.run = await recordRunItem(
            this.dir,
            this.run,
            { label: event.step.id, status: "running", at: this.clock() },
            // The step's own invocation flags, redacted on the way in by
            // recordRunItem -- a provider key given where a path belonged must
            // not reach a file nothing ever rewrites.
            event.params,
          );
        });
        return;
      case "stepFinished":
        await this.guard(async () => {
          const item: RunItem = {
            label: event.step.id,
            status: event.outcome.status as RunItemStatus,
            at: event.outcome.finishedAt !== "" ? event.outcome.finishedAt : this.clock(),
          };
          // The step's OWN sentence, when it has one. It is about this failure;
          // the exit-code guidance the panel renders can only be about a class
          // of them, and a history that kept only the class would lose the part
          // that says which machine this was.
          const reason = (event.outcome.reason ?? "").trim();
          if (reason !== "") item.detail = reason;
          this.run = await recordRunItem(this.dir, this.run, item);
        });
        return;
      default:
        return;
    }
  }

  /**
   * Closes the record.
   *
   * The status is DERIVED from the items rather than taken from the caller
   * when `ok` is not given -- any failed item fails the run, a `preserved` or
   * `skipped` one does not, and an item still pending means the run did not
   * finish. A caller that knows better (an abort, a run that could not start at
   * all) passes the status it wants.
   */
  async finish(status?: Run["status"]): Promise<Run> {
    const finishedAt = this.clock();
    const resolved = status ?? settleRunStatus(this.run);
    await this.guard(async () => {
      this.run = await finishRun(this.dir, this.run, resolved, finishedAt);
    });
    return this.run;
  }

  private async guard(work: () => Promise<void>): Promise<void> {
    try {
      await work();
    } catch (err) {
      this.lastError_ = err instanceof Error ? err.message : String(err);
    }
  }
}
