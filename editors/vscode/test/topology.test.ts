// Cluster topology projection (memql#3312) -- the orphan and under-replica
// rules, and the derived running version.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  projectDeployments,
  projectNodeSpecs,
  type DeploymentNodeSpec,
  type DeploymentRecord,
} from "../src/state/deploymentHistory.js";
import {
  buildTopology,
  liveNodeTypesFor,
  nodeLabel,
  shortenId,
  type TopologyNode,
} from "../src/state/topology.js";

function nodeRow(payload: Record<string, unknown>, id = String(payload["id"] ?? "n1")): Row {
  return { id, concept: "v1:cluster:node", createdAt: "2026-06-01T00:00:00Z", payload };
}

const deployments: DeploymentRecord[] = projectDeployments([
  {
    id: "v1:cluster:deployment:current",
    createdAt: "2026-06-02T00:00:00Z",
    payload: { deploymentId: "current", version: "2.0.0", status: "succeeded" },
  },
  {
    id: "v1:cluster:deployment:previous",
    createdAt: "2026-06-01T00:00:00Z",
    payload: { deploymentId: "previous", version: "1.0.0", status: "superseded" },
  },
]);

const specs: DeploymentNodeSpec[] = projectNodeSpecs([
  { id: "s1", payload: { deploymentId: "current", nodeType: "bff", replicas: 2 } },
  { id: "s2", payload: { deploymentId: "current", nodeType: "agent", replicas: 1, version: "2.0.1" } },
  { id: "s3", payload: { deploymentId: "previous", nodeType: "bff", replicas: 2 } },
]);

function build(nodeRows: Row[], currentDeploymentId = "current") {
  return buildTopology({ nodeRows, deployments, specs, currentDeploymentId });
}

function byId(nodes: TopologyNode[], id: string): TopologyNode {
  const found = nodes.find((n) => n.id === id);
  assert.ok(found !== undefined, `no node ${id}`);
  return found;
}

// -----------------------------------------------------------------------------
// Orphan rules
// -----------------------------------------------------------------------------

test("a healthy node on the current deployment is not an orphan", () => {
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "current" }, "a")]);
  assert.equal(byId(topology.nodes, "a").orphan, false);
  assert.equal(topology.orphanCount, 0);
});

test("a node carrying a superseded deployment id is an orphan, with the reason", () => {
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "previous" }, "a")]);
  const node = byId(topology.nodes, "a");
  assert.equal(node.orphan, true);
  assert.match(node.orphanReason, /previous/);
  assert.match(node.orphanReason, /not the current deployment/);
});

test("a stopped node is an orphan even on the current deployment", () => {
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "stopped", deploymentId: "current" }, "a")]);
  assert.deepEqual(
    { orphan: byId(topology.nodes, "a").orphan, reason: byId(topology.nodes, "a").orphanReason },
    { orphan: true, reason: "node is stopped" },
  );
});

test("stopped wins over stale-deployment as the reported reason", () => {
  // Both apply; "stopped" is the more specific fact and tells the operator the
  // pod is already gone rather than still serving.
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "stopped", deploymentId: "previous" }, "a")]);
  assert.equal(byId(topology.nodes, "a").orphanReason, "node is stopped");
});

test("a node with no deploymentId is never a stale-deployment orphan", () => {
  // deploymentId is stamped at registration (#1873). A node registered before
  // that has no deployment to compare, and flagging it would turn a missing
  // field into an alarm.
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "healthy" }, "a")]);
  assert.equal(byId(topology.nodes, "a").orphan, false);
});

test("an unknown current deployment suppresses stale-deployment orphaning entirely", () => {
  // Marking the whole cluster orphaned on the strength of a failed history
  // read would be far worse than saying nothing.
  const topology = build(
    [
      nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "previous" }, "a"),
      nodeRow({ id: "b", nodeType: "bff", health: "healthy", deploymentId: "current" }, "b"),
    ],
    "",
  );
  assert.equal(topology.orphanCount, 0);
});

test("a stopped node is still an orphan when the current deployment is unknown", () => {
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "stopped" }, "a")], "");
  assert.equal(topology.orphanCount, 1);
});

// -----------------------------------------------------------------------------
// Running version (derived, not read)
// -----------------------------------------------------------------------------

test("a node's version resolves through its deployment's engine version", () => {
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "current" }, "a")]);
  assert.equal(byId(topology.nodes, "a").version, "2.0.0");
});

test("a per-tier pin wins over the deployment's engine version", () => {
  const topology = build([nodeRow({ id: "a", nodeType: "agent", health: "healthy", deploymentId: "current" }, "a")]);
  assert.equal(byId(topology.nodes, "a").version, "2.0.1");
});

test("an orphan reports the version IT is running, not the current one", () => {
  // The whole value of showing an orphan is knowing which release it is still
  // serving.
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "previous" }, "a")]);
  assert.equal(byId(topology.nodes, "a").version, "1.0.0");
});

test("a node with no deploymentId has no resolvable version", () => {
  // `v1:cluster:node` carries no version field, so without a deployment there
  // is nothing to resolve through -- and reporting the cluster's current
  // version anyway would be a guess presented as a fact.
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "healthy" }, "a")]);
  assert.equal(byId(topology.nodes, "a").version, "");
});

// -----------------------------------------------------------------------------
// Replica tally
// -----------------------------------------------------------------------------

