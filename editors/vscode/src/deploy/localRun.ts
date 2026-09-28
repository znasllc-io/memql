// The long local runs the cluster page starts -- update, change version,
// rebuild, pull and rebuild -- held OUTSIDE the page that shows them.
//
// WHY NOT IN THE PANEL. The page used to own its run, and three defects came
// from that one fact:
//
//  1. RUN STATE BLED BETWEEN RUNS. One state object lived as long as the panel,
//     so a second rebuild opened on the first one's finished rows and log, and
//     a rebuild after a failed version change opened on the failure. A run here
//     gets its OWN state, created with it; the next run cannot see the last.
//  2. CLOSING THE TAB KILLED THE RUN -- up to 45 minutes of it, silently. The
//     run lives in the extension host now and outlives the tab; reopening the
//     page (or clicking the run's row) shows it live, still going.
//  3. OPENING THE PAGE OR A RUN MID-RUN hid the live progress and offered acts
//     that started a second run beside the first. `LocalRuns.inFlight` is the
//     one answer to "is something running", and `start` refuses a second.
//
// CANCEL SAYS ONLY WHAT IT CAN DO. The executor stops at the next wave
// boundary and never mid-step (killing a script half-way leaves an artifact
// half-made and unrecorded). A rebuild is ONE step, so once it is building
// there is no boundary left and Cancel is not offered; before that, and
// between steps of a longer run, it is, and pressing it reads "Stopping after
// the current step" until the run settles. The record then says what really
// happened: a run that finished anyway is recorded as finished, not cancelled.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// everything the host does when a run settles -- repaint a tree, refresh the
// construct catalog, raise a toast -- is a callback.

import { randomBytes } from "node:crypto";

import type { ExecEvent, ExecutionReport } from "../install/executor.js";
import type { Graph } from "../install/graph.js";
import { readReceipt, recordedDomain } from "../install/receipt.js";
import {
  installSessionOptions,
  runInstall,
  runRebuild,
  runUpdateRebuild,
  type SessionHooks,
} from "../install/session.js";
import { DEFAULT_LOCAL_DOMAIN } from "../install/stackPin.js";
import { AddClusterState, DEFAULT_INPUTS } from "../state/addCluster.js";
import type { RunKind } from "../state/deployments.js";
import { rebuiltMessage, rebuiltNodes, updatedMessage } from "../state/imageLane.js";
import { failureGuidance } from "../state/installProgress.js";
import { historicalWeights } from "../state/runProgress.js";
import { RunRecorder } from "../state/runRecorder.js";
import type { UpdateStrategy } from "../state/updatePreflight.js";
import type { LogLine, ProgressUpdate } from "../webview/ui/protocol.js";

/** The same DEFAULT ceiling the wizard gives a step; a step's own `timeoutSeconds` outranks it (memql#4076). */
const STEP_TIMEOUT_MS = 600_000;

/**
 * The rebuild's own ceiling: 45 minutes, matching rebuild.json's
 * `timeoutSeconds` (memql#4246). Nine node images from a cold Docker cache is
 * structurally more than ten minutes.
 */
const REBUILD_TIMEOUT_MS = 2_700_000;

export type LocalRunKind = "update" | "changeVersion" | "rebuild" | "pullRebuild";

/**
 * One run, for one instance. `instance` is the registry key the record is filed
 * under; `label` is what the run's sentences call it ("memql.localhost is on
 * v0.24.0"), and the key stands in when a caller has no label.
 */
export type LocalRunRequest = (
  | { kind: "update" | "changeVersion"; instance: string; from: string; to: string }
  | { kind: "rebuild"; instance: string; checkout: string; nodes: string }
  | {
      kind: "pullRebuild";
      instance: string;
      checkout: string;
      nodes: string;
      branch: string;
      strategy: UpdateStrategy;
    }
) & { label?: string };

