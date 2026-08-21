// The cluster-document address: what a `memql-cluster:` uri says, and what a
// reader can recover from it.
//
// The uri is the ONLY state a content provider gets. VS Code hands it a Uri and
// nothing else -- no construct, no cluster handle -- so everything the fetch
// needs has to survive the round trip through `Uri.parse`. That is what these
// cases pin: compose, parse, and the pack-browser coordinates the path yields.
//
// Refs: #4248

import test from "node:test";
import assert from "node:assert/strict";

import {
  CLUSTER_DOCUMENT_SCHEME,
  clusterDocumentUri,
  notConnectedNotice,
  packLocator,
  parseClusterDocumentUri,
} from "../src/constructs/clusterDocument.js";

test("a cluster document uri round-trips its cluster, path and construct key", () => {
  const uri = clusterDocumentUri({
    cluster: "staging",
    originPath: "cognition/queries.memql",
    kind: "query",
    name: "spaceParticipants",
  });
  assert.equal(uri, `${CLUSTER_DOCUMENT_SCHEME}://staging/cognition/queries.memql?kind=query&name=spaceParticipants`);
  assert.deepEqual(
    parseClusterDocumentUri({ authority: "staging", path: "/cognition/queries.memql", query: "kind=query&name=spaceParticipants" }),
    { cluster: "staging", originPath: "cognition/queries.memql", kind: "query", name: "spaceParticipants" },
  );
});

test("a cluster name with a space survives the authority", () => {
  const uri = clusterDocumentUri({ cluster: "my lab", originPath: "a/b.memql", kind: "concept", name: "v1:a:b" });
  assert.ok(uri.startsWith(`${CLUSTER_DOCUMENT_SCHEME}://my%20lab/`));
  assert.equal(parseClusterDocumentUri({ authority: "my%20lab", path: "/a/b.memql", query: "kind=concept&name=v1%3Aa%3Ab" })?.name, "v1:a:b");
});

test("a malformed uri parses to undefined rather than a half-filled ref", () => {
  assert.equal(parseClusterDocumentUri({ authority: "", path: "/a.memql", query: "kind=query&name=x" }), undefined);
  assert.equal(parseClusterDocumentUri({ authority: "c", path: "/", query: "kind=query&name=x" }), undefined);
  assert.equal(parseClusterDocumentUri({ authority: "c", path: "/a.memql", query: "name=x" }), undefined);
});

test("the pack locator splits the domain off the origin path", () => {
  assert.deepEqual(packLocator("cognition/queries.memql"), { domain: "cognition", path: "queries.memql" });
  assert.deepEqual(packLocator("cognition/prompts/reply.tmpl"), { domain: "cognition", path: "prompts/reply.tmpl" });
  assert.deepEqual(packLocator("dsl/cognition/queries.memql"), { domain: "cognition", path: "queries.memql" });
  assert.equal(packLocator("queries.memql"), undefined);
  assert.equal(packLocator(""), undefined);
});

test("the not-connected notice names the cluster and the way back", () => {
  const notice = notConnectedNotice("staging");
  assert.match(notice, /staging/);
  assert.match(notice, /reconnect/i);
});
