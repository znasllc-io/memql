// What a cluster offers, and what a page's action bar draws (memql#3736).
//
// The assertions to read first: INSTALL IS OFFERED FOR `absent` AND FOR
// NOTHING ELSE -- an install over a cluster that already exists rebuilds a k3d
// cluster, a hosts block and a trust-store CA underneath a working stack -- and
// AN ACT WHOSE ONLY OUTCOME IS A REFUSAL IS ABSENT: Deploy with nothing
// prepared, a promote with no rollout to name, a rollback aimed at the
// deployment already running. Each of those was drawn before this redesign.

import test from "node:test";
import assert from "node:assert/strict";

import { roleVisibility, visibleActions } from "../src/deploy/actions.js";
import {
  barOffers,
  localActs,
  localOverviewBar,
  moveFlowFor,
  offersLocal,
  remoteOverviewBar,
  rollbackTarget,
  runDetailBar,
  type PageBar,
} from "../src/deploy/instanceActions.js";
import type { PipelineState } from "../src/deploy/pipelineState.js";
import type { UpgradeVerdict } from "../src/deploy/upgrade.js";
import { localInstance, remoteInstance, type Instance, type Run } from "../src/state/deployments.js";

function local(presence: Instance["presence"], over: Partial<Instance> = {}): Instance {
  return {
    ...localInstance({ presence, receipt: null, connected: presence === "installed-healthy", registered: { name: "local" } }),
    ...over,
  };
}

function remote(over: Partial<Instance> = {}): Instance {
  return { ...remoteInstance({ name: "staging", reachable: true, connected: true }), ...over };
}

const NONE: UpgradeVerdict = { kind: "none", reason: "not under test" };
const OFFER: UpgradeVerdict = {
  kind: "offer",
  target: { instanceName: "local", from: "v0.23.5", to: "v0.24.0", flow: "upgradeToTag" },
  label: "Update to v0.24.0",
  title: "Update local",
  confirmation: "Update local from v0.23.5 to v0.24.0?",
  phrase: "v0.24.0",
};

const ids = (bar: PageBar): string[] => bar.acts.map((a) => a.id);
const primary = (bar: PageBar) => bar.acts.find((a) => a.tone === "primary" || a.tone === "danger");

// -----------------------------------------------------------------------------
// what is legal on this machine
// -----------------------------------------------------------------------------

test("a machine with no cluster is offered exactly one thing: install", () => {
  assert.deepEqual(localActs(local("absent")), ["install"]);
});

test("install is never offered over a cluster that already exists", () => {
  for (const presence of ["installed-healthy", "installed-unreachable", "present-unreceipted"] as const) {
    assert.equal(offersLocal(local(presence), "install"), false, presence);
  }
});

test("an installed cluster offers change version, repair and uninstall", () => {
  assert.deepEqual(localActs(local("installed-healthy")), ["changeVersion", "repair", "uninstall"]);
  assert.deepEqual(localActs(local("installed-unreachable")), ["changeVersion", "repair", "uninstall"]);
});

test("a cluster no install recorded can only be adopted or deleted", () => {
  // memql#5118: nothing recorded what is on this machine, so repair and a
  // version change have nothing to replay -- the old page offered both.
  assert.deepEqual(localActs(local("present-unreceipted")), ["adopt", "uninstall"]);
});

test("a rebuild needs a checkout, and a pull needs a branch as well", () => {
  assert.equal(offersLocal(local("installed-healthy"), "rebuildFromCheckout"), false);
  const pinned = local("installed-healthy", { checkout: "/home/me/.memql/stack" });
  assert.equal(offersLocal(pinned, "rebuildFromCheckout"), true);
  // memql#5073: a release install has a checkout pinned to a tag, and nothing
  // to pull.
  assert.equal(offersLocal(pinned, "updateAndRebuild"), false);
  assert.equal(offersLocal({ ...pinned, checkoutBranch: "main" }, "updateAndRebuild"), true);
  assert.equal(offersLocal(local("absent", { checkout: "/src", checkoutBranch: "main" }), "rebuildFromCheckout"), false);
});

test("an installed cluster missing from the list is offered a reconnect", () => {
  assert.equal(offersLocal(local("installed-healthy", { registered: false }), "reconnect"), true);
  assert.equal(offersLocal(local("installed-healthy"), "reconnect"), false);
});

test("remote clusters have no local lifecycle", () => {
  assert.deepEqual(localActs(remote()), []);
  assert.equal(moveFlowFor(remote()), "deployControl");
  assert.equal(moveFlowFor(local("installed-healthy")), "upgradeToTag");
});