/** What a run's sentences call its cluster. */
function spokenName(request: LocalRunRequest): string {
  const label = (request.label ?? "").trim();
  return label !== "" ? label : request.instance;
}

/** How each kind of run reads: the title while it runs, its bar word, and its done title. */
export interface RunWords {
  title: string;
  busy: string;
  failed: string;
  /** The bar's word once it went through. */
  done: string;
}

export function runWords(request: LocalRunRequest): RunWords {
  switch (request.kind) {
    case "update":
      return { title: "Updating MemQL", busy: "Updating", failed: "Couldn't update", done: "Updated" };
    case "changeVersion":
      return { title: "Changing version", busy: "Changing version", failed: "Couldn't change version", done: "Version changed" };
    case "rebuild":
      return { title: "Rebuilding MemQL", busy: "Rebuilding", failed: "Couldn't rebuild", done: "Rebuilt" };
    case "pullRebuild":
      return {
        title: "Pulling and rebuilding",
        busy: "Pulling and rebuilding",
        failed: "Couldn't pull and rebuild",
        done: "Pulled and rebuilt",
      };
  }
}

/** The run-log kind a request is recorded under (state/runLog.ts reads these back). */
function recordKind(request: LocalRunRequest): RunKind {
  switch (request.kind) {
    case "update":
    case "changeVersion":
      return "upgrade";
    case "rebuild":
      return "rebuild";
    case "pullRebuild":
      return "update";
  }
}

/**
 * A step label in the negative: "Creating the cluster" -> "Couldn't create the
 * cluster".
 *
 * A TABLE, NOT A RULE: English gerunds do not strip cleanly ("Creating" is
 * "create", "Setting" is "set", "Adding" is "add"), and a wrong verb in the one
 * line a failure leads with reads as the extension being broken. A label whose
 * verb is not here falls back to "<label> failed".
 */
const BASE_VERBS: Readonly<Record<string, string>> = {
  Adding: "add",
  Building: "build",
  Checking: "check",
  Creating: "create",
  Downloading: "download",
  Installing: "install",
  Preparing: "prepare",
  Rebuilding: "rebuild",
  Removing: "remove",
  Setting: "set",
  Starting: "start",
  Updating: "update",
};

export function failedStatus(label: string): string {
  const trimmed = label.trim();
  const space = trimmed.indexOf(" ");
  const first = space < 0 ? trimmed : trimmed.slice(0, space);
  const verb = BASE_VERBS[first];
  if (verb === undefined) return trimmed === "" ? "Something failed" : `${trimmed} failed`;
  return `Couldn't ${verb}${space < 0 ? "" : trimmed.slice(space)}`;
}

export type LocalRunStatus = "running" | "stopping" | "failed" | "done" | "stopped";

/** What a run tells whoever is watching it. */
export type LocalRunEvent = { type: "change" } | { type: "log"; lines: LogLine[] };

export interface LocalRunDeps {
  installRoot: string;
  receiptFile: string;
  runsDir: string;
  /** Injected by tests; the real spawn-based runner when absent. */
  runScript?: SessionHooks["run"];
  /** Injected by tests; the graph documents are read from `installRoot` when absent. */
  graphs?: Partial<Record<LocalRunKind, Graph>>;
  now?: () => number;
  entropy?: () => string;
  /** Every log line, labelled, for the Output channel. */
  log?: (line: string) => void;
  /** A run record was created or closed: the tree has something new to show. */
  onRecord?: () => void;
  /** The run settled -- done, failed or stopped. */
  onSettled?: (run: LocalRun) => void;
  /** Brings the page showing this run forward: the Show on another page's refusal. */
  reveal?: () => void;
}

/** A failure the page leads with: the step, its reason, and the command that fixes it. */
export interface RunFailure {
  label: string;
  reason: string;
  remedy: string;
  retryable: boolean;
}

