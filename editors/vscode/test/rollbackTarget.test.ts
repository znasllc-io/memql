// Which deployment Roll back returns to.
//
// It was the newest `succeeded` deployment in the history, and every forward
// deploy lands `succeeded` and becomes the one the cluster runs -- so on the
// ordinary cluster the button's target was the running release, and pressing
// it wrote a new deployment record redeploying what was already there.
//
// `deploymentHistory.rollbackTargetId` replaced that: the newest `succeeded`
// record cut BEFORE the release the cluster runs, carrying a DIFFERENT
// release. These cases pin each clause against the history that needs it, then
// the catalog stamping it on the instance and the page naming it.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DEPLOY_ACTIONS, type DeployActionSpec } from "../src/deploy/actions.js";
import { deployControlByKey } from "../src/deploy/controls.js";
import { remoteChoices, remoteOverviewBar, type RemoteBarInput } from "../src/deploy/instanceActions.js";
import {
  currentDeploymentId,
  projectDeployments,
  rollbackTargetId,
  type DeploymentRecord,
} from "../src/state/deploymentHistory.js";
import type { Instance, Run } from "../src/state/deployments.js";
import { buildCatalog } from "../src/state/deploymentsCatalog.js";
import { remoteOverviewScreen } from "../src/webview/deploymentScreens.js";

interface Fixture {
  id: string;
  status: string;
  day: number;
  version?: string;
  digest?: string;
  previous?: string;
}

function row(r: Fixture): Row {
  return {
    id: `v1:cluster:deployment:${r.id}`,
    concept: "v1:cluster:deployment",
    createdAt: `2026-09-${String(r.day).padStart(2, "0")}T00:00:00Z`,
    payload: {
      deploymentId: r.id,
      status: r.status,
      version: r.version ?? `1.0.${r.day}`,
      imageDigest: r.digest ?? `sha256:${r.id}`,
      ...(r.previous !== undefined ? { previousDeploymentId: r.previous } : {}),
    },
  };
}

function history(...rows: Fixture[]): DeploymentRecord[] {
  return projectDeployments(rows.map(row));
}

test("the running release is never the target -- the one before it is", () => {
  // THE DEFECT. d2 landed last, so it is what runs, and it is also the newest
  // `succeeded` record -- which is what the button used to send.
  const records = history(
    { id: "d1", status: "succeeded", day: 1 },
    { id: "d2", status: "succeeded", day: 2 },
  );
  assert.equal(currentDeploymentId(records), "d2");
  assert.equal(rollbackTargetId(records), "d1");
});

test("a cluster on its first release has nothing to roll back to", () => {
  assert.equal(rollbackTargetId(history({ id: "d1", status: "succeeded", day: 1 })), "");
  assert.equal(rollbackTargetId([]), "");
});

test("records that never landed, or have not yet, are not targets and do not move the anchor", () => {
  // A deploy in flight leaves the previous deployment current, and the engine
  // refuses every status but `succeeded` as a target.
  const records = history(
    { id: "d1", status: "succeeded", day: 1 },
    { id: "bad", status: "failed", day: 2 },
    { id: "old", status: "superseded", day: 3 },
    { id: "d2", status: "succeeded", day: 4 },
    { id: "cut", status: "pending", day: 5 },
    { id: "going", status: "in_progress", day: 6 },
  );
  assert.equal(currentDeploymentId(records), "d2");
  assert.equal(rollbackTargetId(records), "d1");
});

test("after a rollback, the target is measured from the release it returned to", () => {
  // d3 was bad; the operator rolled back to d2 (record rb, `rolled_back`,
  // previousDeploymentId d2). Anchored on rb's own cut time, the answer would
  // be d3 -- the release they just fled -- under a button that says back.
  const records = history(
    { id: "d1", status: "succeeded", day: 1 },
    { id: "d2", status: "succeeded", day: 2 },
    { id: "d3", status: "succeeded", day: 3 },
    { id: "rb", status: "rolled_back", day: 4, version: "1.0.2", digest: "sha256:d2", previous: "d2" },
  );
  assert.equal(currentDeploymentId(records), "rb");
  assert.equal(rollbackTargetId(records), "d1");
});

test("rolled back to the oldest release, there is nowhere further back to go", () => {
  const records = history(
    { id: "d1", status: "succeeded", day: 1 },
    { id: "d2", status: "succeeded", day: 2 },
    { id: "rb", status: "rolled_back", day: 3, version: "1.0.1", digest: "sha256:d1", previous: "d1" },
  );
  assert.equal(rollbackTargetId(records), "");
});

test("a rollback whose target is not in the history read gets no answer rather than a guess", () => {
  // The read is paged. Without the record it returned to there is no anchor,
  // and measuring from the rollback record itself is the forward move above.
  const records = history(
    { id: "d2", status: "succeeded", day: 2 },
    { id: "d3", status: "succeeded", day: 3 },
    { id: "rb", status: "rolled_back", day: 4, version: "1.0.1", previous: "gone" },
  );
  assert.equal(rollbackTargetId(records), "");
});

