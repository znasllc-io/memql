// The typed automation-run surface (memql#3310).
//
// Automations had no invoke path on ANY surface. An automation is registered
// against a bus subscription when the DSL tree loads, so the only way to
// exercise one was to cause its trigger event for real -- which is what made
// automations the hardest construct in the DSL to test, and why the VS Code
// runtime panel could offer a Run affordance for every other runnable kind but
// not this one.
//
// THE DEPLOYED DEFINITION ALWAYS RUNS. Session-define
// (AuthoringSessionDefineBundleMsg) covers the plain construct family --
// query / mutate / logic / spec / trait -- and deliberately not automations,
// which are dispatched by subscription rather than resolved by name at call
// time. A caller who just edited an automation in a buffer and pressed Run is
// therefore running the version on the cluster. `accepted.ranDeployedDefinition`
// and `accepted.definitionNote` carry that fact as DATA so the UI can render a
// banner rather than the developer having to know.
//
// A RUN IS NOT A ROUND-TRIP. The engine streams the trace: one `accepted`
// frame, one frame per step as it completes, then exactly one `complete`. The
// client below reassembles them, calling the caller's per-frame hooks as they
// land so a timeline can render live, and resolving with the whole trace.
//
// AUTHORIZATION IS SERVER-SIDE, ALWAYS. Nothing here checks a role. A run
// executes an automation's whole action chain -- writes, LLM calls, downstream
// automations -- so the engine gates it on owner/admin, the same gate the HTTP
// POST /automations/trigger endpoint carries. A refusal arrives as an
// AutomationRunError with code CODE_PERMISSION_DENIED.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";
import type {
  AutomationRunAcceptedWire,
  AutomationRunCompleteWire,
  AutomationRunStepWire,
  ServerMessage,
} from "../client/wire.js";

// -----------------------------------------------------------------------------
// Plain result types (no wire shapes leak to consumers)
// -----------------------------------------------------------------------------

/**
 * The opening frame of every run. It arrives BEFORE any step, so a UI can
 * render its header and its banner while the run is still in flight.
 */
export interface AutomationRunAccepted {
  /** The resolved automation name. */
  automation: string;
  /**
   * Always true. Render it: the caller has to be told that the cluster's
   * definition ran, not the buffer they are looking at.
   */
  ranDeployedDefinition: boolean;
  /** Ready-to-render banner text for the above. */
  definitionNote: string;
  /** "event" | "schedule" | "manual". */
  triggerKind: string;
  /** The synthesized event's topic. Empty for a schedule-kind run. */
  triggerTopic: string;
  /** The node that received the request -- the cluster the run ran against. */
  requestedOnNodeId: string;
  requestedOnNodeType: string;
  /** Echo of the requested target ("" = the receiving node). */
  targetNodeType: string;
}

/** One step's outcome, delivered as the step completes. */
export interface AutomationRunStep {
  /** 0-based position in the trace. */
  sequence: number;
  /** The step's declared name in the automation body. */
  stepId: string;
  /** "success" | "failed" | "skipped". */
  status: string;
  durationMs: number;
  /** Present only when includeStepOutput was requested. */
  output?: Record<string, unknown>;
  /** The step's error message when status is "failed". */
  error: string;
}

/** The terminating frame. Exactly one per run. */
export interface AutomationRunComplete {
  /** "completed" | "failed" | "cancelled" (a refusal throws instead). */
  status: string;
  durationMs: number;
  stepCount: number;
  /** The automation-level error when a started run failed. */
  error: string;
  /** The node that actually ran the automation. */
  executedOnNodeId: string;
  executedOnNodeType: string;
}

/** A completed run: the banner, the timeline, and the outcome. */
export interface AutomationRunResult {
  /** The execution id, which joins to the engine's logs and checkpoints. */
  runId: string;
  accepted: AutomationRunAccepted;
  steps: AutomationRunStep[];
  complete: AutomationRunComplete;
}

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

/**
 * Canonical gRPC codes this surface returns, by number. The engine carries
 * the code inside the terminating frame rather than as a status error,
 * because an error out of a multiplexed stream handler tears the stream down
 * and takes every other in-flight request with it.
 */
const GRPC_CODE_NAMES: Record<number, string> = {
  0: "OK",
  3: "INVALID_ARGUMENT",
  4: "DEADLINE_EXCEEDED",
  5: "NOT_FOUND",
  7: "PERMISSION_DENIED",
  8: "RESOURCE_EXHAUSTED",
  9: "FAILED_PRECONDITION",
  13: "INTERNAL",
  14: "UNAVAILABLE",
};

