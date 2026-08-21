// Read-only rules (memql#3762).
//
// The design gives a table; the rule underneath is "a file is read-only exactly
// when editing it cannot change what the cluster runs". These test the rule,
// including the two cases the table does not have a row for -- not connected,
// and a file the cluster has never heard of -- because both are where a wrong
// answer locks somebody out of their own checkout.
//
// A LOCAL CLUSTER LOCKS NOTHING (memql#4244). It is rebuilt from a checkout on
// this machine, so an edit to any file it loaded can reach it -- and which of a
// developer's clones is the one it builds from is a HINT, never a lock. Both
// halves are asserted, because the tempting mistake is to spend the second fact
// on a verdict: that locks a file its owner is entitled to edit.

import test from "node:test";
import assert from "node:assert/strict";

import type { CatalogConstruct } from "../src/state/constructCatalog.js";
import {
  checkoutHint,
  constructsByPath,
  readonlyPatterns,
  readonlyVerdict,
  reasonBadge,
  reasonTooltip,
  showsCheckoutHint,
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

test("core engine DSL is read-only against every REMOTE cluster", () => {
  for (const clusterLocal of [false, undefined]) {
    const v = readonlyVerdict({ path: CORE.originPath, catalog, clusterLocal });
    assert.equal(v.readonly, true, `local=${String(clusterLocal)}`);
    assert.equal(v.reason, "coreSealed");
  }
  // ...and editable on a local one WHETHER OR NOT this workspace is its
  // checkout. The second fact cannot lock a file: a developer's other clone of
  // the same repository is their own file, and the editor says so with a hover
  // rather than by taking the buffer away.
  for (const workspaceIsClusterCheckout of [true, false, undefined]) {
    const v = readonlyVerdict({ path: CORE.originPath, catalog, clusterLocal: true, workspaceIsClusterCheckout });
    assert.deepEqual(v, { readonly: false }, `checkout=${String(workspaceIsClusterCheckout)}`);
  }
});

test("a bundle file is read-only only on a remote cluster", () => {
  assert.equal(readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: false }).reason, "remoteCluster");
  assert.equal(readonlyVerdict({ path: BUNDLE.originPath, catalog }).reason, "remoteCluster");
  for (const workspaceIsClusterCheckout of [true, false, undefined]) {
    const v = readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: true, workspaceIsClusterCheckout });
    assert.equal(v.readonly, false, `checkout=${String(workspaceIsClusterCheckout)}`);
  }
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
  // Read as the two REASONS rather than as locked-versus-not: against a remote
  // cluster both files are read-only, and which reason each gets is the claim.
  assert.equal(readonlyVerdict({ path: CORE.originPath, catalog, clusterLocal: false }).reason, "coreSealed");
  assert.equal(readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: false }).reason, "remoteCluster");
});

test("a file holding both core and bundle constructs reads as core", () => {
  // Should not occur -- one file comes from one tree -- but if it does, core is
  // the reason that cannot be resolved by selecting a different cluster.
  const mixed = constructsByPath([
    construct({ originPath: "dsl/x/queries.memql", origin: "bundle", name: "a" }),
    construct({ originPath: "dsl/x/queries.memql", origin: "core", name: "b" }),
  ]);
  const v = readonlyVerdict({ path: "dsl/x/queries.memql", catalog: mixed, clusterLocal: false });
  assert.equal(v.readonly, true);
  assert.equal(v.reason, "coreSealed");
});

test("a leading ./ and back-slashes land on the same key", () => {
  // A workspace-relative path on Windows arrives with back-slashes while the
  // catalog reports POSIX. Landing on different keys would make every file on
  // Windows read as unknown -- silently editable, the failure that looks like
  // nothing is wrong.
  for (const spelling of ["./dsl/cognition/queries.memql", "dsl\\cognition\\queries.memql"]) {
    assert.equal(readonlyVerdict({ path: spelling, catalog, clusterLocal: false }).readonly, true, spelling);
  }
});

test("the catalog path and the workspace path agree with or without the dsl/ prefix", () => {
  const bare = constructsByPath([construct({ originPath: "cognition/queries.memql" })]);
  assert.equal(readonlyVerdict({ path: "dsl/cognition/queries.memql", catalog: bare }).readonly, true);
  assert.equal(readonlyVerdict({ path: "cognition/queries.memql", catalog: bare }).readonly, true);
  const patterns = readonlyPatterns({ catalog: bare, clusterLocal: false });
  assert.deepEqual(patterns, ["cognition/queries.memql", "dsl/cognition/queries.memql"]);
});

