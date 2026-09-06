// Where every node in a goal's world goes.
//
// ===========================================================================
// THE SHAPE, AND WHY IT IS THIS ONE
// ===========================================================================
// A timeline constellation (design D3), prototyped live and corrected with
// the owner: YOU AT THE START, the goal at the far end as a beacon, the
// planner between you and the work, the phases marching left to right toward
// the goal, agents above the road, artifacts and constructs below it.
//
// The correction is worth stating because the obvious arrangement is the
// wrong one: the goal reads as the ORIGIN of the work, so the first sketch
// put it at x=0 with everything radiating out. Watching it materialize made
// the mistake plain -- the goal is what the work ARRIVES AT, and a map whose
// centre is the request has nowhere for progress to go. The beacon at the end
// filling with completed tasks is the whole emotional shape of the surface.
//
// ===========================================================================
// DETERMINISM IS A REQUIREMENT, NOT A NICETY
// ===========================================================================
// Two consumers depend on `layout(sameWorld)` giving the same answer twice:
// Replay, which re-lays the scene at every scrub position, and a deep link,
// which has to frame the node the URL names. Every ordering here is a total
// order over stable row fields -- never insertion order, never a Set's
// iteration order, never a timestamp compared as a Date.
//
// Timestamps are compared as STRINGS throughout the scene library. RFC3339 in
// a fixed offset sorts lexicographically, MemQL writes UTC `Z` stamps, and a
// string compare cannot silently produce NaN the way `new Date("")` does --
// which is the failure that turns a missing timestamp into a node at the
// origin rather than a node the code declines to place.
//
// ===========================================================================
// LANES
// ===========================================================================
// Each lane owns a y. That is what makes the minimum-separation guarantee
// cheap: two nodes can only be close if they are in the SAME lane, so the
// grid spacing inside a lane is the whole proof (see nexusScene.test.ts,
// which asserts it over the 300-node fixture rather than trusting this
// paragraph).

import {
  clusterNodeId,
  conceptIdForKind,
  latestAttempts,
  semanticTasks,
  taskNodeId,
  toolInvocationsByParent,
  nodeIdFor,
  GOAL_NODE_ID,
  YOU_NODE_ID,
  type GoalWorld,
  type NodeKind,
  type TaskRow,
} from "./world";

export type Lane = "road" | "agents" | "artifacts" | "constructs" | "bundle";

export interface LayoutNode {
  id: string;
  kind: NodeKind;
  lane: Lane;
  x: number;
  y: number;
  z: number;
  // The CSS2D label the map renders in the portal's type.
  label: string;
  // The row this node re-reads through the authorized path when opened at
  // /nexus/:planId/node/:nodeId. Empty for `you` and for a cluster, neither
  // of which has a row (world.ts's conceptIdForKind says the same).
  rowId: string;
  conceptId: string;
  // The phase a task (or a cluster) belongs to. Empty for every other kind.
  phase: string;
  // Tool invocations recorded against this node, which the map renders as a
  // counter while the task runs (design D2). Zero everywhere else.
  toolInvocations: number;
  // How many semantic nodes a cluster stands in for. Zero for every other
  // kind -- a cluster is the ONLY node whose count is meaningful, and a
  // non-cluster carrying one would read as "this node is really several".
  clusterCount: number;
}

// An edge is drawn, never positioned: both endpoints are node ids and the
// renderer reads their coordinates. That keeps the edge set stable across a
// re-layout even when the coordinates move.
export interface LayoutEdge {
  from: string;
  to: string;
  // "road" is the spine from you to the goal, which brightens with progress
  // (design 4.3, ambient); "flow" is work handed along; "authored" ties a
  // construct to its bundle; "produced" ties an artifact to the task that
  // made it. The renderer tones them differently; nothing else reads it.
  kind: "road" | "flow" | "authored" | "produced";
}

