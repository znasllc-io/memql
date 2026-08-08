// The automation run loop: preflight -> confirm -> stream a step trace.
//
// Shorter than B2's orchestrator (run/orchestrator.ts) by exactly the part
// that does not apply: there is no bundle, no Gate-1 validation and no
// session-define, because AN AUTOMATION IS NOT SESSION-DEFINABLE. Session
// define covers the plain construct family (query / mutate / logic / spec /
// trait); an automation is registered against a bus subscription when the DSL
// tree loads, so it is dispatched by subscription rather than resolved by name
// at call time. Injecting the buffer would change nothing about what runs.
//
// That single fact drives two requirements this module exists to satisfy:
//
//  1. THE UI MUST SAY THE DEPLOYED DEFINITION RAN. Same Run button as B2's,
//     different semantics. The engine hands the sentence over as data
//     (`accepted.ranDeployedDefinition` + `accepted.definitionNote`) precisely
//     so no client has to know the rule; the outcome below carries it through
//     untouched, and state/automationForm.ts's definitionBanner() is what
//     renders it.
//
//  2. THE NON-LOCAL WRITE CONFIRMATION COVERS AUTOMATIONS. An automation run
//     executes the whole action chain -- writes, LLM calls, downstream
//     automations -- so it is a write kind, and it goes through THE SAME
//     WriteConfirmationGate B2 uses (run/preflight.ts), not a second
//     confirmation path. The gate instance is injected rather than
//     constructed here, so one acknowledgement policy and one reset
//     (clusters.yaml changed) covers both surfaces.
//
// A REFUSAL IS NOT A FAILED RUN. The SDK throws AutomationRunError when the
// engine refused to attempt the run at all and resolves normally when the run
// started and failed. Those are different answers to a developer's question
// and only the second has a timeline, so the outcome union keeps them apart
// rather than folding both into "error".
//
// Deliberately free of `vscode` imports; webview/automationPanel.ts is the
// adapter. Tested under bare `node --test`.

import { AutomationRunError } from "@znasllc-io/memql-sdk-core/automation";
import type {
  AutomationRunAccepted,
  AutomationRunResult,
  AutomationRunStep,
} from "@znasllc-io/memql-sdk-core/automation";

import { Latest } from "../async/latest.js";
import type { AutomationTarget } from "../constructs/runnable.js";
import { extractErrorId } from "./call.js";
import type { WriteConfirmationGate } from "./preflight.js";
import type { RunCluster } from "./orchestrator.js";
import { automationConfirmationMessage } from "../state/automationForm.js";
import { StepTraceModel } from "../state/stepTrace.js";

// AutomationTarget is declared next to RunTarget in constructs/runnable.ts (it
// is what a LENS carries, and the lens planner owns that shape) and re-exported
// here so consumers of the run loop do not have to know which module each piece
// lives in -- the same courtesy orchestrator.ts extends for isWriteKind.
export type { AutomationTarget };

/** The request the form builds. Mirrors the SDK's options minus the streaming hooks. */
export interface AutomationRunRequest {
  /** The synthesized trigger event's payload. Omitted entirely for a fire-now run. */
  payload?: Record<string, unknown>;
  /** The concept the payload row belongs to, when the trigger pattern is a glob. */
  concept?: string;
  /** "" runs on the receiving node; anything else is a mesh hop. */
  targetNodeType?: string;
  /** Attach each step's own output to its trace entry. Off by default -- outputs can be large. */
  includeStepOutput?: boolean;
}

/**
 * The narrow engine surface a run needs -- one function, for the same reason
 * RunEngine is four: a stub function is a far cheaper fixture than a live
 * Connection, and this module's behaviour is the part worth testing
 * exhaustively.
 */
export interface AutomationRunEngine {
  runAutomation(
    automation: string,
    request: AutomationRunRequest,
    hooks: {
      onAccepted(accepted: AutomationRunAccepted): void;
      onStep(step: AutomationRunStep): void;
    },
  ): Promise<AutomationRunResult>;
}

export interface AutomationRunDeps {
  /** The selected cluster, or undefined when none is selected. */
  cluster(): RunCluster | undefined;
  /** The live engine surface, or undefined when not connected. */
  engine(): AutomationRunEngine | undefined;
  /** Shows the write confirmation. Resolves true to proceed. */
  confirmWrite(message: string): Promise<boolean>;
  /**
   * B2's gate, SHARED. Injected rather than constructed so an acknowledgement
   * is one decision per (cluster, construct) across both run surfaces, and so
   * the single reset on a clusters.yaml change covers this one too.
   */
  writeGate: WriteConfirmationGate;
}

export type AutomationRunOutcome =
  /** The run STARTED. `trace.complete.status` is "completed", "failed" or "cancelled". */
  | { status: "ok"; target: AutomationTarget; trace: StepTraceModel }
  /** The engine REFUSED the run -- it never started. `trace.refusal` says why. */
  | { status: "refused"; target: AutomationTarget; trace: StepTraceModel }
  /** The user dismissed the non-local write confirmation. */
  | { status: "declined"; target: AutomationTarget }
  /** A newer run superseded this one. Nothing to render. */
  | { status: "superseded" }
  /** The request never reached a runner: no cluster, not connected, transport failure. */
  | {
      status: "error";
      target: AutomationTarget;
      phase: AutomationRunPhase;
      message: string;
      errorId: string;
      trace: StepTraceModel;
    };

