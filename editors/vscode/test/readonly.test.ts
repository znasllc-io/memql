// Read-only rules (memql#3762).
//
// The design gives a table; the rule underneath is "a file is read-only exactly
// when editing it cannot change what the cluster runs". These test the rule,
// including the two cases the table does not have a row for -- not connected,
// and a file the cluster has never heard of -- because both are where a wrong
// answer locks somebody out of their own checkout.

import test from "node:test";
import assert from "node:assert/strict";

import type { CatalogConstruct } from "../src/state/constructCatalog.js";
import {
  constructsByPath,
  readonlyPatterns,
  readonlyVerdict,
  reasonBadge,
  reasonTooltip,
} from "../src/constructs/readonly.js";

function construct(over: Partial<CatalogConstruct> = {}): CatalogConstruct {
  return {
    name: "spaceParticipants",
    kind: "query",
    namespace: "cognition",
    origin: "core",
    originPath: "dsl/cognition/queries.memql",
    description: "",
    runnable: true,
    args: [],
    boundConcept: "participant",
    sourceHash: "abc",
    source: "",
    ...over,
  };
}

const CORE = construct();
const BUNDLE = construct({
  name: "orders",
  origin: "bundle",
  originPath: "dsl/shop/queries.memql",
});
const PROMOTED = construct({
  name: "trained",
  origin: "promoted",
  originPath: "",
  namespace: "",
});

const catalog = constructsByPath([CORE, BUNDLE, PROMOTED]);

// -----------------------------------------------------------------------------
// the table
// -----------------------------------------------------------------------------

test("core engine DSL is read-only against any cluster", () => {
  for (const clusterLocal of [true, false, undefined]) {
    const v = readonlyVerdict({ path: CORE.originPath, catalog, clusterLocal });
    assert.equal(v.readonly, true, `local=${String(clusterLocal)}`);
    assert.equal(v.reason, "coreSealed");
  }
});

test("a product bundle is editable against a local cluster", () => {
  const v = readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: true });
  assert.equal(v.readonly, false);
  assert.equal(v.reason, undefined);
});

test("a product bundle is read-only against a remote cluster", () => {
  const v = readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: false });
  assert.equal(v.readonly, true);
  assert.equal(v.reason, "remoteCluster");
});

test("an absent local flag means NOT local, matching ClusterConfig", () => {
  // Every cluster already in an operator's clusters.yaml predates the field, so
  // an absent flag reading as "local" would make a staging bundle editable --
  // the exact inversion the flag's own default exists to prevent.
  const v = readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: undefined });
  assert.equal(v.readonly, true);
  assert.equal(v.reason, "remoteCluster");
});

test("switching cluster flips a bundle file, and back", () => {
  // The acceptance criterion, stated as the round trip rather than one
  // direction: a verdict that latches would pass a one-way test.
  const path = BUNDLE.originPath;
  assert.equal(readonlyVerdict({ path, catalog, clusterLocal: true }).readonly, false);
  assert.equal(readonlyVerdict({ path, catalog, clusterLocal: false }).readonly, true);
  assert.equal(readonlyVerdict({ path, catalog, clusterLocal: true }).readonly, false);
});

// -----------------------------------------------------------------------------
// the two rows the table does not have
// -----------------------------------------------------------------------------

test("NOT CONNECTED leaves every file editable, core included", () => {
  // The most important assertion here. An absent cluster is not an answer --
  // and unlike #3759's `unknown`, getting this one wrong does not produce a
  // false alarm, it locks a developer out of their own checkout because their
  // laptop went to sleep.
  for (const path of [CORE.originPath, BUNDLE.originPath, "anything/else.memql"]) {
    const v = readonlyVerdict({ path, catalog: undefined, clusterLocal: false });
    assert.equal(v.readonly, false, path);
  }
});

test("a NEW file is never read-only, under any combination", () => {
  // The training path. Blocking it breaks the epic, so this asserts the whole
  // cross product rather than one representative case.
  for (const clusterLocal of [true, false, undefined]) {
    for (const cat of [catalog, undefined]) {
      const v = readonlyVerdict({ path: "dsl/mine/newThing.memql", catalog: cat, clusterLocal });
      assert.equal(v.readonly, false, `local=${String(clusterLocal)} catalog=${cat !== undefined}`);
    }
  }
});