export interface LayoutResult {
  // The payload the acceptance criterion names: nodeId -> position + lane +
  // kind. A Map rather than an array because every consumer looks nodes up
  // by id (the URL names one, hover names one, an event names one).
  nodes: Map<string, LayoutNode>;
  edges: LayoutEdge[];
  // Phase columns in draw order, with the x each occupies. Replay marks the
  // boundaries on its scrubber from this rather than recomputing them.
  phases: PhaseColumn[];
  // The x the goal sits at, which is also the far end of the road and what
  // the camera frames on first paint.
  goalX: number;
}

export interface PhaseColumn {
  name: string;
  x: number;
  width: number;
  // Semantic task nodes in this phase, BEFORE any collapse. A collapsed
  // phase still reports its real count -- that is the number the cluster
  // node renders.
  count: number;
  collapsed: boolean;
}

export interface LayoutOptions {
  // Phases the operator has expanded by clicking their cluster node. A phase
  // in this set is drawn in full however dense it is; the threshold is a
  // default, not a ceiling (design 4.2, and memql#4376's "expands on click").
  expanded?: ReadonlySet<string>;
  // Overridable so the density test can drive the collapse without
  // fabricating 150 rows for every case that needs one.
  clusterThreshold?: number;
}

// ---------------------------------------------------------------------------
// The constants. Every one is a scene unit; the camera's framing is derived
// from them rather than the other way round.
// ---------------------------------------------------------------------------

const YOU_X = 0;
const PLANNER_X = 4;
// Where the first phase column starts. Far enough from the planner that the
// road has a visible run before the work begins.
const PHASE_X0 = 11;
// Gutter between phase columns.
const PHASE_GAP = 6;
// The narrowest a phase column may be, so a one-task phase still reads as a
// column rather than a point.
const PHASE_MIN_WIDTH = 2.4;
// Gap from the end of the last phase to the goal.
const GOAL_GAP = 9;

// Tasks wrap into rows along z; a row runs across the column and the next
// row steps deeper in x. Six is what fits the prototype's camera without the
// labels colliding.
const TASKS_PER_ROW = 6;
const TASK_Z_GAP = 2.4;
const TASK_ROW_X_GAP = 2.4;

const AGENTS_PER_ROW = 5;
const AGENT_X_GAP = 4.2;
const AGENT_Z_GAP = 3.2;

const ARTIFACTS_PER_ROW = 8;
const ARTIFACT_X_GAP = 3;
const ARTIFACT_Z_GAP = 2.6;

const CONSTRUCTS_PER_ROW = 8;
const CONSTRUCT_X_GAP = 3;
const CONSTRUCT_Z_GAP = 2.6;

const LANE_Y: Record<Lane, number> = {
  agents: 4.5,
  road: 0,
  artifacts: -3.6,
  constructs: -6.4,
  bundle: -9.2,
};

// The density above which a phase collapses to one node (design 4.2).
export const DEFAULT_CLUSTER_THRESHOLD = 150;

// The closest two nodes may sit. Not a rendering constant -- it is the
// property the layout test asserts, and it is stated here so the test and the
// spacing constants above cannot drift apart.
export const MIN_SEPARATION = 1.5;

// ---------------------------------------------------------------------------

// phaseOrder decides the left-to-right order of the columns.
//
// The plan's own `phases[]` is the authority when it names them, because that
// is the planner's declared decomposition and an operator reading the map
// expects the order the plan states. Phases that appear only on tasks (a
// planner that wrote task phases without recording them on the plan, which
// the schema permits) are appended, ordered by the lowest `seq` any of their
// tasks carries and then by name -- a total order, so a re-layout cannot
// reshuffle them.
function phaseOrder(world: GoalWorld, tasks: readonly TaskRow[]): string[] {
  const seen = new Set<string>();
  const order: string[] = [];

  for (const phase of world.plan?.phases ?? []) {
    if (phase.name === "" || seen.has(phase.name)) continue;
    seen.add(phase.name);
    order.push(phase.name);
  }

  const minSeq = new Map<string, number>();
  for (const task of tasks) {
    const current = minSeq.get(task.phase);
    if (current === undefined || task.seq < current) minSeq.set(task.phase, task.seq);
  }

  const extra = [...minSeq.keys()].filter((name) => !seen.has(name));
  extra.sort((a, b) => {
    const sa = minSeq.get(a) ?? 0;
    const sb = minSeq.get(b) ?? 0;
    if (sa !== sb) return sa - sb;
    return a < b ? -1 : a > b ? 1 : 0;
  });
  order.push(...extra);

  // A phase column with no tasks is dropped: the plan may declare phases it
  // never reached, and an empty column reads as work that vanished rather
  // than work that has not started. The goal's own fill is where "not there
  // yet" is expressed.
  return order.filter((name) => minSeq.has(name));
}

