// The Cluster tab's state machine (memql#3312).
//
// The supersession tests are the point: four independent round-trips feed this
// panel, and #3304 exists because hand-rolled generation counters are exactly
// what gets forgotten. These assert that a response from a superseded
// generation writes NOTHING -- not the rows, not the status, not the outcome.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { DeploymentStatus, NextVersionSuggestion } from "@znasllc-io/memql-sdk-core/deploy";

import { ClusterPanelState, type ClusterRowSets } from "../src/deploy/clusterPanelState.js";
import type { DeployOutcome } from "../src/deploy/controller.js";

function deploymentRow(payload: Record<string, unknown>, createdAt = "2026-06-01T00:00:00Z"): Row {
  return { id: `v1:cluster:deployment:${payload["deploymentId"]}`, createdAt, payload };
}

function nodeRow(id: string, payload: Record<string, unknown>): Row {
  return { id, createdAt: "2026-06-01T00:00:00Z", payload };
}

const ROWS: ClusterRowSets = {
  nodes: [
    nodeRow("bff-1", { nodeType: "bff", health: "healthy", deploymentId: "current" }),
    nodeRow("bff-old", { nodeType: "bff", health: "healthy", deploymentId: "previous" }),
  ],
  deployments: [
    deploymentRow({ deploymentId: "current", version: "2.0.0", status: "succeeded" }, "2026-06-02T00:00:00Z"),
    deploymentRow({ deploymentId: "previous", version: "1.0.0", status: "superseded" }, "2026-06-01T00:00:00Z"),
  ],
  specs: [
    { id: "s1", payload: { deploymentId: "current", nodeType: "bff", replicas: 2 } },
    { id: "s2", payload: { deploymentId: "previous", nodeType: "bff", replicas: 2 } },
  ],
};

const EMPTY_ROWS: ClusterRowSets = { nodes: [], deployments: [], specs: [] };

function outcome(line: string): DeployOutcome {
  return { kind: "success", line, auditEventId: "a", permissionDenied: false };
}

const STATUS = { env: "staging", version: "2.0.0" } as DeploymentStatus;
const SUGGESTION = { currentVersion: "2.0.0", nextPatch: "2.0.1" } as NextVersionSuggestion;

// -----------------------------------------------------------------------------
// Derivation
// -----------------------------------------------------------------------------

test("one load derives topology, history, the current deployment and the orphans", () => {
  const state = new ClusterPanelState();
  return state.loadData(async () => ROWS).then(() => {
    assert.equal(state.currentDeployment, "current");
    assert.deepEqual(state.deployments.map((d) => d.deploymentId), ["current", "previous"]);
    assert.equal(state.topology.nodes.length, 2);
    assert.equal(state.topology.orphanCount, 1);
    // bff declares 2 and runs 1 non-orphaned node.
    assert.equal(state.topology.underReplicaCount, 1);
  });
});

test("a load failure surfaces the message and leaves the panel usable", async () => {
  const state = new ClusterPanelState();
  await state.loadData(async () => {
    throw new Error("query failed");
  });
  assert.equal(state.error, "query failed");
  assert.deepEqual(state.deployments, []);
});

test("a later success clears a previous load error", async () => {
  const state = new ClusterPanelState();
  await state.loadData(async () => {
    throw new Error("query failed");
  });
  await state.loadData(async () => ROWS);
  assert.equal(state.error, "");
});

// -----------------------------------------------------------------------------
// Selection + composition
// -----------------------------------------------------------------------------

test("selecting a deployment previews its composition, flagging leftovers", async () => {
  const state = new ClusterPanelState();
  await state.loadData(async () => ROWS);
  assert.equal(state.selectDeployment("previous"), true);
  const composition = state.composition();
  assert.deepEqual(composition.map((t) => t.nodeType), ["bff"]);
  assert.equal(composition[0].liveNodes, 1);
  assert.equal(composition[0].orphaned, true, "a superseded deployment still holding a node");
});

test("the current deployment's composition is not flagged", async () => {
  const state = new ClusterPanelState();
  await state.loadData(async () => ROWS);
  state.selectDeployment("current");
  assert.equal(state.composition()[0].orphaned, false);
});

