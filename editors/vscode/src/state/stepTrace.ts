// The step trace's model: a TIMELINE that fills in live, not a row list.
//
// B2's result view answers "what did this construct return" -- rows, grouped
// by concept, rendered through each concept's own @displayCard. An automation
// returns nothing of the sort. What a developer wants from an automation run
// is the SEQUENCE: which steps ran, in what order, how long each took, which
// one broke. That is a different question, so it gets a different surface,
// and this module is the state behind it.
//
// THREE THINGS ARE LOAD-BEARING:
//
//  1. ORDER IS `sequence`, NEVER ARRIVAL. Steps stream back as they complete
//     and can be reordered in flight (the engine notes as much: the trace
//     rides the event bus on the cross-node path). Rendering in arrival order
//     would show a timeline whose order is a property of the network. Every
//     read sorts by `sequence`.
//
//  2. A REFUSAL IS NOT A FAILED RUN. The engine distinguishes "the automation
//     could not be attempted" (unknown name, wrong role, @disabled, a @filter
//     miss, nobody picked it up -- an AutomationRunError) from "the automation
//     ran and broke" (a normal resolve with complete.status === "failed" and
//     the trace intact). Only the second has a timeline worth rendering, and
//     collapsing them into one error state would tell a developer their
//     automation is broken when in fact it never started.
//
//  3. THE TRACE EXISTS BEFORE THE FIRST STEP. `onAccepted` fires ahead of any
//     step, so the header and the deployed-definition banner render while the
//     run is still in flight. A model that could only be built from a finished
//     result would make the live timeline impossible.
//
// Deliberately free of `vscode` imports; webview/automationPanel.ts renders
// this. Tested under bare `node --test`.

import type {
  AutomationRunAccepted,
  AutomationRunComplete,
  AutomationRunStep,
} from "@znasllc-io/memql-sdk-core/automation";

/** Where a run is, from the trace's point of view. */
export type TraceStatus =
  /** Accepted (or not yet), steps still arriving. */
  | "running"
  /** The run finished; complete.status says how. */
  | "completed"
  | "failed"
  | "cancelled"
  /** The engine REFUSED the run -- it never started. */
  | "refused"
  /** The request never reached a runner (no cluster, transport failure, declined). */
  | "error";

/** A refusal, kept apart from a failure. See point 2 in the module comment. */
export interface TraceRefusal {
  code: number;
  codeName: string;
  message: string;
  runId: string;
  /** Present when the refusal arrived after the accepted frame. */
  accepted?: AutomationRunAccepted;
  /**
   * The automation was known to be @disabled in the buffer BEFORE the run.
   *
   * The engine answers @disabled and a @filter miss with the same
   * FAILED_PRECONDITION and nothing in the reply separates them, so this is
   * the only thing that can (memql#3333). It comes from the language server's
   * `disabled` flag on the construct, carried through the run target -- not
   * from the refusal itself.
   *
   * Absent means "not known to be disabled", not "enabled": an older language
   * server does not report the flag at all. So absence keeps the old
   * both-possibilities wording rather than asserting the filter rejected it.
   */
  disabled?: boolean;
}

export class StepTraceModel {
  private acceptedFrame: AutomationRunAccepted | undefined;
  private completeFrame: AutomationRunComplete | undefined;
  private refusalValue: TraceRefusal | undefined;
  private errorValue = "";
  private runIdValue = "";

  // Keyed by `sequence` rather than appended, because a step can legitimately
  // arrive twice: the cross-node path relays each frame over the bus, and a
  // duplicate delivery is a property of at-least-once delivery, not a bug to
  // crash on. Last write wins, which is right -- a redelivery carries the same
  // outcome.
  private readonly bySequence = new Map<number, AutomationRunStep>();

  get accepted(): AutomationRunAccepted | undefined {
    return this.acceptedFrame;
  }

  get complete(): AutomationRunComplete | undefined {
    return this.completeFrame;
  }

  get refusal(): TraceRefusal | undefined {
    return this.refusalValue;
  }

  get error(): string {
    return this.errorValue;
  }

  get runId(): string {
    return this.runIdValue;
  }

  /** The steps, always in `sequence` order. */
  get steps(): AutomationRunStep[] {
    return [...this.bySequence.values()].sort((a, b) => a.sequence - b.sequence);
  }

  get status(): TraceStatus {
    if (this.errorValue !== "") return "error";
    if (this.refusalValue !== undefined) return "refused";
    const status = this.completeFrame?.status ?? "";
    if (status === "completed" || status === "failed" || status === "cancelled") return status;
    // An unknown terminal status is treated as still running rather than
    // guessed at: a status this build does not know is a newer engine's, and
    // inventing a mapping for it would report an outcome nobody asserted.
    return "running";
  }