test("a record carrying the running release is skipped, as after a repair", () => {
  // A repair (memql#4209) lands `succeeded`, stamped with the version it
  // re-synced. Excluding the running RECORD alone would name d2 -- the same
  // release under an older id.
  const records = history(
    { id: "d1", status: "succeeded", day: 1, version: "1.0.1" },
    { id: "d2", status: "succeeded", day: 2, version: "1.0.2" },
    { id: "repair", status: "succeeded", day: 3, version: "1.0.2" },
  );
  assert.equal(currentDeploymentId(records), "repair");
  assert.equal(rollbackTargetId(records), "d1");
});

test("the release is compared by digest when a record names no version", () => {
  const records = history(
    { id: "d1", status: "succeeded", day: 1, version: "", digest: "sha256:aaa" },
    { id: "d2", status: "succeeded", day: 2, version: "", digest: "sha256:bbb" },
    { id: "repair", status: "succeeded", day: 3, version: "", digest: "sha256:bbb" },
  );
  assert.equal(rollbackTargetId(records), "d1");
});

test("a record with nothing to redeploy is skipped -- the engine would refuse it", () => {
  const records = history(
    { id: "d1", status: "succeeded", day: 1 },
    { id: "blank", status: "succeeded", day: 2, version: "", digest: "" },
    { id: "d3", status: "succeeded", day: 3 },
  );
  assert.equal(rollbackTargetId(records), "d1");
});

test("the answer does not depend on the order the records arrive in", () => {
  const rows: Fixture[] = [
    { id: "d1", status: "succeeded", day: 1 },
    { id: "d2", status: "succeeded", day: 2 },
    { id: "d3", status: "succeeded", day: 3 },
  ];
  const forwards = rollbackTargetId(history(...rows));
  const backwards = rollbackTargetId([...history(...rows)].reverse());
  assert.equal(forwards, "d2");
  assert.equal(backwards, forwards);
});

// -----------------------------------------------------------------------------
// the catalog carries it, and the page names it
// -----------------------------------------------------------------------------

test("the catalog stamps the rollback target on the connected instance", async () => {
  const catalog = await buildCatalog({
    clustersPath: "/nowhere/clusters.yaml",
    receiptPath: "/nowhere/install-receipt.json",
    runsDir: "/nowhere/runs",
    presence: async () => ({
      verdict: "absent",
      evidence: { receipt: false, registry: false, liveCluster: false },
      endpoint: "",
    }),
    readClusters: async () => ({
      ok: true as const,
      file: { clusters: [{ name: "staging", endpoint: "a:443" }], selectedCluster: "" },
    }),
    readReceiptFile: async () => null,
    listRunsIn: async () => [],
    connection: { clusterName: "staging", connected: true },
    readDeployments: async () => ({
      deployments: [row({ id: "d1", status: "succeeded", day: 1 }), row({ id: "d2", status: "succeeded", day: 2 })],
      specs: [],
    }),
  });
  const staging = catalog.instances.find((i) => i.name === "staging");
  assert.equal(staging?.rollbackTargetId, "d1");
});

const ROLLBACK = DEPLOY_ACTIONS.find((a) => a.id === "rollback") as DeployActionSpec;

function page(instance: Instance, runs: readonly Run[] = [], actions: DeployActionSpec[] = [ROLLBACK]): string {
  const input: RemoteBarInput = {
    instance,
    connection: "connected",
    upgrade: { kind: "none", reason: "not under test" },
    pipeline: { kind: "present", line: "", engineMessage: "", actions, rollouts: [] },
    runs,
  };
  const parts = remoteOverviewScreen({
    instance,
    bar: remoteOverviewBar(input),
    choices: remoteChoices(input),
    connection: "connected",
    runs,
    nowMs: 0,
    pipeline: input.pipeline,
    upgrade: input.upgrade,
    detailsOpen: false,
  });
  return parts.head + parts.body + parts.actions;
}

const REMOTE: Instance = { name: "staging", kind: "remote", presence: "installed-healthy", connected: true };

test("the page names the release Roll back returns to, before anything is pressed", () => {
  const runs: Run[] = [
    { id: "d1", instance: "staging", kind: "rollout", startedAt: "2026-09-01T00:00:00Z", status: "succeeded", items: [], toVersion: "1.0.1" },
  ];
  const html = page({ ...REMOTE, rollbackTargetId: "d1" }, runs);
  // The act names the release; Details names the record, for a support case.
  assert.match(html, /data-act="deploy" data-value="rollback:d1"[^>]*>Roll back to 1\.0\.1…</);
  assert.match(html, /<dt>Roll back to<\/dt><dd class="mq-mono">d1<\/dd>/);
});

test("with no target there is no Roll back to press, and its key sends nothing", () => {
  // An act whose only outcome is a refusal is absent from the page. A key
  // posted anyway resolves to the control's refusal, never to a request.
  const html = page(REMOTE);
  assert.doesNotMatch(html, /data-value="rollback/);
  assert.doesNotMatch(html, /Roll back to/);
  const control = deployControlByKey("rollback", { instance: REMOTE, runs: [], rollouts: [], preview: undefined });
  assert.equal(control?.request, undefined);
  assert.match(control?.refusal ?? "", /nothing to roll back to/);
});

test("the rollback target is named only where the Roll back action is", () => {
  const html = page({ ...REMOTE, rollbackTargetId: "d1" }, [], []);
  assert.doesNotMatch(html, /data-value="rollback/);
  assert.doesNotMatch(html, /Roll back to/);
});