export class LocalRun {
  /** A fresh state per run: nothing a previous run left can show here. */
  readonly state: AddClusterState;
  readonly words: RunWords;
  status: LocalRunStatus = "running";
  startedAt: number;
  endedAt: number | undefined;
  /** The run could not be attempted at all (a throw, not a failed step). */
  error = "";
  /** What the run left the cluster running, once it succeeded. */
  result = "";
  /** The run-log record of the current attempt. */
  recordId = "";
  private readonly lines: LogLine[] = [];
  private readonly listeners = new Set<(event: LocalRunEvent) => void>();
  private controller: AbortController | undefined;
  private currentWave: readonly string[] = [];
  private attempt: Promise<void> = Promise.resolve();

  constructor(
    readonly request: LocalRunRequest,
    private readonly deps: LocalRunDeps,
  ) {
    const now = deps.now ?? Date.now;
    this.state = new AddClusterState({ now });
    this.words = runWords(request);
    this.startedAt = now();
  }

  /** Subscribe; returns the unsubscribe. */
  onEvent(listener: (event: LocalRunEvent) => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  /** Every line so far, oldest first, for a page that opens mid-run. */
  get log(): readonly LogLine[] {
    return this.lines;
  }

  get inFlight(): boolean {
    return this.status === "running" || this.status === "stopping";
  }

  /** Brings the page showing this run forward, when its starter said how. */
  reveal(): void {
    this.deps.reveal?.();
  }

  /**
   * Whether Cancel can still do something: a step that has not started yet,
   * beyond the wave that is running. False for a run in its last wave -- a
   * rebuild once it is building -- where Cancel would only promise a stop the
   * executor will not make.
   */
  get cancellable(): boolean {
    if (this.status !== "running") return false;
    const steps = this.state.steps;
    if (steps.length === 0) return true;
    return steps.some((step) => step.state === "pending" && !this.currentWave.includes(step.id));
  }

  /** The step the page leads with when the run failed, or undefined. */
  get failure(): RunFailure | undefined {
    if (this.status !== "failed") return undefined;
    const failed = this.state.failed ?? this.state.failures[0];
    if (failed === undefined) {
      return { label: "", reason: this.error === "" ? "The run stopped." : this.error, remedy: "", retryable: true };
    }
    const guidance = failureGuidance(failed.exitCode, failed.remedy, failed.reason);
    return {
      label: failed.label !== "" ? failed.label : failed.id,
      reason: failed.reason.trim() !== "" ? failed.reason.trim() : guidance.headline,
      remedy: failed.remedy,
      retryable: guidance.retryable,
    };
  }

  /** The live values of the page's progress region. */
  progress(now: number = (this.deps.now ?? Date.now)()): ProgressUpdate {
    const raw = this.state.progress(now);
    // "Step 1 of 1" counts nothing a person needs counted: a one-step run
    // (a rebuild) says where it is through its phases instead.
    const computed = { ...raw, stepText: this.state.steps.length === 1 ? "" : raw.stepText };
    const settled = this.endedAt === undefined ? {} : { endedAt: this.endedAt };
    switch (this.status) {
      case "running":
        return {
          ...(this.state.steps.length === 0 ? {} : { percent: computed.percent }),
          status: computed.status,
          stepText: computed.stepText,
          startedAt: this.startedAt,
          state: "running",
        };
      case "stopping":
        return {
          percent: computed.percent,
          status: "Stopping after the current step",
          stepText: computed.stepText,
          startedAt: this.startedAt,
          state: "stopping",
        };
      case "stopped":
        return {
          percent: computed.percent,
          status: "Stopped",
          stepText: computed.stepText,
          startedAt: this.startedAt,
          ...settled,
          state: "stopping",
        };
      case "failed": {
        const failure = this.failure;
        return {
          percent: computed.percent,
          status: failure === undefined || failure.label === "" ? this.words.failed : failedStatus(failure.label),
          stepText: computed.stepText,
          startedAt: this.startedAt,
          ...settled,
          state: "failed",
        };
      }
      case "done": {
        const total = this.state.steps.length;
        return {
          percent: 100,
          status: this.result,
          stepText: total <= 1 ? "" : `${total} steps`,
          startedAt: this.startedAt,
          ...settled,
          state: "done",
          title: doneTitle(this.request),
        };
      }
    }
  }

  /** Stop at the next wave boundary, when there is one. Returns whether it will. */
  cancel(): boolean {
    if (!this.cancellable || this.controller === undefined) return false;
    this.controller.abort();
    this.status = "stopping";
    this.emit({ type: "change" });
    return true;
  }

  /**
   * Run the failed steps again, as a new attempt of the same run.
   *
   * THE SAME RUN, NOT A NEW ONE: its rows go back to pending (what each came
   * to last time survives as `previousState`) and the log starts afresh, so the
   * failure being read is never the one that is no longer happening.
   */
  retry(): Promise<void> {
    if (this.status !== "failed" && this.status !== "stopped") return this.attempt;
    this.state.retry();
    this.error = "";
    this.status = "running";
    this.startedAt = (this.deps.now ?? Date.now)();
    this.endedAt = undefined;
    this.lines.length = 0;
    this.emit({ type: "log", lines: [] });
    this.emit({ type: "change" });
    this.attempt = this.execute();
    return this.attempt;
  }

  /** Begin. Called once by `LocalRuns.start`. */
  begin(): Promise<void> {
    this.attempt = this.execute();
    return this.attempt;
  }

  /** Resolves when the current attempt settles. */
  settled(): Promise<void> {
    return this.attempt;
  }

  private async execute(): Promise<void> {
    const deps = this.deps;
    const request = this.request;
    const kind = recordKind(request);
    // The bar weighs each step by how long it last took on this machine.
    this.state.setStepWeights(await historicalWeights(deps.runsDir, { kinds: [kind] }));
    const recorder = await RunRecorder.begin({
      dir: deps.runsDir,
      instance: request.instance,
      kind,
      ...(request.kind === "update" || request.kind === "changeVersion"
        ? { ...(request.from !== "" ? { fromVersion: request.from } : {}), toVersion: request.to }
        : {}),
      entropy: (deps.entropy ?? (() => randomBytes(4).toString("hex")))(),
    });
    this.recordId = recorder.current.id;
    // The tree reads the run log, so it shows this run before its first step
    // reports -- a rebuild is minutes long, a long time for a click to leave
    // no trace.
    deps.onRecord?.();

    const controller = new AbortController();
    this.controller = controller;
    const hooks: SessionHooks = {
      onEvent: (event) => {
        this.fold(event);
        void recorder.apply(event);
      },
      signal: controller.signal,
      ...(deps.runScript !== undefined ? { run: deps.runScript } : {}),
      ...(deps.graphs?.[request.kind] !== undefined ? { graph: deps.graphs[request.kind] } : {}),
    };

    let report: ExecutionReport | undefined;
    try {
      report = await this.dispatch(hooks);
    } catch (err) {
      // A THROW IS NOT A FAILED STEP: every step failure arrives as an event.
      // Reaching here means the run could not be attempted at all.
      this.error = err instanceof Error ? err.message : String(err);
      this.push([{ text: this.error, tone: "error" }]);
    } finally {
      this.controller = undefined;
    }

    // WHAT REALLY HAPPENED. `cancelled` only when the executor actually
    // stopped before a wave; a Cancel pressed during the last wave changes
    // nothing, and the run is recorded as whatever it came to.
    const stopped = report?.cancelled === true;
    const ok = this.error === "" && report?.ok === true && !stopped;
    await recorder.finish(stopped ? "cancelled" : ok ? "succeeded" : "failed");
    this.state.finish({ ok, ...(stopped ? { cancelled: true } : {}) });
    this.endedAt = (deps.now ?? Date.now)();
    if (ok) this.result = resultSentence(request, report);
    this.status = stopped ? "stopped" : ok ? "done" : "failed";
    this.currentWave = [];
    deps.onRecord?.();
    this.emit({ type: "change" });
    deps.onSettled?.(this);
  }

  private async dispatch(hooks: SessionHooks): Promise<ExecutionReport> {
    const { deps, request } = this;
    switch (request.kind) {
      case "update":
      case "changeVersion": {
        // EVERY ANSWER BUT THE TAG COMES OFF THE RECEIPT, which is exactly what
        // a repair does: this machine already answered these questions, and
        // asking again invites a different answer. The tag is the whole verb.
        const receipt = await readReceipt(deps.receiptFile).catch(() => null);
        return runInstall(
          installSessionOptions({
            root: deps.installRoot,
            receiptFile: deps.receiptFile,
            domain: recordedDomain(receipt) || DEFAULT_LOCAL_DOMAIN,
            ownerEmail: DEFAULT_INPUTS.ownerEmail,
            ownerFirstName: DEFAULT_INPUTS.ownerFirstName,
            ownerLastName: DEFAULT_INPUTS.ownerLastName,
            tag: request.to,
            timeoutMs: STEP_TIMEOUT_MS,
          }),
          hooks,
        );
      }
      case "rebuild":
        return runRebuild(
          {
            root: deps.installRoot,
            receiptFile: deps.receiptFile,
            skip: new Set<string>(),
            stepParams: {},
            stackDir: request.checkout,
            nodes: request.nodes,
            timeoutMs: REBUILD_TIMEOUT_MS,
          },
          hooks,
        );
      case "pullRebuild":
        return runUpdateRebuild(
          {
            root: deps.installRoot,
            receiptFile: deps.receiptFile,
            skip: new Set<string>(),
            stepParams: {},
            stackDir: request.checkout,
            nodes: request.nodes,
            branch: request.branch,
            strategy: request.strategy,
            timeoutMs: REBUILD_TIMEOUT_MS,
          },
          hooks,
        );
    }
  }

  /** One executor event into the state, the log and the listeners. */
  private fold(event: ExecEvent): void {
    this.state.apply(event);
    if (event.type === "waveStarted") this.currentWave = [...event.ids];
    if (event.type === "stepLog") {
      this.push([{ label: event.step.label, text: event.line }]);
      return;
    }
    if (event.type === "stepFinished" && event.outcome.status === "failed") {
      const reason = (event.outcome.reason ?? "").trim();
      if (reason !== "") this.push([{ label: event.step.label, text: reason, tone: "error" }]);
    }
    this.emit({ type: "change" });
  }

  private push(lines: LogLine[]): void {
    this.lines.push(...lines);
    for (const line of lines) this.deps.log?.(line.label === undefined ? line.text : `${line.label}: ${line.text}`);
    this.emit({ type: "log", lines });
  }

  private emit(event: LocalRunEvent): void {
    for (const listener of [...this.listeners]) {
      try {
        listener(event);
      } catch {
        // A watcher that throws must not take the run down with it.
      }
    }
  }
}

/** The done screen's title: where the cluster is now, by the name the page shows. */
export function doneTitle(request: LocalRunRequest): string {
  switch (request.kind) {
    case "update":
    case "changeVersion":
      return `${spokenName(request)} is on ${request.to}`;
    case "rebuild":
      return "Rebuilt from your checkout";
    case "pullRebuild":
      return "Pulled and rebuilt";
  }
}

/** What the cluster runs now, read off the envelopes the run produced. */
function resultSentence(request: LocalRunRequest, report: ExecutionReport | undefined): string {
  const result = (id: string): Record<string, unknown> | undefined =>
    report?.outcomes.find((o) => o.id === id)?.envelope?.result as Record<string, unknown> | undefined;
  switch (request.kind) {
    case "update":
    case "changeVersion":
      return "Ready to use";
    case "rebuild": {
      const nodes = rebuiltNodes(request.nodes, result("rebuildFromCheckout"));
      const sentence = rebuiltMessage(spokenName(request), result("rebuildFromCheckout"));
      return nodes === "" ? sentence : `${sentence.replace(/\.$/, "")} for ${nodes}.`;
    }
    case "pullRebuild":
      return updatedMessage(spokenName(request), result("updateCheckout"), result("rebuildFromCheckout"));
  }
}

/**
 * A run the Add Cluster page holds the slot for -- an install, a repair or an
 * uninstall. Those runs live in that page (their password prompt, their
 * recovery key); the slot only needs to know one is going, what to call it
 * and how to show it.
 */
export interface HeldRun {
  /** What the cluster is busy doing, lower case: "installing". */
  busy: string;
  /** Brings the page running it forward. */
  reveal(): void;
}

/** What is running on this machine now, as a refusal names it and its Show reaches it. */
export interface SlotBusy {
  busy: string;
  reveal(): void;
}

/**
 * The one sentence a run refused by the slot says. The fix is its Show
 * button, which reveals the run that holds the slot.
 */
export function slotRefusal(busy: SlotBusy): string {
  return `MemQL: The local cluster is busy ${busy.busy}.`;
}

/** The button beside `slotRefusal`. */
export const SLOT_SHOW = "Show";

/**
 * The machine's one run slot.
 *
 * One at a time, because every run here changes the same cluster and the same
 * receipt: two at once is two answers to what the machine is. `current` keeps
 * the LAST run after it settles, so a page reopened after a failure can still
 * show it with Retry, until the next run replaces it or the page is left.
 *
 * TWO PAGES SHARE IT. The Deployment page's runs (update, change version,
 * rebuild, pull and rebuild) are `LocalRun`s the slot owns; the Add Cluster
 * page's (install, repair, uninstall) stay in that page and `hold` the slot
 * while they go. Either page asks `busy()` before it starts, and a run that
 * would be the second is refused with `slotRefusal` and a Show.
 */
export class LocalRuns {
  private current_: LocalRun | undefined;
  private held_: HeldRun | undefined;
  private readonly listeners = new Set<(run: LocalRun) => void>();