  /** True once the run has reached any terminal state. */
  get settled(): boolean {
    return this.status !== "running";
  }

  noteRunId(runId: string): void {
    if (runId !== "") this.runIdValue = runId;
  }

  noteAccepted(accepted: AutomationRunAccepted): void {
    this.acceptedFrame = accepted;
  }

  noteStep(step: AutomationRunStep): void {
    this.bySequence.set(step.sequence, step);
  }

  noteComplete(complete: AutomationRunComplete): void {
    this.completeFrame = complete;
  }

  noteRefusal(refusal: TraceRefusal): void {
    this.refusalValue = refusal;
    if (refusal.runId !== "") this.runIdValue = refusal.runId;
    // A refusal can arrive after the accepted frame (the engine sends
    // `accepted` and then refuses on the @filter), so keep whichever we have
    // rather than overwriting a good frame with an absent one.
    if (this.acceptedFrame === undefined && refusal.accepted !== undefined) {
      this.acceptedFrame = refusal.accepted;
    }
  }

  /** noteError records a failure that is not the engine's answer at all -- no cluster, transport down, the form declined. */
  noteError(message: string): void {
    this.errorValue = message;
  }

  /** counts tallies the steps by status, for the timeline's summary line. */
  get counts(): { success: number; failed: number; skipped: number; other: number } {
    const out = { success: 0, failed: 0, skipped: 0, other: 0 };
    for (const step of this.bySequence.values()) {
      if (step.status === "success") out.success++;
      else if (step.status === "failed") out.failed++;
      else if (step.status === "skipped") out.skipped++;
      else out.other++;
    }
    return out;
  }
}

/**
 * formatDuration renders a step's millisecond duration.
 *
 * Sub-second durations keep their milliseconds because most automation steps
 * are one of those and "0s" for all of them would make the column useless.
 */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "-";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(ms < 10000 ? 2 : 1)}s`;
}

/**
 * describeRefusal turns a refusal code into text that says WHAT TO DO.
 *
 * The spec's error-handling table asks for exactly this on two of these rows:
 * an insufficient role "names the role it requires, never a silent no-op", and
 * a cluster-side refusal gets an actionable message. The rest follow the same
 * rule -- each code has one likely cause and one next action, and the engine's
 * own message is appended rather than replaced, because it carries the
 * specifics (which automation, which filter).
 */
export function describeRefusal(refusal: TraceRefusal): string {
  const suffix = refusal.message.trim() === "" ? "" : ` -- ${refusal.message}`;
  switch (refusal.code) {
    case 3: // INVALID_ARGUMENT
      return `The request could not be turned into a trigger event${suffix}. The automation's trigger pattern may be a glob the engine cannot make concrete: name the concept the payload row belongs to.`;
    case 4: // DEADLINE_EXCEEDED
      return `The run outlived its timeout${suffix}. Work already dispatched is NOT cancelled -- it may still be running on the cluster.`;
    case 5: // NOT_FOUND
      return `No automation of that name is registered on the answering node${suffix}. Check the name, and whether the definition is deployed to the node type this run targeted.`;
    case 7: // PERMISSION_DENIED
      return `Running an automation requires the CLUSTER OWNER or an ADMIN role${suffix}. Your identity on this cluster does not hold either.`;
    case 8: // RESOURCE_EXHAUSTED
      return `Too many operator-initiated runs are already in flight on that node${suffix}. Wait for one to finish and try again.`;
    case 9: // FAILED_PRECONDITION
      // The engine gives @disabled and a @filter miss the same code, so this
      // branch is the only place the two can be told apart -- and only when
      // the language server reported the flag (memql#3333). Naming the actual
      // cause matters because the two have opposite next actions: re-enable
      // the construct, versus fix the payload.
      if (refusal.disabled === true) {
        return `The automation is @disabled${suffix}, so the loader skipped it and no run of it can be attempted. Its @filter was never consulted. Remove the annotation and redeploy to run it.`;
      }
      return `The automation refused this event${suffix}. It is either @disabled, or its @filter rejects the payload -- which is the same question a real trigger would have asked.`;
    case 14: // UNAVAILABLE
      return `No node of the requested type picked the run up${suffix}. That node type is not running, or the run topics are not being forwarded across the mesh.`;
    default:
      return `The engine refused the run (${refusal.codeName})${suffix}.`;
  }
}
