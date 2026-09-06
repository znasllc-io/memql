// A goal's history, read out of the rows themselves.
//
// ===========================================================================
// WHY THERE IS NO BACKEND HERE
// ===========================================================================
// MemQL is append-only and every concept the map draws stamps the moments
// that matter -- `createdAt`, `startedAt`, `completedAt`, the plan's
// `phases[].startedAt/completedAt`, the bundle's `activatedAt`. So the
// timeline Replay scrubs is not a recording of anything: it is the rows,
// read in time order. Nothing is persisted for it, nothing is replayed off
// the bus, and a goal from six months ago replays exactly as well as one from
// this morning.
//
// ===========================================================================
// AN EVENT IS NEVER INVENTED
// ===========================================================================
// The rule this module is really about: a moment with no timestamp produces
// NO event. Not an event at the plan's createdAt, not an event at "now", not
// an event ordered after the last one it could plausibly follow. A task with
// `status: "succeeded"` and an empty `completedAt` is a task whose completion
// this cluster did not record, and the honest rendering of that is a node
// that never lands in the replay -- not a landing at a fabricated time, which
// would put a lie on a scrubber an operator reads as evidence.
//
// There is exactly ONE derived event, and it is derived from a real
// timestamp on a real row rather than conjured: a construct that is `active`
// inside a bundle carrying an `activatedAt` emits `construct.activated` AT
// THE BUNDLE'S activation. That is what promotion does -- the bundle
// activates and its constructs go live with it (dsl/authoring/mutations.memql:
// activateAuthoringBundle) -- and the construct concept records no per-status
// timestamp of its own. A construct that is active with no bundle activation
// behind it emits nothing, which is the same rule applied to a case the
// derivation cannot reach.
//
// ===========================================================================
// A RETRY RE-LIGHTS, IT DOES NOT DUPLICATE
// ===========================================================================
// Every ATTEMPT row emits its own started/completed pair, and every one of
// them carries the SAME `nodeId` (world.ts's taskNodeId keys on
// logicalStepId). So a retried step appears once in the scene and three times
// in the timeline, which is exactly what the prototype's re-light does and
// what an operator needs to see: the node did not multiply, it went again.
//
// That is also why this walks every semantic task row rather than
// `latestAttempts` -- collapsing first would erase the retries from the
// history while leaving them in the counter, which is the worst of both.

import {
  nodeIdFor,
  semanticTasks,
  taskNodeId,
  GOAL_NODE_ID,
  type GoalWorld,
} from "./world";

export type EventKind =
  | "plan.created"
  | "plan.succeeded"
  | "plan.failed"
  | "plan.cancelled"
  | "task.created"
  | "task.started"
  | "task.completed"
  | "task.failed"
  | "agent.raised"
  | "bundle.created"
  | "bundle.activated"
  | "construct.created"
  | "construct.activated"
  | "artifact.produced";

export interface SceneEvent {
  // Stable and unique across a world, so React can key the event list and a
  // scrub position can be compared without an index into an array that a
  // re-read may have re-ordered.
  id: string;
  // RFC3339, exactly as the row carried it. Never reformatted here: the
  // scrubber compares these as strings and the list renders them through the
  // portal's own time treatment.
  at: string;
  kind: EventKind;
  // The node this event lights. The event list doubles as the map's keyboard
  // index (design 4.4), so every event must name a node that exists in the
  // layout -- there is a test for exactly that.
  nodeId: string;
  // The row behind the event, for the list's Enter-opens-the-detail path.
  rowId: string;
  // A short sentence, already written for a screen reader rather than
  // assembled at render time from three fields and a separator.
  label: string;
  // >1 marks a retry, which is the one thing the list says that the map's
  // re-light cannot: WHICH attempt this was.
  attempt: number;
}

// The tie-break when two events share a timestamp, which is common -- a task
// created and started in the same write, a bundle activated with its
// constructs. Creation before start before finish, so a node cannot appear to
// finish before it arrived.
const KIND_RANK: Record<EventKind, number> = {
  "plan.created": 0,
  "agent.raised": 1,
  "task.created": 2,
  "task.started": 3,
  "artifact.produced": 4,
  "bundle.created": 4,
  "construct.created": 4,
  "task.completed": 5,
  "task.failed": 5,
  "construct.activated": 6,
  "bundle.activated": 7,
  "plan.succeeded": 8,
  "plan.failed": 8,
  "plan.cancelled": 8,
};

function push(out: SceneEvent[], event: SceneEvent | null): void {
  if (event !== null) out.push(event);
}

// at() is the whole no-invention rule in one function: no timestamp, no
// event. Whitespace counts as absent -- the wire can carry "" and " " for a
// datetime that was never written, and neither is a moment.
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

