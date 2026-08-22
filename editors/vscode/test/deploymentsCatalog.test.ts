// What the Deployments tree renders (memql#3737).
//
// The assertion this file exists for is the FRESH MACHINE: no local cluster,
// no clusters.yaml, no network. The tree's failure mode there is not an
// exception an operator can read -- it is a blank panel, which says nothing
// and looks like a feature that does not work. So every read is driven here,
// failing, with the catalog still producing rows.
//
// The second is the DIRECTION of each failure. An unreadable receipt, an
// undeterminable presence and an unreachable cluster all have a safe side and
// an unsafe one, and the unsafe side of presence in particular is an install
// run over a cluster that already exists.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import type { ClustersFile } from "../src/clusters/model.js";
import type { PresenceResult } from "../src/clusters/presence.js";
import { emptyReceipt, type Receipt, type ReceiptEntry } from "../src/install/receipt.js";
import { newLocalRun, type Run } from "../src/state/deployments.js";
import {
  buildCatalog,
  instanceContextValue,
  instanceRowStatus,
  relativeTime,
  runRowStatus,
  versionTransition,
  type CatalogInputs,
} from "../src/state/deploymentsCatalog.js";

const NOW = Date.parse("2026-08-14T12:00:00Z");

function presenceOf(verdict: PresenceResult["verdict"]): () => Promise<PresenceResult> {
  return async () => ({ verdict, evidence: { receipt: true, registry: false }, endpoint: "" });
}

function clusters(file: Partial<ClustersFile> = {}): CatalogInputs["readClusters"] {
  return async () => ({
    ok: true as const,
    file: { clusters: [], selectedCluster: "", ...file },
  });
}

function receiptWith(entries: Partial<ReceiptEntry>[]): Receipt {
  return {
    ...emptyReceipt("install", "2026-08-01T00:00:00Z"),
    entries: entries.map((e) => ({
      stepId: "stackCheckout",
      script: "install/clone-stack.sh",
      receipt: "checkout",
      preExisting: false,
      params: {},
      result: {},
      changed: true,
      recordedAt: "2026-08-01T00:00:00Z",
      ...e,
    })),
  };
}

function baseInputs(over: Partial<CatalogInputs> = {}): CatalogInputs {
  return {
    clustersPath: "/nowhere/clusters.yaml",
    receiptPath: "/nowhere/install-receipt.json",
    runsDir: "/nowhere/runs",
    presence: presenceOf("absent"),
    readClusters: clusters(),
    readReceiptFile: async () => null,
    listRunsIn: async () => [],
    ...over,
  };
}

function deploymentRow(payload: Record<string, unknown>, createdAt: string): Row {
  return {
    id: `v1:cluster:deployment:${String(payload["deploymentId"])}`,
    concept: "v1:cluster:deployment",
    createdAt,
    payload,
  };
}

// -----------------------------------------------------------------------------
// the fresh machine
// -----------------------------------------------------------------------------

test("a fresh machine still gets a local row, and it is the only one", async () => {
  const catalog = await buildCatalog(baseInputs());
  assert.equal(catalog.error, undefined);
  assert.deepEqual(catalog.instances.map((i) => i.name), ["local"]);
  assert.equal(catalog.instances[0].presence, "absent");
  // That row is the entry point the Clusters "+" menu used to hide.
  assert.equal(instanceContextValue(catalog.instances[0]), "memqlLocalInstanceAbsent");
});

test("every read failing at once still produces rows rather than a blank panel", async () => {
  const boom = (): never => {
    throw new Error("no");
  };
  const catalog = await buildCatalog(
    baseInputs({
      presence: async () => boom(),
      readReceiptFile: async () => boom(),
      listRunsIn: async () => boom(),
      readDeployments: async () => boom(),
      readClusters: clusters({ clusters: [{ name: "staging", endpoint: "api:443" }] }),
      connection: { clusterName: "staging", connected: true },
    }),
  );
  assert.deepEqual(catalog.instances.map((i) => i.name), ["local", "staging"]);
  assert.equal(catalog.instances[1].version, undefined);
});

test("presence that cannot be determined never reads as absent", async () => {
  // `absent` is the ONE verdict that offers an install, and an install over a
  // cluster that already exists rebuilds a k3d cluster, a hosts block and a
  // trust-store CA underneath it. Failing toward "something is here" is the
  // direction that cannot destroy anything.
  const catalog = await buildCatalog(
    baseInputs({
      presence: async () => {
        throw new Error("probe exploded");
      },
    }),
  );
  assert.equal(catalog.instances[0].presence, "installed-unreachable");
});

test("a malformed clusters.yaml is the sole row, not a rejection", async () => {
  const catalog = await buildCatalog(
    baseInputs({ readClusters: async () => ({ ok: false as const, error: "bad yaml at line 3" }) }),
  );
  assert.equal(catalog.error, "bad yaml at line 3");
  assert.deepEqual(catalog.instances, []);
});

