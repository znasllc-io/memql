// Where every node in a goal's world goes, in two dimensions.
//
// ===========================================================================
// THE SHAPE, AND WHY IT IS THIS ONE
// ===========================================================================
// YOU AT THE START, the goal at the far end as the thing the work arrives at,
// the run's template just past you, and the work marching left to right
// between them. Who ran each step is drawn above the road; what a step had to
// stop and ask is drawn below it.
//
// The correction is worth restating because the obvious arrangement is the
// wrong one, and the portal learned it the expensive way: the goal reads as
// the ORIGIN of the work, so the first sketch put it at x=0 with everything
// radiating out. Watching it materialize made the mistake plain -- the goal is
// what the work ARRIVES AT, and a map whose centre is the request has nowhere
// for progress to go. The beacon at the end filling as the work lands is the
// whole emotional shape of the surface, and it survives the move from three
// dimensions to two intact, because it was never a fact about the renderer.
//
// ===========================================================================
// THE X AXIS IS DEPENDENCY DEPTH, NOT `seq`
// ===========================================================================
// Steps that can run at the same time sit in the same column. That is the
// map's one structural claim and it is TRUE OF THE ROWS: `dependsOn` is a real
// edge on `v1:work:step`, so a column is a fact about the automation rather
// than a rendering convenience.
//
// In the common case -- every step waiting on the one before it -- every
// column holds exactly one step and the map is a straight road. That is the
// correct picture of that run, not a degenerate one.
//
// ===========================================================================
// FINISHED STRETCHES FOLD; DENSE COLUMNS CLUSTER
// ===========================================================================
// Two different densities, two different devices, and conflating them would
// lose a real distinction.
//
//   - A FOLD is horizontal: a maximal stretch of consecutive columns that are
//     entirely finished collapses into one segment carrying its count. This is
//     what makes a completed forty-seven-step run readable, and it is what
//     memql#4974 asks for under the word "phases" -- there is no phase ROW in
//     the spine, so the folding is applied to the structure that does exist.
//   - A CLUSTER is vertical: one column with more parallel steps than the
//     threshold collapses to a single node standing in for all of them.
//
// Both expand on click, both report their real count while collapsed, and
// neither is a ceiling -- the threshold is a default.
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
// ===========================================================================
// LANES
// ===========================================================================
// Each lane owns a y OFFSET from the road, so two nodes can only be close if
// they are in the same lane and the same column -- which makes the minimum
// separation guarantee cheap, and the vertical gap inside a column the whole
// proof. Scene units, with +y DOWN as SVG has it.

import {
  approvalsOfRun,
  bindingNodeId,
  clusterNodeId,
  conceptIdForKind,
  depths,
  foldNodeId,
  GOAL_NODE_ID,
  nodeIdFor,
  stepNodeId,
  stepsOfRun,
  templateNodeId,
  YOU_NODE_ID,
  type ApprovalRow,
  type GoalWorld,
  stepThought,
  type NodeKind,
  type StepRow,
} from "./world";

export type Lane = "road" | "binding" | "asked";

export interface LayoutNode {
  id: string;
  kind: NodeKind;
  lane: Lane;
  x: number;
  y: number;
  /** What the map prints beside the node. */
  label: string;
  /**
   * The row this node re-reads through the authorized path when opened. Empty
   * for `you`, `cluster` and `fold` -- the viewer and two drawings that stand
   * in for several rows (world.ts's conceptIdForKind says the same).
   */
  rowId: string;
  conceptId: string;
  /** The step key a node belongs to, for the shared selection. "" elsewhere. */
  stepKey: string;
  /** The column this node sits in. -1 for `you` and `goal`. */
  depth: number;
  /** The row's own status, so the renderer never re-derives one. */
  status: string;
  /**
   * How many rows a cluster or a fold stands in for. ZERO everywhere else -- a
   * plain node carrying a count would read as "this node is really several".
   */
  standsFor: number;
  /** Of those, how many the machine had to think about. Zero elsewhere. */
  thoughtful?: number;
}

/**
 * An edge is drawn, never positioned: both endpoints are node ids and the
 * renderer reads their coordinates. That keeps the edge set stable across a
 * re-layout even when the coordinates move.
 */
export interface LayoutEdge {
  from: string;
  to: string;
  /**
   * `flow` is a real `dependsOn` edge between two steps; `ranBy` ties a step to
   * the mark saying what ran it; `asked` ties a step to the approval it
   * raised. The road is NOT an edge kind -- it is a polyline, below.
   */
  kind: "flow" | "ranBy" | "asked";
}

