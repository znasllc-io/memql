import { describe, expect, it } from "vitest";

import {
  DEFAULT_CLUSTER_THRESHOLD,
  DEFAULT_FOLD_THRESHOLD,
  MIN_SEPARATION,
  layout,
  type LayoutNode,
} from "../../src/nexus/scene/layout";
import { EMPTY_WORLD, GOAL_NODE_ID, YOU_NODE_ID } from "../../src/nexus/scene/world";
import {
  approval,
  chainWorld,
  fanOutWorld,
  moment,
  retryWorld,
  run,
  step,
  world,
} from "../../src/nexus/scene/fixtures";

function nodesOfKind(result: ReturnType<typeof layout>, kind: string): LayoutNode[] {
  return [...result.nodes.values()].filter((node) => node.kind === kind);
}

describe("the shape", () => {
  it("puts you at the start and the goal at the far end", () => {
    const result = layout(chainWorld(3));
    const you = result.nodes.get(YOU_NODE_ID);
    const goal = result.nodes.get(GOAL_NODE_ID);
    expect(you?.x).toBe(0);
    expect(goal?.x).toBe(result.goalX);
    expect(goal!.x).toBeGreaterThan(you!.x);
  });

  // The correction the portal earned the expensive way: the goal is what the
  // work ARRIVES AT. Every step must sit between you and it.
  it("puts every step between you and the goal", () => {
    const result = layout(chainWorld(6));
    const goalX = result.goalX;
    for (const node of nodesOfKind(result, "step")) {
      expect(node.x).toBeGreaterThan(0);
      expect(node.x).toBeLessThan(goalX);
    }
  });

  it("lays out an empty world to you alone, not to nothing", () => {
    const result = layout(EMPTY_WORLD);
    expect(result.nodes.get(YOU_NODE_ID)).toBeDefined();
    expect(result.nodes.get(GOAL_NODE_ID)).toBeUndefined();
    expect(result.columns).toHaveLength(0);
  });

  it("draws the goal and the road even when the run has not compiled", () => {
    const result = layout(world({ run: null, runs: [] }));
    expect(result.nodes.get(GOAL_NODE_ID)).toBeDefined();
    expect(result.road.length).toBeGreaterThanOrEqual(2);
  });

  it("labels the template node `compiling` before the automation is known", () => {
    const result = layout(world({ run: run({ automationName: "", status: "compiling" }) }));
    expect(nodesOfKind(result, "template")[0]?.label).toBe("compiling");
  });
});

describe("columns are dependency depth, not seq", () => {
  it("gives a chain one column per step", () => {
    const result = layout(chainWorld(4, "running"));
    expect(result.columns.map((c) => c.depth)).toEqual([0, 1, 2, 3]);
    expect(result.columns.every((c) => c.count === 1)).toBe(true);
  });

  it("stacks steps that can run together into one column", () => {
    const result = layout(fanOutWorld(3));
    const middle = result.columns.find((c) => c.depth === 1);
    expect(middle?.count).toBe(3);
    const xs = new Set(
      nodesOfKind(result, "step")
        .filter((n) => n.depth === 1)
        .map((n) => n.x),
    );
    expect(xs.size).toBe(1);
  });

  it("centres a column on the road", () => {
    const result = layout(fanOutWorld(3));
    const ys = nodesOfKind(result, "step")
      .filter((n) => n.depth === 1)
      .map((n) => n.y)
      .sort((a, b) => a - b);
    expect(ys[1]).toBeCloseTo(0);
    expect(ys[0]).toBeCloseTo(-ys[2]!);
  });
});