// -----------------------------------------------------------------------------
// the patterns written to workspace settings
// -----------------------------------------------------------------------------

test("the patterns are exactly the read-only files, in both spellings, sorted", () => {
  // BOTH SPELLINGS, because the catalog's key is relative to the DSL tree root
  // (`cognition/queries.memql`) while a repo checkout holds the same file at
  // `dsl/cognition/queries.memql`. A pattern naming a path that does not exist
  // is inert in `files.readonlyInclude`; a missing one leaves a file unmarked.
  const remote = readonlyPatterns({ catalog, clusterLocal: false });
  assert.deepEqual(remote, [
    "cognition/queries.memql",
    "dsl/cognition/queries.memql",
    "dsl/shop/queries.memql",
    "shop/queries.memql",
  ]);

  // A LOCAL cluster marks nothing at all, core included -- and there is no
  // second input that could put a file back in this list.
  assert.deepEqual(readonlyPatterns({ catalog, clusterLocal: true }), []);
});

test("sorted, so a reconnect does not churn the settings file", () => {
  const shuffled = constructsByPath([
    construct({ originPath: "dsl/z/queries.memql", name: "z" }),
    construct({ originPath: "dsl/a/queries.memql", name: "a" }),
    construct({ originPath: "dsl/m/queries.memql", name: "m" }),
  ]);
  assert.deepEqual(readonlyPatterns({ catalog: shuffled, clusterLocal: false }), [
    "a/queries.memql",
    "dsl/a/queries.memql",
    "dsl/m/queries.memql",
    "dsl/z/queries.memql",
    "m/queries.memql",
    "z/queries.memql",
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

test("a badge is short enough for the editor to accept it", () => {
  // NOT A STYLE RULE. VS Code's `FileDecoration.validate` refuses a badge
  // longer than two code points and drops the WHOLE decoration -- badge, colour
  // and hover -- logging to a channel nobody watches. The words this used to
  // carry ("core", "remote") were four and six, which is why the badge never
  // appeared beside a read-only file.
  for (const reason of ["coreSealed", "remoteCluster"] as const) {
    const badge = reasonBadge(reason);
    assert.ok([...badge].length >= 1 && [...badge].length <= 2, `${reason} badge ${JSON.stringify(badge)}`);
  }
});

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
  // ...and OPEN ITS CHECKOUT, which is the half a developer sitting in a second
  // clone of the same repository would otherwise have no way to guess.
  assert.match(remote, /checkout/);
  assert.match(remote, /staging/);
});

test("a local cluster whose checkout is elsewhere gets a hint, not a lock", () => {
  const hint = checkoutHint("local", "/home/me/.memql/src");
  assert.match(hint, /not the checkout/);
  assert.match(hint, /\/home\/me\/\.memql\/src/);
  assert.match(hint, /local/);
});

test("the hint is shown for a known file on a local cluster this is not the checkout of", () => {
  const wrongClone = { path: CORE.originPath, catalog, clusterLocal: true };
  assert.equal(showsCheckoutHint(wrongClone), true);
  assert.equal(showsCheckoutHint({ ...wrongClone, path: BUNDLE.originPath }), true);
  // And the file is EDITABLE while it says so -- the pair is the whole design.
  assert.equal(readonlyVerdict(wrongClone).readonly, false);
});

test("the hint is silent in the three cases where it would be false", () => {
  // In the checkout: the developer is in the right folder, so there is nothing
  // to say. A remote cluster: it is rebuilt from nothing this developer has,
  // and its read-only tooltip already says so. A file the cluster never loaded:
  // a new file reaches a cluster by being promoted, not by its directory.
  assert.equal(
    showsCheckoutHint({ path: CORE.originPath, catalog, clusterLocal: true, workspaceIsClusterCheckout: true }),
    false,
  );
  assert.equal(showsCheckoutHint({ path: CORE.originPath, catalog, clusterLocal: false }), false);
  assert.equal(showsCheckoutHint({ path: "dsl/mine/newThing.memql", catalog, clusterLocal: true }), false);
  // ...and with no cluster at all there is no checkout to be the wrong one.
  assert.equal(showsCheckoutHint({ path: CORE.originPath, catalog: undefined, clusterLocal: true }), false);
});
