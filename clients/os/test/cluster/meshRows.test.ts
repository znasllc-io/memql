import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  groupByType,
  hearingState,
  linkSides,
  meshFingerprint,
  meshNodeFromRow,
  meshSentence,
  meshSentenceParts,
  runningNodes,
  spanOf,
  stateSentence,
  stateTone,
  tally,
  type MeshNode,
} from "../../src/apps/cluster/mesh/rows";

// The Mesh section's pure half (epic memql#5338): the projection of a
// `v1:cluster:node` row and its `mesh` report, and the verdict each node gets.
// Every case is judged against the REPORT's own time -- `lastSeen` -- never
// the browser's clock, which is why every fixture here pins both.

const REPORTED = "2026-09-22T12:00:00Z";

function node(over: { id?: string; nodeType?: string; lastSeen?: string; health?: string; mesh?: Record<string, unknown> | null }): Row {
  const row: Row = {
    id: `v1:cluster:node:${over.id ?? "edge-a"}`,
    nodeType: over.nodeType ?? "edge",
    address: "10.0.0.7:50062",
    health: over.health ?? "healthy",
    lastSeen: over.lastSeen ?? REPORTED,
    createdAt: "2026-09-22T09:00:00Z",
  };
  if (over.mesh !== null) {
    row.mesh = over.mesh ?? {
      receives: true,
      since: "2026-09-22T09:00:00Z",
      links: [{ node: "bff-a", type: "bff", via: "dialed" }],
      heard: 1204,
      duplicates: 310,
      originated: 7,
      relayed: 3,
      dropped: 0,
      hopLimited: 0,
      lastHeardAt: "2026-09-22T11:59:58Z",
    };
  }
  return row;
}

function project(over: Parameters<typeof node>[0]): MeshNode {
  return meshNodeFromRow(node(over));
}

describe("the projection", () => {
  it("strips the concept prefix, so a read and an event key the same node", () => {
    expect(project({ id: "edge-a" }).id).toBe("edge-a");
    expect(meshNodeFromRow({ ...node({}), id: "edge-a" }).id).toBe("edge-a");
  });

  it("reads an EVENT's payload, which wraps the row under `payload`", () => {
    const event = { id: "v1:cluster:node:edge-a", payload: { nodeType: "edge", mesh: { heard: 5, links: [] } } } as unknown as Row;
    const n = meshNodeFromRow(event);
    expect(n.nodeType).toBe("edge");
    expect(n.report?.heard).toEqual({ kind: "measured", value: 5 });
  });

  it("keeps a node with no report as NOT REPORTED, its counts absent rather than zero", () => {
    const n = project({ mesh: null });
    expect(n.report).toBeNull();
    expect(hearingState(n)).toBe("notReported");
  });

  it("reads a missing count as absent and a zero count as a measured zero", () => {
    const n = project({ mesh: { receives: true, since: "2026-09-22T09:00:00Z", links: [], heard: 0 } });
    expect(n.report?.heard).toEqual({ kind: "measured", value: 0 });
    expect(n.report?.relayed.kind).toBe("absent");
  });
});