// -----------------------------------------------------------------------------
// the local page's bar
// -----------------------------------------------------------------------------

test("the local bar's primary is what the state asks for", () => {
  const cases: [Instance, Parameters<typeof localOverviewBar>[0]["connection"], string, string][] = [
    [local("absent"), "none", "Not installed", "install"],
    [local("present-unreceipted"), "none", "Not connected", "adopt"],
    [local("installed-healthy", { registered: false }), "none", "Not in your list", "reconnect"],
    [local("installed-healthy"), "signIn", "Not signed in", "signIn"],
    [local("installed-unreachable"), "unreachable", "Not running", "repair"],
    [local("installed-unreachable"), "none", "Not running", "repair"],
    [local("installed-healthy"), "none", "Not connected", "connect"],
  ];
  for (const [instance, connection, state, act] of cases) {
    const bar = localOverviewBar({ instance, connection, upgrade: NONE });
    assert.equal(bar.state, state, `${instance.presence}/${connection}`);
    assert.equal(primary(bar)?.id, act, `${instance.presence}/${connection}`);
  }
});

test("a signed-out local cluster says Sign in -- the owner's first ask", () => {
  const bar = localOverviewBar({ instance: local("installed-healthy"), connection: "signIn", upgrade: OFFER });
  assert.deepEqual(ids(bar), ["changeVersion", "signIn"]);
  // No update is offered in front of a sign-in: the sign-in is the state's ask.
  assert.equal(bar.acts.some((a) => a.id === "update"), false);
});

test("a connected cluster behind the newest release leads with the update, the target on the act", () => {
  const bar = localOverviewBar({ instance: local("installed-healthy"), connection: "connected", upgrade: OFFER });
  assert.equal(bar.state, "Connected");
  assert.deepEqual(ids(bar), ["changeVersion", "update"]);
  assert.equal(primary(bar)?.value, "v0.24.0");
  assert.equal(primary(bar)?.label, "Update to v0.24.0…");
});

test("a cluster that needs nothing is not handed a button", () => {
  const bar = localOverviewBar({ instance: local("installed-healthy"), connection: "connected", upgrade: NONE });
  assert.deepEqual(ids(bar), ["changeVersion"]);
  assert.equal(primary(bar), undefined);
});

test("a cluster running its own build leads with Rebuild, and offers the pull when there is a branch", () => {
  const instance = local("installed-healthy", {
    checkout: "/home/me/.memql/stack",
    checkoutBranch: "main",
    imageSource: "checkout",
  });
  const bar = localOverviewBar({ instance, connection: "connected", upgrade: OFFER });
  assert.deepEqual(ids(bar), ["updateAndRebuild", "changeVersion", "rebuildFromCheckout"]);
  assert.equal(primary(bar)?.id, "rebuildFromCheckout");
});

test("repair and uninstall are not on a healthy bar; they are in the title menu", () => {
  const bar = localOverviewBar({ instance: local("installed-healthy"), connection: "connected", upgrade: NONE });
  assert.equal(bar.acts.some((a) => a.id === "repair" || a.id === "uninstall"), false);
});

test("no bar ever carries more than three acts or more than one button", () => {
  const instances = [
    local("absent"),
    local("installed-healthy", { checkout: "/c", checkoutBranch: "main", imageSource: "checkout" }),
    local("installed-unreachable"),
    local("present-unreceipted"),
  ];
  for (const instance of instances) {
    for (const connection of ["none", "connecting", "connected", "signIn", "unreachable", "notConfigured"] as const) {
      for (const upgrade of [NONE, OFFER]) {
        const bar = localOverviewBar({ instance, connection, upgrade });
        assert.ok(bar.acts.length <= 3);
        assert.ok(bar.acts.filter((a) => a.tone !== undefined).length <= 1);
      }
    }
  }
});

// -----------------------------------------------------------------------------
// a remote cluster's bar
// -----------------------------------------------------------------------------

function pipeline(over: Partial<PipelineState> = {}): PipelineState {
  return { kind: "present", line: "", engineMessage: "", actions: visibleActions(roleVisibility("owner")), rollouts: [], ...over };
}

function remoteRun(id: string, status: Run["status"], toVersion: string, startedAt: string): Run {
  return { id, instance: "staging", kind: "rollout", status, toVersion, startedAt, items: [] };
}

