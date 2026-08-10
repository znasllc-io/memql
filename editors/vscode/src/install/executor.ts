// The graph executor: walking the waves, proving each step, writing the receipt.
//
// It is deliberately a SMALL amount of code, because almost every decision it
// could make has already been made in the graph document:
//
//   - WHAT MAY OVERLAP is topoOrder's answer, not this file's. A wave is run
//     concurrently and waited for; the executor never re-derives independence,
//     so it cannot get it wrong (and cannot serialise a two-minute install
//     into six by being cautious).
//   - WHETHER A STEP WORKED is evaluateVerify's answer, read off the script's
//     RESULT envelope. An exit code of 0 is a precondition and nothing more:
//     a step that exits 0 with `installed: false` FAILED.
//   - WHAT UNINSTALL WILL NEED is the receipt's job, written after every single
//     step through an atomic rename, so a run killed with ^C is still fully
//     reversible.
//
// The one judgement this file does make is what a failure costs. A failed step
// makes its DEPENDENT SUBTREE unsafe -- those steps were told to wait for
// something that did not happen -- and nothing else. Independent branches run
// to completion, so the operator sees every failure this run has rather than
// the first one and a shrug.
//
// Free of `vscode` imports -- cli.ts runs this under plain node.
//
// Refs: #3373 #3357

import {
  evaluateVerify,
  resolvePreExisting,
  topoOrder,
  type Graph,
  type Step,
} from "./graph.js";
import { appendReceiptEntry, type ReceiptEntry } from "./receipt.js";
import { runCapabilityScript, type CapabilityEnvelope, type RunScript } from "./runner.js";

/**
 * What became of a step.
 *
 * `preserved` is its own status rather than a flavour of failure: an uninstall
 * step that refuses because the artifact pre-existed is the system working as
 * designed -- the operator keeps their own k3d cluster -- and treating it as a
 * failure would abandon every removal downstream of it.
 */
export type StepStatus = "ok" | "failed" | "skipped" | "preserved";

export interface StepOutcome {
  id: string;
  script: string;
  status: StepStatus;
  /** null when the step never ran. */
  exitCode: number | null;
  envelope: CapabilityEnvelope | null;
  /** Whether the step's own verify predicate held. */
  verified: boolean;
  /** The pre-existence verdict, as the receipt recorded it. */
  preExisting: boolean;
  params: Record<string, string>;
  /** Human sentence for a non-ok status. */
  reason?: string;
  /** On a skip: whether the condition dependents were waiting for already holds. */
  satisfied?: boolean;
  startedAt: string;
  finishedAt: string;
}

export interface ExecutionReport {
  graph: string;
  /** True when no step failed. Skips and preservations are not failures. */
  ok: boolean;
  waves: string[][];
  outcomes: StepOutcome[];
  /**
   * The run stopped because the caller asked it to, rather than because the
   * graph finished.
   *
   * Separate from `ok`, which keeps its meaning: nothing FAILED. A cancelled
   * run is usually ok by that definition -- every step that ran, worked -- and
   * folding the two would make "did anything break?" unanswerable. A caller
   * that needs "did the whole graph run?" asks both.
   */
  cancelled?: boolean;
}

/** What the caller wants done with a step. */
export type StepPlan =
  | {
      action: "run";
      /** Run-time params: everything the graph deliberately does not pin. */
      params: Record<string, string>;
      /**
       * The caller knows this step's artifact pre-existed, so the script's
       * exit-3 refusal is the expected, correct outcome rather than a failure.
       */
      preservedOnRefusal?: boolean;
    }
  | {
      action: "skip";
      reason: string;
      /**
       * The state this step would have established already holds, so the steps
       * waiting on it may proceed.
       *
       * This is what an uninstall needs. `removeCheckout` waits for
       * `removeCluster`; when no cluster was ever created there is nothing to
       * remove, and the condition removeCheckout was waiting for -- no cluster
       * -- is already true. Without this distinction an install that stopped
       * before the cluster leaves an uninstall that skips everything and takes
       * nothing back. Absent (the default) means the skip BLOCKS dependents,
       * which is the right reading on the install side: no cluster, no secret
       * seeded into it.
       */
      satisfied?: boolean;
    };

