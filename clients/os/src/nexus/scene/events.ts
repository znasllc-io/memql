// A goal's history, read out of the rows themselves.
//
// ===========================================================================
// WHY THERE IS NO BACKEND HERE
// ===========================================================================
// MemQL is append-only and every concept this map draws stamps the moments
// that matter -- `createdAt`, `startedAt`, `finishedAt`, an approval's
// `requestedAt` and `decidedAt`. So the timeline Replay scrubs is not a
// recording of anything: it is the rows, read in time order. Nothing is
// persisted for it, nothing is replayed off the bus, and a goal from six
// months ago replays exactly as well as one from this morning.
//
// ===========================================================================
// AN EVENT IS NEVER INVENTED
// ===========================================================================
// The rule this module is really about: a moment with no timestamp produces
// NO event. Not an event at the run's createdAt, not an event at "now", not an
// event ordered after the last one it could plausibly follow. A step with
// `status: "done"` and an empty `finishedAt` is a step whose completion this
// cluster did not record, and the honest rendering of that is a node that
// never lands in the replay -- not a landing at a fabricated time, which would
// put a lie on a scrubber somebody reads as evidence.
//
// There is NO derived event at all in this version, which is a simplification
// the spine bought: the portal had one (a construct going live at its
// bundle's activation, because the construct row records no per-status
// timestamp), and every population here dates its own transitions.
//
// ===========================================================================
// A RETRY RE-LIGHTS, IT DOES NOT DUPLICATE
// ===========================================================================
// Every ATTEMPT row emits its own started/finished pair, and every one of them
// carries the SAME node id (world.ts keys a step's node on `runId:key`). So a
// retried step appears once in the scene and three times in the timeline,
// which is exactly what somebody needs to see: the node did not multiply, it
// went again.
//
// That is also why this walks every step ROW rather than `latestAttempts` --
// collapsing first would erase the retries from the history while leaving them
// in the counter, which is the worst of both.

import {
  approvalNodeId,
  compareSteps,
  GOAL_NODE_ID,
  stepNodeId,
  templateNodeId,
  type GoalWorld,
} from "./world";

export type EventKind =
  | "goal.created"
  | "goal.closed"
  | "run.started"
  | "run.succeeded"
  | "run.failed"
  | "run.cancelled"
  | "step.created"
  | "step.started"
  | "step.completed"
  | "step.failed"
  | "approval.raised"
  | "approval.decided";

export interface SceneEvent {
  /**
   * Stable and unique across a world, so React can key the event list and a
   * scrub position can be compared without an index into an array that a
   * re-read may have re-ordered.
   */
  id: string;
  /**
   * RFC3339, exactly as the row carried it. Never reformatted here: the
   * scrubber compares these as strings and the surface renders them through
   * the OS's own time treatment.
   */
  at: string;
  kind: EventKind;
  /**
   * The node this event lights. The event list doubles as the map's keyboard
   * index, so every event must name a node that exists in the layout -- there
   * is a test for exactly that.
   */
  nodeId: string;
  /** The row behind the event, for the Enter-opens-the-detail path. */
  rowId: string;
  /**
   * A short sentence, already written for a screen reader rather than
   * assembled at render time from three fields and a separator.
   */
  label: string;
  /** >1 marks a retry -- the one thing the list says that a re-light cannot:
   *  WHICH attempt this was. */
  attempt: number;
}

/**
 * The tie-break when two events share a timestamp, which is common -- a step
 * created and started in the same write, an approval raised as its step parks.
 * Creation before start before finish, so a node cannot appear to finish
 * before it arrived.
 */
const KIND_RANK: Record<EventKind, number> = {
  "goal.created": 0,
  "run.started": 1,
  "step.created": 2,
  "step.started": 3,
  "approval.raised": 4,
  "approval.decided": 5,
  "step.completed": 6,
  "step.failed": 6,
  "run.succeeded": 7,
  "run.failed": 7,
  "run.cancelled": 7,
  "goal.closed": 8,
};

function push(out: SceneEvent[], event: SceneEvent | null): void {
  if (event !== null) out.push(event);
}

/**
 * The whole no-invention rule in one function: no timestamp, no event.
 *
 * Whitespace counts as absent -- the wire can carry "" and " " for a datetime
 * that was never written, and neither is a moment.
 */
function at(
  stamp: string,
  build: (moment: string) => Omit<SceneEvent, "id" | "at">,
): SceneEvent | null {
  const moment = stamp.trim();
  if (moment === "") return null;
  const body = build(moment);
  return {
    id: `${body.kind}|${body.nodeId}|${moment}|${body.rowId}|${body.attempt}`,
    at: moment,
    ...body,
  };
}

const RUN_TERMINAL: Record<string, EventKind> = {
  succeeded: "run.succeeded",
  failed: "run.failed",
  cancelled: "run.cancelled",
  abandoned: "run.failed",
};