test("a tier at its declared replica count is not flagged", () => {
  const topology = build([
    nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "current" }, "a"),
    nodeRow({ id: "b", nodeType: "bff", health: "healthy", deploymentId: "current" }, "b"),
  ]);
  const bff = topology.tiers.find((t) => t.nodeType === "bff");
  assert.deepEqual({ expected: bff?.expected, running: bff?.running, under: bff?.under }, {
    expected: 2,
    running: 2,
    under: false,
  });
  // The agent tier declares 1 and has none running, so the CLUSTER is still
  // short even though bff is whole -- which is the number the summary line
  // reports.
  assert.equal(topology.underReplicaCount, 1);
});

test("a tier short of its declared count is flagged", () => {
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "current" }, "a")]);
  assert.equal(topology.tiers.find((t) => t.nodeType === "bff")?.under, true);
});

test("a declared tier with nothing running still appears, flagged", () => {
  // A tier that failed to come up at all is the loudest possible
  // under-replica and must not vanish for want of a node to project.
  const topology = build([]);
  const agent = topology.tiers.find((t) => t.nodeType === "agent");
  assert.deepEqual({ expected: agent?.expected, running: agent?.running, under: agent?.under }, {
    expected: 1,
    running: 0,
    under: true,
  });
});

test("orphans do not count toward a tier's running total", () => {
  // A tier held up entirely by nodes from a superseded release is NOT
  // correctly provisioned, and counting them would hide exactly that.
  const topology = build([
    nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "current" }, "a"),
    nodeRow({ id: "b", nodeType: "bff", health: "healthy", deploymentId: "previous" }, "b"),
  ]);
  const bff = topology.tiers.find((t) => t.nodeType === "bff");
  assert.equal(bff?.running, 1);
  assert.equal(bff?.under, true);
});

test("a draining node still counts as serving", () => {
  // Excluding it would make every ordinary rolling restart report a spurious
  // under-replica alarm for as long as the drain lasts.
  const topology = build([
    nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "current" }, "a"),
    nodeRow({ id: "b", nodeType: "bff", health: "draining", deploymentId: "current" }, "b"),
  ]);
  assert.equal(topology.tiers.find((t) => t.nodeType === "bff")?.running, 2);
});

test("offline nodes do not count as serving", () => {
  const topology = build([
    nodeRow({ id: "a", nodeType: "bff", health: "offline", deploymentId: "current" }, "a"),
  ]);
  assert.equal(topology.tiers.find((t) => t.nodeType === "bff")?.running, 0);
});

test("an undeclared but running tier is tallied and never flagged", () => {
  const topology = build([
    nodeRow({ id: "a", nodeType: "voice", health: "healthy", deploymentId: "current" }, "a"),
  ]);
  const voice = topology.tiers.find((t) => t.nodeType === "voice");
  assert.deepEqual({ expected: voice?.expected, running: voice?.running, under: voice?.under }, {
    expected: 0,
    running: 1,
    under: false,
  });
});

test("no current deployment means no declared expectations at all", () => {
  const topology = build([nodeRow({ id: "a", nodeType: "bff", health: "healthy" }, "a")], "");
  assert.equal(topology.tiers.find((t) => t.nodeType === "bff")?.expected, 0);
  assert.equal(topology.underReplicaCount, 0);
});

// -----------------------------------------------------------------------------
// Live-node indexes and small helpers
// -----------------------------------------------------------------------------

test("live nodes are indexed per deployment for the composition preview", () => {
  const topology = build([
    nodeRow({ id: "a", nodeType: "bff", health: "healthy", deploymentId: "previous" }, "a"),
    nodeRow({ id: "b", nodeType: "bff", health: "healthy", deploymentId: "previous" }, "b"),
    nodeRow({ id: "c", nodeType: "bff", health: "stopped", deploymentId: "previous" }, "c"),
  ]);
  assert.deepEqual([...liveNodeTypesFor(topology, "previous")], [["bff", 2]]);
  assert.deepEqual([...liveNodeTypesFor(topology, "current")], []);
  assert.deepEqual([...liveNodeTypesFor(topology, "")], []);
});

test("nodes are sorted by type then id so the grid does not shuffle", () => {
  const topology = build([
    nodeRow({ id: "z", nodeType: "voice", health: "healthy" }, "z"),
    nodeRow({ id: "b", nodeType: "bff", health: "healthy" }, "b"),
    nodeRow({ id: "a", nodeType: "bff", health: "healthy" }, "a"),
  ]);
  assert.deepEqual(topology.nodes.map((n) => n.id), ["a", "b", "z"]);
});

function label(nodeType: string, flavor: string, id: string): string {
  const node: TopologyNode = {
    id,
    nodeType,
    flavor,
    health: "healthy",
    address: "",
    lastSeen: "",
    deploymentId: "",
    version: "",
    orphan: false,
    orphanReason: "",
    live: true,
  };
  return nodeLabel(node);
}

test("nodeLabel names the flavor only when there is one", () => {
  assert.equal(label("cognition", "", "x"), "cognition");
  assert.equal(label("bff", "shop", "x"), "bff / shop");
  // A typeless node still has to be identifiable in the grid, so it falls back
  // to the id and then to a placeholder.
  assert.equal(label("", "", "x"), "x");
  assert.equal(label("", "", ""), "(unnamed node)");
});

test("shortenId keeps the identifying tail of a canonical id", () => {
  assert.equal(shortenId("v1:cluster:node:abcdef0123456789"), "abcdef012345");
  assert.equal(shortenId("bff-local"), "bff-local");
  assert.equal(shortenId(""), "");
  assert.equal(shortenId("v1:cluster:node:abcdef0123456789", 24), "abcdef0123456789");
});
