// The Nexus scene library: layout, events and time travel, with no GPU.
//
// Everything the map can be WRONG about that does not need a renderer to
// notice lives here -- where a node goes, what order the history is in, what
// existed at a moment. The Map (nexusMap.test.tsx) tests the surface that
// draws these answers; this file tests the answers.
//
// The determinism assertions are not ceremony. Replay re-lays the scene at
// every scrub position and a deep link has to frame the node the URL names,
// so `layout(sameWorld)` giving two different answers is a node that moves
// while you are looking at it and a camera that lands beside the thing you
// asked for.

import { describe, expect, it } from "vitest";

import {
  DEFAULT_CLUSTER_THRESHOLD,
  MIN_SEPARATION,
  layout,
} from "../../src/nexus/scene/layout";
import { events, timelineBounds } from "../../src/nexus/scene/events";
import { NOW, goalProgress, scene } from "../../src/nexus/scene/scene";
import { GOAL_NODE_ID, YOU_NODE_ID, semanticTasks } from "../../src/nexus/scene/world";
import {
  MOMENT,
  cancelledGoal,
  denseGoal,
  emptyGoal,
  failedGoal,
  springCatalogGoal,
} from "../../src/nexus/scene/fixtures";

describe("layout", () => {
  it("puts you at the start and the goal at the end", () => {
    const { nodes, goalX } = layout(springCatalogGoal());
    const you = nodes.get(YOU_NODE_ID);
    const goal = nodes.get(GOAL_NODE_ID);
    expect(you).toBeDefined();
    expect(goal).toBeDefined();
    expect(you!.x).toBeLessThan(goal!.x);
    expect(goal!.x).toBe(goalX);

    // Every task sits BETWEEN them. This is the correction the owner made to
    // the prototype (the goal was at the origin); if it ever regresses, the
    // map stops being a timeline and this is the assertion that says so.
    for (const node of nodes.values()) {
      if (node.kind !== "task") continue;
      expect(node.x).toBeGreaterThan(you!.x);
      expect(node.x).toBeLessThan(goal!.x);
    }
  });

  it("is deterministic for equal input", () => {
    const a = layout(springCatalogGoal());
    const b = layout(springCatalogGoal());
    expect([...a.nodes.keys()].sort()).toEqual([...b.nodes.keys()].sort());
    for (const [id, node] of a.nodes) {
      expect(b.nodes.get(id)).toEqual(node);
    }
    expect(a.goalX).toBe(b.goalX);
    expect(a.phases).toEqual(b.phases);
  });

  it("is deterministic under a re-ordered input, not merely a re-run", () => {
    // The trap a same-input re-run cannot catch: an ordering that depends on
    // the order rows arrived in. The feed delivers a seed and then CDC, so
    // the same world genuinely reaches the layout in different orders.
    const world = springCatalogGoal();
    const shuffled = {
      ...world,
      tasks: [...world.tasks].reverse(),
      agents: [...world.agents].reverse(),
      artifacts: [...world.artifacts].reverse(),
      constructs: [...world.constructs].reverse(),
    };
    const a = layout(world);
    const b = layout(shuffled);
    for (const [id, node] of a.nodes) {
      expect(b.nodes.get(id)).toEqual(node);
    }
  });

  it("collapses a retried step onto one node", () => {
    const world = springCatalogGoal();
    const { nodes } = layout(world);
    const taskNodes = [...nodes.values()].filter((n) => n.kind === "task");
    // Seven semantic rows, but two of them are attempts at one step.
    expect(semanticTasks(world.tasks)).toHaveLength(7);
    expect(taskNodes).toHaveLength(6);

    // ...and the surviving node reads the LATEST attempt's row, which is the
    // one an operator opening it wants.
    const normalise = taskNodes.find((n) => n.label === "shape-normalise");
    expect(normalise?.rowId).toBe("shape-normalise-a2");
  });

  it("counts tool invocations on the parent step rather than drawing them", () => {
    const { nodes } = layout(springCatalogGoal());
    const normalise = [...nodes.values()].find((n) => n.label === "shape-normalise");
    expect(normalise?.toolInvocations).toBe(2);
    // And no node was drawn for either invocation.
    expect([...nodes.values()].some((n) => n.label === "tool-read-1")).toBe(false);
  });

  it("wraps a dense phase into rows instead of one long line", () => {
    // Just under the collapse threshold, so the tasks are drawn individually.
    const world = denseGoal(30);
    const { nodes, phases } = layout(world);
    const tasks = [...nodes.values()].filter((n) => n.kind === "task");
    expect(tasks).toHaveLength(30);
    expect(phases[0]?.collapsed).toBe(false);

    // More than one distinct x within the phase == it wrapped.
    const xs = new Set(tasks.map((n) => n.x));
    expect(xs.size).toBeGreaterThan(1);
    // ...and more than one distinct z, because a row spreads across the road.
    const zs = new Set(tasks.map((n) => n.z));
    expect(zs.size).toBeGreaterThan(1);
  });

  it("collapses a phase over the threshold to one cluster node carrying the count", () => {
    const world = denseGoal(DEFAULT_CLUSTER_THRESHOLD + 1);
    const { nodes, phases } = layout(world);
    expect(phases[0]?.collapsed).toBe(true);
    expect(phases[0]?.count).toBe(DEFAULT_CLUSTER_THRESHOLD + 1);

    const clusters = [...nodes.values()].filter((n) => n.kind === "cluster");
    expect(clusters).toHaveLength(1);
    expect(clusters[0]?.clusterCount).toBe(DEFAULT_CLUSTER_THRESHOLD + 1);
    expect([...nodes.values()].some((n) => n.kind === "task")).toBe(false);
  });

  it("expands a collapsed phase on request", () => {
    const world = denseGoal(DEFAULT_CLUSTER_THRESHOLD + 1);
    const { nodes } = layout(world, { expanded: new Set(["sweep"]) });
    expect([...nodes.values()].some((n) => n.kind === "cluster")).toBe(false);
    expect([...nodes.values()].filter((n) => n.kind === "task")).toHaveLength(
      DEFAULT_CLUSTER_THRESHOLD + 1,
    );
  });

  it("never places two nodes closer than the minimum separation", () => {
    // Run on the EXPANDED 300-node fixture, which is the worst case the
    // budget test also uses: 300 tasks in one phase, wrapped, plus every
    // other lane. A collapse would make this assertion trivially true.
    const { nodes } = layout(denseGoal(300), { expanded: new Set(["sweep"]) });
    const all = [...nodes.values()];
    expect(all.length).toBeGreaterThan(300);

    let worst = Number.POSITIVE_INFINITY;
    let offenders = "";
    for (let i = 0; i < all.length; i += 1) {
      for (let j = i + 1; j < all.length; j += 1) {
        const a = all[i]!;
        const b = all[j]!;
        const d = Math.hypot(a.x - b.x, a.y - b.y, a.z - b.z);
        if (d < worst) {
          worst = d;
          offenders = `${a.id} <-> ${b.id}`;
        }
      }
    }
    expect(`${worst >= MIN_SEPARATION} (${worst.toFixed(3)} between ${offenders})`).toBe(
      `true (${worst.toFixed(3)} between ${offenders})`,
    );
  });

  it("draws a road from you to the goal and nothing dangling", () => {
    const { nodes, edges } = layout(springCatalogGoal());
    expect(edges.some((e) => e.kind === "road" && e.from === YOU_NODE_ID && e.to === GOAL_NODE_ID)).toBe(
      true,
    );
    // Every edge names nodes that exist. A dangling endpoint is a line drawn
    // to the origin, which reads as a node at (0,0,0) that is not there.
    for (const edge of edges) {
      expect(nodes.has(edge.from)).toBe(true);
      expect(nodes.has(edge.to)).toBe(true);
    }
  });

  it("draws a goal with no tasks at all", () => {
    const { nodes, phases, goalX } = layout(emptyGoal());
    expect(phases).toHaveLength(0);
    expect(nodes.get(GOAL_NODE_ID)?.x).toBe(goalX);
    expect(nodes.get(YOU_NODE_ID)!.x).toBeLessThan(goalX);
    expect([...nodes.values()].filter((n) => n.kind === "task")).toHaveLength(0);
  });
});