test("a clusters read that REJECTS is the same row, not an unhandled rejection", async () => {
  const catalog = await buildCatalog(
    baseInputs({
      readClusters: async () => {
        throw new Error("EACCES");
      },
    }),
  );
  assert.equal(catalog.error, "EACCES");
});

// -----------------------------------------------------------------------------
// instances
// -----------------------------------------------------------------------------

test("the local instance takes its version and name from what was recorded", async () => {
  const catalog = await buildCatalog(
    baseInputs({
      presence: presenceOf("installed-healthy"),
      readReceiptFile: async () =>
        receiptWith([{ params: { tag: "v0.17.0", domain: "memql.localhost" } }]),
      readClusters: clusters({
        clusters: [{ name: "parity", endpoint: "api.memql.localhost:443", local: true }],
      }),
      connection: { clusterName: "parity", connected: true },
    }),
  );
  const local = catalog.instances[0];
  assert.equal(local.name, "parity");
  assert.equal(local.version, "v0.17.0");
  assert.equal(local.connected, true);
  assert.equal(instanceContextValue(local), "memqlLocalInstance");
});

test("remote instances follow the local one, sorted by name", async () => {
  const catalog = await buildCatalog(
    baseInputs({
      readClusters: clusters({
        clusters: [
          { name: "staging", endpoint: "a:443" },
          { name: "prod", endpoint: "b:443" },
        ],
      }),
    }),
  );
  assert.deepEqual(catalog.instances.map((i) => i.name), ["local", "prod", "staging"]);
});

test("only the connected remote resolves a version; the rest say unknown", async () => {
  const catalog = await buildCatalog(
    baseInputs({
      readClusters: clusters({
        clusters: [
          { name: "staging", endpoint: "a:443" },
          { name: "prod", endpoint: "b:443" },
        ],
      }),
      connection: { clusterName: "staging", connected: true },
      readDeployments: async () => ({
        deployments: [
          deploymentRow(
            { deploymentId: "d1", status: "succeeded", version: "v0.9.2" },
            "2026-08-13T00:00:00Z",
          ),
        ],
        specs: [],
      }),
    }),
  );
  const byName = new Map(catalog.instances.map((i) => [i.name, i]));
  assert.equal(byName.get("staging")?.version, "v0.9.2");
  assert.equal(byName.get("staging")?.presence, "installed-healthy");
  // The extension holds one connection at a time, so every other remote is a
  // cluster this editor cannot reach -- which is what its row says.
  assert.equal(byName.get("prod")?.version, undefined);
  assert.equal(byName.get("prod")?.presence, "installed-unreachable");
});

// -----------------------------------------------------------------------------
// runs
// -----------------------------------------------------------------------------

test("every run in the log belongs to the local instance, whatever it is named", async () => {
  // The log is per-machine and there is one local install per machine, so
  // filtering by name would drop the runs recorded before the operator
  // registered the cluster and gave it one.
  const runs: Run[] = [
    newLocalRun({ id: "r1", instance: "local", kind: "install", startedAt: "2026-08-05T00:00:00Z" }),
  ];
  const catalog = await buildCatalog(
    baseInputs({
      presence: presenceOf("installed-healthy"),
      readClusters: clusters({ clusters: [{ name: "parity", endpoint: "a:443", local: true }] }),
      listRunsIn: async () => runs,
    }),
  );
  assert.equal(catalog.runs.get("parity")?.length, 1);
});

test("an instance with no runs has no children, which is not an empty state", async () => {
  const catalog = await buildCatalog(
    baseInputs({
      presence: presenceOf("installed-healthy"),
      readClusters: clusters({ clusters: [{ name: "staging", endpoint: "a:443" }] }),
    }),
  );
  assert.deepEqual(catalog.runs.get("staging"), undefined);
  assert.deepEqual(catalog.runs.get("local"), []);
});

test("the connected remote's runs come from its deployment rows", async () => {
  const catalog = await buildCatalog(
    baseInputs({
      readClusters: clusters({ clusters: [{ name: "staging", endpoint: "a:443" }] }),
      connection: { clusterName: "staging", connected: true },
      readDeployments: async () => ({
        deployments: [
          deploymentRow(
            { deploymentId: "d1", status: "succeeded", version: "v0.9.1" },
            "2026-08-11T00:00:00Z",
          ),
          deploymentRow(
            { deploymentId: "d2", status: "rolled_back", version: "v0.9.2", previousDeploymentId: "d1" },
            "2026-08-13T00:00:00Z",
          ),
        ],
        specs: [],
      }),
    }),
  );
  const runs = catalog.runs.get("staging") ?? [];
  assert.deepEqual(runs.map((r) => r.id), ["d2", "d1"]);
  assert.equal(runs[0].kind, "rollout");
  assert.equal(runs[0].fromVersion, "v0.9.1");
});

