// State -> view-kit view models (memql#3312).
//
// These pin the WORDING, which is the entire output of the topology and
// replica surfaces. "1 of 2 [under-replica]" versus "2 of 2" is the difference
// between an operator paging someone and going to lunch, and asserting on it
// is what proves the rules in state/topology.ts actually reach the screen.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  compositionViews,
  historyViews,
  replicaTallyViews,
  topologySummary,
  topologyViews,
} from "../src/deploy/clusterView.js";
import {
  composeDeployment,
  projectDeployments,
  projectNodeSpecs,
} from "../src/state/deploymentHistory.js";
import { buildTopology, liveNodeTypesFor } from "../src/state/topology.js";

function nodeRow(id: string, payload: Record<string, unknown>): Row {
  return { id, createdAt: "2026-06-01T00:00:00Z", payload };
}

const deployments = projectDeployments([
  {
    id: "d",
    createdAt: "2026-06-02T00:00:00Z",
    payload: { deploymentId: "current", version: "2.0.0", status: "succeeded", environment: "staging", provider: "azure" },
  },
  {
    id: "p",
    createdAt: "2026-06-01T00:00:00Z",
    payload: { deploymentId: "previous", version: "1.0.0", status: "superseded", environment: "staging", provider: "azure" },
  },
]);

const specs = projectNodeSpecs([
  { id: "s1", payload: { deploymentId: "current", nodeType: "bff", replicas: 2, imageDigest: "sha256:0123456789abcdef" } },
  { id: "s2", payload: { deploymentId: "current", nodeType: "agent", replicas: 1, version: "2.0.1" } },
  { id: "s3", payload: { deploymentId: "previous", nodeType: "bff", replicas: 2 } },
]);

function topologyOf(nodeRows: Row[]) {
  return buildTopology({ nodeRows, deployments, specs, currentDeploymentId: "current" });
}

// -----------------------------------------------------------------------------
// Topology tiles
// -----------------------------------------------------------------------------

test("a tile carries the label, short ids, resolved version and health", () => {
  const views = topologyViews(
    topologyOf([
      nodeRow("v1:cluster:node:bff-local", {
        nodeType: "bff",
        flavor: "shop",
        health: "healthy",
        address: "10.0.0.1:50051",
        deploymentId: "current",
      }),
    ]),
  );
  assert.equal(views.length, 1);
  assert.equal(views[0].label, "bff / shop");
  assert.equal(views[0].version, "2.0.0");
  assert.equal(views[0].deployment, "current");
  assert.equal(views[0].health, "healthy");
  assert.match(views[0].detail, /bff-local -- 10\.0\.0\.1:50051/);
  assert.equal(views[0].orphan, false);
});

test("an orphan tile carries its reason for the tooltip", () => {
  const views = topologyViews(
    topologyOf([nodeRow("n", { nodeType: "bff", health: "healthy", deploymentId: "previous" })]),
  );
  assert.equal(views[0].orphan, true);
  assert.match(views[0].orphanReason, /not the current deployment/);
  assert.equal(views[0].version, "1.0.0", "an orphan reports the release IT runs");
});

test("a long deployment id is shortened for the tile", () => {
  const views = topologyViews(
    topologyOf([nodeRow("n", { nodeType: "bff", health: "healthy", deploymentId: "current" })]),
  );
  assert.ok(views[0].deployment.length <= 12);
});

test("the tile detail collapses to the id when the address adds nothing", () => {
  const views = topologyViews(topologyOf([nodeRow("bff-local", { nodeType: "bff", health: "healthy" })]));
  assert.equal(views[0].detail, "bff-local");
});

// -----------------------------------------------------------------------------
// Replica tally
// -----------------------------------------------------------------------------

test("a whole tier reads N of N with no flag", () => {
  const views = replicaTallyViews(
    topologyOf([
      nodeRow("a", { nodeType: "bff", health: "healthy", deploymentId: "current" }),
      nodeRow("b", { nodeType: "bff", health: "healthy", deploymentId: "current" }),
    ]),
  );
  const bff = views.find((v) => v.name === "bff");
  assert.equal(bff?.count, "2 of 2");
  assert.equal(bff?.flag, "");
});

