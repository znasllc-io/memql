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
import { redactResult } from "../install/secrets.js";
import {
  newLocalRun,
  settleRunStatus,
  type Run,
  type RunItem,
  type RunItemStatus,
  type RunKind,
} from "./deployments.js";
import { finishRun, finishRunSettled, mintRunId, recordRunItem, startRun } from "./runLog.js";

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
          // WHAT THE STEP ACTUALLY DID, not just whether it failed (memql#3886).
          //
          // This used to record a detail ONLY on a failure with a reason, and
          // `recordRunItem` replaces the item at a label wholesale -- so a
          // successful step overwrote the parameter detail `stepStarted` had
          // just written and landed as a bare {at, label, status}. A completed
          // install's record therefore held no fact about any step beyond its
          // name, which is why the deployment view had nothing to show.
          const detail: string[] = [];

          // The step's OWN sentence, when it has one. It is about this failure;
          // the exit-code guidance the panel renders can only be about a class
          // of them, and a history that kept only the class would lose the part
          // that says which machine this was. First, so a failure leads.
          const reason = (event.outcome.reason ?? "").trim();
          if (reason !== "") detail.push(reason);

          // The exit code ONLY when there is no sentence, which is the rule the
          // test beside this one was already asserting: a step's own sentence is
          // about THIS machine, and the exit code can only be about a class of
          // failure, so appending it to a sentence dilutes the specific with the
          // generic. When a step fails mutely, though, the class is all there
          // is -- and the capability contract makes it real information (2 bad
          // param, 3 refused, 4 prerequisite missing, 5 op failed). Zero is
          // never carried: "it worked" is already the status.
          const exit = event.outcome.exitCode;
          if (reason === "" && typeof exit === "number" && exit !== 0) {
            detail.push(`exit ${exit}`);
          }

          // A skip that dependents can proceed through is a materially
          // different outcome from one that leaves them blocked, and the
          // executor already draws that distinction.
          if (event.outcome.status === "skipped" && event.outcome.satisfied === true) {
            detail.push("the condition dependents needed already holds");
          }
          // NOTHING IS ADDED FOR `preExisting`. A preserved step's reason
          // already says why it was kept, in the operator's terms ("you created
          // this k3d cluster"), and a generic "pre-existing, left alone" beside
          // it restates the same fact worse.

          // The step's own result fields -- what `enrolmentState` and
          // `linkState` say, which is exactly the information a stuck operator
          // needs. Credential-bearing fields are withheld by name AND by value
          // shape; see redactResult, and note that `redactSecrets` alone would
          // NOT have caught the magic link.
          const result = event.outcome.envelope?.result;
          if (result !== undefined && result !== null) {
            const safe = redactResult(result as Record<string, unknown>);
            const rendered = Object.keys(safe)
              .sort()
              .map((key) => `${key}=${safe[key]}`)
              .join(" ");
            if (rendered !== "") detail.push(rendered);
          }

          if (detail.length > 0) item.detail = detail.join(" · ");
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
    await this.guard(async () => {
      // The DERIVED status is settled inside the write lock, over the merged
      // items, rather than over `this.run` -- which can be missing a step that
      // a concurrent `recordRunItem` landed on disk, and settled to "running"
      // for a run whose every item had finished. An explicitly supplied status
      // is a fact the caller owns and is written as given.
      this.run =
        status === undefined
          ? await finishRunSettled(this.dir, this.run, finishedAt, settleRunStatus)
          : await finishRun(this.dir, this.run, status, finishedAt);
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
