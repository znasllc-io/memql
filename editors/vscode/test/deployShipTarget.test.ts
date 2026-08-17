// Which record the Deploy button ships (memql#4017).
//
// The button used to ship `runs[0]` -- the newest record in the catalog the
// page last read -- resolved at the CLICK. Two things are wrong with that and
// they fail differently:
//
//   IT IS THE NEWEST RECORD, NOT THE PENDING ONE. `deploy` transitions a record
//   pending -> in_progress (component/deploycontrol/deploy.go) and does not
//   check the status it is transitioning FROM. So on a cluster whose newest
//   record has landed, the button re-shipped a `succeeded` deployment: a deploy
//   nobody asked for, of a release that was already there.
//
//   IT IS RESOLVED AT THE CLICK. Right almost always, and wrong for whichever
//   operator loses a race -- two cuts against one cluster between the page's
//   catalog reads and the ship names the other one's record. The same shape was
//   removed from the upgrade button's remote path in #4015 (memql#3997) by
//   carrying the id the cut RETURNED; a standalone Deploy has no cut to return
//   one, so the id is resolved when the page is BUILT and printed on it.
//
// The answer is one derivation (`pendingDeploymentId`) stamped onto the
// instance the page renders, so what ships is the record the operator was
// looking at -- and the page says which one that is before anything is pressed.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import type { ClustersFile } from "../src/clusters/model.js";
import type { PresenceResult } from "../src/clusters/presence.js";
import {
  currentDeploymentId,
  pendingDeploymentId,
  projectDeployments,
  type DeploymentRecord,
} from "../src/state/deploymentHistory.js";
import { remoteInstance, type Instance, type Run } from "../src/state/deployments.js";
import { buildCatalog, type CatalogInputs } from "../src/state/deploymentsCatalog.js";
import { renderRemoteInstance } from "../src/webview/deploymentScreens.js";
import type { DeployActionSpec } from "../src/deploy/actions.js";
import { DEPLOY_ACTIONS } from "../src/deploy/actions.js";

// -----------------------------------------------------------------------------
// fixtures
// -----------------------------------------------------------------------------

function deploymentRow(payload: Record<string, unknown>, createdAt: string): Row {
  return {
    id: `v1:cluster:deployment:${String(payload["deploymentId"])}`,
    concept: "v1:cluster:deployment",
    createdAt,
    payload,
  };
}

function records(...rows: { id: string; status: string; createdAt: string }[]): DeploymentRecord[] {
  return projectDeployments(
    rows.map((r) => deploymentRow({ deploymentId: r.id, status: r.status }, r.createdAt)),
  );
}

// -----------------------------------------------------------------------------
// pendingDeploymentId -- the derivation
// -----------------------------------------------------------------------------

test("the newest CUT-but-unshipped record is the ship target", () => {
  const target = pendingDeploymentId(
    records(
      { id: "d1", status: "pending", createdAt: "2026-06-01T00:00:00Z" },
      { id: "d2", status: "pending", createdAt: "2026-06-02T00:00:00Z" },
    ),
  );
  assert.equal(target, "d2");
});

test("an in_progress record is NOT a ship target -- it is already shipping", () => {
  // `deploy` moves a record pending -> in_progress. Naming one that is already
  // in flight would restart a deploy that is running.
  assert.equal(
    pendingDeploymentId(records({ id: "d1", status: "in_progress", createdAt: "2026-06-01T00:00:00Z" })),
    "",
  );
});

test("no terminal status is a ship target", () => {
  // Every one of these describes a deploy that is OVER. `deploy` does not check
  // the status it transitions from, so naming a landed record here is what put
  // a succeeded deployment back in flight.
  for (const status of ["succeeded", "failed", "superseded", "rolled_back"]) {
    assert.equal(
      pendingDeploymentId(records({ id: "d1", status, createdAt: "2026-06-01T00:00:00Z" })),
      "",
      `${status} must not be shippable`,
    );
  }
});

test("a NEWER landed record does not hide an older pending one", () => {
  // The exact case `runs[0]` got wrong: the newest record is not the cut one.
  // A rollback lands a new record at `rolled_back` on top of a version somebody
  // cut and has not shipped yet, and that cut is still the thing to ship.
  const all = records(
    { id: "cut", status: "pending", createdAt: "2026-06-01T00:00:00Z" },
    { id: "landed", status: "rolled_back", createdAt: "2026-06-02T00:00:00Z" },
  );
  assert.equal(pendingDeploymentId(all), "cut");
  assert.equal(currentDeploymentId(all), "landed", "the two answers are independent");
});