const HISTORY: Run[] = [
  remoteRun("dep-3", "succeeded", "v0.23.5", "2026-09-27T10:00:00Z"),
  remoteRun("dep-2", "failed", "v0.23.4", "2026-09-20T10:00:00Z"),
  remoteRun("dep-1", "succeeded", "v0.23.3", "2026-09-10T10:00:00Z"),
];

test("Deploy is absent when nothing is prepared, and names the version when something is", () => {
  const bare = remoteOverviewBar({
    instance: remote({ currentDeploymentId: "dep-3" }),
    connection: "connected",
    upgrade: NONE,
    pipeline: pipeline(),
    visibility: roleVisibility("owner"),
    runs: HISTORY,
  });
  assert.equal([...bare.acts, ...bare.more].some((a) => a.id === "deploy"), false);
  // With nothing prepared, preparing is the offered act.
  assert.equal(primary(bare)?.id, "cutVersion");

  const runs = [remoteRun("dep-4", "running", "v0.24.0", "2026-09-28T09:00:00Z"), ...HISTORY];
  const pending = remoteOverviewBar({
    instance: remote({ currentDeploymentId: "dep-3", pendingDeploymentId: "dep-4" }),
    connection: "connected",
    upgrade: NONE,
    pipeline: pipeline(),
    visibility: roleVisibility("owner"),
    runs,
  });
  assert.equal(primary(pending)?.id, "deploy");
  assert.equal(primary(pending)?.label, "Deploy v0.24.0");
  assert.equal(primary(pending)?.value, "dep-4");
});

test("the rollback targets the newest landed deployment OLDER than the one running", () => {
  // THE BUG: it took the newest succeeded record, which IS the current one --
  // and the engine then re-shipped what was already running.
  assert.equal(rollbackTarget(HISTORY, "dep-3")?.id, "dep-1");
  assert.equal(rollbackTarget(HISTORY, "dep-1"), undefined);
  assert.equal(rollbackTarget(HISTORY, ""), undefined);
  assert.equal(rollbackTarget(HISTORY, "gone"), undefined);

  const bar = remoteOverviewBar({
    instance: remote({ currentDeploymentId: "dep-3" }),
    connection: "connected",
    upgrade: NONE,
    pipeline: pipeline(),
    visibility: roleVisibility("owner"),
    runs: HISTORY,
  });
  const rollback = [...bar.acts, ...bar.more].find((a) => a.id === "rollback");
  assert.equal(rollback?.value, "dep-1");
  assert.equal(rollback?.label, "Roll back to v0.23.3…");
});

test("promote and abort are offered per in-flight rollout, and never with a blank name", () => {
  const without = remoteOverviewBar({
    instance: remote({ currentDeploymentId: "dep-3" }),
    connection: "connected",
    upgrade: NONE,
    pipeline: pipeline(),
    visibility: roleVisibility("owner"),
    runs: HISTORY,
  });
  assert.equal([...without.acts, ...without.more].some((a) => a.id === "rolloutPromote" || a.id === "rolloutAbort"), false);

  const withRollout = remoteOverviewBar({
    instance: remote({ currentDeploymentId: "dep-3" }),
    connection: "connected",
    upgrade: NONE,
    pipeline: pipeline({ rollouts: ["bff"] }),
    visibility: roleVisibility("owner"),
    runs: HISTORY,
  });
  const all = [...withRollout.acts, ...withRollout.more];
  assert.deepEqual(
    all.filter((a) => a.id === "rolloutPromote" || a.id === "rolloutAbort").map((a) => [a.id, a.value, a.label]),
    [
      ["rolloutPromote", "bff", "Promote bff"],
      ["rolloutAbort", "bff", "Abort bff…"],
    ],
  );
  // More than fits: the rest are behind More, not dropped.
  assert.ok(withRollout.acts.length <= 3);
  assert.equal(withRollout.acts.some((a) => a.id === "more"), true);
});

test("a developer is offered prepare and deploy, never rollback or rollouts", () => {
  const bar = remoteOverviewBar({
    instance: remote({ currentDeploymentId: "dep-3", pendingDeploymentId: "dep-3" }),
    connection: "connected",
    upgrade: NONE,
    pipeline: pipeline({ actions: visibleActions(roleVisibility("developer")), rollouts: ["bff"] }),
    visibility: roleVisibility("developer"),
    runs: HISTORY,
  });
  const all = [...bar.acts, ...bar.more].map((a) => a.id);
  assert.deepEqual(all.sort(), ["cutVersion", "deploy"]);
});