describe("events", () => {
  it("is ordered, and orders creation before start before finish at one instant", () => {
    const list = events(springCatalogGoal());
    for (let i = 1; i < list.length; i += 1) {
      expect(list[i - 1]!.at <= list[i]!.at).toBe(true);
    }
    const created = list.findIndex((e) => e.kind === "plan.created");
    const succeeded = list.findIndex((e) => e.kind === "plan.succeeded");
    expect(created).toBeLessThan(succeeded);
  });

  it("re-lights the same node on a retry rather than growing a second one", () => {
    const list = events(springCatalogGoal());
    const starts = list.filter((e) => e.kind === "task.started" && e.label.includes("shape-normalise"));
    expect(starts).toHaveLength(2);
    // One node, two lightings -- which is the whole point.
    expect(new Set(starts.map((e) => e.nodeId)).size).toBe(1);
    expect(starts[1]?.attempt).toBe(2);
    expect(starts[1]?.label).toContain("attempt 2");
  });

  it("invents nothing for a missing timestamp", () => {
    const world = springCatalogGoal();
    const stripped = {
      ...world,
      tasks: world.tasks.map((t) => ({ ...t, completedAt: "" })),
    };
    const list = events(stripped);
    expect(list.some((e) => e.kind === "task.completed")).toBe(false);
    expect(list.some((e) => e.kind === "task.failed")).toBe(false);
    // ...and the events that DO have timestamps are untouched.
    expect(list.some((e) => e.kind === "task.started")).toBe(true);
  });

  it("treats a whitespace-only stamp as absent, not as a moment", () => {
    const world = emptyGoal();
    const list = events({
      ...world,
      plan: world.plan === null ? null : { ...world.plan, createdAt: "   " },
    });
    expect(list.some((e) => e.kind === "plan.created")).toBe(false);
  });

  it("names only nodes the layout actually draws", () => {
    // The event list is the map's accessible index (design 4.4), so an event
    // pointing at a node that is not in the scene is a keyboard path that
    // focuses nothing.
    const world = springCatalogGoal();
    const { nodes } = layout(world);
    for (const event of events(world)) {
      expect(`${event.kind} -> ${event.nodeId}`).toBe(
        `${event.kind} -> ${nodes.has(event.nodeId) ? event.nodeId : "MISSING"}`,
      );
    }
  });

  it("derives a construct activation from the bundle's, and only when there is one", () => {
    const world = springCatalogGoal();
    const withActivation = events(world).filter((e) => e.kind === "construct.activated");
    // Two of the three constructs are active; the staged one is not.
    expect(withActivation).toHaveLength(2);
    expect(withActivation.every((e) => e.at === world.bundle!.activatedAt)).toBe(true);

    const noActivation = events({
      ...world,
      bundle: world.bundle === null ? null : { ...world.bundle, activatedAt: "" },
    });
    expect(noActivation.some((e) => e.kind === "construct.activated")).toBe(false);
  });

  it("reports empty bounds for a world with no dated history", () => {
    expect(timelineBounds([])).toEqual({ first: "", last: "", count: 0 });
    const bounds = timelineBounds(events(springCatalogGoal()));
    expect(bounds.count).toBeGreaterThan(0);
    expect(bounds.first <= bounds.last).toBe(true);
  });
});