/** Where a failure happened, so the UI can say WHERE it broke rather than only that it did. */
export type AutomationRunPhase = "preflight" | "invoke";

export class AutomationRunner {
  // ONE guard over the whole run, taken with begin(): a second Run click IS
  // what makes the first stale, and the confirmation modal in the middle is
  // the longest window in the run for the world to move on underneath. Same
  // reasoning as RunOrchestrator's; see src/async/latest.ts for why there is
  // exactly one guard type in this extension.
  private readonly latest = new Latest<"automationRun">();

  constructor(private readonly deps: AutomationRunDeps) {}

  /**
   * run drives one automation run, filling `trace` as frames land.
   *
   * The trace model is passed IN rather than returned at the end, because the
   * timeline has to render while the run is in flight -- `onAccepted` fires
   * before any step, and steps arrive one at a time. `onProgress` fires after
   * every frame so the adapter can repaint; it is called only while this run
   * is still the current one, so a superseded run cannot paint over a newer
   * one's timeline.
   */
  async run(
    target: AutomationTarget,
    request: AutomationRunRequest,
    trace: StepTraceModel,
    onProgress: () => void = () => {},
  ): Promise<AutomationRunOutcome> {
    const token = this.latest.begin();

    const cluster = this.deps.cluster();
    if (cluster === undefined) {
      return this.fail(target, trace, "preflight", "No cluster selected. Pick one in the Clusters view.");
    }
    const engine = this.deps.engine();
    if (engine === undefined) {
      return this.fail(
        target,
        trace,
        "preflight",
        `Not connected to ${cluster.label}. Select the cluster in the Clusters view to connect.`,
      );
    }

    // "automation" is a write kind (see constructs/runnable.ts), so this is
    // the ordinary path rather than an automation-specific rule bolted on.
    if (this.deps.writeGate.required("automation", cluster.local, cluster.name, target.name)) {
      const confirmed = await this.deps.confirmWrite(
        automationConfirmationMessage(target.name, cluster.label),
      );
      if (!this.latest.isCurrent(token)) return { status: "superseded" };
      if (!confirmed) return { status: "declined", target };
      this.deps.writeGate.acknowledge(cluster.name, target.name);
    }

    let result: AutomationRunResult;
    try {
      result = await engine.runAutomation(target.name, request, {
        onAccepted: (accepted) => {
          if (!this.latest.isCurrent(token)) return;
          trace.noteAccepted(accepted);
          onProgress();
        },
        onStep: (step) => {
          if (!this.latest.isCurrent(token)) return;
          trace.noteStep(step);
          onProgress();
        },
      });
    } catch (err) {
      if (!this.latest.isCurrent(token)) return { status: "superseded" };
      // The REFUSAL branch. An AutomationRunError means the engine declined to
      // attempt the run; anything else means the request never got an answer
      // at all (the socket died, the dispatcher threw). Reporting a refusal as
      // a generic error would lose the code, and the code is what carries the
      // next action -- see describeRefusal in state/stepTrace.ts.
      if (err instanceof AutomationRunError) {
        trace.noteRefusal({
          code: err.code,
          codeName: err.codeName,
          message: refusalMessage(err),
          runId: err.runId,
          ...(err.accepted !== undefined ? { accepted: err.accepted } : {}),
        });
        onProgress();
        return { status: "refused", target, trace };
      }
      return this.fail(target, trace, "invoke", err instanceof Error ? err.message : String(err));
    }
    if (!this.latest.isCurrent(token)) return { status: "superseded" };

    trace.noteRunId(result.runId);
    trace.noteAccepted(result.accepted);
    // Re-note every step from the resolved result. The hooks already delivered
    // them, but a frame the hooks missed (a superseded window that later
    // became current again is impossible; a dropped hook call is not) would
    // otherwise leave a gap in the timeline -- and the model keys on
    // `sequence`, so re-noting a step already present is a no-op.
    for (const step of result.steps) trace.noteStep(step);
    trace.noteComplete(result.complete);
    onProgress();
    return { status: "ok", target, trace };
  }

  private fail(
    target: AutomationTarget,
    trace: StepTraceModel,
    phase: AutomationRunPhase,
    message: string,
  ): AutomationRunOutcome {
    trace.noteError(message);
    // The engine's ERR- id is pulled out so the UI can render it separately
    // and copyably: it is the only handle a developer has on the server-side
    // log entry for their failure.
    return { status: "error", target, phase, message, errorId: extractErrorId(message), trace };
  }
}

// refusalMessage strips the SDK's own prefix off the error text.
//
// AutomationRunError formats as `run automation <name>: <CODE>: <message>`,
// which is right for a thrown Error's `.message` and wrong for a UI that has
// already rendered the automation's name in the header and the code in its own
// field. What is left is the engine's sentence, which describeRefusal appends
// to its explanation.
function refusalMessage(err: AutomationRunError): string {
  const marker = `: ${err.codeName}: `;
  const at = err.message.indexOf(marker);
  if (at < 0) return err.message;
  const tail = err.message.slice(at + marker.length);
  return tail === "(no message)" ? "" : tail;
}