// byDrawOrder is the total order tasks are laid out in within a phase.
function byDrawOrder(a: TaskRow, b: TaskRow): number {
  if (a.seq !== b.seq) return a.seq - b.seq;
  if (a.createdAt !== b.createdAt) return a.createdAt < b.createdAt ? -1 : 1;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

// gridZ centres a row of `count` items on z=0 so a phase reads as symmetric
// about the road however many tasks it holds.
function gridZ(index: number, count: number, gap: number): number {
  return (index - (count - 1) / 2) * gap;
}

export function layout(world: GoalWorld, options: LayoutOptions = {}): LayoutResult {
  const expanded = options.expanded ?? new Set<string>();
  const threshold = options.clusterThreshold ?? DEFAULT_CLUSTER_THRESHOLD;

  const nodes = new Map<string, LayoutNode>();
  const edges: LayoutEdge[] = [];

  const semantic = semanticTasks(world.tasks);
  const attempts = latestAttempts(semantic);
  const invocations = toolInvocationsByParent(world.tasks);

  // One row per NODE, not per row: a retried step collapses to its latest
  // attempt here (world.ts's header explains why the node and the row are
  // different identities).
  const drawn = [...attempts.values()];

  const byPhase = new Map<string, TaskRow[]>();
  for (const task of drawn) {
    const list = byPhase.get(task.phase);
    if (list === undefined) byPhase.set(task.phase, [task]);
    else list.push(task);
  }
  for (const list of byPhase.values()) list.sort(byDrawOrder);

  const order = phaseOrder(world, drawn);

  // ---- the columns ------------------------------------------------------
  const phases: PhaseColumn[] = [];
  let cursor = PHASE_X0;
  for (const name of order) {
    const list = byPhase.get(name) ?? [];
    const collapsed = list.length > threshold && !expanded.has(name);
    const rows = collapsed ? 1 : Math.max(1, Math.ceil(list.length / TASKS_PER_ROW));
    const width = Math.max(PHASE_MIN_WIDTH, (rows - 1) * TASK_ROW_X_GAP + PHASE_MIN_WIDTH);
    phases.push({ name, x: cursor, width, count: list.length, collapsed });
    cursor += width + PHASE_GAP;
  }

  // With no phases at all the goal still needs somewhere to be, far enough
  // from you that the road is visible. `cursor` is already PHASE_X0 in that
  // case, so the same expression covers both.
  const lastEnd = phases.length === 0 ? PHASE_X0 : cursor - PHASE_GAP;
  const goalX = lastEnd + GOAL_GAP;

  // ---- you, the planner, the goal ---------------------------------------
  nodes.set(YOU_NODE_ID, {
    id: YOU_NODE_ID,
    kind: "you",
    lane: "road",
    x: YOU_X,
    y: LANE_Y.road,
    z: 0,
    label: "You",
    rowId: "",
    conceptId: "",
    phase: "",
    toolInvocations: 0,
    clusterCount: 0,
  });

  nodes.set(GOAL_NODE_ID, {
    id: GOAL_NODE_ID,
    kind: "goal",
    lane: "road",
    x: goalX,
    y: LANE_Y.road,
    z: 0,
    label: world.plan?.goal ?? "",
    rowId: world.plan?.id ?? "",
    conceptId: conceptIdForKind("goal"),
    phase: "",
    toolInvocations: 0,
    clusterCount: 0,
  });

  edges.push({ from: YOU_NODE_ID, to: GOAL_NODE_ID, kind: "road" });

  if (world.planner !== null) {
    const id = nodeIdFor("planner", world.planner.id);
    nodes.set(id, {
      id,
      kind: "planner",
      lane: "agents",
      x: PLANNER_X,
      y: LANE_Y.agents,
      z: 0,
      label: world.planner.name,
      rowId: world.planner.id,
      conceptId: conceptIdForKind("planner"),
      phase: "",
      toolInvocations: 0,
      clusterCount: 0,
    });
    edges.push({ from: YOU_NODE_ID, to: id, kind: "flow" });
  }

  // ---- the work ---------------------------------------------------------
  let previousColumnAnchor = world.planner === null
    ? YOU_NODE_ID
    : nodeIdFor("planner", world.planner.id);

  for (const column of phases) {
    if (column.collapsed) {
      const id = clusterNodeId(column.name);
      nodes.set(id, {
        id,
        kind: "cluster",
        lane: "road",
        x: column.x,
        y: LANE_Y.road,
        z: 0,
        label: column.name === "" ? `${column.count} tasks` : `${column.name} (${column.count})`,
        rowId: "",
        conceptId: "",
        phase: column.name,
        toolInvocations: 0,
        clusterCount: column.count,
      });
      edges.push({ from: previousColumnAnchor, to: id, kind: "flow" });
      previousColumnAnchor = id;
      continue;
    }

    const list = byPhase.get(column.name) ?? [];
    let firstOfColumn = "";
    list.forEach((task, index) => {
      const row = Math.floor(index / TASKS_PER_ROW);
      const col = index % TASKS_PER_ROW;
      // The LAST row is usually short; centring it on its own width rather
      // than on TASKS_PER_ROW keeps a phase symmetric about the road instead
      // of hanging its remainder off one side.
      const inRow = Math.min(TASKS_PER_ROW, list.length - row * TASKS_PER_ROW);
      const id = taskNodeId(task);
      if (firstOfColumn === "") firstOfColumn = id;
      nodes.set(id, {
        id,
        kind: "task",
        lane: "road",
        x: column.x + row * TASK_ROW_X_GAP,
        y: LANE_Y.road,
        z: gridZ(col, inRow, TASK_Z_GAP),
        label: task.kind === "" ? task.id : task.kind,
        rowId: task.id,
        conceptId: conceptIdForKind("task"),
        phase: task.phase,
        toolInvocations: invocations.get(id) ?? 0,
        clusterCount: 0,
      });
    });
    if (firstOfColumn !== "") {
      edges.push({ from: previousColumnAnchor, to: firstOfColumn, kind: "flow" });
      previousColumnAnchor = firstOfColumn;
    }
  }

  // ---- agents above the work --------------------------------------------
  // A GRID rather than an even spread between the planner and the goal: an
  // even spread's separation shrinks with the agent count, so a goal that
  // raised twenty specialists would place them a fraction of a unit apart and
  // the minimum-separation guarantee would depend on how busy the plan was.
  const specialists = [...world.agents]
    .filter((agent) => agent.id !== world.planner?.id)
    .sort((a, b) =>
      a.createdAt !== b.createdAt
        ? a.createdAt < b.createdAt
          ? -1
          : 1
        : a.id < b.id
          ? -1
          : a.id > b.id
            ? 1
            : 0,
    );

  specialists.forEach((agent, index) => {
    const row = Math.floor(index / AGENTS_PER_ROW);
    const col = index % AGENTS_PER_ROW;
    const id = nodeIdFor("specialist", agent.id);
    nodes.set(id, {
      id,
      kind: "specialist",
      lane: "agents",
      x: PLANNER_X + AGENT_X_GAP * (col + 1),
      y: LANE_Y.agents,
      z: gridZ(row, Math.ceil(specialists.length / AGENTS_PER_ROW), AGENT_Z_GAP),
      label: agent.name,
      rowId: agent.id,
      conceptId: conceptIdForKind("specialist"),
      phase: "",
      toolInvocations: 0,
      clusterCount: 0,
    });
  });

  // ---- artifacts below the road -----------------------------------------
  const artifacts = [...world.artifacts].sort((a, b) =>
    a.createdAt !== b.createdAt
      ? a.createdAt < b.createdAt
        ? -1
        : 1
      : a.id < b.id
        ? -1
        : a.id > b.id
          ? 1
          : 0,
  );
  artifacts.forEach((artifact, index) => {
    const row = Math.floor(index / ARTIFACTS_PER_ROW);
    const col = index % ARTIFACTS_PER_ROW;
    const id = nodeIdFor("artifact", artifact.id);
    nodes.set(id, {
      id,
      kind: "artifact",
      lane: "artifacts",
      x: PHASE_X0 + ARTIFACT_X_GAP * col,
      y: LANE_Y.artifacts,
      z: gridZ(row, Math.ceil(artifacts.length / ARTIFACTS_PER_ROW), ARTIFACT_Z_GAP),
      label: artifact.title === "" ? artifact.id : artifact.title,
      rowId: artifact.id,
      conceptId: conceptIdForKind("artifact"),
      phase: "",
      toolInvocations: 0,
      clusterCount: 0,
    });
  });

  // ---- what the goal authored -------------------------------------------
  const constructs = [...world.constructs].sort((a, b) =>
    a.createdAt !== b.createdAt
      ? a.createdAt < b.createdAt
        ? -1
        : 1
      : a.id < b.id
        ? -1
        : a.id > b.id
          ? 1
          : 0,
  );
  constructs.forEach((construct, index) => {
    const row = Math.floor(index / CONSTRUCTS_PER_ROW);
    const col = index % CONSTRUCTS_PER_ROW;
    const id = nodeIdFor("construct", construct.id);
    nodes.set(id, {
      id,
      kind: "construct",
      lane: "constructs",
      x: PHASE_X0 + CONSTRUCT_X_GAP * col,
      y: LANE_Y.constructs,
      z: gridZ(row, Math.ceil(constructs.length / CONSTRUCTS_PER_ROW), CONSTRUCT_Z_GAP),
      label: construct.name === "" ? construct.id : construct.name,
      rowId: construct.id,
      conceptId: conceptIdForKind("construct"),
      phase: "",
      toolInvocations: 0,
      clusterCount: 0,
    });
  });

  if (world.bundle !== null) {
    const id = nodeIdFor("bundle", world.bundle.id);
    nodes.set(id, {
      id,
      kind: "bundle",
      lane: "bundle",
      // Near the end, because the bundle is what the goal LEAVES BEHIND --
      // it becomes real at the point the work lands, not while it runs.
      x: goalX - GOAL_GAP / 2,
      y: LANE_Y.bundle,
      z: 0,
      label: world.bundle.title === "" ? world.bundle.id : world.bundle.title,
      rowId: world.bundle.id,
      conceptId: conceptIdForKind("bundle"),
      phase: "",
      toolInvocations: 0,
      clusterCount: 0,
    });
    for (const construct of constructs) {
      edges.push({ from: nodeIdFor("construct", construct.id), to: id, kind: "authored" });
    }
  }

  // Every drawn artifact hangs off the goal rather than off the task that
  // produced it: `artifact.producedByPlanId` names the PLAN, and the row
  // carries no task pointer, so an edge to a task would be a guess. Stated
  // rather than silently omitted, because "why is this not wired to its
  // step" is the first question the picture raises.
  for (const artifact of artifacts) {
    edges.push({ from: nodeIdFor("artifact", artifact.id), to: GOAL_NODE_ID, kind: "produced" });
  }

  return { nodes, edges, phases, goalX };
}