describe("scene(world, at)", () => {
  const world = springCatalogGoal();

  it("returns the world untouched at NOW", () => {
    expect(scene(world, NOW)).toBe(world);
  });

  it("shows nothing but the planner before the first event", () => {
    // MOMENT(-1) predates the plan itself. The planner is a system agent
    // older than the goal, so it survives -- which is deliberate (see
    // scene.ts) and is exactly the boundary worth pinning.
    const before = scene(world, MOMENT(-1));
    expect(before.plan).toBeNull();
    expect(before.tasks).toHaveLength(0);
    expect(before.artifacts).toHaveLength(0);
    expect(before.bundle).toBeNull();
  });

  it("includes an event's own row exactly at that event's moment", () => {
    const at = MOMENT(1);
    const at1 = scene(world, at);
    expect(at1.plan).not.toBeNull();
    // The gather tasks were created at T(1); "exactly at" must include them,
    // not exclude them by an off-by-one on a strict comparison.
    expect(at1.tasks.some((t) => t.id === "gather-fetch")).toBe(true);
  });

  it("shows everything after the last event", () => {
    const after = scene(world, MOMENT(999));
    expect(after.tasks).toHaveLength(world.tasks.length);
    expect(after.artifacts).toHaveLength(world.artifacts.length);
    expect(after.plan?.status).toBe("succeeded");
  });

  it("derives a task's status at the moment rather than showing today's", () => {
    // The first attempt at `step-normalise` ran from T(6) to T(8). Both
    // sides of that boundary are pinned, because "derived" and "whatever the
    // row says today" agree at the end and only differ in the middle -- an
    // assertion taken after T(8) alone would pass against a scene() that did
    // no derivation at all.
    const mid = scene(world, MOMENT(7));
    const attempt1 = mid.tasks.find((t) => t.id === "shape-normalise-a1");
    expect(attempt1?.status).toBe("running");
    expect(mid.tasks.some((t) => t.id === "shape-normalise-a2")).toBe(false);
    expect(mid.tasks.some((t) => t.id === "publish-render")).toBe(false);

    const afterFailure = scene(world, MOMENT(8)).tasks.find(
      (t) => t.id === "shape-normalise-a1",
    );
    expect(afterFailure?.status).toBe("failed");

    // A task that has started but not finished reads as running, whatever
    // the row says today.
    const running = scene(world, MOMENT(16)).tasks.find((t) => t.id === "publish-render");
    expect(running?.status).toBe("running");
  });

  it("keeps a construct at draft until its bundle activates", () => {
    const beforeActivation = scene(world, MOMENT(17));
    expect(beforeActivation.constructs.every((c) => c.status === "draft")).toBe(true);
    expect(beforeActivation.bundle?.status).toBe("draft");

    const afterActivation = scene(world, MOMENT(20));
    expect(afterActivation.bundle?.status).toBe("active");
    expect(afterActivation.constructs.some((c) => c.status === "active")).toBe(true);
  });

  it("drops dependency edges while the bundle has not arrived", () => {
    expect(scene(world, MOMENT(2)).edges).toHaveLength(0);
    expect(scene(world, MOMENT(20)).edges).toHaveLength(world.edges.length);
  });

  it("keeps an undated row present at every moment", () => {
    const undated = {
      ...world,
      artifacts: world.artifacts.map((a) => ({ ...a, createdAt: "" })),
    };
    expect(scene(undated, MOMENT(-1)).artifacts).toHaveLength(world.artifacts.length);
  });

  it("fills the goal from the tasks that have landed at that moment", () => {
    expect(goalProgress(scene(world, MOMENT(-1))).fraction).toBe(0);
    expect(goalProgress(scene(world, MOMENT(-1))).total).toBe(0);

    const half = goalProgress(scene(world, MOMENT(12)));
    expect(half.completed).toBeGreaterThan(0);
    expect(half.fraction).toBeGreaterThan(0);
    expect(half.fraction).toBeLessThan(1);
    expect(half.lit).toBe(false);

    const end = goalProgress(scene(world, MOMENT(999)));
    expect(end.lit).toBe(true);
  });

  it("does not light a goal whose plan has not succeeded, however many tasks landed", () => {
    const stopped = failedGoal();
    expect(goalProgress(stopped).lit).toBe(false);
    expect(goalProgress(cancelledGoal()).lit).toBe(false);
  });
});