export function events(world: GoalWorld): SceneEvent[] {
  const out: SceneEvent[] = [];
  const goal = world.goal;
  const run = world.run;

  if (goal !== null) {
    push(
      out,
      at(goal.createdAt, () => ({
        kind: "goal.created",
        nodeId: GOAL_NODE_ID,
        rowId: goal.id,
        label: goal.statement === "" ? "Goal set" : `Goal set: ${goal.statement}`,
        attempt: 1,
      })),
    );
    push(
      out,
      at(goal.closedAt, () => ({
        kind: "goal.closed",
        nodeId: GOAL_NODE_ID,
        rowId: goal.id,
        label: goal.closeReason === "" ? "Goal closed" : `Goal closed: ${goal.closeReason}`,
        attempt: 1,
      })),
    );
  }

  if (run !== null) {
    const runNode = templateNodeId(run);
    push(
      out,
      at(run.startedAt, () => ({
        kind: "run.started",
        nodeId: runNode,
        rowId: run.id,
        label:
          run.automationName === "" ? "Run started" : `Run started: ${run.automationName}`,
        attempt: 1,
      })),
    );
    // The terminal event takes its KIND from the row's status and its MOMENT
    // from finishedAt. A run that is `succeeded` with no `finishedAt` produces
    // nothing, which is the rule applied to the case that would most tempt a
    // fabrication -- the outcome is known and the moment is not.
    const terminal = RUN_TERMINAL[run.status];
    if (terminal !== undefined) {
      push(
        out,
        at(run.finishedAt, () => ({
          kind: terminal,
          nodeId: runNode,
          rowId: run.id,
          label:
            terminal === "run.failed" && run.errorMessage !== ""
              ? `Run failed: ${run.errorMessage}`
              : terminal === "run.cancelled"
                ? "Run cancelled"
                : terminal === "run.failed"
                  ? "Run failed"
                  : "Run finished",
          attempt: 1,
        })),
      );
    }

    // EVERY ATTEMPT ROW, not the latest of each key -- see the header.
    const rows = world.steps.filter((step) => step.runId === run.id).sort(compareSteps);
    for (const step of rows) {
      const nodeId = stepNodeId(step);
      const name = step.key === "" ? step.callName : step.key;
      push(
        out,
        at(step.createdAt, () => ({
          kind: "step.created",
          nodeId,
          rowId: step.id,
          label: `${name} queued`,
          attempt: step.attempt,
        })),
      );
      push(
        out,
        at(step.startedAt, () => ({
          kind: "step.started",
          nodeId,
          rowId: step.id,
          label: step.attempt > 1 ? `${name} started, attempt ${step.attempt}` : `${name} started`,
          attempt: step.attempt,
        })),
      );
      const failed = step.status === "failed";
      push(
        out,
        at(step.finishedAt, () => ({
          kind: failed ? "step.failed" : "step.completed",
          nodeId,
          rowId: step.id,
          label: failed
            ? step.errorMessage === ""
              ? `${name} failed`
              : `${name} failed: ${step.errorMessage}`
            : `${name} finished`,
          attempt: step.attempt,
        })),
      );
    }

    for (const approval of world.approvals.filter((a) => a.runId === run.id)) {
      const nodeId = approvalNodeId(approval);
      const what = approval.kind === "" ? "approval" : approval.kind;
      push(
        out,
        at(approval.requestedAt, () => ({
          kind: "approval.raised",
          nodeId,
          rowId: approval.id,
          label: `Asked you: ${what}`,
          attempt: 1,
        })),
      );
      push(
        out,
        at(approval.decidedAt, () => ({
          kind: "approval.decided",
          nodeId,
          rowId: approval.id,
          label:
            approval.decision === "" ? `Decided: ${what}` : `${approval.decision}: ${what}`,
          attempt: 1,
        })),
      );
    }
  }

  return out.sort(compareEvents);
}

/**
 * Total order over events: moment, then kind rank, then id.
 *
 * The final id tie-break is what makes it TOTAL. Two step rows written in the
 * same millisecond with the same kind would otherwise sort by whichever the
 * fold produced first, and a scrubber that re-orders between renders is a
 * scrubber that jumps under a cursor.
 */
export function compareEvents(a: SceneEvent, b: SceneEvent): number {
  if (a.at !== b.at) return a.at < b.at ? -1 : 1;
  const rank = KIND_RANK[a.kind] - KIND_RANK[b.kind];
  if (rank !== 0) return rank;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

export interface TimelineBounds {
  /** The first moment, or "" when there is nothing dated. */
  from: string;
  /** The last moment, or "". */
  to: string;
  count: number;
}

/**
 * The span the scrubber covers.
 *
 * An EMPTY list gives empty bounds rather than a zero-width span at "now": a
 * goal whose rows carry no timestamps has no timeline, and a scrubber over one
 * point would be a control that looks like it works.
 */
export function timelineBounds(list: readonly SceneEvent[]): TimelineBounds {
  if (list.length === 0) return { from: "", to: "", count: 0 };
  const sorted = [...list].sort(compareEvents);
  return {
    from: sorted[0]?.at ?? "",
    to: sorted[sorted.length - 1]?.at ?? "",
    count: sorted.length,
  };
}