describe("determinism", () => {
  // Replay re-lays at every scrub position and a deep link has to frame the
  // node the URL names. Both break the moment this stops holding.
  it("gives the same answer for the same world twice", () => {
    const w = fanOutWorld(5);
    const a = layout(w);
    const b = layout(w);
    expect([...a.nodes.keys()]).toEqual([...b.nodes.keys()]);
    for (const [id, node] of a.nodes) {
      expect(b.nodes.get(id)?.x).toBe(node.x);
      expect(b.nodes.get(id)?.y).toBe(node.y);
    }
    expect(a.road).toEqual(b.road);
  });

  // The collection folds events in the order the cluster sent them, so a
  // layout that depended on input order would reshuffle on an update --
  // exactly when somebody is watching it.
  it("does not depend on the order the rows arrived in", () => {
    const w = fanOutWorld(4);
    const shuffled = { ...w, steps: [...w.steps].reverse() };
    const a = layout(w);
    const b = layout(shuffled);
    for (const [id, node] of a.nodes) {
      expect(b.nodes.get(id)?.x).toBe(node.x);
      expect(b.nodes.get(id)?.y).toBe(node.y);
    }
  });
});

describe("minimum separation", () => {
  it("keeps every pair of nodes at least MIN_SEPARATION apart", () => {
    const result = layout(fanOutWorld(8));
    const nodes = [...result.nodes.values()];
    for (let i = 0; i < nodes.length; i += 1) {
      for (let j = i + 1; j < nodes.length; j += 1) {
        const a = nodes[i]!;
        const b = nodes[j]!;
        const distance = Math.hypot(a.x - b.x, a.y - b.y);
        expect(distance).toBeGreaterThanOrEqual(MIN_SEPARATION);
      }
    }
  });
});

describe("a retry re-lights one node", () => {
  it("draws one node for three attempts and names the latest row", () => {
    const result = layout(retryWorld());
    const steps = nodesOfKind(result, "step");
    expect(steps).toHaveLength(1);
    expect(steps[0]?.rowId).toBe("v1:work:step:flaky-3");
    expect(steps[0]?.status).toBe("done");
  });
});

describe("density: clusters are vertical, folds are horizontal", () => {
  it("collapses a column denser than the threshold", () => {
    const result = layout(fanOutWorld(DEFAULT_CLUSTER_THRESHOLD + 4));
    const column = result.columns.find((c) => c.depth === 1);
    expect(column?.clustered).toBe(true);
    expect(column?.count).toBe(DEFAULT_CLUSTER_THRESHOLD + 4);
    const cluster = nodesOfKind(result, "cluster")[0];
    expect(cluster?.standsFor).toBe(DEFAULT_CLUSTER_THRESHOLD + 4);
    // The steps it stands for are NOT also drawn.
    expect(nodesOfKind(result, "step").filter((n) => n.depth === 1)).toHaveLength(0);
  });

  it("expands a cluster on request, and the threshold is not a ceiling", () => {
    const w = fanOutWorld(DEFAULT_CLUSTER_THRESHOLD + 4);
    const result = layout(w, { expandedColumns: new Set([1]) });
    expect(result.columns.find((c) => c.depth === 1)?.clustered).toBe(false);
    expect(nodesOfKind(result, "step").filter((n) => n.depth === 1)).toHaveLength(
      DEFAULT_CLUSTER_THRESHOLD + 4,
    );
  });

  it("folds a finished stretch into one segment carrying its count", () => {
    const result = layout(chainWorld(DEFAULT_FOLD_THRESHOLD + 2, "done"));
    const folds = nodesOfKind(result, "fold");
    expect(folds).toHaveLength(1);
    expect(folds[0]?.standsFor).toBe(DEFAULT_FOLD_THRESHOLD + 2);
    expect(result.columns.filter((c) => c.folded)).toHaveLength(1);
  });

  it("does not fold a stretch that is still working", () => {
    const result = layout(chainWorld(DEFAULT_FOLD_THRESHOLD + 2, "running"));
    expect(nodesOfKind(result, "fold")).toHaveLength(0);
  });

  it("does not fold a stretch shorter than the threshold", () => {
    const result = layout(chainWorld(DEFAULT_FOLD_THRESHOLD - 1, "done"));
    expect(nodesOfKind(result, "fold")).toHaveLength(0);
  });

  it("expands a fold on request", () => {
    const result = layout(chainWorld(DEFAULT_FOLD_THRESHOLD + 2, "done"), {
      expandedFolds: new Set([0]),
    });
    expect(nodesOfKind(result, "fold")).toHaveLength(0);
    expect(nodesOfKind(result, "step")).toHaveLength(DEFAULT_FOLD_THRESHOLD + 2);
  });

  // A fold occupies ONE slot rather than leaving the gap of the columns it
  // replaced, which is the difference between a fold and a hidden column.
  it("closes the gap the folded columns left", () => {
    const folded = layout(chainWorld(8, "done"));
    const expanded = layout(chainWorld(8, "done"), { expandedFolds: new Set([0]) });
    expect(folded.goalX).toBeLessThan(expanded.goalX);
  });
});