test("a short tier reads N of M and is flagged under-replica", () => {
  const views = replicaTallyViews(
    topologyOf([nodeRow("a", { nodeType: "bff", health: "healthy", deploymentId: "current" })]),
  );
  const bff = views.find((v) => v.name === "bff");
  assert.equal(bff?.count, "1 of 2");
  assert.equal(bff?.flag, "under-replica");
});

test("an undeclared tier reads N running, not N of 0", () => {
  // "of 0" would look like a violated expectation when in fact no expectation
  // exists.
  const views = replicaTallyViews(
    topologyOf([nodeRow("a", { nodeType: "voice", health: "healthy", deploymentId: "current" })]),
  );
  const voice = views.find((v) => v.name === "voice");
  assert.equal(voice?.count, "1 running");
  assert.equal(voice?.detail, "no replica count declared");
  assert.equal(voice?.flag, "");
});

// -----------------------------------------------------------------------------
// Composition
// -----------------------------------------------------------------------------

function compositionFor(deploymentId: string, nodeRows: Row[]) {
  const topology = topologyOf(nodeRows);
  const deployment = deployments.find((d) => d.deploymentId === deploymentId);
  return compositionViews(
    composeDeployment(
      deployment,
      specs,
      liveNodeTypesFor(topology, deploymentId),
      deploymentId === "current",
    ),
  );
}

test("an inherited version is marked (engine); a pinned one is not", () => {
  // The concept treats a pin and the spine as genuinely different states, and
  // rendering them identically would hide that a tier is pinned away from the
  // rest of the release.
  const views = compositionFor("current", []);
  assert.equal(views.find((v) => v.name === "agent")?.detail, "2.0.1");
  assert.match(views.find((v) => v.name === "bff")?.detail ?? "", /^2\.0\.0 \(engine\) -- 0123456789ab$/);
});

test("composition counts declared replicas, and flags a superseded deployment's leftovers", () => {
  const views = compositionFor("previous", [
    nodeRow("a", { nodeType: "bff", health: "healthy", deploymentId: "previous" }),
  ]);
  const bff = views.find((v) => v.name === "bff");
  assert.equal(bff?.count, "2 replicas");
  assert.equal(bff?.flag, "1 orphaned");
});

test("the current deployment's composition is never flagged", () => {
  const views = compositionFor("current", [
    nodeRow("a", { nodeType: "bff", health: "healthy", deploymentId: "current" }),
  ]);
  assert.equal(views.find((v) => v.name === "bff")?.flag, "");
});

// -----------------------------------------------------------------------------
// History + summary
// -----------------------------------------------------------------------------

test("history marks the current deployment and only it", () => {
  const views = historyViews(deployments, "current");
  assert.deepEqual(views.map((v) => [v.id, v.current]), [
    ["current", true],
    ["previous", false],
  ]);
  assert.equal(views[0].environment, "staging");
  assert.equal(views[0].provider, "azure");
  assert.equal(views[0].status, "succeeded");
});

test("an unknown current deployment marks nothing", () => {
  assert.deepEqual(historyViews(deployments, "").map((v) => v.current), [false, false]);
});

test("the summary names the counts an operator would act on", () => {
  const clean = topologyOf([
    nodeRow("a", { nodeType: "bff", health: "healthy", deploymentId: "current" }),
    nodeRow("b", { nodeType: "bff", health: "healthy", deploymentId: "current" }),
    nodeRow("c", { nodeType: "agent", health: "healthy", deploymentId: "current" }),
  ]);
  assert.equal(topologySummary(clean), "3 nodes");

  const messy = topologyOf([
    nodeRow("a", { nodeType: "bff", health: "healthy", deploymentId: "previous" }),
  ]);
  assert.match(topologySummary(messy), /^1 nodes, 1 orphaned, 2 node types under-replicated$/);
});