  get current(): LocalRun | undefined {
    return this.current_;
  }

  /** A run of this slot's own is going (the Add Cluster page's are `busy()`). */
  get inFlight(): boolean {
    return this.current_?.inFlight === true;
  }

  /** Whatever is running on this machine now, from either page; undefined when nothing is. */
  busy(): SlotBusy | undefined {
    const run = this.current_;
    if (run !== undefined && run.inFlight) return { busy: run.words.busy.toLowerCase(), reveal: () => run.reveal() };
    return this.held_;
  }

  /**
   * The Add Cluster page takes the slot for a run of its own. Returns the
   * release, which the page calls when the run settles; undefined, and nothing
   * held, when something is already running.
   */
  hold(run: HeldRun): (() => void) | undefined {
    if (this.busy() !== undefined) return undefined;
    this.held_ = run;
    return () => {
      if (this.held_ === run) this.held_ = undefined;
    };
  }

  /** Start a run, or undefined when one is already going on either page. */
  start(request: LocalRunRequest, deps: LocalRunDeps): LocalRun | undefined {
    if (this.busy() !== undefined) return undefined;
    const run = new LocalRun(request, deps);
    this.current_ = run;
    for (const listener of [...this.listeners]) listener(run);
    void run.begin();
    return run;
  }

  /** Forget a settled run, so the page stops leading with it. */
  dismiss(run: LocalRun): void {
    if (this.current_ === run && !run.inFlight) this.current_ = undefined;
  }

  /** Subscribe to new runs; returns the unsubscribe. */
  onDidStart(listener: (run: LocalRun) => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }
}

/** The extension host's one slot. */
export const localRuns = new LocalRuns();