/** INVALID_ARGUMENT -- the request could not be turned into a trigger event. */
export const CODE_INVALID_ARGUMENT = 3;
/** DEADLINE_EXCEEDED -- the run outlived its timeout. Work already dispatched is NOT cancelled. */
export const CODE_DEADLINE_EXCEEDED = 4;
/** NOT_FOUND -- no automation of that name is registered on the answering node. */
export const CODE_NOT_FOUND = 5;
/** PERMISSION_DENIED -- running an automation requires a cluster owner or admin. */
export const CODE_PERMISSION_DENIED = 7;
/** RESOURCE_EXHAUSTED -- too many operator runs already in flight on that node. */
export const CODE_RESOURCE_EXHAUSTED = 8;
/** FAILED_PRECONDITION -- the automation is @disabled, or its @filter rejects this event. */
export const CODE_FAILED_PRECONDITION = 9;
/** UNAVAILABLE -- no node of the requested type picked the run up. */
export const CODE_UNAVAILABLE = 14;

/**
 * A run the engine REFUSED -- it never started.
 *
 * Distinct from a run that started and failed, which resolves normally with
 * `complete.status === "failed"` and its step trace intact. That split is the
 * engine's, not this client's: "the automation could not be attempted" and
 * "the automation ran and broke" are different answers to a developer's
 * question, and only the second one has a timeline worth rendering.
 */
export class AutomationRunError extends Error {
  readonly code: number;
  readonly codeName: string;
  readonly runId: string;
  /** The accepted frame, so a UI can still show which node refused. */
  readonly accepted?: AutomationRunAccepted;

  constructor(automation: string, code: number, message: string, runId: string, accepted?: AutomationRunAccepted) {
    const name = GRPC_CODE_NAMES[code] ?? String(code);
    super(`run automation ${automation}: ${name}: ${message || "(no message)"}`);
    this.name = "AutomationRunError";
    this.code = code;
    this.codeName = name;
    this.runId = runId;
    this.accepted = accepted;
  }

  /** True when the refusal was the role gate, not a failure. */
  get isPermissionDenied(): boolean {
    return this.code === CODE_PERMISSION_DENIED;
  }

  /**
   * True when no node of the requested target type answered. In a mesh this
   * usually means that node type is not running -- or that the run topics are
   * not being forwarded across it.
   */
  get isUnavailable(): boolean {
    return this.code === CODE_UNAVAILABLE;
  }
}

// -----------------------------------------------------------------------------
// Client
// -----------------------------------------------------------------------------

export interface RunAutomationOptions {
  /** The automation's DSL name. Required. */
  automation: string;

  /**
   * The synthesized trigger event's payload -- what the automation body reads
   * as args.payload.<field>.
   *
   * There is no declared payload schema in the general case: an automation
   * binds its whole triggering event as `args` and reads through it freely.
   * The two ways to build one are to project an existing row of the trigger
   * concept, or to paste JSON.
   *
   * Omit it for a @trigger(schedule=...) automation: it has no concept and no
   * event, and fires now with an EMPTY event -- exactly what a cron firing
   * hands the executor.
   */
  payload?: Record<string, unknown>;

  /**
   * The concept the payload row belongs to. Only needed when the automation's
   * trigger pattern is a glob the engine cannot make concrete on its own;
   * otherwise it is derived from the automation's own @trigger.
   */
  concept?: string;

  /** Override the synthesized event's topic outright. Rarely needed. */
  topic?: string;

  /**
   * Run on a specific node type ("cognition", "agent", "planner", ...).
   *
   * Omit for "the node that receives this request", which is the ordinary
   * case. Set it when the automation's steps reach integrations compiled into
   * one node type only -- testing such an automation means running it where
   * those integrations live.
   */
  targetNodeType?: string;

  /**
   * Wall-clock bound on the run. Zero/omitted takes the server default (60s);
   * the server also caps it (300s). A timeout does NOT cancel work already
   * dispatched on another node.
   */
  timeoutMs?: number;

  /**
   * Attach each step's own result payload to its trace entry. Off by default:
   * step outputs can be large, and a timeline needs only name / status /
   * duration until a row is expanded.
   */
  includeStepOutput?: boolean;

  /** Called once, before any step, so a UI can render its header immediately. */
  onAccepted?: (accepted: AutomationRunAccepted) => void;

  /** Called per step as it completes, so the timeline fills in live. */
  onStep?: (step: AutomationRunStep) => void;

  signal?: AbortSignal;
}

/**
 * A thin typed client over the RunAutomation surface.
 *
 * Construct it from a Dispatcher (the same one Connection exposes) and call
 * run(). It sends one RunAutomationMsg, reassembles the streamed frames, and
 * resolves with the complete trace.
 */
export class AutomationClient {
  constructor(private readonly dispatcher: Dispatcher) {
    if (!dispatcher) throw new Error("AutomationClient: dispatcher is required");
  }

