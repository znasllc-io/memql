// The instance and run model (memql#3736).
//
// The assertions worth reading first are the three local-presence cases and
// the unresolvable remote version. The first three are what decide whether an
// operator is offered an install over a cluster they already have; the fourth
// encodes the rule that a derived version an editor could not work out is drawn
// as the word "unknown" and never as a blank, so "we could not tell" is never
// mistaken for "there isn't one".

import test from "node:test";
import assert from "node:assert/strict";

import {
  LOCAL_INSTANCE_NAME,
  displayVersion,
  localInstance,
  newLocalRun,
  nodeSpecDetail,
  remoteInstance,
  remoteItemStatus,
  remotePresence,
  remoteRunStatus,
  runIsTerminal,
  runsFromDeployments,
  settleRunStatus,
  sortInstances,
  upsertRunItem,
  type Run,
  type RunItem,
  type RunItemStatus,
} from "../src/state/deployments.js";
import {
  emptyReceipt,
  type Receipt,
  type ReceiptEntry,
} from "../src/install/receipt.js";
import type {
  DeploymentNodeSpec,
  DeploymentRecord,
} from "../src/state/deploymentHistory.js";

// -----------------------------------------------------------------------------
// fixtures
// -----------------------------------------------------------------------------

function entry(over: Partial<ReceiptEntry> = {}): ReceiptEntry {
  return {
    stepId: "stackCheckout",
    script: "install/clone-stack.sh",
    receipt: "checkout",
    preExisting: false,
    params: {},
    result: {},
    changed: true,
    recordedAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

function receiptWith(entries: ReceiptEntry[]): Receipt {
  return { ...emptyReceipt("install", "2026-08-01T00:00:00Z"), entries };
}

function deployment(over: Partial<DeploymentRecord> = {}): DeploymentRecord {
  return {
    deploymentId: "d1",
    status: "succeeded",
    version: "v0.9.2",
    environment: "staging",
    provider: "azure",
    region: "eastus",
    imageDigest: "sha256:abcdef0123456789",
    createdAt: "2026-08-10T00:00:00Z",
    updatedAt: "2026-08-10T00:05:00Z",
    triggeredBy: "znas",
    previousDeploymentId: "",
    ...over,
  };
}

function spec(over: Partial<DeploymentNodeSpec> = {}): DeploymentNodeSpec {
  return {
    deploymentId: "d1",
    nodeType: "bff",
    version: "",
    replicas: 2,
    imageDigest: "sha256:abcdef0123456789",
    updatedAt: "2026-08-10T00:04:00Z",
    ...over,
  };
}

// -----------------------------------------------------------------------------
// the local instance -- all three presence verdicts
// -----------------------------------------------------------------------------

test("local absent: a row exists with no version and no domain", () => {
  const instance = localInstance({ presence: "absent", receipt: null, connected: false });
  // The row is the entry point an operator with no cluster starts from, so its
  // existence is the assertion -- an empty view would give them nowhere to go.
  assert.equal(instance.name, LOCAL_INSTANCE_NAME);
  assert.equal(instance.kind, "local");
  assert.equal(instance.presence, "absent");
  assert.equal(instance.version, undefined);
  assert.equal(instance.domain, undefined);
  assert.equal(instance.connected, false);
});

test("local installed-healthy: version and domain come off the receipt", () => {
  const instance = localInstance({
    presence: "installed-healthy",
    receipt: receiptWith([
      entry({ stepId: "seedBootstrap", receipt: "", params: { domain: "memql.localhost" } }),
      entry({ stepId: "stackCheckout", params: { tag: "v0.17.0", dest: "/home/x/.memql/stack" } }),
    ]),
    connected: true,
  });
  assert.equal(instance.presence, "installed-healthy");
  assert.equal(instance.version, "v0.17.0");
  assert.equal(instance.domain, "memql.localhost");
  assert.equal(instance.connected, true);
});

test("local installed-unreachable is still an installed instance", () => {
  const instance = localInstance({
    presence: "installed-unreachable",
    receipt: receiptWith([entry({ params: { tag: "v0.16.1" } })]),
    connected: false,
  });
  // Unreachable is not absent: something is on the machine, it is simply not
  // answering. Reading it as absent is how an install runs over a working
  // cluster.
  assert.equal(instance.presence, "installed-unreachable");
  assert.equal(instance.version, "v0.16.1");
});

test("a registered local cluster is named by the registry, not by the default", () => {
  const instance = localInstance({
    presence: "installed-healthy",
    receipt: null,
    registered: { name: "parity", domain: "lab.example.com" },
    connected: true,
  });
  assert.equal(instance.name, "parity");
  assert.equal(instance.domain, "lab.example.com");
});

test("the registry domain wins over the one the receipt recorded", () => {
  const instance = localInstance({
    presence: "installed-healthy",
    receipt: receiptWith([entry({ params: { domain: "memql.localhost" } })]),
    registered: { name: "local", domain: "lab.example.com" },
    connected: true,
  });
  assert.equal(instance.domain, "lab.example.com");
});

// -----------------------------------------------------------------------------
// remote instances
// -----------------------------------------------------------------------------

test("a remote instance is never absent -- only reachable or not", () => {
  assert.equal(remotePresence(true), "installed-healthy");
  assert.equal(remotePresence(false), "installed-unreachable");
});

test("a remote instance's version is the CURRENT deployment's", () => {
  const instance = remoteInstance({
    name: "staging",
    domain: "staging.example.com",
    reachable: true,
    connected: true,
    deployments: [
      // A newer deploy that has not landed must not claim the cluster's version.
      deployment({ deploymentId: "d2", status: "in_progress", version: "v0.9.3", createdAt: "2026-08-11T00:00:00Z" }),
      deployment({ deploymentId: "d1", status: "succeeded", version: "v0.9.2" }),
    ],
    currentDeploymentId: "d1",
  });
  assert.equal(instance.version, "v0.9.2");
  assert.equal(instance.kind, "remote");
  assert.equal(instance.presence, "installed-healthy");
});

test("a remote version that cannot resolve renders as unknown, never blank", () => {
  const unreachable = remoteInstance({
    name: "staging",
    reachable: false,
    connected: false,
    deployments: [],
    currentDeploymentId: "",
  });
  assert.equal(unreachable.version, undefined);
  assert.equal(displayVersion(unreachable.version), "unknown");
  assert.equal(unreachable.presence, "installed-unreachable");

  // A current id that names a record the page never loaded is the same case.
  const missingRecord = remoteInstance({
    name: "staging",
    reachable: true,
    connected: true,
    deployments: [deployment({ deploymentId: "other" })],
    currentDeploymentId: "d-not-loaded",
  });
  assert.equal(displayVersion(missingRecord.version), "unknown");
});

test("displayVersion says the word for every empty spelling", () => {
  assert.equal(displayVersion(undefined), "unknown");
  assert.equal(displayVersion(""), "unknown");
  assert.equal(displayVersion("   "), "unknown");
  assert.equal(displayVersion("v1.2.3"), "v1.2.3");
});

test("local sorts above remote, then remote by name", () => {
  const sorted = sortInstances([
    remoteInstance({ name: "staging", reachable: true, connected: false }),
    remoteInstance({ name: "prod", reachable: true, connected: false }),
    localInstance({ presence: "absent", receipt: null, connected: false }),
  ]);
  assert.deepEqual(
    sorted.map((i) => i.name),
    ["local", "prod", "staging"],
  );
});

// -----------------------------------------------------------------------------
// remote runs
// -----------------------------------------------------------------------------

test("every deployment status maps to a run status", () => {
  assert.equal(remoteRunStatus("pending"), "running");
  assert.equal(remoteRunStatus("in_progress"), "running");
  assert.equal(remoteRunStatus("succeeded"), "succeeded");
  assert.equal(remoteRunStatus("failed"), "failed");
  assert.equal(remoteRunStatus("superseded"), "superseded");
  assert.equal(remoteRunStatus("rolled_back"), "rolled_back");
});

test("an unrecognised deployment status asserts no outcome", () => {
  // `running` is the only value that claims nothing. `failed` would manufacture
  // an alarm and `succeeded` would close the book, both about a deploy this
  // model has simply never seen.
  assert.equal(remoteRunStatus("quiesced"), "running");
  assert.equal(remoteRunStatus(""), "running");
});

test("remote runs come back newest first, with items per node type", () => {
  const runs = runsFromDeployments({
    instance: "staging",
    deployments: [
      deployment({ deploymentId: "d1", version: "v0.9.1", createdAt: "2026-08-09T00:00:00Z" }),
      deployment({
        deploymentId: "d2",
        version: "v0.9.2",
        previousDeploymentId: "d1",
        createdAt: "2026-08-10T00:00:00Z",
        updatedAt: "2026-08-10T00:06:00Z",
      }),
    ],
    specs: [
      spec({ deploymentId: "d2", nodeType: "cognition" }),
      spec({ deploymentId: "d2", nodeType: "bff" }),
      spec({ deploymentId: "d1", nodeType: "bff" }),
    ],
  });

  assert.deepEqual(runs.map((r) => r.id), ["d2", "d1"]);
  const [newest] = runs;
  assert.equal(newest.kind, "rollout");
  assert.equal(newest.instance, "staging");
  assert.equal(newest.fromVersion, "v0.9.1");
  assert.equal(newest.toVersion, "v0.9.2");
  assert.equal(newest.finishedAt, "2026-08-10T00:06:00Z");
  assert.deepEqual(newest.items.map((i) => i.label), ["bff", "cognition"]);
  assert.equal(newest.items[0].status, "ok");
});

test("a deploy still in flight has no finish time", () => {
  const [run] = runsFromDeployments({
    instance: "staging",
    deployments: [deployment({ status: "in_progress", updatedAt: "2026-08-10T00:05:00Z" })],
    specs: [spec()],
  });
  // `updatedAt` is the last TRANSITION, so on a running deploy it is when it
  // last advanced. Reporting that as "finished" draws a live deploy as a done one.
  assert.equal(run.status, "running");
  assert.equal(run.finishedAt, undefined);
  assert.equal(run.items[0].status, "running");
});

test("a landed-then-replaced deployment leaves its tiers ok", () => {
  // superseded and rolled_back both describe a deploy that LANDED. Blaming the
  // tier for what happened afterwards would be a failure the tier did not have.
  assert.equal(remoteItemStatus("superseded"), "ok");
  assert.equal(remoteItemStatus("rolled_back"), "ok");
  assert.equal(remoteItemStatus("failed"), "failed");
  assert.equal(remoteItemStatus("cancelled"), "skipped");
});

test("a tier says whether its version was pinned or inherited", () => {
  const record = deployment({ version: "v0.9.2" });
  assert.equal(
    nodeSpecDetail(spec({ version: "" }), record),
    "v0.9.2 (inherited) · 2 replicas · digest abcdef012345",
  );
  assert.equal(
    nodeSpecDetail(spec({ version: "v0.9.5", replicas: 1, imageDigest: "" }), record),
    "v0.9.5 (pinned) · 1 replica",
  );
});

test("a tier with no resolvable version says so rather than printing nothing", () => {
  assert.equal(
    nodeSpecDetail(spec({ version: "", imageDigest: "" }), deployment({ version: "" })),
    "version unknown · 2 replicas",
  );
});

// -----------------------------------------------------------------------------
// local runs
// -----------------------------------------------------------------------------

test("a new local run opens as running with no items", () => {
  const run = newLocalRun({
    id: "r1",
    instance: "local",
    kind: "install",
    startedAt: "2026-08-14T10:00:00Z",
  });
  assert.equal(run.status, "running");
  assert.deepEqual(run.items, []);
  assert.equal(run.fromVersion, undefined);
  assert.equal(runIsTerminal(run.status), false);
});

test("all six item states round-trip through a run", () => {
  const states: RunItemStatus[] = ["pending", "running", "ok", "failed", "skipped", "preserved"];
  let run = newLocalRun({ id: "r1", instance: "local", kind: "uninstall", startedAt: "t0" });
  for (const status of states) {
    run = upsertRunItem(run, { label: status, status });
  }
  assert.deepEqual(run.items.map((i) => i.status), states);
});

test("a retried step replaces its earlier record instead of appearing twice", () => {
  let run = newLocalRun({ id: "r1", instance: "local", kind: "install", startedAt: "t0" });
  run = upsertRunItem(run, { label: "clusterUp", status: "failed", detail: "docker is not running" });
  run = upsertRunItem(run, { label: "clusterUp", status: "ok" });
  assert.equal(run.items.length, 1);
  assert.equal(run.items[0].status, "ok");
  assert.equal(run.items[0].detail, undefined);
});

test("upsert does not mutate the run it was given", () => {
  const run = newLocalRun({ id: "r1", instance: "local", kind: "install", startedAt: "t0" });
  const next = upsertRunItem(run, { label: "detect", status: "ok" });
  assert.equal(run.items.length, 0);
  assert.equal(next.items.length, 1);
});

test("preserved and skipped do not fail a run; a failure does", () => {
  const base = newLocalRun({ id: "r1", instance: "local", kind: "uninstall", startedAt: "t0" });
  const kept = withItems(base, [
    { label: "removeCluster", status: "preserved", detail: "you created this k3d cluster" },
    { label: "removeBinary", status: "ok" },
    { label: "removeHosts", status: "skipped" },
  ]);
  assert.equal(settleRunStatus(kept), "succeeded");

  const broken = withItems(base, [{ label: "removeCluster", status: "failed" }, { label: "removeBinary", status: "ok" }]);
  assert.equal(settleRunStatus(broken), "failed");
});

test("a run with work outstanding does not settle", () => {
  const base = newLocalRun({ id: "r1", instance: "local", kind: "install", startedAt: "t0" });
  assert.equal(settleRunStatus(withItems(base, [{ label: "a", status: "ok" }, { label: "b", status: "running" }])), "running");
  assert.equal(settleRunStatus(withItems(base, [{ label: "a", status: "ok" }, { label: "b", status: "pending" }])), "running");
  // A failure outranks outstanding work: the run is over either way, and the
  // failure is the fact the operator needs.
  assert.equal(settleRunStatus(withItems(base, [{ label: "a", status: "failed" }, { label: "b", status: "pending" }])), "failed");
});

test("terminal statuses are exactly the ones that will not change again", () => {
  assert.equal(runIsTerminal("running"), false);
  for (const status of ["succeeded", "failed", "cancelled", "superseded", "rolled_back"] as const) {
    assert.equal(runIsTerminal(status), true, status);
  }
});

function withItems(run: Run, items: RunItem[]): Run {
  return { ...run, items };
}
