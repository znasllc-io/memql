// Deployment history + composition projection (memql#3312).
//
// The two assertions worth reading first are the `rolled_back` one and the
// empty-current one -- both encode a rule that is easy to get backwards and
// whose failure mode is the panel confidently lying to an operator mid-incident.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  composeDeployment,
  currentDeploymentId,
  indexDeployments,
  indexSpecsByTier,
  projectDeployments,
  projectNodeSpecs,
  resolveTierVersion,
  shortenDigest,
  tierKey,
  type DeploymentRecord,
} from "../src/state/deploymentHistory.js";

// A wire row as browseConceptPage returns it: intrinsics at the top level,
// concept fields nested under payload.
function deploymentRow(payload: Record<string, unknown>, createdAt = "2026-06-01T00:00:00Z"): Row {
  return { id: `v1:cluster:deployment:${payload["deploymentId"]}`, concept: "v1:cluster:deployment", createdAt, payload };
}

function specRow(payload: Record<string, unknown>, createdAt = "2026-06-01T00:00:00Z"): Row {
  return { id: "v1:cluster:deploymentNodeSpec:x", concept: "v1:cluster:deploymentNodeSpec", createdAt, payload };
}

// -----------------------------------------------------------------------------
// projectDeployments
// -----------------------------------------------------------------------------

test("deployments come back newest-cut first", () => {
  const records = projectDeployments([
    deploymentRow({ deploymentId: "d1", version: "1.0.0", status: "superseded" }, "2026-06-01T00:00:00Z"),
    deploymentRow({ deploymentId: "d3", version: "3.0.0", status: "succeeded" }, "2026-06-03T00:00:00Z"),
    deploymentRow({ deploymentId: "d2", version: "2.0.0", status: "failed" }, "2026-06-02T00:00:00Z"),
  ]);
  assert.deepEqual(records.map((r) => r.deploymentId), ["d3", "d2", "d1"]);
});

test("an append-only timeline collapses to its newest payload", () => {
  // The concept appends a payload version per status transition under one row
  // id, so the same deploymentId legitimately arrives more than once.
  const records = projectDeployments([
    deploymentRow({ deploymentId: "d1", status: "pending", updatedAt: "2026-06-01T00:00:00Z" }),
    deploymentRow({ deploymentId: "d1", status: "in_progress", updatedAt: "2026-06-01T00:05:00Z" }),
    deploymentRow({ deploymentId: "d1", status: "succeeded", updatedAt: "2026-06-01T00:09:00Z" }),
  ]);
  assert.equal(records.length, 1);
  assert.equal(records[0].status, "succeeded");
});

test("ordering follows createdAt, NOT updatedAt", () => {
  // A list that re-orders itself every time a status advances is one an
  // operator stops trusting. The older deploy transitioning most recently must
  // not jump to the top.
  const records = projectDeployments([
    deploymentRow(
      { deploymentId: "old", status: "succeeded", updatedAt: "2026-06-09T00:00:00Z" },
      "2026-06-01T00:00:00Z",
    ),
    deploymentRow(
      { deploymentId: "new", status: "pending", updatedAt: "2026-06-05T00:00:00Z" },
      "2026-06-05T00:00:00Z",
    ),
  ]);
  assert.deepEqual(records.map((r) => r.deploymentId), ["new", "old"]);
});

test("a record with no deploymentId payload falls back to the row id", () => {
  // Without a handle there is nothing Deploy / RollbackDeployment could target,
  // so the record would render but be impossible to act on.
  const records = projectDeployments([
    { id: "v1:cluster:deployment:abc", createdAt: "2026-06-01T00:00:00Z", payload: { status: "succeeded" } },
  ]);
  assert.equal(records[0].deploymentId, "v1:cluster:deployment:abc");
});

test("a record with neither a payload id nor a row id is dropped", () => {
  assert.deepEqual(projectDeployments([{ payload: { status: "succeeded" } }]), []);
});

test("records cut in the same instant still get a total order", () => {
  const records = projectDeployments([
    deploymentRow({ deploymentId: "b" }, "2026-06-01T00:00:00Z"),
    deploymentRow({ deploymentId: "a" }, "2026-06-01T00:00:00Z"),
  ]);
  assert.deepEqual(records.map((r) => r.deploymentId), ["a", "b"]);
});

// -----------------------------------------------------------------------------
// currentDeploymentId
// -----------------------------------------------------------------------------

function records(...rows: { id: string; status: string; createdAt: string }[]): DeploymentRecord[] {
  return projectDeployments(
    rows.map((r) => deploymentRow({ deploymentId: r.id, status: r.status }, r.createdAt)),
  );
}