test("nothing cut is the empty string, which is a state and not a failure", () => {
  // The ordinary condition of a cluster nobody is mid-deploy on. The surface
  // says so; it does not fall back to another record.
  assert.equal(pendingDeploymentId([]), "");
  assert.equal(
    pendingDeploymentId(records({ id: "d1", status: "succeeded", createdAt: "2026-06-01T00:00:00Z" })),
    "",
  );
});

test("two records cut in the same instant still resolve to ONE target", () => {
  // A coarse clock, or a fixture. The tie-break is the same total order
  // projectDeployments sorts by, applied here independently so a caller that
  // filtered or re-ordered the list cannot silently get a different answer.
  const same = "2026-06-01T00:00:00Z";
  const forwards = pendingDeploymentId(
    records({ id: "a", status: "pending", createdAt: same }, { id: "b", status: "pending", createdAt: same }),
  );
  const backwards = pendingDeploymentId(
    [...records({ id: "a", status: "pending", createdAt: same }, { id: "b", status: "pending", createdAt: same })].reverse(),
  );
  assert.equal(forwards, backwards);
  assert.notEqual(forwards, "");
});

// -----------------------------------------------------------------------------
// the instance carries it, so the page holds what it rendered
// -----------------------------------------------------------------------------

test("a remote instance carries the ship target alongside its current version", () => {
  const instance = remoteInstance({
    name: "staging",
    reachable: true,
    connected: true,
    deployments: [
      { ...record("d2", "pending"), version: "v0.9.3" },
      { ...record("d1", "succeeded"), version: "v0.9.2" },
    ],
    currentDeploymentId: "d1",
    pendingDeploymentId: "d2",
  });
  // The version stays the CURRENT deployment's -- a cut that has not shipped
  // has not moved the cluster, and saying otherwise mid-deploy is the failure
  // remoteInstance's own comment names.
  assert.equal(instance.version, "v0.9.2");
  assert.equal(instance.pendingDeploymentId, "d2");
});

test("an instance with nothing cut carries no ship target at all", () => {
  const instance = remoteInstance({
    name: "staging",
    reachable: true,
    connected: true,
    deployments: [record("d1", "succeeded")],
    currentDeploymentId: "d1",
    pendingDeploymentId: "",
  });
  assert.equal(instance.pendingDeploymentId, undefined);
});

function record(id: string, status: string): DeploymentRecord {
  return {
    deploymentId: id,
    status,
    version: "v0.9.2",
    provider: "azure",
    region: "eastus",
    imageDigest: "sha256:abcdef0123456789",
    createdAt: "2026-08-10T00:00:00Z",
    updatedAt: "2026-08-10T00:05:00Z",
    triggeredBy: "znas",
    previousDeploymentId: "",
  };
}

// -----------------------------------------------------------------------------
// the catalog resolves it once, from the same read that resolves the version
// -----------------------------------------------------------------------------

function presenceOf(verdict: PresenceResult["verdict"]): () => Promise<PresenceResult> {
  return async () => ({ verdict, evidence: { receipt: true, registry: false }, endpoint: "" });
}

function clusters(file: Partial<ClustersFile> = {}): CatalogInputs["readClusters"] {
  return async () => ({ ok: true as const, file: { clusters: [], selectedCluster: "", ...file } });
}

function catalogInputs(over: Partial<CatalogInputs> = {}): CatalogInputs {
  return {
    clustersPath: "/nowhere/clusters.yaml",
    receiptPath: "/nowhere/install-receipt.json",
    runsDir: "/nowhere/runs",
    presence: presenceOf("absent"),
    readClusters: clusters({ clusters: [{ name: "staging", endpoint: "a:443" }] }),
    readReceiptFile: async () => null,
    listRunsIn: async () => [],
    connection: { clusterName: "staging", connected: true },
    ...over,
  };
}

test("the catalog stamps the ship target on the connected instance", async () => {
  const catalog = await buildCatalog(
    catalogInputs({
      readDeployments: async () => ({
        deployments: [
          deploymentRow({ deploymentId: "d1", status: "succeeded", version: "v0.9.2" }, "2026-08-11T00:00:00Z"),
          deploymentRow({ deploymentId: "d2", status: "pending", version: "v0.9.3" }, "2026-08-12T00:00:00Z"),
        ],
        specs: [],
      }),
    }),
  );
  const staging = catalog.instances.find((i) => i.name === "staging");
  assert.equal(staging?.version, "v0.9.2", "the cut has not landed, so the version is still the old one");
  assert.equal(staging?.pendingDeploymentId, "d2");
});