describe("the verdict", () => {
  it("a linked node that heard recently is hearing", () => {
    expect(hearingState(project({}))).toBe("hearing");
  });

  it("identity sends only -- never an island, whatever it heard", () => {
    const n = project({ nodeType: "identity", mesh: { receives: false, since: "2026-09-22T09:00:00Z", links: [], heard: 0 } });
    expect(hearingState(n)).toBe("sendsOnly");
    expect(stateTone("sendsOnly")).toBe("muted");
  });

  it("a linked node that has heard nothing past the grace is NOT HEARING -- the island", () => {
    const n = project({ mesh: { receives: true, since: "2026-09-22T11:50:00Z", links: [{ node: "bff-a", type: "bff", via: "dialed" }], heard: 0 } });
    expect(hearingState(n)).toBe("notHearing");
    expect(stateTone("notHearing")).toBe("warn");
  });

  it("the same zero inside the grace is only starting", () => {
    const n = project({ mesh: { receives: true, since: "2026-09-22T11:58:30Z", links: [], heard: 0 } });
    expect(hearingState(n)).toBe("starting");
  });

  it("a node with no stream in either direction has no links", () => {
    const n = project({ mesh: { receives: true, since: "2026-09-22T11:00:00Z", links: [], heard: 40, lastHeardAt: "2026-09-22T11:59:00Z" } });
    expect(hearingState(n)).toBe("noLinks");
  });

  it("a node that heard, but nothing for five minutes before its report, is quiet", () => {
    const n = project({
      mesh: { receives: true, since: "2026-09-22T09:00:00Z", links: [{ node: "bff-a", type: "bff", via: "both" }], heard: 90, lastHeardAt: "2026-09-22T11:54:00Z" },
    });
    expect(hearingState(n)).toBe("quiet");
  });

  it("judges quiet against the REPORT, not the browser: an old report of a recent event is not quiet", () => {
    // Reported an hour before the test's clock, and heard two seconds before
    // that report. Against "now" this would read an hour quiet; it is not.
    const n = project({ lastSeen: "2026-09-22T11:00:00Z", mesh: { receives: true, since: "2026-09-22T09:00:00Z", links: [{ node: "bff-a", type: "bff", via: "dialed" }], heard: 3, lastHeardAt: "2026-09-22T10:59:58Z" } });
    expect(hearingState(n)).toBe("hearing");
  });
});

describe("a span of time", () => {
  it("is said in the unit a person would use, and never rounded up", () => {
    expect(spanOf(1)).toBe("1 minute");
    expect(spanOf(119)).toBe("119 minutes");
    expect(spanOf(120)).toBe("2 hours");
    expect(spanOf(179)).toBe("2 hours");
    expect(spanOf(180)).toBe("3 hours");
    expect(spanOf(48 * 60)).toBe("2 days");
  });

  it("is how the verdict dates a long silence", () => {
    const n = project({ mesh: { receives: true, since: "2026-09-22T09:00:00Z", links: [{ node: "bff-a", type: "bff", via: "dialed" }], heard: 0 } });
    expect(stateSentence(n)).toBe("This node has heard nothing in the 3 hours since it started, although it is linked to the mesh.");
  });
});

describe("the running nodes", () => {
  it("keeps what wrote its row in the last five minutes, and counts the rest", () => {
    const now = new Date("2026-09-22T12:02:00Z");
    const { running, hidden } = runningNodes(
      [
        project({ id: "fresh" }),
        project({ id: "old", lastSeen: "2026-09-22T11:40:00Z" }),
        project({ id: "stopped", health: "stopped" }),
      ],
      now,
    );
    expect(running.map((n) => n.id)).toEqual(["fresh"]);
    expect(hidden).toBe(2);
  });
});

describe("the groups", () => {
  it("orders types the way a reader meets them, and ids within each", () => {
    const groups = groupByType([
      project({ id: "identity-a", nodeType: "identity" }),
      project({ id: "edge-b", nodeType: "edge" }),
      project({ id: "bff-b", nodeType: "bff" }),
      project({ id: "edge-a", nodeType: "edge" }),
      project({ id: "bff-a", nodeType: "bff" }),
    ]);
    expect(groups.map((g) => g.label)).toEqual(["BFF", "Edge", "Identity"]);
    expect(groups[1]?.nodes.map((n) => n.id)).toEqual(["edge-a", "edge-b"]);
  });
});