test("a promoted construct contributes no path at all", () => {
  // Its originPath is "". Indexing that would file every database-resident
  // construct under one empty key and make "" look like a real file.
  assert.equal(catalog.has(""), false);
  assert.equal(readonlyVerdict({ path: "", catalog, clusterLocal: false }).readonly, false);
});

// -----------------------------------------------------------------------------
// classification
// -----------------------------------------------------------------------------

test("the verdict comes from the catalog's origin, not from the path", () => {
  // Both files live under `dsl/`, which is the convention for a product bundle
  // as well as for the engine's own tree -- so a path-shaped rule gets this
  // pair wrong on the first product that follows the convention.
  assert.equal(readonlyVerdict({ path: CORE.originPath, catalog, clusterLocal: true }).reason, "coreSealed");
  assert.equal(readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: true }).readonly, false);
});

test("a file holding both core and bundle constructs reads as core", () => {
  // Should not occur -- one file comes from one tree -- but if it does, core is
  // the reason that cannot be resolved by selecting a different cluster.
  const mixed = constructsByPath([
    construct({ originPath: "dsl/x/queries.memql", origin: "bundle", name: "a" }),
    construct({ originPath: "dsl/x/queries.memql", origin: "core", name: "b" }),
  ]);
  const v = readonlyVerdict({ path: "dsl/x/queries.memql", catalog: mixed, clusterLocal: true });
  assert.equal(v.readonly, true);
  assert.equal(v.reason, "coreSealed");
});

test("a leading ./ and back-slashes land on the same key", () => {
  // A workspace-relative path on Windows arrives with back-slashes while the
  // catalog reports POSIX. Landing on different keys would make every file on
  // Windows read as unknown -- silently editable, the failure that looks like
  // nothing is wrong.
  for (const spelling of ["./dsl/cognition/queries.memql", "dsl\\cognition\\queries.memql"]) {
    assert.equal(readonlyVerdict({ path: spelling, catalog, clusterLocal: true }).readonly, true, spelling);
  }
});

// -----------------------------------------------------------------------------
// the patterns written to workspace settings
// -----------------------------------------------------------------------------

test("the patterns are exactly the read-only files, sorted", () => {
  const remote = readonlyPatterns({ catalog, clusterLocal: false });
  assert.deepEqual(remote, ["dsl/cognition/queries.memql", "dsl/shop/queries.memql"]);

  const local = readonlyPatterns({ catalog, clusterLocal: true });
  assert.deepEqual(local, ["dsl/cognition/queries.memql"]);
});

test("sorted, so a reconnect does not churn the settings file", () => {
  const shuffled = constructsByPath([
    construct({ originPath: "dsl/z/queries.memql", name: "z" }),
    construct({ originPath: "dsl/a/queries.memql", name: "a" }),
    construct({ originPath: "dsl/m/queries.memql", name: "m" }),
  ]);
  assert.deepEqual(readonlyPatterns({ catalog: shuffled, clusterLocal: true }), [
    "dsl/a/queries.memql",
    "dsl/m/queries.memql",
    "dsl/z/queries.memql",
  ]);
});

test("not connected CLEARS the patterns rather than leaving the last answer", () => {
  // Workspace settings persist across restarts, so a stale marking that
  // outlives the connection justifying it looks permanent.
  assert.deepEqual(readonlyPatterns({ catalog: undefined, clusterLocal: false }), []);
});

// -----------------------------------------------------------------------------
// what the operator is told
// -----------------------------------------------------------------------------

test("the two reasons read differently, and each names its way out", () => {
  assert.notEqual(reasonBadge("coreSealed"), reasonBadge("remoteCluster"));

  const core = reasonTooltip("coreSealed", "staging");
  const remote = reasonTooltip("remoteCluster", "staging");
  assert.notEqual(core, remote);

  // The core sentence points at the training path; the remote one points at the
  // selection that resolves it. A badge that only said "read-only" would leave
  // an operator with no next step in either case.
  assert.match(core, /new file/);
  assert.match(remote, /Select the local cluster/);
  assert.match(remote, /staging/);
});