export type ExecEvent =
  | { type: "waveStarted"; index: number; ids: string[] }
  | { type: "stepStarted"; step: Step; params: Record<string, string> }
  | { type: "stepLog"; step: Step; line: string }
  | { type: "stepFinished"; step: Step; outcome: StepOutcome };

export interface ExecuteOptions {
  graph: Graph;
  /** Resolves a step to the capability script that runs it. */
  scriptPath: (step: Step) => string;
  /** Run-time params, or a decision to skip. Defaults to running with none. */
  plan?: (step: Step) => StepPlan;
  /** Injectable for tests; defaults to the real spawn-based runner. */
  run?: RunScript;
  /** When set, every executed step is recorded here as it finishes. */
  receiptFile?: string;
  onEvent?: (event: ExecEvent) => void | Promise<void>;
  /**
   * Stops the run at the next WAVE BOUNDARY.
   *
   * Deliberately not mid-step. A capability script is the thing that creates a
   * k3d cluster or writes a hosts block, and killing one partway through would
   * leave that artifact half-made AND unrecorded -- the receipt entry is
   * written when the step finishes, so an interrupted step is exactly the case
   * the uninstall could not clean up afterwards. Letting the in-flight wave
   * complete costs the operator one step's wait and keeps the receipt a true
   * account of the machine, which is what makes Cancel safe to offer at all.
   */
  signal?: AbortSignal;
  /** Per-step timeout in milliseconds. */
  timeoutMs?: number;
  cwd?: string;
  env?: NodeJS.ProcessEnv;
}

const REFUSED = 3;

/**
 * Runs a graph.
 *
 * Wave by wave: everything in a wave starts together and the wave is awaited
 * before the next begins, which is exactly the schedule the graph document
 * describes. A step whose dependency failed or was skipped is skipped without
 * being invoked -- it was told to wait for something that did not happen.
 */
export async function executeGraph(options: ExecuteOptions): Promise<ExecutionReport> {
  const { graph } = options;
  const run = options.run ?? runCapabilityScript;
  const plan = options.plan ?? ((): StepPlan => ({ action: "run", params: {} }));
  const waves = topoOrder(graph);
  const outcomes = new Map<string, StepOutcome>();

  let cancelled = false;
  for (const [index, wave] of waves.entries()) {
    // Checked between waves, never inside one. See ExecuteOptions.signal.
    if (options.signal?.aborted === true) {
      cancelled = true;
      break;
    }
    await emit(options, { type: "waveStarted", index, ids: wave });
    await Promise.all(
      wave.map(async (id) => {
        const step = graph.steps.find((s) => s.id === id)!;
        const outcome = await runStep(options, run, plan, step, outcomes);
        outcomes.set(id, outcome);
        await emit(options, { type: "stepFinished", step, outcome });
      }),
    );
  }

  const ordered = graph.steps.map((s) => outcomes.get(s.id)!).filter(Boolean);
  return {
    graph: graph.name,
    ok: ordered.every((o) => o.status !== "failed"),
    waves,
    outcomes: ordered,
    ...(cancelled ? { cancelled: true } : {}),
  };
}