// -----------------------------------------------------------------------------
// what a row says
// -----------------------------------------------------------------------------

test("an instance row always prints a version, and unknown is a word", async () => {
  const healthy = instanceRowStatus({
    name: "staging",
    kind: "remote",
    presence: "installed-healthy",
    connected: true,
  }, undefined);
  assert.equal(healthy.description, "healthy - unknown");
  assert.equal(healthy.icon, "healthy");

  const versioned = instanceRowStatus({
    name: "local",
    kind: "local",
    presence: "installed-healthy",
    version: "v0.17.0",
    connected: false,
  }, undefined);
  assert.equal(versioned.description, "healthy - v0.17.0");
});

test("an unreachable instance says so and keeps its version", () => {
  const status = instanceRowStatus({
    name: "local",
    kind: "local",
    presence: "installed-unreachable",
    version: "v0.16.1",
    connected: false,
  }, undefined);
  assert.equal(status.icon, "unreachable");
  assert.equal(status.description, "not answering - v0.16.1");
});

test("an absent instance says not installed rather than an unknown version", () => {
  const status = instanceRowStatus({
    name: "local",
    kind: "local",
    presence: "absent",
    connected: false,
  }, undefined);
  assert.equal(status.icon, "absent");
  assert.equal(status.description, "not installed");
});

// -----------------------------------------------------------------------------
// checkout mode (memql#4246)
// -----------------------------------------------------------------------------

test("a checkout-mode instance's row names the checkout, not the recorded version", () => {
  const status = instanceRowStatus({
    name: "local",
    kind: "local",
    presence: "installed-healthy",
    version: "v0.17.0",
    connected: false,
    imageSource: "checkout",
    rebuild: {
      commit: "abc1234def",
      ref: "tag:v0.17.0",
      dirtyCount: 4,
      nodes: "bff agent",
      recordedAt: "2026-08-21T10:00:00.000Z",
    },
  }, undefined);
  assert.equal(status.description, "healthy - checkout abc1234 (4 uncommitted)");
  assert.match(status.tooltip, /built from the checkout/);
  assert.match(status.tooltip, /returns it to released images/);
});

test("released mode leaves the row exactly as it was before checkout mode existed", () => {
  const status = instanceRowStatus({
    name: "local",
    kind: "local",
    presence: "installed-healthy",
    version: "v0.17.0",
    connected: false,
    imageSource: "released",
  }, undefined);
  assert.equal(status.description, "healthy - v0.17.0");
});

test("a run row names the verb, the transition, the status and when", () => {
  const run: Run = {
    ...newLocalRun({
      id: "r1",
      instance: "local",
      kind: "upgrade",
      startedAt: "2026-08-12T12:00:00Z",
      fromVersion: "v0.16.1",
      toVersion: "v0.17.0",
    }),
    status: "succeeded",
    finishedAt: "2026-08-12T12:05:00Z",
  };
  const status = runRowStatus(run, NOW);
  assert.equal(status.label, "upgrade");
  assert.equal(status.description, "v0.16.1 -> v0.17.0  succeeded  1d ago");
  assert.equal(status.icon, "succeeded");
});

test("an install has no predecessor, so it renders no arrow", () => {
  assert.equal(versionTransition({ ...newLocalRun({ id: "r", instance: "local", kind: "install", startedAt: "t" }), toVersion: "v0.17.0" }), "v0.17.0");
  assert.equal(versionTransition(newLocalRun({ id: "r", instance: "local", kind: "install", startedAt: "t" })), "");
});

test("a landed-then-replaced run is neither a success tick nor an error", () => {
  // Drawing them as failures blames the run for a later decision; drawing them
  // as plain successes hides that the cluster is no longer running them.
  for (const status of ["superseded", "rolled_back"] as const) {
    const run: Run = { ...newLocalRun({ id: "r", instance: "s", kind: "rollout", startedAt: "t" }), status };
    assert.equal(runRowStatus(run, NOW).icon, "replaced");
  }
});

test("relative time is coarse, and never negative or NaN", () => {
  assert.equal(relativeTime("2026-08-14T11:59:30Z", NOW), "just now");
  assert.equal(relativeTime("2026-08-14T11:30:00Z", NOW), "30m ago");
  assert.equal(relativeTime("2026-08-14T04:00:00Z", NOW), "8h ago");
  assert.equal(relativeTime("2026-08-05T12:00:00Z", NOW), "9d ago");
  // Clock skew between a cluster and this machine is ordinary; a run dated
  // slightly ahead is not a run that happened in the future.
  assert.equal(relativeTime("2026-08-14T12:00:30Z", NOW), "just now");
  assert.equal(relativeTime("", NOW), "");
  assert.equal(relativeTime(undefined, NOW), "");
  assert.equal(relativeTime("not a date", NOW), "");
});
