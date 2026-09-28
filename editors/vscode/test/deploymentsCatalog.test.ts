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
  clusterState,
  connectionWordFor,
  deploymentsContextKeys,
  formatWhen,
  instanceContextValue,
  pollIntervalMs,
  relativeTime,
  runRowStatus,
  versionTransition,
  POLL_ACTIVE_MS,
  POLL_IDLE_MS,
  type CatalogInputs,
} from "../src/state/deploymentsCatalog.js";
import type { Instance } from "../src/state/deployments.js";

const NOW = Date.parse("2026-08-14T12:00:00Z");

function presenceOf(verdict: PresenceResult["verdict"]): () => Promise<PresenceResult> {
  return async () => ({ verdict, evidence: { receipt: true, registry: false, liveCluster: false }, endpoint: "" });
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

test("a branch install shows its branch and commit, never the word unknown", async () => {
  const catalog = await buildCatalog(
    baseInputs({
      presence: presenceOf("installed-healthy"),
      readReceiptFile: async () =>
        receiptWith([{ result: { commit: "3f2a9c1e5b7d", refKind: "branch", ref: "main", dest: "/home/me/.memql/stack" } }]),
      readClusters: clusters({ clusters: [{ name: "local", endpoint: "api.memql.localhost:443", local: true }] }),
    }),
  );
  const local = catalog.instances[0];
  assert.equal(local.version, undefined, "a branch install records no tag, and none is invented");
  assert.equal(local.versionLabel, "main @ 3f2a9c1");
});

test("a branch or commit install shows what it runs and compares nothing, even when the registry recorded a release", async () => {
  // The registry holds what the cluster REPORTED -- the nearest release for a
  // build of main -- and `version` is what an update is compared from, so
  // borrowing it would offer a branch install an "update" to a tag.
  const registry = clusters({ clusters: [{ name: "local", endpoint: "api.memql.localhost:443", local: true, version: "v0.23.5" }] });
  const branch = await buildCatalog(
    baseInputs({
      presence: presenceOf("installed-healthy"),
      readReceiptFile: async () =>
        receiptWith([{ result: { commit: "3f2a9c1e5b7d", refKind: "branch", ref: "main", dest: "/home/me/.memql/stack" } }]),
      readClusters: registry,
    }),
  );
  assert.equal(branch.instances[0].version, undefined);
  assert.equal(branch.instances[0].versionLabel, "main @ 3f2a9c1");

  const commit = await buildCatalog(
    baseInputs({
      presence: presenceOf("installed-healthy"),
      readReceiptFile: async () => receiptWith([{ result: { commit: "9e8d7c6b5a4f", refKind: "commit", dest: "/home/me/.memql/stack" } }]),
      readClusters: registry,
    }),
  );
  assert.equal(commit.instances[0].version, undefined);
  assert.equal(commit.instances[0].versionLabel, "9e8d7c6", "a commit build is named by its commit, not the release it reported");
});

test("a cluster whose receipt names no release falls back to the release the registry recorded", async () => {
  const catalog = await buildCatalog(
    baseInputs({
      presence: presenceOf("installed-healthy"),
      readClusters: clusters({
        clusters: [
          { name: "local", endpoint: "api.memql.localhost:443", local: true, version: "v0.23.5" },
          { name: "staging", endpoint: "a:443", version: "v0.22.0" },
        ],
      }),
    }),
  );
  const byName = new Map(catalog.instances.map((i) => [i.name, i]));
  assert.equal(byName.get("local")?.version, "v0.23.5");
  assert.equal(byName.get("local")?.versionLabel, "v0.23.5");
  // A remote this editor is not connected to still has the release it last saw.
  assert.equal(byName.get("staging")?.versionLabel, "v0.22.0");
});

test("a local cluster registered in the list is marked so; one found only on disk is not", async () => {
  const listed = await buildCatalog(
    baseInputs({
      presence: presenceOf("installed-healthy"),
      readClusters: clusters({ clusters: [{ name: "local", endpoint: "a:443", local: true }] }),
    }),
  );
  assert.equal(listed.instances[0].registered, true);
  const unlisted = await buildCatalog(baseInputs({ presence: presenceOf("installed-healthy") }));
  assert.equal(unlisted.instances[0].registered, false);
});

// -----------------------------------------------------------------------------
// where this editor stands: one vocabulary for the heading and the page
// -----------------------------------------------------------------------------

test("the connection word follows the manager, and an expired or refused credential is a sign-in", () => {
  assert.equal(connectionWordFor({ status: "disconnected" }, "local"), "none");
  assert.equal(connectionWordFor({ status: "connecting", clusterName: "local" }, "local"), "connecting");
  assert.equal(connectionWordFor({ status: "connected", clusterName: "local", nodeId: "n" }, "local"), "connected");
  // A state about a DIFFERENT cluster says nothing about this one.
  assert.equal(connectionWordFor({ status: "connected", clusterName: "staging", nodeId: "n" }, "local"), "none");
  for (const reason of ["missingCredential", "credentialExpired", "wrongTokenClass", "reauthenticationRequired"] as const) {
    assert.equal(connectionWordFor({ status: "error", clusterName: "local", message: "", reason }, "local"), "signIn", reason);
  }
  assert.equal(connectionWordFor({ status: "error", clusterName: "local", message: "", reason: "notConfigured" }, "local"), "notConfigured");
  assert.equal(connectionWordFor({ status: "error", clusterName: "local", message: "", reason: "lost" }, "local"), "unreachable");
});

const INSTALLED: Instance = { name: "local", kind: "local", presence: "installed-healthy", connected: false, registered: true };

test("a signed-out cluster reads 'Not signed in', never 'not answering'", () => {
  // The owner's case: a front door that answers and a credential that is gone.
  // The old heading said "not answering", from a probe, and offered nothing.
  const state = clusterState(INSTALLED, "signIn");
  assert.equal(state.word, "Not signed in");
  assert.equal(state.heading, "Sign in");
  assert.equal(state.tone, "warn");
});

test("a dropped local cluster whose front door is silent is 'Not running'; one that answers is 'Can't reach'", () => {
  assert.equal(clusterState({ ...INSTALLED, presence: "installed-unreachable" }, "unreachable").word, "Not running");
  assert.equal(clusterState(INSTALLED, "unreachable").word, "Can't reach");
  assert.equal(clusterState({ ...INSTALLED, kind: "remote", name: "staging" }, "unreachable").word, "Can't reach");
});

test("what is on the machine speaks only where the connection cannot", () => {
  assert.equal(clusterState({ ...INSTALLED, presence: "absent" }, "none").word, "Not installed");
  assert.equal(clusterState({ ...INSTALLED, registered: false }, "none").word, "Not in your list");
  assert.equal(clusterState({ ...INSTALLED, presence: "present-unreceipted" }, "none").key, "unreceipted");
  assert.equal(clusterState(INSTALLED, "connected").word, "Connected");
  assert.equal(clusterState(INSTALLED, "none").word, "Not connected");
});

test("a cluster no install recorded is its own title-menu value, and is offered no lifecycle there", () => {
  assert.equal(instanceContextValue({ ...INSTALLED, presence: "present-unreceipted" }), "memqlLocalInstanceUnreceipted");
  assert.deepEqual(deploymentsContextKeys({ ...INSTALLED, presence: "present-unreceipted", checkout: "/c" }), {
    "memql.deploymentsInstance": "memqlLocalInstanceUnreceipted",
    "memql.deploymentsHasCheckout": false,
    "memql.deploymentsHasBranch": false,
  });
});

test("the title menu's checkout and branch keys gate Rebuild, Open checkout and Pull", () => {
  assert.deepEqual(deploymentsContextKeys(INSTALLED), {
    "memql.deploymentsInstance": "memqlLocalInstance",
    "memql.deploymentsHasCheckout": false,
    "memql.deploymentsHasBranch": false,
  });
  assert.deepEqual(deploymentsContextKeys({ ...INSTALLED, checkout: "/c", checkoutBranch: "main" }), {
    "memql.deploymentsInstance": "memqlLocalInstance",
    "memql.deploymentsHasCheckout": true,
    "memql.deploymentsHasBranch": true,
  });
  assert.deepEqual(deploymentsContextKeys(undefined), {
    "memql.deploymentsInstance": "",
    "memql.deploymentsHasCheckout": false,
    "memql.deploymentsHasBranch": false,
  });
});

// -----------------------------------------------------------------------------
// what a run row says
// -----------------------------------------------------------------------------

test("a run row says what happened as a verb, from where to where, and when", () => {
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
  assert.equal(status.label, "Updated");
  // The dot already says it succeeded; the description does not say it again.
  assert.equal(status.description, "v0.16.1 → v0.17.0 · 1d ago");
  assert.equal(status.icon, "succeeded");
  assert.doesNotMatch(status.tooltip, /\d{4}-\d{2}-\d{2}T/, "an RFC3339 stamp reached the tooltip");
  assert.match(status.tooltip, /^Started \d{1,2} Aug, \d{2}:\d{2} · took 5m 0s$/);
});

test("a move back to an older release is a version change, never an 'upgrade'", () => {
  const back: Run = {
    ...newLocalRun({ id: "r", instance: "local", kind: "upgrade", startedAt: "t", fromVersion: "v0.18.0", toVersion: "v0.17.0" }),
    status: "succeeded",
  };
  assert.equal(runRowStatus(back, NOW).label, "Changed version");
});

test("an outcome that is not a success is said in the label, once", () => {
  const base = newLocalRun({ id: "r", instance: "local", kind: "rebuild", startedAt: "2026-08-14T11:00:00Z" });
  assert.equal(runRowStatus({ ...base, status: "failed" }, NOW).label, "Rebuild failed");
  assert.equal(runRowStatus({ ...base, status: "cancelled" }, NOW).label, "Rebuild cancelled");
  assert.equal(runRowStatus({ ...base, status: "interrupted" }, NOW).label, "Rebuild interrupted");
  assert.match(runRowStatus({ ...base, status: "interrupted" }, NOW).tooltip, /stopped when the editor closed/);
  assert.equal(runRowStatus({ ...base, status: "running" }, NOW).label, "Rebuilding");
  for (const status of ["failed", "cancelled", "interrupted"] as const) {
    assert.doesNotMatch(runRowStatus({ ...base, status }, NOW).description, new RegExp(status));
  }
});

test("a failed run's tooltip carries the reason its failed step recorded, and not the log file", () => {
  const run: Run = {
    ...newLocalRun({ id: "r", instance: "local", kind: "upgrade", startedAt: "2026-08-14T11:00:00Z" }),
    status: "failed",
    items: [{ label: "clusterUp", status: "failed", detail: "Port 443 is already in use. · log=r.clusterUp.log" }],
  };
  assert.match(runRowStatus(run, NOW).tooltip, /Port 443 is already in use\./);
  assert.doesNotMatch(runRowStatus(run, NOW).tooltip, /log=/);
});

test("a prepared remote record reads Prepared, not Deploying", () => {
  const run: Run = {
    ...newLocalRun({ id: "d", instance: "s", kind: "rollout", startedAt: "2026-08-14T11:00:00Z" }),
    status: "running",
    toVersion: "v0.9.3",
  };
  assert.equal(runRowStatus(run, NOW).label, "Deploying");
  const prepared = runRowStatus(run, NOW, { prepared: true });
  assert.equal(prepared.label, "Prepared");
  assert.equal(prepared.tone, "idle");
});

test("a moment is written in this machine's words, with the year only when it is not this one", () => {
  assert.match(formatWhen("2026-08-12T12:00:00Z", NOW), /^\d{1,2} Aug, \d{2}:\d{2}$/);
  assert.match(formatWhen("2025-08-12T12:00:00Z", NOW), /^\d{1,2} Aug 2025, \d{2}:\d{2}$/);
  assert.equal(formatWhen("", NOW), "");
});

test("a visible view re-reads briskly while something runs, slowly otherwise", () => {
  const idle = newLocalRun({ id: "r", instance: "local", kind: "install", startedAt: "t" });
  assert.equal(pollIntervalMs([]), POLL_IDLE_MS);
  assert.equal(pollIntervalMs([{ ...idle, status: "succeeded" }]), POLL_IDLE_MS);
  assert.equal(pollIntervalMs([idle]), POLL_ACTIVE_MS);
  assert.ok(POLL_ACTIVE_MS < POLL_IDLE_MS);
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
  const replaced: Run = {
    ...newLocalRun({ id: "r", instance: "s", kind: "rollout", startedAt: "2026-08-10T12:00:00Z" }),
    status: "superseded",
    toVersion: "v0.9.1",
  };
  assert.equal(runRowStatus(replaced, NOW).label, "Deployed");
  assert.match(runRowStatus(replaced, NOW).description, /replaced/);
  assert.equal(runRowStatus({ ...replaced, status: "rolled_back" }, NOW).label, "Rolled back");
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
