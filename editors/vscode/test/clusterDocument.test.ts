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
  detailsRefusal,
  panelClusterRefusal,
  fetchFailedNotice,
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

test("details are refused when the connection is not the document's cluster, and the toast names both", () => {
  // The lens outlives the connection, and a cluster document's body is rewritten
  // to the not-connected notice while the lens still says where it came from. So
  // "open its details" has to answer against the cluster the DOCUMENT names, not
  // whatever is connected now -- otherwise a click renders another cluster's
  // construct of the same name with nothing saying so.
  assert.equal(detailsRefusal("staging", "staging"), undefined);

  const crossed = detailsRefusal("staging", "prod");
  assert.match(String(crossed), /staging/);
  assert.match(String(crossed), /prod/);
  assert.match(String(crossed), /reconnect/i);

  const none = detailsRefusal("staging", undefined);
  assert.match(String(none), /staging/);
  assert.equal(/prod/.test(String(none)), false, "there is no other cluster to name when nothing is connected");
});

test("a document with no cluster claim is not refused", () => {
  // Nothing in this tree posts one, but the handler's argument is untrusted:
  // an empty claim must not become a refusal naming an empty cluster.
  assert.equal(detailsRefusal("", "prod"), undefined);
});

test("the fetch-failed notice points at the channel and carries no raw error", () => {
  const notice = fetchFailedNotice("staging", "cognition/queries.memql");
  assert.match(notice, /staging/);
  assert.match(notice, /cognition\/queries\.memql/);
  assert.match(notice, /MemQL Connection/);
  // Every line is a comment: the notice is rendered INTO a .memql buffer.
  for (const line of notice.split("\n").filter((l) => l !== "")) {
    assert.match(line, /^\/\//, `"${line}" is not a comment line`);
  }
});

// memql#4253. The construct PANEL is a singleton that outlives any one
// connection, so the two buttons that reach into a cluster have the same defect
// the lens had: a record opened under `staging` would be served by whatever is
// connected when the button is pressed. This is that decision, composed from
// detailsRefusal so there is exactly ONE cluster comparison in the tree.
test("a panel action is refused when the connection is not the panel's own cluster", () => {
  // The cluster the record came from IS the connected one: act.
  assert.equal(panelClusterRefusal("staging", "staging", "read its source"), undefined);

  // A different cluster: the mismatch wins, and names both.
  const crossed = String(panelClusterRefusal("staging", "prod", "read its source"));
  assert.match(crossed, /staging/);
  assert.match(crossed, /prod/);

  // Nothing connected, and the panel knows its cluster: the more specific
  // "reconnect to staging" wins over the generic offer.
  const gone = String(panelClusterRefusal("staging", undefined, "read its source"));
  assert.match(gone, /staging/);
  assert.equal(/connect to a cluster to/.test(gone), false);

  // Nothing connected and NO claim to check: fall back to naming what is
  // needed, in the caller's own words.
  assert.equal(
    panelClusterRefusal("", undefined, "browse its rows in the portal"),
    "MemQL: connect to a cluster to browse its rows in the portal.",
  );

  // A panel with no claim, over a live connection, is never refused -- the
  // same "" rule detailsRefusal applies.
  assert.equal(panelClusterRefusal("", "prod", "read its source"), undefined);
});
