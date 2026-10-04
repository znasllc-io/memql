// What a cluster offers, and what a page's action bar draws (memql#3736).
//
// The assertions to read first: INSTALL IS OFFERED FOR `absent` AND FOR
// NOTHING ELSE -- an install over a cluster that already exists rebuilds a k3d
// cluster, a hosts block and a trust-store CA underneath a working stack -- and
// AN ACT WHOSE ONLY OUTCOME IS A REFUSAL IS ABSENT: Deploy with nothing
// prepared, a promote with no rollout to name, a rollback aimed at the
// deployment already running. Each of those was drawn before this redesign.
//
// What a remote deploy act SENDS is deploy/controls.ts's (deployControls.test,
// rollbackTarget.test); what is pinned here is WHERE each is drawn -- Deploy
// and Roll back on the bar, a rollout's Promote and Abort on its own row, the
// versions to prepare as a short list -- and that the bar stays within three.

import test from "node:test";
import assert from "node:assert/strict";

import { roleVisibility, visibleActions } from "../src/deploy/actions.js";
import { deployControlByKey } from "../src/deploy/controls.js";
import {
  barOffers,
  localActs,
  localOverviewBar,
  moveFlowFor,
  offersLocal,
  remoteChoices,
  remoteControlFacts,
  remoteOverviewBar,
  runDetailBar,
  type PageBar,
  type RemoteBarInput,
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

function rollout(name: string, phase: string): PipelineState["rollouts"][number] {
  return { name, kind: "bluegreen", phase, activeColor: "", previewColor: "", canaryWeight: 0, currentStep: -1, latestAnalysisResult: "" };
}

function remoteRun(id: string, status: Run["status"], toVersion: string, startedAt: string): Run {
  return { id, instance: "staging", kind: "rollout", status, toVersion, startedAt, items: [] };
}

const HISTORY: Run[] = [
  remoteRun("dep-3", "succeeded", "v0.23.5", "2026-09-27T10:00:00Z"),
  remoteRun("dep-2", "failed", "v0.23.4", "2026-09-20T10:00:00Z"),
  remoteRun("dep-1", "succeeded", "v0.23.3", "2026-09-10T10:00:00Z"),
];

/** A connected remote page's input, as the panel builds it. */
function remoteInput(over: Partial<RemoteBarInput> & { instance?: Instance } = {}): RemoteBarInput {
  return {
    instance: remote({ currentDeploymentId: "dep-3", rollbackTargetId: "dep-1" }),
    connection: "connected",
    upgrade: NONE,
    pipeline: pipeline(),
    visibility: roleVisibility("owner"),
    runs: HISTORY,
    ...over,
  };
}

const all = (bar: PageBar) => [...bar.acts, ...bar.more];

test("Deploy is absent when nothing is prepared, and names the version when something is", () => {
  const bare = remoteOverviewBar(remoteInput());
  assert.equal(all(bare).some((a) => a.value === "deploy"), false);
  // With nothing prepared, preparing is what is offered: a version per bump,
  // as choices on the page rather than a fourth act on the bar.
  assert.deepEqual(
    remoteChoices(remoteInput()).versions.map((choice) => [choice.act.id, choice.act.value, choice.act.label]),
    [
      ["deploy", "cutVersion:patch", "Prepare next patch"],
      ["deploy", "cutVersion:minor", "Prepare next minor"],
      ["deploy", "cutVersion:major", "Prepare next major"],
    ],
  );

  const runs = [remoteRun("dep-4", "running", "v0.24.0", "2026-09-28T09:00:00Z"), ...HISTORY];
  const input = remoteInput({ instance: remote({ currentDeploymentId: "dep-3", pendingDeploymentId: "dep-4" }), runs });
  const pending = remoteOverviewBar(input);
  assert.equal(primary(pending)?.id, "deploy");
  assert.equal(primary(pending)?.label, "Deploy v0.24.0");
  // The act posts the control's key; the record is fixed on the instance.
  assert.equal(primary(pending)?.value, "deploy");
  assert.deepEqual(deployControlByKey("deploy", remoteControlFacts(input))?.request, { id: "deploy", deploymentId: "dep-4" });
});

test("the versions to prepare name each version, with the bump as a quiet note", () => {
  const choices = remoteChoices(
    remoteInput({
      preview: {
        suggestion: { currentVersion: "v0.23.5", nextPatch: "v0.23.6", nextMinor: "v0.24.0", nextMajor: "v1.0.0", source: "deployment" },
        message: "",
      },
    }),
  );
  assert.deepEqual(
    choices.versions.map((choice) => [choice.act.value, choice.act.label, choice.note]),
    [
      ["cutVersion:patch:v0.23.6", "Prepare v0.23.6", "patch"],
      ["cutVersion:minor:v0.24.0", "Prepare v0.24.0", "minor"],
      ["cutVersion:major:v1.0.0", "Prepare v1.0.0", "major"],
    ],
  );
  assert.equal(choices.previewMessage, "");
  const failed = remoteChoices(remoteInput({ preview: { suggestion: null, message: "suggestion unavailable" } }));
  assert.equal(failed.previewMessage, "suggestion unavailable");
});

test("Roll back is offered toward the release the history names, and names it", () => {
  // Which release is deploymentHistory.rollbackTargetId's rule
  // (rollbackTarget.test): the newest landed one before the release running.
  const rollback = all(remoteOverviewBar(remoteInput())).find((a) => (a.value ?? "").startsWith("rollback:"));
  assert.equal(rollback?.id, "deploy");
  assert.equal(rollback?.value, "rollback:dep-1");
  assert.equal(rollback?.label, "Roll back to v0.23.3…");
  // No target, no act -- not a Roll back aimed at whatever landed last.
  const none = remoteOverviewBar(remoteInput({ instance: remote({ currentDeploymentId: "dep-3" }) }));
  assert.equal(all(none).some((a) => (a.value ?? "").startsWith("rollback")), false);
});

test("promote and abort are offered per in-flight rollout, on its own row, and never with a blank name", () => {
  // Reported but settled: the group says none is in progress, and offers nothing.
  const settled = remoteChoices(remoteInput({ pipeline: pipeline({ rollouts: [rollout("bff", "Healthy")] }) }));
  assert.deepEqual(settled.rollouts, []);
  // Not reported at all: no group.
  assert.equal(remoteChoices(remoteInput()).rollouts, undefined);

  const input = remoteInput({
    pipeline: pipeline({ rollouts: [rollout("bff", "Paused"), rollout("agent", "Healthy"), rollout("", "Paused")] }),
  });
  const rows = remoteChoices(input).rollouts ?? [];
  assert.deepEqual(
    rows.map((row) => [row.name, row.phase, row.acts.map((a) => [a.id, a.value, a.label, a.ariaLabel])]),
    [
      [
        "bff",
        "Paused",
        [
          ["deploy", "rolloutAction:promote:bff", "Promote", "Promote bff"],
          ["deploy", "rolloutAction:abort:bff", "Abort…", "Abort bff"],
        ],
      ],
    ],
  );
  // The bar carries none of them, and stays within three with nothing behind More.
  const bar = remoteOverviewBar(input);
  assert.equal(all(bar).some((a) => (a.value ?? "").startsWith("rolloutAction")), false);
  assert.ok(bar.acts.length <= 3);
  assert.equal(bar.acts.some((a) => a.id === "more"), false);
});

test("a developer is offered prepare and deploy, never rollback or rollouts", () => {
  const input = remoteInput({
    instance: remote({ currentDeploymentId: "dep-3", pendingDeploymentId: "dep-3", rollbackTargetId: "dep-1" }),
    pipeline: pipeline({ actions: visibleActions(roleVisibility("developer")), rollouts: [rollout("bff", "Paused")] }),
    visibility: roleVisibility("developer"),
  });
  assert.deepEqual(all(remoteOverviewBar(input)).map((a) => a.value), ["deploy"]);
  const choices = remoteChoices(input);
  assert.equal(choices.versions.length, 3);
  assert.equal(choices.rollouts, undefined);
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

test("roll back is offered only on the page of the run it returns to, aimed at THAT run", () => {
  const instance = remote({ currentDeploymentId: "dep-3", rollbackTargetId: "dep-1" });
  const bar = (run: Run) =>
    runDetailBar({ instance, run, connection: "connected", pipeline: pipeline(), visibility: roleVisibility("owner"), runInFlight: false });
  assert.deepEqual(bar(HISTORY[0]!).acts, [], "the running deployment");
  assert.deepEqual(bar(HISTORY[1]!).acts, [], "a failed one");
  const older = bar(HISTORY[2]!);
  assert.equal(older.acts[0]?.id, "deploy");
  assert.equal(older.acts[0]?.value, "rollback:dep-1");
  assert.equal(older.acts[0]?.label, "Roll back to v0.23.3…");
  assert.equal(older.acts[0]?.tone, "danger");
  // A landed release further back than the one Roll back returns to: its page
  // offers nothing, rather than a second, different rule for the same act.
  assert.deepEqual(bar(remoteRun("dep-0", "succeeded", "v0.23.2", "2026-09-01T10:00:00Z")).acts, []);
  // An admin is not an owner.
  assert.deepEqual(
    runDetailBar({ instance, run: HISTORY[2]!, connection: "connected", pipeline: pipeline(), visibility: roleVisibility("admin"), runInFlight: false }).acts,
    [],
  );
});

// -----------------------------------------------------------------------------
// the untrusted boundary
// -----------------------------------------------------------------------------

test("a posted act runs only when the page built it, target and all", () => {
  const input = remoteInput({ pipeline: pipeline({ rollouts: [rollout("bff", "Paused")] }), upgrade: { ...OFFER, target: { ...OFFER.target, flow: "deployControl" } } as UpgradeVerdict });
  const bar = remoteOverviewBar(input);
  // A bar act is narrowed against the bar, value and all.
  assert.equal(barOffers(bar, "update", "v0.24.0")?.id, "update");
  assert.equal(barOffers(bar, "update", "v0.25.0"), undefined);
  assert.equal(barOffers(bar, "more", undefined), undefined);
  // A deploy control is resolved by key against the controls the page's facts
  // expand to: the one drawn resolves, a rollback aimed elsewhere and a rollout
  // nobody reported do not.
  const facts = remoteControlFacts(input);
  assert.deepEqual(deployControlByKey("rollback:dep-1", facts)?.request, { id: "rollback", toDeploymentId: "dep-1" });
  assert.equal(deployControlByKey("rollback:dep-3", facts), undefined);
  assert.equal(deployControlByKey("rolloutAction:abort:agent", facts), undefined);
  assert.deepEqual(deployControlByKey("rolloutAction:abort:bff", facts)?.request, { id: "rolloutAction", rollout: "bff", subAction: "abort" });
});