test("the newest succeeded deployment is current", () => {
  const current = currentDeploymentId(
    records(
      { id: "d1", status: "succeeded", createdAt: "2026-06-01T00:00:00Z" },
      { id: "d2", status: "succeeded", createdAt: "2026-06-02T00:00:00Z" },
    ),
  );
  assert.equal(current, "d2");
});

test("a rolled_back deployment IS current -- it is the live one", () => {
  // A rollback creates a NEW record pointing at the historical digest, and the
  // driveDeploymentInProgress automation lands it in `rolled_back`
  // (deployment-console.md, "Terminal-status parity", #2168). Treating that as
  // not-current would mark the entire cluster orphaned for the whole life of a
  // rolled-back release -- exactly when an operator can least afford it.
  const current = currentDeploymentId(
    records(
      { id: "d1", status: "succeeded", createdAt: "2026-06-01T00:00:00Z" },
      { id: "d2", status: "rolled_back", createdAt: "2026-06-02T00:00:00Z" },
    ),
  );
  assert.equal(current, "d2");
});

test("superseded, failed, pending and in_progress are never current", () => {
  for (const status of ["superseded", "failed", "pending", "in_progress"]) {
    const current = currentDeploymentId(
      records(
        { id: "landed", status: "succeeded", createdAt: "2026-06-01T00:00:00Z" },
        { id: "newer", status, createdAt: "2026-06-02T00:00:00Z" },
      ),
    );
    assert.equal(current, "landed", `status ${status} must not become current`);
  }
});

test("a deploy in flight leaves the previous deployment current", () => {
  // The nodes still serving are the OLD deployment's, so they must not be
  // flagged orphaned for the duration of a rollout.
  const current = currentDeploymentId(
    records(
      { id: "live", status: "succeeded", createdAt: "2026-06-01T00:00:00Z" },
      { id: "shipping", status: "in_progress", createdAt: "2026-06-02T00:00:00Z" },
    ),
  );
  assert.equal(current, "live");
});

test("nothing landed yields an empty current, not a guess", () => {
  assert.equal(currentDeploymentId(records({ id: "d1", status: "pending", createdAt: "2026-06-01T00:00:00Z" })), "");
  assert.equal(currentDeploymentId([]), "");
});

test("the answer does not depend on the caller preserving the sort", () => {
  const sorted = records(
    { id: "d1", status: "succeeded", createdAt: "2026-06-01T00:00:00Z" },
    { id: "d2", status: "succeeded", createdAt: "2026-06-02T00:00:00Z" },
  );
  assert.equal(currentDeploymentId([...sorted].reverse()), "d2");
});

// -----------------------------------------------------------------------------
// projectNodeSpecs
// -----------------------------------------------------------------------------

test("specs collapse per (deployment, nodeType) to the newest", () => {
  const specs = projectNodeSpecs([
    specRow({ deploymentId: "d1", nodeType: "bff", replicas: 1, updatedAt: "2026-06-01T00:00:00Z" }),
    specRow({ deploymentId: "d1", nodeType: "bff", replicas: 2, updatedAt: "2026-06-02T00:00:00Z" }),
    specRow({ deploymentId: "d1", nodeType: "agent", replicas: 3, updatedAt: "2026-06-01T00:00:00Z" }),
  ]);
  assert.equal(specs.length, 2);
  assert.equal(specs.find((s) => s.nodeType === "bff")?.replicas, 2);
});

test("a replicas count arriving as a protojson string still counts", () => {
  // An int64 renders as a STRING over protojson. Reading it as 0 would silently
  // disable under-replica flagging for the tier, which is a wrong answer
  // dressed as a correct one.
  const specs = projectNodeSpecs([
    specRow({ deploymentId: "d1", nodeType: "bff", replicas: "2" }),
  ]);
  assert.equal(specs[0].replicas, 2);
});

test("specs missing a deploymentId or nodeType are dropped", () => {
  const specs = projectNodeSpecs([
    specRow({ nodeType: "bff", replicas: 1 }),
    specRow({ deploymentId: "d1", replicas: 1 }),
  ]);
  assert.deepEqual(specs, []);
});

test("tierKey cannot collide across a canonical id containing colons", () => {
  assert.notEqual(tierKey("v1:cluster:d", "bff"), tierKey("v1:cluster", "d:bff"));
});

// -----------------------------------------------------------------------------
// resolveTierVersion
// -----------------------------------------------------------------------------

test("a per-tier pin overrides the deployment's engine version", () => {
  const resolved = resolveTierVersion(
    { deploymentId: "d1", nodeType: "bff", version: "9.9.9", replicas: 1, imageDigest: "", updatedAt: "" },
    projectDeployments([deploymentRow({ deploymentId: "d1", version: "1.0.0" })])[0],
  );
  assert.deepEqual(resolved, { version: "9.9.9", inherited: false });
});

