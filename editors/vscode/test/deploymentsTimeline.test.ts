// Deployments as the SELECTED cluster's flat timeline (memql#4426).
//
// Two shapings and one heading, all pure, all here rather than inline in the
// provider -- for the reason state/deploymentsCatalog.ts's header gives about
// everything else it holds: the Deployments view's failure mode is a BLANK
// PANEL or a panel showing the WRONG cluster, and no unit test of a
// TreeDataProvider adapter would see either.
//
// THE CASE THIS FILE IS REALLY FOR is two registered clusters. The old view
// rendered both, always, whatever was selected -- so an operator with a local
// cluster and a staging cluster saw staging's rollouts under a `staging` row
// while they were connected to local, and nothing on the view said which one
// they were looking at. Selection filtering is the fix, and "it follows the
// selection" is the assertion.
//
// Refs: #4426 #4423

import test from "node:test";
import assert from "node:assert/strict";

import type { ClustersFile } from "../src/clusters/model.js";
import type { PresenceResult } from "../src/clusters/presence.js";
import type { ReleaseListing } from "../src/version/releaseCache.js";
import { newLocalRun, type Instance, type Run } from "../src/state/deployments.js";
import {
  buildCatalog,
  runDuration,
  runsForSelected,
  selectedInstanceContext,
  selectedViewDescription,
  type CatalogInputs,
} from "../src/state/deploymentsCatalog.js";

function presenceOf(verdict: PresenceResult["verdict"]): () => Promise<PresenceResult> {
  return async () => ({ verdict, evidence: { receipt: true, registry: false }, endpoint: "" });
}

function clusters(file: Partial<ClustersFile> = {}): CatalogInputs["readClusters"] {
  return async () => ({ ok: true as const, file: { clusters: [], selectedCluster: "", ...file } });
}

function run(over: Partial<Run> & { id: string; startedAt: string }): Run {
  return {
    ...newLocalRun({
      id: over.id,
      instance: over.instance ?? "local",
      kind: over.kind ?? "upgrade",
      startedAt: over.startedAt,
    }),
    ...over,
  };
}

function inputs(over: Partial<CatalogInputs> = {}): CatalogInputs {
  return {
    clustersPath: "/nowhere/clusters.yaml",
    receiptPath: "/nowhere/install-receipt.json",
    runsDir: "/nowhere/runs",
    presence: presenceOf("installed-healthy"),
    readClusters: clusters(),
    readReceiptFile: async () => null,
    listRunsIn: async () => [],
    ...over,
  };
}

const LISTING: ReleaseListing = {
  tags: ["v0.20.0", "v0.19.0"],
  fetchedAt: Date.parse("2026-08-14T12:00:00Z"),
  error: "",
};

// ---------------------------------------------------------------------------
// runsForSelected
// ---------------------------------------------------------------------------

test("nothing selected yields no instance and no runs", async () => {
  // The empty case IS the mechanism: the provider turns this into `[]`, and the
  // manifest's welcome renders in the space. A row of any kind here would
  // delete the welcome silently.
  const catalog = await buildCatalog(
    inputs({ listRunsIn: async () => [run({ id: "a", startedAt: "2026-08-01T00:00:00Z" })] })
  );
  assert.deepEqual(runsForSelected(catalog, undefined), { instance: undefined, runs: [] });
});

test("a selection that names no instance yields nothing rather than an error", async () => {
  // An ordinary race, not a fault: clusters.yaml is shared with the MemQL
  // Cockpit, so a cluster can be removed there between this editor selecting it
  // and this catalog being read. A synthetic error row would suppress the
  // welcome that correctly describes what is left.
  const catalog = await buildCatalog(inputs());
  const selected = runsForSelected(catalog, { clusterName: "ghost", connected: false });
  assert.equal(selected.instance, undefined);
  assert.deepEqual(selected.runs, []);
});

