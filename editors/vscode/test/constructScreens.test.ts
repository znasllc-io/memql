// The construct detail page's actions.
//
// THE PAGE'S JOB HERE IS TO HAVE NO DEAD END. Three situations, three different
// affordances: the file is in this workspace (open it), the file exists but is
// not on this machine (read it from the cluster), or there is no file at all --
// a promoted construct, whose source is already rendered below, so offering to
// fetch it from the cluster would ask for something that is not there.
//
// Refs: #4248 #3752

import test from "node:test";
import assert from "node:assert/strict";

import { renderConstructPage } from "../src/webview/constructScreens.js";
import type { CatalogConstruct } from "../src/state/constructCatalog.js";

function construct(over: Partial<CatalogConstruct> = {}): CatalogConstruct {
  return {
    name: "spaceParticipants",
    kind: "query",
    namespace: "cognition",
    origin: "core",
    originPath: "cognition/queries.memql",
    description: "",
    runnable: false,
    args: [],
    boundConcept: "",
    sourceHash: "",
    source: "",
    ...over,
  };
}

const CLUSTER_SOURCE = 'data-act="viewSourceFromCluster"';

test("a file that is not in this workspace offers to read it from the cluster", () => {
  const html = renderConstructPage({
    construct: construct(),
    fileInWorkspace: false,
    offerClusterSource: true,
    error: "",
  });
  assert.ok(html.includes(CLUSTER_SOURCE), "no cluster-source button on a file that is not here");
  assert.ok(html.includes("View source from cluster"));
});

test("a file that IS in this workspace opens from disk instead", () => {
  const html = renderConstructPage({
    construct: construct(),
    fileInWorkspace: true,
    offerClusterSource: false,
    error: "",
  });
  assert.equal(html.includes(CLUSTER_SOURCE), false, "the cluster round trip is offered for a file that is right here");
  assert.ok(html.includes('data-act="openFile"'));
});

test("a promoted construct is offered no cluster source -- there is no file to serve", () => {
  const html = renderConstructPage({
    construct: construct({ origin: "promoted", originPath: "", source: "logic x { }" }),
    fileInWorkspace: false,
    offerClusterSource: true,
    error: "",
  });
  assert.equal(html.includes(CLUSTER_SOURCE), false, "a construct with no file was offered a fetch of it");
});

// -----------------------------------------------------------------------------
// the retired browse-rows button (memql#4252, removed by epic memql#4984)
// -----------------------------------------------------------------------------

const BROWSE_ROWS = 'data-act="browseRows"';

// A concept used to offer "Browse rows in portal", which opened the portal's
// `/concepts/<id>` page. The portal is retired and MemQL OS has no concept
// browser, so there is no page to open and the button was REMOVED rather than
// pointed somewhere that answers 404.
//
// The test is kept, inverted, rather than deleted: `renderConstructPage` still
// takes a `kind` and still branches on it, so "a concept draws no browse-rows
// button" is a claim that can become false again -- and the natural way for it
// to become false is somebody restoring the button when a concept browser
// lands, without noticing there is nothing at the far end yet.
test("no kind offers to browse rows -- the button went with the portal", () => {
  for (const kind of ["concept", "query", "mutation", "automation", "tool", "spec", "shape", "prompt", "provider"]) {
    const html = renderConstructPage({
      construct: construct({ kind }),
      fileInWorkspace: false,
      offerClusterSource: false,
      error: "",
    });
    assert.equal(html.includes(BROWSE_ROWS), false, `${kind} drew a browse-rows button`);
  }
});