export interface Column {
  /** Dependency depth. For a fold, the depth it starts at. */
  depth: number;
  x: number;
  width: number;
  /** Steps in this column BEFORE any collapse. A collapsed column still
   *  reports its real count -- that is the number the cluster node renders. */
  count: number;
  /** Steps in this column that have finished. */
  done: number;
  clustered: boolean;
  /** True for a fold, which stands for `depth..throughDepth` inclusive. */
  folded: boolean;
  throughDepth: number;
}

/**
 * A point on the road, and what the stretch ARRIVING at it was like.
 *
 * ===========================================================================
 * THE ROAD'S WEIGHT IS THE RAIL'S INK, AND THAT IS THE WHOLE DESIGN
 * ===========================================================================
 * The step rail already says the product's claim in one glance: a
 * deterministic step is a hollow node on a hairline and a reasoning step is a
 * filled node on thick ink, so a long run reads as a thin grey thread with a
 * few dense knots in it. The map says the SAME sentence with the SAME marks,
 * so a person learns the language once and reads both surfaces with it -- the
 * road is thin across the stretches that cost nothing and thick where the
 * machine had to think.
 *
 * `thought` is therefore not decoration: it is the one fact this picture is
 * for, carried on the segment rather than derived in the renderer, so the map
 * and the rail cannot disagree about which steps cost something.
 */
export interface RoadPoint {
  x: number;
  y: number;
  /** The stretch arriving here ran a step the machine had to think about. */
  thought: boolean;
  /** The stretch arriving here has entirely landed. */
  done: boolean;
}

export interface LayoutResult {
  /**
   * nodeId -> node. A Map rather than an array because every consumer looks
   * nodes up by id (the URL names one, hover names one, an event names one).
   */
  nodes: Map<string, LayoutNode>;
  edges: LayoutEdge[];
  columns: Column[];
  /**
   * The road: the polyline the renderer draws from you to the goal, through
   * each column's centre. A polyline rather than an edge list because the
   * renderer brightens the PORTION behind the work, which is a length along a
   * path and not a set of segments between nodes.
   */
  road: RoadPoint[];
  /** How far along `road` the finished work reaches, 0..1. */
  roadProgress: number;
  /** The x the goal sits at -- the far end of the road, and what the viewport
   *  frames on first paint. */
  goalX: number;
  /** The extent every node occupies, for the viewport's fit. */
  bounds: { minX: number; minY: number; maxX: number; maxY: number };
}

export interface LayoutOptions {
  /**
   * Columns the person has expanded by clicking a cluster, keyed by depth. A
   * depth in this set is drawn in full however dense it is; the threshold is a
   * default, not a ceiling.
   */
  expandedColumns?: ReadonlySet<number>;
  /** Folds the person has expanded, keyed by the depth the fold starts at. */
  expandedFolds?: ReadonlySet<number>;
  /** Overridable so the density test can drive the collapse without
   *  fabricating a hundred rows for every case that needs one. */
  clusterThreshold?: number;
  /** Overridable for the same reason. */
  foldThreshold?: number;
}

// ---------------------------------------------------------------------------
// The constants. Every one is a SCENE unit; the viewport's framing is derived
// from them rather than the other way round.
// ---------------------------------------------------------------------------

const YOU_X = 0;
const TEMPLATE_X = 5;
/** Where the first column starts. Far enough past the template that the road
 *  has a visible run before the work begins. */
const COLUMN_X0 = 11.5;
const COLUMN_GAP = 5.6;
const COLUMN_WIDTH = 3.6;
/** A fold is wider than a column so its count has somewhere to sit, and so a
 *  reader can see at a glance that it stands for more than one. */
const FOLD_WIDTH = 5.2;
const GOAL_GAP = 9;

/** Vertical gap between two steps sharing a column. */
const STEP_Y_GAP = 2.8;

const LANE_OFFSET: Record<Lane, number> = {
  binding: -3.4,
  road: 0,
  asked: 3.6,
};

/** The density above which a column collapses to one node. */
export const DEFAULT_CLUSTER_THRESHOLD = 12;
/** How many consecutive finished columns it takes before they fold. Two is
 *  not worth a device; three reads as a stretch. */
export const DEFAULT_FOLD_THRESHOLD = 3;

