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

import { clusterRowStatus, clusterRowText } from "../src/clusters/status.js";
import type { ConnectionState } from "../src/connection/manager.js";

const CLUSTER = { name: "local", endpoint: "api.memql.localhost:443", token: "eyJ.a.b" };

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
  const other = { name: "staging", endpoint: "api.example.com:443", token: "eyJ.a.b" };
  const view = clusterRowStatus(other, {
    status: "connected",
    clusterName: "local",
    nodeId: "bff-0",
  });
  assert.equal(view.icon, "idle");
  assert.equal(view.tooltip, "api.example.com:443");
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
    { name: "x", endpoint: "api.memql.localhost:443", token: "mql_pat_abc" },
    { status: "disconnected" },
  );
  assert.equal(view.icon, "credential");
  assert.match(view.tooltip, /Personal Access Token/);
});

test("a cluster with an endpoint and no token asks for a JWT, not for a PAT", () => {
  const view = clusterRowStatus(
    { name: "x", endpoint: "api.memql.localhost:443" },
    { status: "disconnected" },
  );
  assert.equal(view.icon, "credential");
  assert.match(view.tooltip, /JWT access token/);
  assert.doesNotMatch(view.tooltip, /MemQL Cockpit first/);
});

// -----------------------------------------------------------------------------
// The recorded version on the row (memql#3995)
// -----------------------------------------------------------------------------
//
// THIS IS THE SURFACE THAT MAKES A DISCONNECTED OR NEVER-DIALLED CLUSTER'S
// VERSION VISIBLE AT ALL. Every other place a version could appear needs a live
// session; the row is read off clusters.yaml, so it answers with the cluster
// switched off -- which is the situation the motivating incident happened in.
//
// The row COMPOSES two decisions that are taken elsewhere: the connection
// verdict (clusterRowStatus, above) and the version verdict (describeVersion).
// Composing them here rather than in the tree is what keeps the tree a mapping
// onto ThemeIcons.

const LISTING = { tags: ["v0.18.0", "v0.17.0"], fetchedAt: 1000 };

test("an idle row with nothing recorded says nothing -- and never the endpoint", () => {
  // The ordinary case on a fresh install. Appending "unknown" to every row
  // would be noise; the connection page says the word where there is room.
  // And the ENDPOINT is not the subtitle any more (memql#4194, audit 10):
  // internal addresses do not sit on permanent display in the sidebar.
  const text = clusterRowText(CLUSTER, { status: "disconnected" }, LISTING);
  assert.equal(text.description, "");
  assert.match(text.tooltip, /api\.memql\.localhost:443/, "the address lives in the hover");
});

test("a row shows the recorded version, and the endpoint only in the tooltip", () => {
  const text = clusterRowText(
    { ...CLUSTER, version: "v0.18.0" },
    { status: "disconnected" },
    LISTING,
  );
  assert.doesNotMatch(text.description, /api\.memql\.localhost/);
  assert.match(text.description, /v0\.18\.0/);
  assert.match(text.tooltip, /Endpoint: api\.memql\.localhost:443/);
});

test("a row's subtitle leads with the verdict when there is one", () => {
  // memql#4195: the list answers "which of my clusters needs me".
  const missingCredential = clusterRowText(
    { name: "x", endpoint: "api.example.com:443", version: "v0.18.0" },
    { status: "disconnected" },
    LISTING,
  );
  assert.match(missingCredential.description, /^needs sign-in/);
  assert.match(missingCredential.description, /v0\.18\.0/);

  const connected = clusterRowText(
    { ...CLUSTER, version: "v0.18.0" },
    { status: "connected", clusterName: "local", nodeId: "bff-0" },
    LISTING,
  );
  assert.match(connected.description, /^connected/);
});

test("a failed dial's tooltip is brief and names the Connection channel", () => {
  // Raw transport errors belong to the MemQL Connection output channel
  // (memql#4194, audit 12); a hover can be neither scrolled nor copied.
  const longMessage = `dial tcp 10.0.0.4:50051: ${"x".repeat(300)}\nsecond line of stack`;
  const text = clusterRowText(
    CLUSTER,
    { status: "error", clusterName: "local", reason: "unreachable", message: longMessage },
    LISTING,
  );
  assert.doesNotMatch(text.tooltip, /second line of stack/);
  assert.ok(text.tooltip.split("\n")[0]!.length <= 141, "first line is capped");
  assert.match(text.tooltip, /MemQL Connection output channel/);
});

test("a recorded release behind the extension pin gets the skew sentence", () => {
  const behind = clusterRowText(
    { ...CLUSTER, version: "v0.9.0" },
    { status: "disconnected" },
    LISTING,
  );
  assert.match(behind.tooltip, /older than the v\d+\.\d+\.\d+ this extension ships for/);
  const current = clusterRowText(
    { ...CLUSTER, version: "v99.0.0" },
    { status: "disconnected" },
    LISTING,
  );
  assert.doesNotMatch(current.tooltip, /older than the/);
});

test("a row behind the newest release carries the availability clause", () => {
  const text = clusterRowText(
    { ...CLUSTER, version: "v0.17.0" },
    { status: "disconnected" },
    LISTING,
  );
  assert.match(text.description, /v0\.17\.0/);
  assert.match(text.description, /v0\.18\.0/);
});

test("a DISCONNECTED cluster still shows its version -- the whole point", () => {
  const text = clusterRowText(
    { ...CLUSTER, version: "v0.17.0" },
    { status: "disconnected" },
    LISTING,
  );
  assert.match(text.description, /v0\.17\.0/, "no session is needed to read this");
});

test("the tooltip carries the version sentence beneath the connection verdict", () => {
  const text = clusterRowText(
    { ...CLUSTER, version: "v0.17.0" },
    { status: "connected", clusterName: "local", nodeId: "bff-0" },
    LISTING,
  );
  assert.match(text.tooltip, /Connected \(node bff-0\)/, "the connection verdict stays first");
  assert.match(text.tooltip, /v0\.18\.0/, "and the version sentence follows");
});

test("the tooltip adds no version paragraph when nothing is known", () => {
  // A row that grew a paragraph saying "we know nothing" on every cluster
  // would make the tooltip worth less, not more.
  const text = clusterRowText(CLUSTER, { status: "disconnected" }, undefined);
  assert.equal(text.tooltip, "Endpoint: api.memql.localhost:443");
});

test("an unfetched listing does not make a known version disappear", () => {
  const text = clusterRowText(
    { ...CLUSTER, version: "v0.17.0" },
    { status: "disconnected" },
    undefined,
  );
  assert.match(text.description, /v0\.17\.0/);
  assert.doesNotMatch(text.description, /available/i, "and claims nothing about what exists");
});

test("the error row's description is left entirely alone", () => {
  // The synthetic row clusters.yaml-failed produces has no cluster to version.
  const text = clusterRowText({ name: "", endpoint: "" }, { status: "disconnected" }, LISTING);
  assert.equal(text.description, "");
});