describe("the sentence above the list", () => {
  it("names the node that needs a look", () => {
    const deaf = project({ id: "edge-a", mesh: { receives: true, since: "2026-09-22T11:00:00Z", links: [{ node: "bff-a", type: "bff", via: "dialed" }], heard: 0 } });
    expect(meshSentence(tally([project({ id: "bff-a", nodeType: "bff" }), deaf]))).toBe("edge-a is not hearing the cluster.");
  });

  it("names two, then counts the rest", () => {
    const deaf = (id: string) =>
      project({ id, mesh: { receives: true, since: "2026-09-22T11:00:00Z", links: [], heard: 0 } });
    expect(meshSentence(tally([deaf("edge-a"), deaf("edge-b"), deaf("mcp-a")]))).toBe(
      "3 nodes are not hearing the cluster: edge-a, edge-b and 1 other.",
    );
  });

  it("keeps a quiet node apart from a deaf one -- one stopped hearing, the other never heard", () => {
    const deaf = project({ id: "edge-b", mesh: { receives: true, since: "2026-09-22T09:00:00Z", links: [{ node: "bff-b", type: "bff", via: "dialed" }], heard: 0 } });
    const quiet = project({
      id: "planner-b",
      nodeType: "planner",
      mesh: { receives: true, since: "2026-09-22T09:00:00Z", links: [{ node: "bff-a", type: "bff", via: "dialed" }], heard: 90, lastHeardAt: "2026-09-22T11:49:00Z" },
    });
    // The planner sorts before the edge in the list; the deaf node is still
    // named first, because it is the more serious of the two readings.
    expect(meshSentence(tally([quiet, deaf]))).toBe("edge-b is not hearing the cluster. planner-b has gone quiet.");
  });

  it("hands every named node over as its own piece, so the page can open it and keep it whole", () => {
    const deaf = (id: string) =>
      project({ id, mesh: { receives: true, since: "2026-09-22T11:00:00Z", links: [], heard: 0 } });
    expect(meshSentenceParts(tally([deaf("edge-a"), deaf("edge-b")]))).toEqual([
      "2 nodes are not hearing the cluster: ",
      { node: "edge-a" },
      " and ",
      { node: "edge-b" },
      ".",
    ]);
    expect(meshSentenceParts(tally([deaf("edge-a"), deaf("edge-b"), deaf("mcp-a")]))).toEqual([
      "3 nodes are not hearing the cluster: ",
      { node: "edge-a" },
      ", ",
      { node: "edge-b" },
      " and 1 other",
      ".",
    ]);
  });

  it("never calls the mesh healthy on the strength of nodes that did not report", () => {
    expect(meshSentence(tally([project({ mesh: null })]))).toBe("No node has reported what it hears yet.");
    expect(meshSentence(tally([project({ id: "a" }), project({ id: "b", mesh: null })]))).toBe(
      "Every node that takes events is hearing the cluster. 1 node has not reported.",
    );
  });
});

describe("the links", () => {
  it("puts a peer linked both ways on both sides, and a missing peer as unlisted", () => {
    const n = project({
      id: "agent-a",
      nodeType: "agent",
      mesh: {
        receives: true,
        since: "2026-09-22T09:00:00Z",
        links: [
          { node: "bff-a", type: "bff", via: "both" },
          { node: "workbench-a", type: "workbench", via: "dialed" },
          { node: "planner-a", type: "planner", via: "accepted" },
        ],
        heard: 9,
      },
    });
    const bff = project({ id: "bff-a", nodeType: "bff" });
    const sides = linkSides(n, new Map([["bff-a", bff]]));
    expect(sides.opened.map((p) => p.id)).toEqual(["bff-a", "workbench-a"]);
    expect(sides.accepted.map((p) => p.id)).toEqual(["bff-a", "planner-a"]);
    expect(sides.opened[0]?.node).toBe(bff);
    expect(sides.opened[1]?.node).toBeNull();
  });
});

describe("the arrival cue", () => {
  it("does not ring on a heartbeat: counters moving is not news", () => {
    const before = project({});
    const after = project({
      lastSeen: "2026-09-22T12:01:00Z",
      mesh: {
        receives: true,
        since: "2026-09-22T09:00:00Z",
        links: [{ node: "bff-a", type: "bff", via: "dialed" }],
        heard: 1300,
        duplicates: 400,
        relayed: 9,
        lastHeardAt: "2026-09-22T12:00:59Z",
      },
    });
    expect(meshFingerprint(after)).toBe(meshFingerprint(before));
  });

  it("rings when the verdict or the links change", () => {
    const before = project({});
    const deaf = project({ mesh: { receives: true, since: "2026-09-22T09:00:00Z", links: [{ node: "bff-a", type: "bff", via: "dialed" }], heard: 0 } });
    const relinked = project({ mesh: { receives: true, since: "2026-09-22T09:00:00Z", links: [{ node: "bff-b", type: "bff", via: "dialed" }], heard: 4, lastHeardAt: "2026-09-22T11:59:00Z" } });
    expect(meshFingerprint(deaf)).not.toBe(meshFingerprint(before));
    expect(meshFingerprint(relinked)).not.toBe(meshFingerprint(before));
  });
});