export function events(world: GoalWorld): SceneEvent[] {
  const out: SceneEvent[] = [];
  const plan = world.plan;

  if (plan !== null) {
    push(
      out,
      at(plan.createdAt, () => ({
        kind: "plan.created",
        nodeId: GOAL_NODE_ID,
        rowId: plan.id,
        label: `Goal set: ${plan.goal}`,
        attempt: 1,
      })),
    );

    // The plan's terminal moment is ONE event whose kind is read off the
    // status. A plan sitting at `running` with a completedAt is a row this
    // cluster wrote inconsistently; it lands as `plan.succeeded` only when
    // the status says so, and otherwise produces nothing rather than a
    // guess about which ending it had.
    const terminal: EventKind | null =
      plan.status === "succeeded"
        ? "plan.succeeded"
        : plan.status === "failed"
          ? "plan.failed"
          : plan.status === "cancelled"
            ? "plan.cancelled"
            : null;
    if (terminal !== null) {
      push(
        out,
        at(plan.completedAt, () => ({
          kind: terminal,
          nodeId: GOAL_NODE_ID,
          rowId: plan.id,
          label:
            terminal === "plan.succeeded"
              ? "Goal reached"
              : terminal === "plan.failed"
                ? `Goal failed${plan.errorMessage === "" ? "" : `: ${plan.errorMessage}`}`
                : "Goal cancelled",
          attempt: 1,
        })),
      );
    }
  }

  for (const agent of world.agents) {
    push(
      out,
      at(agent.createdAt, () => ({
        kind: "agent.raised",
        nodeId: nodeIdFor(agent.id === world.planner?.id ? "planner" : "specialist", agent.id),
        rowId: agent.id,
        label: `${agent.name} raised`,
        attempt: 1,
      })),
    );
  }

  for (const task of semanticTasks(world.tasks)) {
    const nodeId = taskNodeId(task);
    const name = task.kind === "" ? task.id : task.kind;
    push(
      out,
      at(task.createdAt, () => ({
        kind: "task.created",
        nodeId,
        rowId: task.id,
        label: `${name} queued`,
        attempt: task.attemptNumber,
      })),
    );
    push(
      out,
      at(task.startedAt, () => ({
        kind: "task.started",
        nodeId,
        rowId: task.id,
        label:
          task.attemptNumber > 1
            ? `${name} started again (attempt ${task.attemptNumber})`
            : `${name} started`,
        attempt: task.attemptNumber,
      })),
    );
    if (task.status === "succeeded" || task.status === "failed") {
      const failed = task.status === "failed";
      push(
        out,
        at(task.completedAt, () => ({
          kind: failed ? "task.failed" : "task.completed",
          nodeId,
          rowId: task.id,
          label: failed
            ? `${name} failed${task.errorMessage === "" ? "" : `: ${task.errorMessage}`}`
            : `${name} completed`,
          attempt: task.attemptNumber,
        })),
      );
    }
  }

  for (const artifact of world.artifacts) {
    push(
      out,
      at(artifact.createdAt, () => ({
        kind: "artifact.produced",
        nodeId: nodeIdFor("artifact", artifact.id),
        rowId: artifact.id,
        label: `Produced ${artifact.title === "" ? artifact.id : artifact.title}`,
        attempt: 1,
      })),
    );
  }

  const bundle = world.bundle;
  if (bundle !== null) {
    push(
      out,
      at(bundle.createdAt, () => ({
        kind: "bundle.created",
        nodeId: nodeIdFor("bundle", bundle.id),
        rowId: bundle.id,
        label: `Bundle ${bundle.title === "" ? bundle.id : bundle.title} opened`,
        attempt: 1,
      })),
    );
    push(
      out,
      at(bundle.activatedAt, () => ({
        kind: "bundle.activated",
        nodeId: nodeIdFor("bundle", bundle.id),
        rowId: bundle.id,
        label: `Bundle ${bundle.title === "" ? bundle.id : bundle.title} activated`,
        attempt: 1,
      })),
    );
  }

  for (const construct of world.constructs) {
    push(
      out,
      at(construct.createdAt, () => ({
        kind: "construct.created",
        nodeId: nodeIdFor("construct", construct.id),
        rowId: construct.id,
        label: `Authored ${construct.kind} ${construct.name}`,
        attempt: 1,
      })),
    );
    // The one derived event -- see this file's header for why it is a
    // derivation rather than an invention, and why an active construct
    // without a bundle activation emits nothing.
    if (construct.status === "active" && bundle !== null) {
      push(
        out,
        at(bundle.activatedAt, () => ({
          kind: "construct.activated",
          nodeId: nodeIdFor("construct", construct.id),
          rowId: construct.id,
          label: `${construct.kind} ${construct.name} went live`,
          attempt: 1,
        })),
      );
    }
  }

  out.sort(compareEvents);
  return out;
}

export function compareEvents(a: SceneEvent, b: SceneEvent): number {
  if (a.at !== b.at) return a.at < b.at ? -1 : 1;
  const ra = KIND_RANK[a.kind];
  const rb = KIND_RANK[b.kind];
  if (ra !== rb) return ra - rb;
  if (a.attempt !== b.attempt) return a.attempt - b.attempt;
  if (a.nodeId !== b.nodeId) return a.nodeId < b.nodeId ? -1 : 1;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

export interface TimelineBounds {
  // Empty strings when the world produced no events at all, which is a
  // legitimate state (a goal set on a cluster that stamped no timestamps, a
  // world that has not seeded yet). Replay renders its empty state on it
  // rather than a scrubber with no travel.
  first: string;
  last: string;
  count: number;
}

export function timelineBounds(list: readonly SceneEvent[]): TimelineBounds {
  if (list.length === 0) return { first: "", last: "", count: 0 };
  return {
    first: list[0]?.at ?? "",
    last: list[list.length - 1]?.at ?? "",
    count: list.length,
  };
}