test("the timeline is the SELECTED cluster's, not everything registered", async () => {
  // The defect the epic was filed for, in one assertion. Both clusters are
  // registered and both have history; the view shows one.
  const catalog = await buildCatalog(
    inputs({
      readClusters: clusters({
        clusters: [
          { name: "local", endpoint: "", domain: "memql.test", local: true },
          { name: "staging", endpoint: "wss://staging.example", domain: "staging.example" },
        ] as ClustersFile["clusters"],
      }),
      listRunsIn: async () => [
        run({ id: "local-1", instance: "local", kind: "install", startedAt: "2026-08-01T00:00:00Z" }),
      ],
    })
  );
  assert.equal(catalog.instances.length, 2, "both clusters should still exist in the catalog");

  const local = runsForSelected(catalog, { clusterName: "local", connected: true });
  assert.equal(local.instance?.name, "local");
  assert.deepEqual(local.runs.map((r) => r.id), ["local-1"]);

  // Selecting the other one follows it, and shows none of local's history.
  const staging = runsForSelected(catalog, { clusterName: "staging", connected: true });
  assert.equal(staging.instance?.name, "staging");
  assert.deepEqual(staging.runs, [], "local's runs leaked into staging's timeline");
});

test("the timeline is newest first, and the order is re-derived", async () => {
  // Fed deliberately out of order. `listRuns` sorts its own output, so a caller
  // could reasonably assume this is redundant -- and that assumption is exactly
  // what would let a history render backwards the day something upstream
  // filters or re-orders the list.
  const catalog = await buildCatalog(
    inputs({
      listRunsIn: async () => [
        run({ id: "b", startedAt: "2026-08-02T00:00:00Z" }),
        run({ id: "d", startedAt: "2026-08-04T00:00:00Z" }),
        run({ id: "a", startedAt: "2026-08-01T00:00:00Z" }),
        run({ id: "c", startedAt: "2026-08-03T00:00:00Z" }),
      ],
      readClusters: clusters({
        clusters: [{ name: "local", endpoint: "", domain: "memql.test", local: true }] as ClustersFile["clusters"],
      }),
    })
  );
  const selected = runsForSelected(catalog, { clusterName: "local", connected: true });
  assert.deepEqual(selected.runs.map((r) => r.id), ["d", "c", "b", "a"]);
});

test("a selected cluster with no runs is a heading and no rows, not an empty state", async () => {
  // Distinct from "nothing selected", and the description is what tells them
  // apart on screen. "Installed, never upgraded" is the normal case.
  const catalog = await buildCatalog(
    inputs({
      readClusters: clusters({
        clusters: [{ name: "local", endpoint: "", domain: "memql.test", local: true }] as ClustersFile["clusters"],
      }),
    })
  );
  const selected = runsForSelected(catalog, { clusterName: "local", connected: true });
  assert.notEqual(selected.instance, undefined, "the instance vanished with its runs");
  assert.deepEqual(selected.runs, []);
  assert.notEqual(selectedViewDescription(selected.instance, undefined), "");
});

// ---------------------------------------------------------------------------
// selectedViewDescription -- the instance facts, promoted out of the row
// ---------------------------------------------------------------------------

function instanceOf(over: Partial<Instance> = {}): Instance {
  return {
    name: "local",
    kind: "local",
    presence: "installed-healthy",
    connected: true,
    version: "v0.19.1",
    ...over,
  };
}

test("the description names the cluster, its health and its version", () => {
  assert.equal(selectedViewDescription(instanceOf(), undefined), "local · healthy · v0.19.1");
});

test("an update is its own segment, and says so", () => {
  // NOT the row's wording. A tree row appends the clause to the version and
  // reads `v0.19.1 - v0.20.0 available`, which is one field; here the version
  // is its own segment, so a bare `v0.20.0 available` beside `v0.19.1` would
  // read as two versions with no statement about either.
  assert.equal(
    selectedViewDescription(instanceOf(), LISTING),
    "local · healthy · v0.19.1 · update v0.20.0 available"
  );
});

test("a cluster already on the newest release says nothing extra", () => {
  assert.equal(
    selectedViewDescription(instanceOf({ version: "v0.20.0" }), LISTING),
    "local · healthy · v0.20.0"
  );
});

