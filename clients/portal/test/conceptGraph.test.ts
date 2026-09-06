// The conceptGraph scene's arithmetic (epic memql#4661, task memql#4672).
//
// Everything the graph can be wrong about that does not need a GPU is here,
// which is the same split the scene registry makes between scenes/ and
// scenes/ConceptGraphCanvas.tsx -- and for the same reason: a scene that renders
// nothing at all does so SILENTLY, with a live WebGL context and a full scene
// graph behind it, so the parts that can be asserted have to be assertable.

import { describe, expect, it } from "vitest";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import {
  GRAPH_NODE_CAP,
  buildConceptGraph,
  spherePoint,
} from "../src/scenes/conceptGraph";

const AGENT: Concept = {
  id: "v1:agents:agent",
  version: "v1",
  domain: "agents",
  entity: "agent",
  type: "concept",
  description: "",
  displayCard: { primary: "name", status: "kind" },
  relationships: [
    {
      type: "references",
      as: "reportsTo",
      field: "managerId",
      target: "v1:agents:agent",
      direction: "outgoing",
    },
  ],
};

function agent(id: string, name: string, managerId = ""): Row {
  return { id, name, kind: "specialist", ...(managerId === "" ? {} : { managerId }) } as unknown as Row;
}

describe("nodes", () => {
  it("labels every node through the concept's own display card", () => {
    const graph = buildConceptGraph(AGENT, [agent("agent:a1", "Planner")]);
    expect(graph.nodes).toHaveLength(1);
    expect(graph.nodes[0]?.label).toBe("Planner");
    expect(graph.nodes[0]?.status).toBe("specialist");
  });

  it("falls back to the id rather than labelling a node with nothing", () => {
    // A row whose primary field is empty is a real state, and an unlabelled
    // point in a constellation is a point nobody can identify.
    const graph = buildConceptGraph(AGENT, [{ id: "agent:a9" } as unknown as Row]);
    expect(graph.nodes[0]?.label).toBe("agent:a9");
  });

  it("caps the node count and REPORTS what it left out", () => {
    // A sphere of ten thousand points is not a reading of anything. But a
    // graph that quietly showed 300 of 4,000 would be read as the whole
    // population, which is worse than showing fewer and saying so.
    const many = Array.from({ length: GRAPH_NODE_CAP + 25 }, (_, i) =>
      agent(`agent:x${i}`, `Agent ${i}`),
    );
    const graph = buildConceptGraph(AGENT, many);
    expect(graph.nodes).toHaveLength(GRAPH_NODE_CAP);
    expect(graph.omitted).toBe(25);
  });

  it("reports nothing omitted when nothing was", () => {
    expect(buildConceptGraph(AGENT, [agent("agent:a1", "One")]).omitted).toBe(0);
  });
});

describe("edges", () => {
  it("comes from the DECLARATION, which is the only place the answer exists", () => {
    // A foreign key is a string like any other string. Before ConceptInfo
    // carried relationships (memql#4662) there was no way to know that
    // `managerId` points at an agent and `name` does not, so this scene was
    // not possible at all.
    const graph = buildConceptGraph(AGENT, [
      agent("agent:a1", "Planner"),
      agent("agent:a2", "Scribe", "agent:a1"),
    ]);
    expect(graph.edges).toEqual([{ from: "agent:a2", to: "agent:a1", label: "reportsTo" }]);
  });

  it("labels an edge with what it MEANS, never with the engine's structural type", () => {
    // `as` is the domain label; `type` is the engine's word. Falling back to
    // `type` would present "references" to a person as though somebody had
    // chosen it as a noun (memql#3652).
    const unlabelled: Concept = {
      ...AGENT,
      relationships: [
        { type: "parent", as: "", field: "managerId", target: "v1:agents:agent", direction: "outgoing" },
      ],
    };
    const graph = buildConceptGraph(unlabelled, [
      agent("agent:a1", "Planner"),
      agent("agent:a2", "Scribe", "agent:a1"),
    ]);
    expect(graph.edges[0]?.label).toBe("managerId");
    expect(graph.edges[0]?.label).not.toBe("parent");
  });

  it("draws only edges whose BOTH ends are loaded", () => {
    // A pointer to a row that is not in this set has no position to draw to,
    // and drawing it to a stub would invent a node the reader takes for real.
    const graph = buildConceptGraph(AGENT, [agent("agent:a2", "Scribe", "agent:elsewhere")]);
    expect(graph.edges).toEqual([]);
    expect(graph.nodes).toHaveLength(1);
  });

  it("draws no self-edge", () => {
    const graph = buildConceptGraph(AGENT, [agent("agent:a1", "Planner", "agent:a1")]);
    expect(graph.edges).toEqual([]);
  });

  it("skips a relationship whose pointer sits in a nested block", () => {
    // A dotted path is a real declaration and simply has no top-level cell
    // here to read from -- so it produces no edge rather than an edge to
    // nothing.
    const nested: Concept = {
      ...AGENT,
      relationships: [
        {
          type: "references",
          as: "raisedBy",
          field: "lineage.originatingPlanId",
          target: "v1:planner:plan",
          direction: "outgoing",
        },
      ],
    };
    expect(buildConceptGraph(nested, [agent("agent:a1", "Planner")]).edges).toEqual([]);
  });

  it("draws nothing for a concept that declares no relationships", () => {
    const bare: Concept = { ...AGENT, relationships: [] };
    const graph = buildConceptGraph(bare, [agent("agent:a1", "One"), agent("agent:a2", "Two")]);
    expect(graph.nodes).toHaveLength(2);
    expect(graph.edges).toEqual([]);
  });
});

describe("layout", () => {
  it("is deterministic by index, so one more row nudges the picture rather than replacing it", () => {
    // A random layout would move every node whenever a row arrived, and a
    // graph that reshuffles on every CDC event is unreadable.
    const first = spherePoint(3, 10);
    const again = spherePoint(3, 10);
    expect(again).toEqual(first);
  });

  it("keeps every point on the unit sphere", () => {
    for (let i = 0; i < 50; i += 1) {
      const p = spherePoint(i, 50);
      const r = Math.sqrt(p.x * p.x + p.y * p.y + p.z * p.z);
      // The Fibonacci lattice puts every point at radius 1 by construction.
      expect(Math.abs(r - 1)).toBeLessThan(1e-9);
    }
  });

  it("does not divide by zero for a single row", () => {
    expect(spherePoint(0, 1)).toEqual({ x: 0, y: 0, z: 0 });
    expect(buildConceptGraph(AGENT, [agent("agent:a1", "Only")]).nodes).toHaveLength(1);
  });

  it("spreads points rather than stacking them", () => {
    // The anti-vacuous half: a layout that returned the origin for everything
    // would satisfy "deterministic" and draw a single dot.
    const points = Array.from({ length: 12 }, (_, i) => spherePoint(i, 12));
    const distinct = new Set(points.map((p) => `${p.x.toFixed(4)},${p.y.toFixed(4)}`));
    expect(distinct.size).toBe(12);
  });
});
