import test from "node:test";
import assert from "node:assert/strict";

import { landingFor, matchCluster, workspaceCandidates } from "../src/handoff/resolve.js";
import type { ClusterConfig } from "../src/clusters/model.js";

const cluster = (over: Partial<ClusterConfig>): ClusterConfig => ({ name: "x", endpoint: "", ...over });

test("a cluster matches by domain or by the endpoint the domain composes", () => {
  const byDomain = cluster({ name: "lab", domain: "Lab.Example.com" });
  const byEndpoint = cluster({ name: "edge", endpoint: "api.edge.example.com:443" });
  const other = cluster({ name: "other", domain: "other.test" });
  assert.equal(matchCluster([other, byDomain], "lab.example.com", "").kind, "one");
  assert.equal(matchCluster([other, byEndpoint], "edge.example.com", "").kind, "one");
  assert.equal(matchCluster([other], "lab.example.com", "").kind, "none");
});

test("several matches prefer the selected cluster and name the rest", () => {
  const a = cluster({ name: "a", domain: "d.test" });
  const b = cluster({ name: "b", domain: "d.test" });
  const m = matchCluster([a, b], "d.test", "b");
  assert.equal(m.kind, "one");
  if (m.kind === "one") {
    assert.equal(m.cluster.name, "b");
    assert.deepEqual(m.alsoMatched, ["a"]);
  }
});

test("the landing follows the design table", () => {
  const c = { origin: "core", originPath: "cognition/queries.memql" };
  assert.deepEqual(landingFor({ clusterLocal: false, checkout: "", workspaceFolderCount: 1 }), { kind: "notLoaded" });
  assert.deepEqual(
    landingFor({ construct: { origin: "promoted", originPath: "" }, clusterLocal: false, checkout: "", workspaceFolderCount: 1 }),
    { kind: "detailPage" },
  );
  assert.deepEqual(
    landingFor({ construct: c, existingIn: { folder: "/w", relativePath: "dsl/cognition/queries.memql" }, clusterLocal: true, checkout: "/w", workspaceFolderCount: 1 }),
    { kind: "workspaceFile", folder: "/w", relativePath: "dsl/cognition/queries.memql" },
  );
  assert.deepEqual(landingFor({ construct: c, clusterLocal: true, checkout: "/home/me/.memql/src", workspaceFolderCount: 0 }), {
    kind: "openCheckout",
    checkout: "/home/me/.memql/src",
    mode: "thisWindow",
  });
  assert.deepEqual(landingFor({ construct: c, clusterLocal: true, checkout: "/home/me/.memql/src", workspaceFolderCount: 1 }), {
    kind: "openCheckout",
    checkout: "/home/me/.memql/src",
    mode: "ask",
  });
  assert.deepEqual(landingFor({ construct: c, clusterLocal: false, checkout: "", workspaceFolderCount: 1 }), { kind: "clusterDocument" });
  // A local cluster with no recorded checkout still has the cluster to read from.
  assert.deepEqual(landingFor({ construct: c, clusterLocal: true, checkout: "", workspaceFolderCount: 1 }), { kind: "clusterDocument" });
});

test("workspace candidates try the checkout layout first", () => {
  assert.deepEqual(workspaceCandidates("cognition/queries.memql"), ["dsl/cognition/queries.memql", "cognition/queries.memql"]);
  assert.deepEqual(workspaceCandidates("dsl/cognition/queries.memql"), ["dsl/cognition/queries.memql", "cognition/queries.memql"]);
});