test("an unreachable selected cluster keeps its heading and says it is not answering", () => {
  // Design D2's distinction, at the description level: selected-but-unreachable
  // is not the empty state, and this line is where the Deployments view carries
  // it.
  assert.equal(
    selectedViewDescription(instanceOf({ presence: "installed-unreachable" }), undefined),
    "local · not answering · v0.19.1"
  );
});

test("a machine with nothing installed makes no version claim", () => {
  assert.equal(
    selectedViewDescription(instanceOf({ presence: "absent", version: undefined }), LISTING),
    "local · not installed"
  );
});

test("an unresolvable version prints the word, never a blank", () => {
  // `displayVersion`'s rule, carried into the heading: a blank reads as a fact
  // about the cluster ("it has no version") when it is a fact about the read.
  assert.equal(
    selectedViewDescription(instanceOf({ version: undefined }), undefined),
    "local · healthy · unknown"
  );
});

test("a checkout-mode cluster names the checkout, and is offered no update", () => {
  // memql#4246's rule at the heading level. The recorded release is not what
  // the cluster is running, so "an update to it is available" would be a claim
  // about a version it is not on.
  const description = selectedViewDescription(
    instanceOf({
      imageSource: "checkout",
      rebuild: {
        commit: "abc1234def",
        ref: "main",
        dirtyCount: 4,
        nodes: "",
        recordedAt: "2026-08-14T10:00:00Z",
      },
    }),
    LISTING
  );
  assert.equal(description, "local · healthy · checkout abc1234 (4 uncommitted)");
  assert.ok(!description.includes("available"), "a checkout build was offered a release update");
});

test("no selection means no heading", () => {
  assert.equal(selectedViewDescription(undefined, LISTING), "");
});

test("a remote cluster's heading names it, not the local one", () => {
  assert.equal(
    selectedViewDescription(instanceOf({ name: "staging", kind: "remote", version: "v0.9.2" }), undefined),
    "staging · healthy · v0.9.2"
  );
});

// ---------------------------------------------------------------------------
// the title menu's scope
// ---------------------------------------------------------------------------

test("the instance context key carries the row vocabulary, and empties with the selection", () => {
  // The three values are the ones the row's contextValue carried; the fourth
  // case -- nothing selected -- is "" rather than a guess, because inventing a
  // value is how "no cluster" would come to mean "local" and put Uninstall in
  // front of an operator with nothing to uninstall.
  assert.equal(selectedInstanceContext(instanceOf()), "memqlLocalInstance");
  assert.equal(selectedInstanceContext(instanceOf({ presence: "absent" })), "memqlLocalInstanceAbsent");
  assert.equal(selectedInstanceContext(instanceOf({ kind: "remote" })), "memqlRemoteInstance");
  assert.equal(selectedInstanceContext(undefined), "");
});

// ---------------------------------------------------------------------------
// runDuration
// ---------------------------------------------------------------------------

test("a duration is coarse but keeps its seconds", () => {
  assert.equal(runDuration("2026-08-14T10:00:00Z", "2026-08-14T10:04:12Z"), "4m 12s");
  assert.equal(runDuration("2026-08-14T10:00:00Z", "2026-08-14T10:00:09Z"), "9s");
  assert.equal(runDuration("2026-08-14T10:00:00Z", "2026-08-14T11:30:00Z"), "1h 30m");
});

test("a run with no finish has no duration, rather than a zero one", () => {
  // Three different reasons, one answer. A run in flight, an interrupted run
  // whose finish was never written, and an unparseable stamp are all facts
  // about the RECORD; printing "0s" would make each a claim about the run.
  assert.equal(runDuration("2026-08-14T10:00:00Z", undefined), "");
  assert.equal(runDuration("2026-08-14T10:00:00Z", ""), "");
  assert.equal(runDuration(undefined, "2026-08-14T10:00:00Z"), "");
  assert.equal(runDuration("not a date", "2026-08-14T10:00:00Z"), "");
});

test("clock skew reads as no time at all, never as a negative duration", () => {
  // A cluster's clock and this machine's differ routinely, and a run that
  // finished a moment "before" it started did not run backwards.
  assert.equal(runDuration("2026-08-14T10:00:05Z", "2026-08-14T10:00:00Z"), "0s");
});