test("a remote cluster this editor is not signed in to offers Sign in and nothing else", () => {
  const bar = remoteOverviewBar({ instance: remote({ connected: false }), connection: "signIn", upgrade: NONE, pipeline: undefined, runs: [] });
  assert.equal(bar.state, "Not signed in");
  assert.deepEqual(ids(bar), ["signIn"]);
});

test("no pipeline means no deploy acts, including the update", () => {
  const bar = remoteOverviewBar({
    instance: remote(),
    connection: "connected",
    upgrade: { ...OFFER, target: { ...OFFER.target, flow: "deployControl" } } as UpgradeVerdict,
    pipeline: pipeline({ kind: "notConfigured", actions: [], line: "Deployments aren't set up for this cluster." }),
    runs: [],
  });
  assert.deepEqual(bar.acts, []);
});

// -----------------------------------------------------------------------------
// one run's bar
// -----------------------------------------------------------------------------

function localRun(kind: Run["kind"], status: Run["status"], over: Partial<Run> = {}): Run {
  return { id: "r1", instance: "local", kind, status, startedAt: "2026-09-27T10:00:00Z", items: [], ...over };
}

test("a failed or interrupted local run offers Retry, as the act it came from", () => {
  const instance = local("installed-healthy", { checkout: "/c" });
  const retry = (run: Run) => primary(runDetailBar({ instance, run, connection: "connected", runInFlight: false }));
  assert.deepEqual(
    [retry(localRun("upgrade", "failed", { toVersion: "v0.24.0" }))?.id, retry(localRun("upgrade", "failed", { toVersion: "v0.24.0" }))?.value],
    ["changeVersion", "v0.24.0"],
  );
  assert.equal(retry(localRun("rebuild", "interrupted"))?.id, "rebuildFromCheckout");
  assert.equal(retry(localRun("repair", "failed"))?.id, "repair");
  assert.equal(retry(localRun("install", "failed"))?.id, "repair");
  assert.equal(retry(localRun("install", "failed"))?.label, "Retry");
  // Not when the act is not legal now: no branch, no pull.
  assert.equal(retry(localRun("update", "failed")), undefined);
  // A run that went through offers nothing.
  assert.deepEqual(runDetailBar({ instance, run: localRun("upgrade", "succeeded"), connection: "connected", runInFlight: false }).acts, []);
});

test("while a run is going, a past run's page offers only a way to it", () => {
  const bar = runDetailBar({ instance: local("installed-healthy"), run: localRun("repair", "failed"), connection: "connected", runInFlight: true });
  assert.deepEqual(ids(bar), ["showRun"]);
});

test("roll back is offered only on a landed remote run that is not the one running, aimed at THAT run", () => {
  const instance = remote({ currentDeploymentId: "dep-3" });
  const bar = (run: Run) =>
    runDetailBar({ instance, run, connection: "connected", pipeline: pipeline(), visibility: roleVisibility("owner"), runInFlight: false });
  assert.deepEqual(bar(HISTORY[0]!).acts, [], "the running deployment");
  assert.deepEqual(bar(HISTORY[1]!).acts, [], "a failed one");
  const older = bar(HISTORY[2]!);
  assert.equal(older.acts[0]?.id, "rollback");
  assert.equal(older.acts[0]?.value, "dep-1");
  assert.equal(older.acts[0]?.tone, "danger");
  // An admin is not an owner.
  assert.deepEqual(
    runDetailBar({ instance, run: HISTORY[2]!, connection: "connected", pipeline: pipeline(), visibility: roleVisibility("admin"), runInFlight: false }).acts,
    [],
  );
});

// -----------------------------------------------------------------------------
// the untrusted boundary
// -----------------------------------------------------------------------------

test("a posted act runs only when the bar offered it, target and all", () => {
  const bar = remoteOverviewBar({
    instance: remote({ currentDeploymentId: "dep-3" }),
    connection: "connected",
    upgrade: NONE,
    pipeline: pipeline({ rollouts: ["bff"] }),
    visibility: roleVisibility("owner"),
    runs: HISTORY,
  });
  assert.equal(barOffers(bar, "rollback", "dep-1")?.id, "rollback");
  // A rollback aimed at a different deployment than the one on the button.
  assert.equal(barOffers(bar, "rollback", "dep-3"), undefined);
  assert.equal(barOffers(bar, "deploy", "dep-3"), undefined);
  assert.equal(barOffers(bar, "rolloutAbort", "agent"), undefined);
  assert.equal(barOffers(bar, "more", undefined), undefined);
});