/**
 * The closest two nodes may sit. Not a rendering constant -- it is the
 * guarantee `test/nexus/scene.test.ts` asserts over the dense fixture, and the
 * gaps above are what prove it.
 */
export const MIN_SEPARATION = 1.5;

const FINISHED = new Set(["done", "skipped", "cancelled"]);

function labelOfStep(step: StepRow): string {
  return step.key !== "" ? step.key : step.callName;
}

function bindingLabel(step: StepRow): string {
  const b = step.binding;
  if (b.model !== "") return b.model;
  if (b.provider !== "") return b.provider;
  if (b.workerId !== "") return b.workerId;
  if (b.surface !== "") return b.surface;
  return b.nodeId;
}

/**
 * The world laid out.
 *
 * An EMPTY world lays out to you and nothing else -- not to an empty result.
 * A goal that has not been read yet, a refused read and a goal whose run has
 * not compiled are all real states, and each of them still has a viewer.
 */
export function layout(world: GoalWorld, options: LayoutOptions = {}): LayoutResult {
  const clusterThreshold = options.clusterThreshold ?? DEFAULT_CLUSTER_THRESHOLD;
  const foldThreshold = options.foldThreshold ?? DEFAULT_FOLD_THRESHOLD;
  const expandedColumns = options.expandedColumns ?? new Set<number>();
  const expandedFolds = options.expandedFolds ?? new Set<number>();

  const nodes = new Map<string, LayoutNode>();
  const edges: LayoutEdge[] = [];

  function put(node: LayoutNode) {
    nodes.set(node.id, node);
  }

  put({
    id: YOU_NODE_ID,
    kind: "you",
    lane: "road",
    x: YOU_X,
    y: 0,
    label: "you",
    rowId: "",
    conceptId: "",
    stepKey: "",
    depth: -1,
    status: "",
    standsFor: 0,
  });

  const goal = world.goal;
  const run = world.run;

  if (run === null) {
    const goalX = TEMPLATE_X + GOAL_GAP;
    if (goal !== null) put(goalNode(goal.id, goal.statement, goal.status, goalX));
    return {
      nodes,
      edges,
      columns: [],
      road: [
        { x: YOU_X, y: 0, thought: false, done: false },
        { x: goal === null ? YOU_X : goalX, y: 0, thought: false, done: false },
      ],
      roadProgress: 0,
      goalX: goal === null ? YOU_X : goalX,
      bounds: bounds(nodes),
    };
  }

  put({
    id: templateNodeId(run),
    kind: "template",
    lane: "road",
    x: TEMPLATE_X,
    y: 0,
    label: run.automationName !== "" ? run.automationName : "compiling",
    rowId: run.id,
    conceptId: conceptIdForKind("template"),
    stepKey: "",
    depth: -1,
    status: run.status,
    standsFor: 0,
  });

  // -------------------------------------------------------------------------
  // Columns
  // -------------------------------------------------------------------------
  const steps = stepsOfRun(world, run.id);
  const depthByKey = depths(steps);

  const byDepth = new Map<number, StepRow[]>();
  for (const step of steps) {
    const depth = depthByKey.get(step.key) ?? 0;
    const held = byDepth.get(depth);
    if (held === undefined) byDepth.set(depth, [step]);
    else held.push(step);
  }
  // Sorted rather than relying on Map insertion order, which follows the row
  // order and would move a column when a step arrived out of order.
  const depthList = [...byDepth.keys()].sort((a, b) => a - b);

  // Fold runs of consecutive, entirely-finished columns. Computed over the
  // depth list BEFORE any x is assigned, so a fold occupies one slot rather
  // than leaving the gap of the columns it replaced.
  const groups = foldGroups(depthList, byDepth, foldThreshold, expandedFolds);

  const columns: Column[] = [];
  const roadPoints: RoadPoint[] = [
    { x: YOU_X, y: 0, thought: false, done: true },
    { x: TEMPLATE_X, y: 0, thought: false, done: true },
  ];
  let finishedThrough = 1; // index into roadPoints reached by finished work

  let x = COLUMN_X0;
  for (const group of groups) {
    if (group.folded) {
      const members = group.depths.flatMap((d) => byDepth.get(d) ?? []);
      columns.push({
        depth: group.depths[0] ?? 0,
        x,
        width: FOLD_WIDTH,
        count: members.length,
        done: members.length,
        clustered: false,
        folded: true,
        throughDepth: group.depths[group.depths.length - 1] ?? 0,
      });
      const thoughtful = members.filter(stepThought).length;
      put({
        id: foldNodeId(run.id, group.depths[0] ?? 0, group.depths[group.depths.length - 1] ?? 0),
        kind: "fold",
        lane: "road",
        x,
        y: 0,
        label: `${members.length} done`,
        // A FOLD THAT HID THINKING SAYS SO. The thick road already carries the
        // fact, but somebody reading the fold itself -- or a screen reader,
        // which sees no road at all -- would otherwise be told only a count.
        thoughtful,
        rowId: "",
        conceptId: "",
        stepKey: "",
        depth: group.depths[0] ?? 0,
        status: "done",
        standsFor: members.length,
      });
      roadPoints.push({ x, y: 0, thought: members.some(stepThought), done: true });
      finishedThrough = roadPoints.length - 1;
      x += COLUMN_GAP;
      continue;
    }

    for (const depth of group.depths) {
      const members = (byDepth.get(depth) ?? []).slice().sort(compareForColumn);
      const done = members.filter((step) => FINISHED.has(step.status)).length;
      const clustered = members.length > clusterThreshold && !expandedColumns.has(depth);
      columns.push({
        depth,
        x,
        width: clustered ? COLUMN_WIDTH : COLUMN_WIDTH,
        count: members.length,
        done,
        clustered,
        folded: false,
        throughDepth: depth,
      });

      if (clustered) {
        put({
          id: clusterNodeId(run.id, depth),
          kind: "cluster",
          lane: "road",
          x,
          y: 0,
          label: `${members.length} in parallel`,
          rowId: "",
          conceptId: "",
          stepKey: "",
          depth,
          status: done === members.length ? "done" : "running",
          standsFor: members.length,
        });
      } else {
        const span = members.length - 1;
        members.forEach((step, index) => {
          const y = (index - span / 2) * STEP_Y_GAP;
          put({
            id: stepNodeId(step),
            kind: "step",
            lane: "road",
            x,
            y,
            label: labelOfStep(step),
            rowId: step.id,
            conceptId: conceptIdForKind("step"),
            stepKey: step.key,
            depth,
            status: step.status,
            standsFor: 0,
          });
          // WHO RAN IT, above the road, and ONLY when the row says. A binding
          // mark on a step nothing has run yet would be this map asserting a
          // machine that has not been chosen.
          if (step.binding.present && bindingLabel(step) !== "") {
            const id = bindingNodeId(step);
            put({
              id,
              kind: "binding",
              lane: "binding",
              x,
              y: y + LANE_OFFSET.binding,
              label: bindingLabel(step),
              rowId: step.id,
              conceptId: conceptIdForKind("binding"),
              stepKey: step.key,
              depth,
              status: step.status,
              standsFor: 0,
            });
            edges.push({ from: stepNodeId(step), to: id, kind: "ranBy" });
          }
        });
      }

      const landed = done === members.length && members.length > 0;
      roadPoints.push({ x, y: 0, thought: members.some(stepThought), done: landed });
      if (landed) finishedThrough = roadPoints.length - 1;
      x += COLUMN_GAP;
    }
  }

  // -------------------------------------------------------------------------
  // Real dependency edges, between steps that are both drawn
  // -------------------------------------------------------------------------
  for (const step of steps) {
    const to = stepNodeId(step);
    if (!nodes.has(to)) continue;
    for (const parentKey of step.dependsOn) {
      const parent = steps.find((candidate) => candidate.key === parentKey);
      if (parent === undefined) continue;
      const from = stepNodeId(parent);
      if (!nodes.has(from)) continue;
      edges.push({ from, to, kind: "flow" });
    }
  }

  // -------------------------------------------------------------------------
  // What it had to ask, below the road
  // -------------------------------------------------------------------------
  const lastX = x - COLUMN_GAP;
  for (const approval of approvalsOfRun(world, run.id)) {
    const owner = approval.stepKey === "" ? null : nodes.get(stepNodeIdForKey(run.id, approval.stepKey));
    // AN APPROVAL WHOSE STEP IS NOT DRAWN IS STILL DRAWN, at the end of the
    // road: it is a demand on a person, and a clustered or folded column is a
    // drawing decision that must not make a question disappear.
    const ax = owner?.x ?? (lastX < COLUMN_X0 ? COLUMN_X0 : lastX);
    const ay = (owner?.y ?? 0) + LANE_OFFSET.asked;
    const id = approvalNodeIdFor(approval);
    put({
      id,
      kind: "approval",
      lane: "asked",
      x: ax,
      y: ay,
      label: approval.kind !== "" ? approval.kind : "approval",
      rowId: approval.id,
      conceptId: conceptIdForKind("approval"),
      stepKey: approval.stepKey,
      depth: owner?.depth ?? -1,
      status: approval.decision === "" ? "waiting" : approval.decision,
      standsFor: 0,
    });
    if (owner !== undefined && owner !== null) {
      edges.push({ from: owner.id, to: id, kind: "asked" });
    }
  }

  // -------------------------------------------------------------------------
  // The beacon
  // -------------------------------------------------------------------------
  const goalX = (columns.length === 0 ? TEMPLATE_X : lastX) + GOAL_GAP;
  if (goal !== null) put(goalNode(goal.id, goal.statement, goal.status, goalX));
  // The last stretch, from the work to the beacon, is never `thought` and is
  // done only when the run is. It is the arrival, not a step.
  roadPoints.push({
    x: goalX,
    y: 0,
    thought: false,
    done: run.status === "succeeded",
  });

  const totalSpan = roadPoints.length - 1;
  return {
    nodes,
    edges,
    columns,
    road: roadPoints,
    roadProgress: totalSpan <= 0 ? 0 : finishedThrough / totalSpan,
    goalX,
    bounds: bounds(nodes),
  };
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function goalNode(rowId: string, statement: string, status: string, x: number): LayoutNode {
  return {
    id: GOAL_NODE_ID,
    kind: "goal",
    lane: "road",
    x,
    y: 0,
    label: statement,
    rowId,
    conceptId: conceptIdForKind("goal"),
    stepKey: "",
    depth: -1,
    status,
    standsFor: 0,
  };
}

function stepNodeIdForKey(runId: string, key: string): string {
  return nodeIdFor("step", `${runId}:${key}`);
}

function approvalNodeIdFor(approval: ApprovalRow): string {
  return nodeIdFor("approval", approval.id);
}

/** Column order: `seq`, then key. Total and stable (world.ts's rule). */
function compareForColumn(a: StepRow, b: StepRow): number {
  if (a.seq !== b.seq) return a.seq - b.seq;
  return a.key < b.key ? -1 : a.key > b.key ? 1 : 0;
}

interface Group {
  depths: number[];
  folded: boolean;
}

/**
 * Group consecutive depth columns, folding maximal finished stretches.
 *
 * A stretch folds when it is at least `threshold` columns long, EVERY step in
 * every one of them has finished, and the person has not expanded it. The
 * stretch must be consecutive in the depth list rather than in depth VALUE,
 * because a run whose depths are 0, 1, 4 has three columns and no gap on
 * screen -- the gap is in the graph, not in the picture.
 */
function foldGroups(
  depthList: readonly number[],
  byDepth: ReadonlyMap<number, StepRow[]>,
  threshold: number,
  expanded: ReadonlySet<number>,
): Group[] {
  const finished = depthList.map((depth) => {
    const members = byDepth.get(depth) ?? [];
    return members.length > 0 && members.every((step) => FINISHED.has(step.status));
  });

  const groups: Group[] = [];
  let i = 0;
  while (i < depthList.length) {
    if (!finished[i]) {
      groups.push({ depths: [depthList[i] as number], folded: false });
      i += 1;
      continue;
    }
    let j = i;
    while (j < depthList.length && finished[j]) j += 1;
    const stretch = depthList.slice(i, j) as number[];
    const head = stretch[0] as number;
    if (stretch.length >= threshold && !expanded.has(head)) {
      groups.push({ depths: stretch, folded: true });
    } else {
      for (const depth of stretch) groups.push({ depths: [depth], folded: false });
    }
    i = j;
  }
  return groups;
}

function bounds(nodes: ReadonlyMap<string, LayoutNode>): {
  minX: number;
  minY: number;
  maxX: number;
  maxY: number;
} {
  let minX = 0;
  let minY = 0;
  let maxX = 0;
  let maxY = 0;
  let first = true;
  for (const node of nodes.values()) {
    if (first) {
      minX = maxX = node.x;
      minY = maxY = node.y;
      first = false;
      continue;
    }
    if (node.x < minX) minX = node.x;
    if (node.x > maxX) maxX = node.x;
    if (node.y < minY) minY = node.y;
    if (node.y > maxY) maxY = node.y;
  }
  return { minX, minY, maxX, maxY };
}