  async run(opts: RunAutomationOptions): Promise<AutomationRunResult> {
    const automation = (opts.automation ?? "").trim();
    if (!automation) throw new Error("run: automation is required");

    const requestId = newShortId();

    return await new Promise<AutomationRunResult>((resolve, reject) => {
      let runId = "";
      let accepted: AutomationRunAccepted | undefined;
      const steps: AutomationRunStep[] = [];
      let settled = false;

      const finish = (fn: () => void): void => {
        if (settled) return;
        settled = true;
        unregister();
        opts.signal?.removeEventListener("abort", onAbort);
        fn();
      };

      const unregister = this.dispatcher.registerStream(requestId, (msg: ServerMessage) => {
        const payload = readServerPayload(msg);

        // A refusal from an interceptor (badge gate, surface pinning) arrives
        // as a QueryError rather than as a run frame. Surface it as a run
        // failure instead of leaving the caller parked forever.
        if (payload?.kind === "queryError") {
          finish(() =>
            reject(
              new AutomationRunError(
                automation,
                CODE_PERMISSION_DENIED,
                payload.value.error?.message ?? "rejected before reaching the automation runner",
                runId,
                accepted,
              ),
            ),
          );
          return;
        }
        if (payload?.kind !== "automationRunEvent") return;

        const evt = payload.value;
        if (evt.runId) runId = evt.runId;

        if (evt.accepted) {
          accepted = acceptedFromWire(evt.accepted);
          opts.onAccepted?.(accepted);
          return;
        }
        if (evt.step) {
          const step = stepFromWire(evt.step);
          steps.push(step);
          opts.onStep?.(step);
          return;
        }
        if (evt.complete) {
          const complete = completeFromWire(evt.complete);
          const code = evt.complete.errorCode ?? 0;
          if (code !== 0) {
            finish(() =>
              reject(
                new AutomationRunError(
                  automation,
                  code,
                  evt.complete?.errorMessage ?? "",
                  runId,
                  accepted,
                ),
              ),
            );
            return;
          }
          finish(() =>
            resolve({
              runId,
              // The engine always sends accepted first, but a client must not
              // crash if a frame is lost: an empty banner is a worse result
              // than a thrown TypeError only in theory.
              accepted: accepted ?? emptyAccepted(automation),
              steps,
              complete,
            }),
          );
        }
      });

      const onAbort = (): void => {
        finish(() => reject(new Error(`run automation ${automation}: aborted`)));
      };
      if (opts.signal) {
        if (opts.signal.aborted) {
          onAbort();
          return;
        }
        opts.signal.addEventListener("abort", onAbort, { once: true });
      }

      try {
        this.dispatcher.send({
          runAutomation: {
            requestId,
            automation,
            ...(opts.payload ? { payload: opts.payload } : {}),
            ...(opts.concept ? { concept: opts.concept } : {}),
            ...(opts.topic ? { topic: opts.topic } : {}),
            ...(opts.targetNodeType ? { targetNodeType: opts.targetNodeType } : {}),
            ...(opts.timeoutMs ? { timeoutMs: opts.timeoutMs } : {}),
            ...(opts.includeStepOutput ? { includeStepOutput: true } : {}),
          },
        });
      } catch (err) {
        finish(() => reject(err));
      }
    });
  }
}

// -----------------------------------------------------------------------------
// wire -> plain translation
// -----------------------------------------------------------------------------

function acceptedFromWire(w: AutomationRunAcceptedWire): AutomationRunAccepted {
  return {
    automation: w.automation ?? "",
    ranDeployedDefinition: w.ranDeployedDefinition === true,
    definitionNote: w.definitionNote ?? "",
    triggerKind: w.triggerKind ?? "",
    triggerTopic: w.triggerTopic ?? "",
    requestedOnNodeId: w.requestedOnNodeId ?? "",
    requestedOnNodeType: w.requestedOnNodeType ?? "",
    targetNodeType: w.targetNodeType ?? "",
  };
}

function stepFromWire(w: AutomationRunStepWire): AutomationRunStep {
  const step: AutomationRunStep = {
    sequence: w.sequence ?? 0,
    stepId: w.stepId ?? "",
    status: w.status ?? "",
    durationMs: toNumber(w.durationMs),
    error: w.error ?? "",
  };
  if (w.output) step.output = w.output;
  return step;
}

function completeFromWire(w: AutomationRunCompleteWire): AutomationRunComplete {
  return {
    status: w.status ?? "",
    durationMs: toNumber(w.durationMs),
    stepCount: w.stepCount ?? 0,
    error: w.error ?? "",
    executedOnNodeId: w.executedOnNodeId ?? "",
    executedOnNodeType: w.executedOnNodeType ?? "",
  };
}

function emptyAccepted(automation: string): AutomationRunAccepted {
  return {
    automation,
    ranDeployedDefinition: true,
    definitionNote: "",
    triggerKind: "",
    triggerTopic: "",
    requestedOnNodeId: "",
    requestedOnNodeType: "",
    targetNodeType: "",
  };
}

// protojson renders int64 as a STRING, so duration_ms arrives as "11" over the
// WebSocket bridge and as 11 from a hand-written fixture. Normalising here
// keeps that entirely out of the consumer's way.
function toNumber(v: number | string | undefined): number {
  if (typeof v === "number") return v;
  if (typeof v === "string" && v !== "") {
    const n = Number(v);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}