test("with nothing selected there is no composition", async () => {
  const state = new ClusterPanelState();
  await state.loadData(async () => ROWS);
  assert.deepEqual(state.composition(), []);
  assert.equal(state.selectedDeployment, undefined);
});

test("re-selecting the same deployment reports no change", async () => {
  const state = new ClusterPanelState();
  await state.loadData(async () => ROWS);
  assert.equal(state.selectDeployment("current"), true);
  assert.equal(state.selectDeployment("current"), false);
});

test("a selection that no longer exists is dropped on reload", async () => {
  // Otherwise the composition pane shows an empty preview with a row
  // highlighted that is not in the list.
  const state = new ClusterPanelState();
  await state.loadData(async () => ROWS);
  state.selectDeployment("previous");
  await state.loadData(async () => ({ ...ROWS, deployments: [ROWS.deployments[0]] }));
  assert.equal(state.selectedDeploymentId, undefined);
});

test("a still-present selection survives a reload", async () => {
  const state = new ClusterPanelState();
  await state.loadData(async () => ROWS);
  state.selectDeployment("current");
  await state.loadData(async () => ROWS);
  assert.equal(state.selectedDeploymentId, "current");
});

// -----------------------------------------------------------------------------
// Supersession
// -----------------------------------------------------------------------------

test("a data response that lands after reset() writes nothing", async () => {
  const state = new ClusterPanelState();
  let release: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  const pending = state.loadData(async () => {
    await gate;
    return ROWS;
  });
  state.reset(); // The cluster switched underneath the in-flight read.
  release?.();
  assert.equal(await pending, false);
  assert.deepEqual(state.deployments, []);
  assert.equal(state.currentDeployment, "");
});

test("a data REJECTION that lands after reset() writes no error either", async () => {
  const state = new ClusterPanelState();
  let fail: (() => void) | undefined;
  const gate = new Promise<void>((_, reject) => {
    fail = () => reject(new Error("stale failure"));
  });
  const pending = state.loadData(async () => {
    await gate;
    return ROWS;
  });
  state.reset();
  fail?.();
  assert.equal(await pending, false);
  assert.equal(state.error, "");
});

test("a status response for the OLD env cannot land on the new one", async () => {
  // The status is per-env, so the one on screen would otherwise be about the
  // wrong environment -- silently.
  const state = new ClusterPanelState();
  let release: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  const pending = state.loadStatus(async () => {
    await gate;
    return { status: STATUS, message: "" };
  });
  state.setEnv("prod");
  release?.();
  assert.equal(await pending, false);
  assert.equal(state.status, null);
});

test("flipping env clears the status and the suggestion immediately", async () => {
  const state = new ClusterPanelState();
  await state.loadStatus(async () => ({ status: STATUS, message: "" }));
  await state.loadSuggestion(async () => ({ suggestion: SUGGESTION, message: "" }));
  assert.equal(state.setEnv("prod"), true);
  assert.equal(state.status, null);
  assert.equal(state.suggestion, null);
  assert.equal(state.env, "prod");
});

test("setting the env it already has is a no-op", async () => {
  const state = new ClusterPanelState();
  await state.loadStatus(async () => ({ status: STATUS, message: "" }));
  assert.equal(state.setEnv("staging"), false);
  assert.equal(state.status, STATUS, "an unchanged env must not discard a good read");
});

test("reset() does NOT bounce the operator's env choice back to staging", async () => {
  // A reconnect silently moving them off prod, while they are watching prod,
  // is a small betrayal at the worst moment.
  const state = new ClusterPanelState();
  state.setEnv("prod");
  state.reset();
  assert.equal(state.env, "prod");
});

test("a superseded action outcome is discarded", async () => {
  const state = new ClusterPanelState();
  const first = state.beginAction();
  const second = state.beginAction();
  assert.equal(state.settleAction(first, outcome("SUCCESS: first")), false);
  assert.equal(state.settleAction(second, outcome("SUCCESS: second")), true);
  assert.equal(state.lastOutcome?.line, "SUCCESS: second");
});

