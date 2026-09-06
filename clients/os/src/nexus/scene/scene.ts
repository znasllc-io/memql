// The world as it stood at a moment.
//
// `scene(world, at)` is what makes Replay possible without a backend: it
// takes the rows the feed already holds and returns the SAME shape, narrowed
// to what existed at `at` and with the statuses those rows had then. The Map
// renders `scene(world, "")` -- the whole world, now -- and Replay renders
// `scene(world, scrubPosition)`. One renderer, one layout, one code path.
//
// ===========================================================================
// PRESENCE: A ROW WITH NO createdAt IS ALWAYS PRESENT
// ===========================================================================
// The engine stamps `createdAt` on every row, so this should never bite. When
// it does -- a projection that omitted it, a fixture, an older node -- the row
// is kept at every scrub position rather than filtered out. Dropping it would
// make a node that genuinely exists disappear as soon as an operator touched
// the scrubber, which reads as a rendering bug; keeping it makes the node
// appear earlier than it should, which reads as what it is (an arrival the
// cluster did not date). The wrong-in-the-visible-direction choice is the
// right one here.
//
// ===========================================================================
// STATUS: DERIVED WHERE THE ROW DATES IT, CURRENT WHERE IT DOES NOT
// ===========================================================================
// A task dates its own transitions (`startedAt`, `completedAt`), so its
// status at a moment is a fact. A CONSTRUCT does not: the concept records
// `draft -> staged -> active -> retired` with no per-status timestamp, so the
// only dated transition in its neighbourhood is its BUNDLE'S `activatedAt`,
// which is what promotion actually turns (dsl/authoring/mutations.memql:
// activateAuthoringBundle). So a construct reads `draft` before its bundle
// activates and its recorded status after -- an approximation, stated here
// and asserted in nexusScene.test.ts, rather than a status silently pinned to
// "whatever it is now" at every point in the past.

import type { BundleRow, ConstructRow, GoalWorld, PlanRow, TaskRow } from "./world";
import { latestAttempts, semanticTasks } from "./world";

// "" means NOW -- the whole world, unnarrowed. Used by the Map, which is not
// scrubbing, so it does not pay for a full copy of every collection.
export const NOW = "";

// existedAt is the presence rule in one place. See the header on why an
// undated row is present.
function existedAt(createdAt: string, at: string): boolean {
  const stamp = createdAt.trim();
  if (stamp === "") return true;
  return stamp <= at;
}

// happenedBy answers "had this dated transition occurred by `at`". An empty
// stamp is NOT a transition that happened -- it is one the row does not
// record, which is the opposite answer.
function happenedBy(stamp: string, at: string): boolean {
  const moment = stamp.trim();
  return moment !== "" && moment <= at;
}

export type DerivedTaskStatus = "queued" | "running" | "succeeded" | "failed" | "paused" | "cancelled";

export function taskStatusAt(task: TaskRow, at: string): string {
  if (at === NOW) return task.status;
  if (happenedBy(task.completedAt, at)) return task.status;
  if (happenedBy(task.startedAt, at)) return "running";
  return "queued";
}

export function planStatusAt(plan: PlanRow, at: string): string {
  if (at === NOW) return plan.status;
  if (happenedBy(plan.completedAt, at)) return plan.status;
  if (happenedBy(plan.startedAt, at)) return "running";
  return "queued";
}

export function bundleStatusAt(bundle: BundleRow, at: string): string {
  if (at === NOW) return bundle.status;
  if (happenedBy(bundle.retiredAt, at)) return "retired";
  if (happenedBy(bundle.activatedAt, at)) return "active";
  // It HAS an activation and we are before it: the row records that it was
  // not yet live, and nothing finer. `draft` is the honest floor.
  if (bundle.activatedAt.trim() !== "") return "draft";
  // Never activated at all: the current status is the only thing the row
  // says, at any moment.
  return bundle.status;
}

export function constructStatusAt(
  construct: ConstructRow,
  bundle: BundleRow | null,
  at: string,
): string {
  if (at === NOW) return construct.status;
  if (bundle === null) return construct.status;
  return happenedBy(bundle.activatedAt, at) ? construct.status : "draft";
}

export function scene(world: GoalWorld, at: string): GoalWorld {
  if (at === NOW) return world;

  const plan =
    world.plan === null || !existedAt(world.plan.createdAt, at)
      ? null
      : { ...world.plan, status: planStatusAt(world.plan, at) };

  const bundle =
    world.bundle === null || !existedAt(world.bundle.createdAt, at)
      ? null
      : { ...world.bundle, status: bundleStatusAt(world.bundle, at) };

  return {
    plan,
    // The planner is present as soon as it exists, independent of the plan:
    // the agent row can predate the goal (a system agent seeded at install
    // is older than every plan it ever drives), and hiding it until the plan
    // arrives would leave the first frames of a replay with a road running
    // from you to nothing.
    planner:
      world.planner !== null && existedAt(world.planner.createdAt, at) ? world.planner : null,
    agents: world.agents.filter((agent) => existedAt(agent.createdAt, at)),
    tasks: world.tasks
      .filter((task) => existedAt(task.createdAt, at))
      .map((task) => ({ ...task, status: taskStatusAt(task, at) })),
    bundle,
    constructs: world.constructs
      .filter((construct) => existedAt(construct.createdAt, at))
      .map((construct) => ({
        ...construct,
        status: constructStatusAt(construct, world.bundle, at),
      })),
    // Dependency edges carry no timestamp of their own, so they follow their
    // ENDPOINTS: an edge is present when the bundle it belongs to is. Drawing
    // an edge between two constructs that have not arrived yet is the one
    // artefact a scrub must not produce.
    edges: bundle === null ? [] : world.edges,
    artifacts: world.artifacts.filter((artifact) => existedAt(artifact.createdAt, at)),
  };
}

export interface GoalProgress {
  completed: number;
  total: number;
  // 0..1. Zero when the goal has no tasks yet -- which is a real state and
  // NOT the same as "no progress on the tasks it has", so callers that need
  // to tell them apart read `total`.
  fraction: number;
  // The goal is lit when the plan succeeded, and only then. A goal whose
  // tasks have all landed but whose plan has not been marked succeeded is
  // still dark, because the plan is the thing that decides.
  lit: boolean;
}

// goalProgress is what fills the beacon (design 4.3): completed semantic
// tasks over all of them, at whatever moment the world represents.
//
// COUNTED PER NODE, NOT PER ROW, and that is a correctness matter rather
// than a preference. A retried step is several task ROWS sharing one
// logicalStepId (world.ts's header), and counting rows puts every failed
// attempt in the denominator forever -- so a goal that retried one step
// twice and then succeeded completely would fill to six sevenths and stop,
// with a beacon that can never light on a plan that ever had to try again.
// The map draws one node for the step; the beacon counts the same one.
export function goalProgress(world: GoalWorld): GoalProgress {
  const tasks = [...latestAttempts(semanticTasks(world.tasks)).values()];
  const completed = tasks.filter((task) => task.status === "succeeded").length;
  const total = tasks.length;
  return {
    completed,
    total,
    fraction: total === 0 ? 0 : completed / total,
    lit: world.plan?.status === "succeeded",
  };
}
