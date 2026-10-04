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
  instanceConnectionWord,
  runDuration,
  runsForSelected,
  selectedViewDescription,
  type CatalogInputs,
} from "../src/state/deploymentsCatalog.js";

function presenceOf(verdict: PresenceResult["verdict"]): () => Promise<PresenceResult> {
  return async () => ({ verdict, evidence: { receipt: true, registry: false, liveCluster: false }, endpoint: "" });
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
    registered: true,
    version: "v0.19.1",
    versionLabel: "v0.19.1",
    ...over,
  };
}

test("the heading names the cluster, where this editor stands, and its version", () => {
  assert.equal(selectedViewDescription(instanceOf(), undefined, "connected"), "local · Connected · v0.19.1");
});

test("the owner's case: a signed-out local cluster says Sign in, and its branch build, never 'unknown'", () => {
  // Reproduced from the old heading, which said "local · not answering ·
  // unknown" for exactly this machine.
  const description = selectedViewDescription(
    instanceOf({ version: undefined, versionLabel: "main @ 3f2a9c1" }),
    undefined,
    "signIn",
  );
  assert.equal(description, "local · Sign in · main @ 3f2a9c1");
  assert.ok(!description.includes("unknown"));
  assert.ok(!description.includes("not answering"));
});

test("an update is its own segment", () => {
  assert.equal(selectedViewDescription(instanceOf(), LISTING, "connected"), "local · Connected · v0.19.1 · v0.20.0 available");
});

test("a cluster already on the newest release says nothing extra", () => {
  assert.equal(
    selectedViewDescription(instanceOf({ version: "v0.20.0", versionLabel: "v0.20.0" }), LISTING, "connected"),
    "local · Connected · v0.20.0",
  );
});

test("a dropped local cluster whose front door is silent reads Not running", () => {
  assert.equal(
    selectedViewDescription(instanceOf({ presence: "installed-unreachable" }), undefined, "unreachable"),
    "local · Not running · v0.19.1",
  );
});

test("a machine with nothing installed makes no version claim", () => {
  assert.equal(
    selectedViewDescription(instanceOf({ presence: "absent", version: undefined, versionLabel: undefined }), LISTING),
    "local · Not installed",
  );
});

test("a version nothing names is left out, not printed as 'unknown'", () => {
  assert.equal(
    selectedViewDescription(instanceOf({ version: undefined, versionLabel: undefined }), undefined, "connected"),
    "local · Connected",
  );
});

test("a cluster running its own build says so, and is offered no release update", () => {
  const description = selectedViewDescription(
    instanceOf({ imageSource: "checkout", versionLabel: "Your build abc1234" }),
    LISTING,
    "connected",
  );
  assert.equal(description, "local · Connected · Your build abc1234");
  assert.ok(!description.includes("available"), "a checkout build was offered a release update");
});

test("no selection means no heading", () => {
  assert.equal(selectedViewDescription(undefined, LISTING), "");
});

test("a remote cluster's heading names it, and says Can't reach rather than 'not answering'", () => {
  assert.equal(
    selectedViewDescription(instanceOf({ name: "staging", kind: "remote", versionLabel: "v0.9.2" }), undefined, "unreachable"),
    "staging · Can't reach · v0.9.2",
  );
});

test("a connection the heading is handed through its facts reads the same way", () => {
  const instance = instanceOf();
  assert.equal(instanceConnectionWord(instance, { clusterName: "local", connected: false, word: "signIn" }), "signIn");
  assert.equal(instanceConnectionWord(instance, { clusterName: "local", connected: true }), "connected");
  assert.equal(instanceConnectionWord(instance, { clusterName: "local", connected: false }), "unreachable");
  assert.equal(instanceConnectionWord(instance, { clusterName: "staging", connected: true }), "none");
  assert.equal(instanceConnectionWord(instance, undefined), "none");
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