test("an action outcome landing after reset() is discarded", async () => {
  const state = new ClusterPanelState();
  const token = state.beginAction();
  state.reset();
  assert.equal(state.settleAction(token, outcome("SUCCESS: stale")), false);
  assert.equal(state.lastOutcome, undefined);
});

test("an access response that lands after reset() writes nothing", async () => {
  const state = new ClusterPanelState();
  let release: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  const pending = state.loadAccess(async () => {
    await gate;
    return "owner" as const;
  });
  state.reset();
  release?.();
  assert.equal(await pending, false);
  assert.equal(state.visibility.kind, "indeterminate");
});

// -----------------------------------------------------------------------------
// Access + status as first-class outcomes
// -----------------------------------------------------------------------------

test("a resolved role decides the visibility", async () => {
  const state = new ClusterPanelState();
  await state.loadAccess(async () => "reader");
  assert.deepEqual(state.visibility, { kind: "resolved", role: "reader" });
});

test("a failed access read becomes indeterminate, quoting the failure", async () => {
  const state = new ClusterPanelState();
  await state.loadAccess(async () => {
    throw new Error("stream closed");
  });
  assert.equal(state.visibility.kind, "indeterminate");
  if (state.visibility.kind === "indeterminate") {
    assert.match(state.visibility.reason, /stream closed/);
  }
});

test("a refused status read is recorded as a message, not as an error", async () => {
  // Topology and history are unaffected, so the panel shows one explained
  // section rather than treating the whole load as broken.
  const state = new ClusterPanelState();
  await state.loadData(async () => ROWS);
  await state.loadStatus(async () => ({ status: null, message: "requires the owner or admin cluster role" }));
  assert.equal(state.status, null);
  assert.match(state.statusMessage, /owner or admin/);
  assert.equal(state.error, "", "the concept-row read succeeded and must stay clean");
  assert.equal(state.deployments.length, 2);
});

// -----------------------------------------------------------------------------
// Live-updates notice
// -----------------------------------------------------------------------------

test("the live-updates notice survives a successful load", async () => {
  // A CDC subscription failing does not stop ordinary queries succeeding on
  // the same healthy connection, so routing it through `error` would wipe it
  // moments after showing it -- leaving the panel looking live when it is not.
  const state = new ClusterPanelState();
  state.setLiveUpdatesDegraded("live updates unavailable: not connected");
  await state.loadData(async () => ROWS);
  assert.match(state.liveUpdatesError, /not connected/);
});

test("the live-updates notice survives reset() and clears only on demand", () => {
  const state = new ClusterPanelState();
  state.setLiveUpdatesDegraded("off");
  state.reset();
  assert.equal(state.liveUpdatesError, "off");
  state.clearLiveUpdatesDegraded();
  assert.equal(state.liveUpdatesError, "");
});

test("reset() clears everything derived from the connection", async () => {
  const state = new ClusterPanelState();
  await state.loadData(async () => ROWS);
  await state.loadStatus(async () => ({ status: STATUS, message: "" }));
  await state.loadAccess(async () => "owner");
  state.selectDeployment("current");
  state.settleAction(state.beginAction(), outcome("SUCCESS: x"));
  state.setConnectionError("boom");

  state.reset();

  assert.deepEqual(state.deployments, []);
  assert.deepEqual(state.specs, []);
  assert.equal(state.topology.nodes.length, 0);
  assert.equal(state.currentDeployment, "");
  assert.equal(state.selectedDeploymentId, undefined);
  assert.equal(state.status, null);
  assert.equal(state.statusMessage, "");
  assert.equal(state.suggestion, null);
  assert.equal(state.visibility.kind, "indeterminate");
  assert.equal(state.error, "");
  assert.equal(state.lastOutcome, undefined);
});

test("an empty cluster derives an empty picture rather than throwing", async () => {
  const state = new ClusterPanelState();
  await state.loadData(async () => EMPTY_ROWS);
  assert.deepEqual(state.deployments, []);
  assert.equal(state.currentDeployment, "");
  assert.equal(state.topology.nodes.length, 0);
  assert.deepEqual(state.composition(), []);
});
