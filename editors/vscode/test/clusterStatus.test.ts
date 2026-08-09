// What a Clusters-tree row SAYS, decided away from the `vscode` API.
//
// memql#3385's second acceptance item is a wording-and-iconography problem:
// "an operator who hits it mid-section sees a red cluster icon with no
// indication that the CREDENTIAL is what expired, as distinct from the cluster
// going away." A red error dot is the same picture for "your token ran out"
// and "the cluster is gone", and those two have completely different next
// actions.
//
// The decision therefore lives in a pure module and is asserted here, the way
// deploy/clusterView.ts holds the Cluster tab's wording. views/clustersTree.ts
// is left as a mapping from these names onto ThemeIcons.

import test from "node:test";
import assert from "node:assert/strict";

import { clusterRowStatus } from "../src/clusters/status.js";
import type { ConnectionState } from "../src/connection/manager.js";

const CLUSTER = { name: "local", endpoint: "cockpit.local.znas.io:443", token: "eyJ.a.b" };

function status(state: ConnectionState, cluster = CLUSTER) {
  return clusterRowStatus(cluster, state);
}

test("a connected cluster names the node it is connected to", () => {
  assert.deepEqual(
    status({ status: "connected", clusterName: "local", nodeId: "bff-0" }),
    { icon: "connected", tooltip: "Connected (node bff-0)" },
  );
});

test("a connecting cluster spins", () => {
  assert.equal(status({ status: "connecting", clusterName: "local" }).icon, "connecting");
});

test("an EXPIRED CREDENTIAL is a credential icon and a credential tooltip, not a generic failure", () => {
  // THE memql#3385 acceptance item. Before this, both branches rendered the
  // same red dot and the same "ERROR:" tooltip.
  const view = status({
    status: "error",
    clusterName: "local",
    reason: "credentialExpired",
    message: "The access token for local expired.",
  });

  assert.equal(view.icon, "credential");
  assert.match(view.tooltip, /^CREDENTIAL EXPIRED: /);
});

test("a wrong-class token and a missing credential also read as credential problems", () => {
  for (const reason of ["wrongTokenClass", "missingCredential"] as const) {
    const view = status({ status: "error", clusterName: "local", reason, message: "nope" });
    assert.equal(view.icon, "credential", reason);
    assert.match(view.tooltip, /^CREDENTIAL: /, reason);
  }
});

test("an UNREACHABLE cluster stays the red failure it always was", () => {
  // The contrast is the whole point: if everything became a credential icon the
  // distinction would be lost in the other direction.
  const view = status({
    status: "error",
    clusterName: "local",
    reason: "unreachable",
    message: "connect ECONNREFUSED",
  });

  assert.equal(view.icon, "failed");
  assert.match(view.tooltip, /^ERROR: /);
});

test("a dropped connection is a failure, not a credential problem", () => {
  const view = status({
    status: "error",
    clusterName: "local",
    reason: "lost",
    message: "Connection to local was lost.",
  });
  assert.equal(view.icon, "failed");
});

test("a row for a DIFFERENT cluster than the connected one shows its own resting state", () => {
  const other = { name: "staging", endpoint: "cockpit.staging.example.com:443", token: "eyJ.a.b" };
  const view = clusterRowStatus(other, {
    status: "connected",
    clusterName: "local",
    nodeId: "bff-0",
  });
  assert.equal(view.icon, "idle");
  assert.equal(view.tooltip, "cockpit.staging.example.com:443");
});

test("a cluster with no endpoint is unconfigured, and says so", () => {
  const view = clusterRowStatus({ name: "x", endpoint: "" }, { status: "disconnected" });
  assert.equal(view.icon, "unconfigured");
  assert.match(view.tooltip, /not configured/i);
});

test("a cluster carrying a PAT is flagged BEFORE anyone tries to connect with it", () => {
  // memql#3383: an operator who follows the old field comment mints a PAT. The
  // tree can say so at rest, without a dial and without a round trip.
  const view = clusterRowStatus(
    { name: "x", endpoint: "cockpit.local.znas.io:443", token: "mql_pat_abc" },
    { status: "disconnected" },
  );
  assert.equal(view.icon, "credential");
  assert.match(view.tooltip, /Personal Access Token/);
});

test("a cluster with an endpoint and no token asks for a JWT, not for a PAT", () => {
  const view = clusterRowStatus(
    { name: "x", endpoint: "cockpit.local.znas.io:443" },
    { status: "disconnected" },
  );
  assert.equal(view.icon, "credential");
  assert.match(view.tooltip, /JWT access token/);
  assert.doesNotMatch(view.tooltip, /memQL Cockpit first/);
});