async function runStep(
  options: ExecuteOptions,
  run: RunScript,
  plan: (step: Step) => StepPlan,
  step: Step,
  outcomes: Map<string, StepOutcome>,
): Promise<StepOutcome> {
  const startedAt = new Date().toISOString();

  // A dependency that failed, or was skipped without satisfying what it was
  // there to establish, makes this step unsafe: it was told to wait for
  // something that did not happen.
  const blocked = (step.dependsOn ?? []).filter((d) => {
    const o = outcomes.get(d);
    return !o || o.status === "failed" || (o.status === "skipped" && !o.satisfied);
  });
  if (blocked.length > 0) {
    return skipped(step, startedAt, `skipped: ${blocked.join(", ")} did not complete`, false);
  }

  const decision = plan(step);
  if (decision.action === "skip") {
    return skipped(step, startedAt, decision.reason, decision.satisfied === true);
  }

  // Graph-pinned params WIN over run-time ones. What the graph pins is policy
  // (which tool, which confirmation phrase); what the caller supplies is run
  // input, and run input does not get to rewrite policy.
  const params = { ...decision.params, ...(step.params ?? {}) };

  await emit(options, { type: "stepStarted", step, params });

  const outcome = await run({
    scriptPath: options.scriptPath(step),
    params,
    capability: step.script,
    cwd: options.cwd,
    env: options.env,
    timeoutMs: options.timeoutMs,
    onLog: (line) => void emit(options, { type: "stepLog", step, line }),
  });

  const envelope = outcome.envelope;
  const finishedAt = new Date().toISOString();
  const base: StepOutcome = {
    id: step.id,
    script: step.script,
    status: "failed",
    exitCode: outcome.exitCode,
    envelope,
    verified: false,
    preExisting: false,
    params,
    startedAt,
    finishedAt,
  };

  if (!envelope) {
    // No envelope means we know nothing about the machine, which is not the
    // same as knowing nothing went wrong.
    return { ...base, reason: outcome.parseError ?? "the script produced no result envelope" };
  }

  base.preExisting = resolvePreExisting(step, envelope);

  // An expected refusal: the artifact was already here, and remove-artifact.sh
  // is the last wall that says so. The system worked; the dependents proceed.
  if (decision.preservedOnRefusal && outcome.exitCode === REFUSED) {
    const preserved: StepOutcome = {
      ...base,
      status: "preserved",
      preExisting: true,
      reason: envelope.error?.message ?? "refused: the artifact was already here before the install",
    };
    await record(options, step, preserved);
    return preserved;
  }

  // THE VERIFY: read off result{}, never off the exit code.
  base.verified = evaluateVerify(step.verify, envelope);
  if (!base.verified) {
    const detail = envelope.error?.message ?? describeVerify(step);
    const withStatus: StepOutcome = {
      ...base,
      reason:
        outcome.exitCode === 0
          ? `the script exited 0 but its verify did not hold: ${detail}`
          : `exit ${outcome.exitCode}: ${detail}`,
    };
    await record(options, step, withStatus);
    return withStatus;
  }

  const ok: StepOutcome = { ...base, status: "ok" };
  await record(options, step, ok);
  return ok;
}

function skipped(step: Step, startedAt: string, reason: string, satisfied: boolean): StepOutcome {
  return {
    id: step.id,
    script: step.script,
    status: "skipped",
    exitCode: null,
    envelope: null,
    verified: false,
    preExisting: false,
    params: {},
    reason,
    satisfied,
    startedAt,
    finishedAt: new Date().toISOString(),
  };
}

function describeVerify(step: Step): string {
  const v = step.verify;
  if (v.kind === "scriptOk") return "the envelope reported ok:false";
  return v.kind === "resultEquals"
    ? `${v.field} is not ${JSON.stringify(v.value)}`
    : `${v.field} did not satisfy ${v.kind}`;
}

/**
 * Writes this step to the receipt before the run moves on.
 *
 * Recorded for every step that produced an envelope, INCLUDING a failed one: a
 * step that failed half-way may still have left an artifact, and a receipt that
 * omits it is an artifact nobody can take back. A step that produced no
 * envelope is not recorded -- there is nothing to key a removal off, and an
 * entry with no target would only make the uninstall exit 2.
 */
async function record(options: ExecuteOptions, step: Step, outcome: StepOutcome): Promise<void> {
  if (!options.receiptFile || !outcome.envelope) return;
  const entry: ReceiptEntry = {
    stepId: step.id,
    script: step.script,
    receipt: step.receipt ?? "",
    preExisting: outcome.preExisting,
    params: outcome.params,
    result: outcome.envelope.result,
    changed: outcome.envelope.changed,
    recordedAt: outcome.finishedAt,
  };
  await appendReceiptEntry(options.receiptFile, options.graph.name, entry);
}

async function emit(options: ExecuteOptions, event: ExecEvent): Promise<void> {
  if (!options.onEvent) return;
  await options.onEvent(event);
}