describe("the road", () => {
  it("runs from you to the goal", () => {
    const result = layout(chainWorld(3, "running"));
    expect(result.road[0]?.x).toBe(0);
    expect(result.road[result.road.length - 1]?.x).toBe(result.goalX);
  });

  it("reports no progress on a run whose steps have not finished", () => {
    const result = layout(chainWorld(4, "running"));
    expect(result.roadProgress).toBeLessThan(0.5);
  });

  it("reports full progress on a run whose every column has landed", () => {
    const result = layout(chainWorld(4, "done"), { expandedFolds: new Set([0]) });
    expect(result.roadProgress).toBeGreaterThan(0.5);
  });
});

describe("what ran it, and what it had to ask", () => {
  it("draws a binding mark above the road only when the row carries one", () => {
    const bound = world({
      steps: [
        step("a", {
          seq: 0,
          status: "running",
          binding: {
            provider: "anthropic",
            model: "claude-opus-5",
            surface: "",
            workerId: "",
            nodeId: "",
            skillIds: [],
            present: true,
          },
        }),
      ],
    });
    const result = layout(bound);
    const marks = nodesOfKind(result, "binding");
    expect(marks).toHaveLength(1);
    expect(marks[0]?.label).toBe("claude-opus-5");
    expect(marks[0]?.y).toBeLessThan(0);
  });

  it("draws no binding mark for a step nothing has run yet", () => {
    expect(nodesOfKind(layout(chainWorld(2, "pending")), "binding")).toHaveLength(0);
  });

  it("hangs an approval under the step that raised it", () => {
    const w = world({
      steps: [step("a", { seq: 0, status: "waiting" })],
      approvals: [approval({ stepKey: "a" })],
    });
    const result = layout(w);
    const mark = nodesOfKind(result, "approval")[0];
    const owner = nodesOfKind(result, "step")[0];
    expect(mark?.x).toBe(owner?.x);
    expect(mark?.y).toBeGreaterThan(owner!.y);
    expect(result.edges.some((e) => e.kind === "asked")).toBe(true);
  });

  // A drawing decision must never make a question disappear.
  it("still draws an approval whose step is not on the map", () => {
    const w = world({
      steps: [step("a", { seq: 0, status: "done", finishedAt: moment(4) })],
      approvals: [approval({ stepKey: "a-step-from-somewhere-else" })],
    });
    expect(nodesOfKind(layout(w), "approval")).toHaveLength(1);
  });
});

describe("edges are real dependencies", () => {
  it("draws a flow edge for every dependsOn between two drawn steps", () => {
    const result = layout(fanOutWorld(3));
    const flows = result.edges.filter((e) => e.kind === "flow");
    // root -> p0,p1,p2 and p0,p1,p2 -> join
    expect(flows).toHaveLength(6);
  });

  it("draws no edge to a step this run does not have", () => {
    const w = world({ steps: [step("a", { seq: 0, dependsOn: ["elsewhere"] })] });
    expect(layout(w).edges.filter((e) => e.kind === "flow")).toHaveLength(0);
  });
});