test("an unconnected remote carries no ship target, because nothing was read", async () => {
  // The extension holds one connection at a time, so every other remote is a
  // cluster this editor cannot see the records of. Offering a target for one
  // would be naming a record nothing read.
  const catalog = await buildCatalog(
    catalogInputs({
      readClusters: clusters({
        clusters: [
          { name: "staging", endpoint: "a:443" },
          { name: "prod", endpoint: "b:443" },
        ],
      }),
      readDeployments: async () => ({
        deployments: [deploymentRow({ deploymentId: "d2", status: "pending" }, "2026-08-12T00:00:00Z")],
        specs: [],
      }),
    }),
  );
  const byName = new Map(catalog.instances.map((i) => [i.name, i]));
  assert.equal(byName.get("staging")?.pendingDeploymentId, "d2");
  assert.equal(byName.get("prod")?.pendingDeploymentId, undefined);
});

test("a deployment read that fails leaves no ship target rather than a stale one", async () => {
  const catalog = await buildCatalog(
    catalogInputs({
      readDeployments: async () => {
        throw new Error("stream closed");
      },
    }),
  );
  assert.equal(catalog.instances.find((i) => i.name === "staging")?.pendingDeploymentId, undefined);
});

// -----------------------------------------------------------------------------
// the page names it
// -----------------------------------------------------------------------------

const DEPLOY_SPEC = DEPLOY_ACTIONS.find((a) => a.id === "deploy") as DeployActionSpec;
const CUT_SPEC = DEPLOY_ACTIONS.find((a) => a.id === "cutVersion") as DeployActionSpec;

function remote(over: Partial<Instance> = {}): Instance {
  return {
    name: "staging",
    kind: "remote",
    presence: "installed-healthy",
    connected: true,
    version: "v0.9.2",
    ...over,
  };
}

function render(
  instance: Instance,
  actions: DeployActionSpec[] = [DEPLOY_SPEC, CUT_SPEC],
  runs: readonly Run[] = [],
): string {
  return renderRemoteInstance({
    instance,
    runs,
    pipeline: { kind: "present", title: "Deploy", detail: "", actions },
    nowMs: 0,
    outcome: "",
    error: "",
    releases: undefined,
    upgrade: { kind: "none", reason: "not under test" },
  });
}

test("the page names the record Deploy will ship", () => {
  // OPTION B OF THE ISSUE, delivered by rendering rather than by a modal: the
  // operator reads which record is about to go BEFORE pressing, rather than
  // being asked to confirm one after.
  const html = render(remote({ pendingDeploymentId: "d2" }));
  assert.match(html, /Deploy ships d2/);
});

test("the named record carries its version when the page has the record", () => {
  const runs: Run[] = [
    { id: "d2", instance: "staging", kind: "rollout", startedAt: "2026-08-12T00:00:00Z", status: "running", items: [], toVersion: "v0.9.3" },
  ];
  const html = render(remote({ pendingDeploymentId: "d2" }), [DEPLOY_SPEC], runs);
  assert.match(html, /Deploy ships d2 \(v0\.9\.3\)/);
});

test("with nothing cut the page says so, instead of naming a record", () => {
  const html = render(remote());
  assert.match(html, /Nothing is cut/);
  assert.doesNotMatch(html, /Deploy ships/);
});

test("the ship line is drawn only where the Deploy action is", () => {
  // A reader sees the history and none of the actions. Telling them nothing is
  // cut would be answering a question their page never raised.
  const html = render(remote({ pendingDeploymentId: "d2" }), []);
  assert.doesNotMatch(html, /Deploy ships/);
  assert.doesNotMatch(html, /Nothing is cut/);

  const cutOnly = render(remote({ pendingDeploymentId: "d2" }), [CUT_SPEC]);
  assert.doesNotMatch(cutOnly, /Deploy ships/);
});

test("a ship target is escaped like every other value on the page", () => {
  const html = render(remote({ pendingDeploymentId: '<img src=x onerror="alert(1)">' }));
  assert.doesNotMatch(html, /<img src=x/);
  assert.match(html, /&lt;img src=x/);
});