test("an empty per-tier version inherits the deployment's -- engine as spine", () => {
  const resolved = resolveTierVersion(
    { deploymentId: "d1", nodeType: "bff", version: "", replicas: 1, imageDigest: "", updatedAt: "" },
    projectDeployments([deploymentRow({ deploymentId: "d1", version: "1.0.0" })])[0],
  );
  assert.deepEqual(resolved, { version: "1.0.0", inherited: true });
});

test("no spec and no deployment resolves to nothing rather than a guess", () => {
  assert.deepEqual(resolveTierVersion(undefined, undefined), { version: "", inherited: true });
});

// -----------------------------------------------------------------------------
// composeDeployment
// -----------------------------------------------------------------------------

const deployments = projectDeployments([
  deploymentRow({ deploymentId: "d1", version: "1.0.0", status: "succeeded" }),
]);

test("composition lists the declared tiers with their resolved versions", () => {
  const specs = projectNodeSpecs([
    specRow({ deploymentId: "d1", nodeType: "bff", replicas: 2, imageDigest: "sha256:abcdef0123456789" }),
    specRow({ deploymentId: "d1", nodeType: "agent", replicas: 1, version: "1.0.1" }),
  ]);
  const tiers = composeDeployment(deployments[0], specs, new Map(), true);
  assert.deepEqual(tiers.map((t) => t.nodeType), ["agent", "bff"]);
  assert.equal(tiers[0].version, "1.0.1");
  assert.equal(tiers[0].versionInherited, false);
  assert.equal(tiers[1].version, "1.0.0");
  assert.equal(tiers[1].versionInherited, true);
});

test("only another deployment's specs are excluded", () => {
  const specs = projectNodeSpecs([
    specRow({ deploymentId: "d1", nodeType: "bff", replicas: 2 }),
    specRow({ deploymentId: "other", nodeType: "agent", replicas: 5 }),
  ]);
  const tiers = composeDeployment(deployments[0], specs, new Map(), true);
  assert.deepEqual(tiers.map((t) => t.nodeType), ["bff"]);
});

test("a non-current deployment with live nodes has those tiers flagged", () => {
  // The question an operator opens a historical deployment to ask: did this
  // one leave anything behind?
  const specs = projectNodeSpecs([specRow({ deploymentId: "d1", nodeType: "bff", replicas: 2 })]);
  const tiers = composeDeployment(deployments[0], specs, new Map([["bff", 2]]), false);
  assert.equal(tiers[0].orphaned, true);
  assert.equal(tiers[0].liveNodes, 2);
});

test("the CURRENT deployment's live nodes are never flagged orphaned", () => {
  const specs = projectNodeSpecs([specRow({ deploymentId: "d1", nodeType: "bff", replicas: 2 })]);
  const tiers = composeDeployment(deployments[0], specs, new Map([["bff", 2]]), true);
  assert.equal(tiers[0].orphaned, false);
});

test("a non-current deployment with no live nodes left is not flagged", () => {
  const specs = projectNodeSpecs([specRow({ deploymentId: "d1", nodeType: "bff", replicas: 2 })]);
  const tiers = composeDeployment(deployments[0], specs, new Map(), false);
  assert.equal(tiers[0].orphaned, false);
});

test("a tier running with no spec row still appears, declaring zero replicas", () => {
  // Omitting it would hide running nodes from the one view that is supposed to
  // account for them; copying the live count into `replicas` would make an
  // undeclared tier look correctly provisioned.
  const tiers = composeDeployment(deployments[0], [], new Map([["voice", 1]]), true);
  assert.equal(tiers.length, 1);
  assert.equal(tiers[0].nodeType, "voice");
  assert.equal(tiers[0].replicas, 0);
  assert.equal(tiers[0].liveNodes, 1);
});

test("composing an absent deployment yields nothing", () => {
  assert.deepEqual(composeDeployment(undefined, [], new Map([["bff", 1]]), true), []);
});

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

test("shortenDigest strips the algorithm prefix and caps the length", () => {
  assert.equal(shortenDigest("sha256:0123456789abcdefdeadbeef"), "0123456789ab");
  assert.equal(shortenDigest("short"), "short");
  assert.equal(shortenDigest(""), "");
});

test("the indexes key by what their consumers look up", () => {
  const specs = projectNodeSpecs([specRow({ deploymentId: "d1", nodeType: "bff", replicas: 2 })]);
  assert.equal(indexSpecsByTier(specs).get(tierKey("d1", "bff"))?.replicas, 2);
  assert.equal(indexDeployments(deployments).get("d1")?.version, "1.0.0");
});
