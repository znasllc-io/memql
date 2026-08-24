// The run detail page: what a deployment says, and what it offers (memql#4427).
//
// WHY THIS PAGE HAS A TEST AT ALL. Deployment rows carried no `command`, so
// clicking one did nothing -- and everything this page shows was already being
// written and had nowhere to be read: the per-step outcomes an install records
// after every wave, the per-tier specs a remote rollout declares, the reason a
// run failed. The risk in surfacing recorded data is not that the page is
// blank; it is that the page INVENTS. A duration for a run still in flight, a
// finish time for one that was interrupted, a commit attributed to the wrong
// rebuild -- each reads as a fact about the run when it is a fact about the
// read, and each is a lie the operator has no way to detect.
//
// So the assertions come in two halves: every run kind renders (nothing falls
// through to a blank), and the three defaultable facts are OMITTED rather than
// defaulted.
//
// THE ACTION TABLE is the other half. `runDetailActions` may only PERMUTE what
// `instanceActions` already offered -- that is what keeps this page from
// becoming a second authority on what an instance can do, and it is what makes
// "never a button whose only outcome is a refusal" hold here for free.
//
// Refs: #4427 #4423

import test from "node:test";
import assert from "node:assert/strict";

import { instanceActions, runDetailActions } from "../src/deploy/instanceActions.js";
import type { RoleVisibility } from "../src/deploy/actions.js";
import {
  newLocalRun,
  type Instance,
  type Run,
  type RunKind,
  type RunStatus,
} from "../src/state/deployments.js";
import { renderRunDetail } from "../src/webview/deploymentScreens.js";

const NOW = Date.parse("2026-08-14T12:00:00Z");

function localInstanceOf(over: Partial<Instance> = {}): Instance {
  return {
    name: "local",
    kind: "local",
    presence: "installed-healthy",
    connected: true,
    version: "v0.19.1",
    checkout: "/home/dev/memql",
    ...over,
  };
}

function remoteInstanceOf(over: Partial<Instance> = {}): Instance {
  return {
    name: "staging",
    kind: "remote",
    presence: "installed-healthy",
    connected: true,
    version: "v0.9.2",
    ...over,
  };
}

function runOf(over: Partial<Run> = {}): Run {
  return {
    ...newLocalRun({
      id: "run-1",
      instance: "local",
      kind: "upgrade",
      startedAt: "2026-08-14T10:00:00Z",
    }),
    ...over,
  };
}

function detail(over: {
  instance?: Instance;
  run?: Run;
  outcome?: string;
  error?: string;
} = {}): string {
  const instance = over.instance ?? localInstanceOf();
  const run = over.run ?? runOf();
  return renderRunDetail({
    instance,
    run,
    actions: runDetailActions({ instance, run }),
    nowMs: NOW,
    outcome: over.outcome ?? "",
    error: over.error ?? "",
  });
}

// ---------------------------------------------------------------------------
// every run kind renders
// ---------------------------------------------------------------------------

const KINDS: readonly RunKind[] = [
  "install",
  "upgrade",
  "repair",
  "uninstall",
  "rebuild",
  "rollout",
];

for (const kind of KINDS) {
  test(`a ${kind} run renders its own heading and facts`, () => {
    const instance = kind === "rollout" ? remoteInstanceOf() : localInstanceOf();
    const html = detail({
      instance,
      run: runOf({
        kind,
        instance: instance.name,
        status: "succeeded",
        finishedAt: "2026-08-14T10:04:12Z",
        ...(kind === "rebuild" ? {} : { fromVersion: "v0.19.0", toVersion: "v0.19.1" }),
      }),
    });
    assert.ok(html.includes(`<h1>${kind}</h1>`), `${kind} did not head the page`);
    assert.ok(html.includes(instance.name), `${kind} did not name its cluster`);
    assert.ok(html.includes("succeeded"), `${kind} did not print its status`);
    assert.ok(html.includes("4m 12s"), `${kind} did not print its duration`);
  });
}

const STATUSES: readonly RunStatus[] = [
  "running",
  "succeeded",
  "failed",
  "cancelled",
  "interrupted",
  "superseded",
  "rolled_back",
];

for (const status of STATUSES) {
  test(`a ${status} run renders rather than falling through`, () => {
    const html = detail({ run: runOf({ status }) });
    assert.ok(html.includes(status), `${status} did not appear on the page`);
    assert.ok(html.includes("<h1>"), `${status} produced no page at all`);
  });
}

// ---------------------------------------------------------------------------
// what it refuses to invent
// ---------------------------------------------------------------------------

test("a run still in flight is given no duration and no finish", () => {
  // Both would be manufactured. The run has not ended, so there is no elapsed
  // time and no finishing instant -- and "0s" reads as a run that did nothing.
  const html = detail({ run: runOf({ status: "running" }) });
  assert.ok(!html.includes("duration"), "a running deploy was given a duration");
  assert.ok(!html.includes("finished"), "a running deploy was given a finish time");
});

test("an interrupted run keeps its start and admits no finish", () => {
  // memql#3886's state: the extension host went away mid-run, so the record
  // says `running` for ever and nothing is left to write a finish to it. The
  // page must not supply one.
  const html = detail({ run: runOf({ status: "interrupted" }) });
  assert.ok(html.includes("2026-08-14T10:00:00Z"), "the start went missing");
  assert.ok(!html.includes(">duration<"), "an interrupted run was given a duration");
});

test("a run that recorded no versions prints no transition", () => {
  // An install has nothing to come from; rendering `unknown -> v0.19.1` would
  // invent a predecessor.
  const html = detail({ run: runOf({ kind: "install", status: "succeeded" }) });
  assert.ok(!html.includes("versions"), "a run with no versions grew a version fact");
});

test("a rebuild says it built from the checkout rather than naming a release", () => {
  // A rebuild is recorded with neither fromVersion nor toVersion, because it
  // does not move the cluster between releases -- it moves it between LANES.
  // The wording comes from state/imageLane.ts so it cannot drift from the four
  // surfaces that warn about the crossing.
  const html = detail({
    instance: localInstanceOf({
      imageSource: "checkout",
      rebuild: {
        commit: "abc1234def",
        ref: "main",
        dirtyCount: 4,
        nodes: "",
        recordedAt: "2026-08-14T10:00:00Z",
      },
    }),
    run: runOf({ kind: "rebuild", status: "succeeded", finishedAt: "2026-08-14T10:20:00Z" }),
  });
  assert.ok(html.includes("built from the checkout"));
  assert.ok(html.includes("checkout abc1234 (4 uncommitted)"), "the lane fact went missing");
});

test("a rebuild whose cluster has since returned to released images says so", () => {
  // The commit lives on the INSTANCE and describes the LAST rebuild, which is
  // this one only if no other has happened since. So the page never folds a
  // commit into the sentence about what this run did, and states the cluster's
  // current lane separately.
  const html = detail({
    instance: localInstanceOf({ imageSource: "released", version: "v0.19.1" }),
    run: runOf({ kind: "rebuild", status: "succeeded", finishedAt: "2026-08-14T10:20:00Z" }),
  });
  assert.ok(html.includes("released v0.19.1 images today"));
  assert.ok(!html.includes("checkout abc"), "a commit was attributed to a cluster not running one");
});

// ---------------------------------------------------------------------------
// the reason, read off the items
// ---------------------------------------------------------------------------

test("a failed run names the step that failed and what it said", () => {
  // `Run` has no `reason` field, and adding one would be a second place for
  // something already recorded. Reading it off the failed item is what makes
  // the sentence true by construction.
  const html = detail({
    run: runOf({
      status: "failed",
      finishedAt: "2026-08-14T10:01:00Z",
      items: [
        { label: "clusterUp", status: "ok" },
        { label: "stackCheckout", status: "failed", detail: "tag v9.9.9 does not exist" },
      ],
    }),
  });
  assert.ok(html.includes("stackCheckout: tag v9.9.9 does not exist"));
});

test("a failed run with no failed item invents no culprit", () => {
  // An abort writes the status directly, so the items can all be fine. Naming
  // one anyway would blame a step that did its job.
  const html = detail({
    run: runOf({ status: "failed", items: [{ label: "clusterUp", status: "ok" }] }),
  });
  assert.ok(!html.includes(">reason<"), "a failed run with no failed step named one");
});

test("a failed wave names every step that failed", () => {
  const html = detail({
    run: runOf({
      status: "failed",
      items: [
        { label: "hostsBlock", status: "failed", detail: "permission denied" },
        { label: "mkcertCA", status: "failed", detail: "mkcert not on PATH" },
      ],
    }),
  });
  assert.ok(html.includes("hostsBlock: permission denied"));
  assert.ok(html.includes("mkcertCA: mkcert not on PATH"));
});

// ---------------------------------------------------------------------------
// items: steps versus node types, and the no-log case
// ---------------------------------------------------------------------------

test("a local run's items are STEPS and a remote run's are NODE TYPES", () => {
  // state/deployments.ts property 2: a local run's items are capability-script
  // executions, a remote run's are `deploymentNodeSpec` DECLARATIONS. One word
  // for both is where the asymmetry would finally get hidden.
  const local = detail({
    run: runOf({ items: [{ label: "clusterUp", status: "ok" }] }),
  });
  assert.ok(local.includes("<h2>Steps</h2>"));
  assert.ok(!local.includes("Node types"));

  const remote = detail({
    instance: remoteInstanceOf(),
    run: runOf({ kind: "rollout", instance: "staging", items: [{ label: "bff", status: "ok" }] }),
  });
  assert.ok(remote.includes("<h2>Node types</h2>"));
  assert.ok(!remote.includes("<h2>Steps</h2>"));
});

test("the no-log case says WHICH no-log case it is", () => {
  // A remote run whose specs were not read and a local run that never recorded
  // a step look identical as an empty list and are completely different facts.
  const local = detail({ run: runOf({ items: [] }) });
  assert.ok(local.includes("This run recorded no steps."));

  const remote = detail({
    instance: remoteInstanceOf(),
    run: runOf({ kind: "rollout", instance: "staging", items: [] }),
  });
  assert.ok(remote.includes("No per-tier specs were read for this deployment."));
});

// ---------------------------------------------------------------------------
// the outcome line
// ---------------------------------------------------------------------------

test("an action's outcome is reported on the page the action was taken from", () => {
  // The remote action buttons exist on this page now, and `runDeploy` reports
  // through one field. Without drawing it, pressing Deploy here would show the
  // operator nothing -- indistinguishable from an action that never ran.
  const html = detail({ instance: remoteInstanceOf(), outcome: "deploy accepted (audit 4c1f)" });
  assert.ok(html.includes("deploy accepted (audit 4c1f)"));
  const failed = detail({ instance: remoteInstanceOf(), outcome: "ERROR: refused, owner required" });
  assert.ok(failed.includes('class="error"'), "an engine refusal was drawn as a notice");
});

// ---------------------------------------------------------------------------
// the action table: (lane, outcome, role)
// ---------------------------------------------------------------------------

function ids(actions: readonly { id: string }[]): string[] {
  return actions.map((a) => a.id);
}

test("a failed local run leads with Repair", () => {
  const instance = localInstanceOf();
  const actions = runDetailActions({ instance, run: runOf({ status: "failed" }) });
  assert.equal(ids(actions)[0], "repair");
});

test("a checkout-mode cluster leads with Rebuild From Checkout", () => {
  const instance = localInstanceOf({
    imageSource: "checkout",
    rebuild: { commit: "abc1234", ref: "main", nodes: "", recordedAt: "2026-08-14T10:00:00Z" },
  });
  const actions = runDetailActions({ instance, run: runOf({ status: "succeeded" }) });
  assert.equal(ids(actions)[0], "rebuildFromCheckout");
});

test("failure outranks the lane", () => {
  // The lane is a standing fact about the cluster and will still be true
  // tomorrow; the failure is a fact about THIS run, which is what the page is
  // about.
  const instance = localInstanceOf({
    imageSource: "checkout",
    rebuild: { commit: "abc1234", ref: "main", nodes: "", recordedAt: "2026-08-14T10:00:00Z" },
  });
  const actions = runDetailActions({ instance, run: runOf({ status: "failed" }) });
  assert.equal(ids(actions)[0], "repair");
});

test("a healthy released-lane cluster keeps the instance's own order", () => {
  const instance = localInstanceOf();
  const run = runOf({ status: "succeeded" });
  assert.deepEqual(ids(runDetailActions({ instance, run })), ids(instanceActions(instance)));
});

test("reordering never adds or removes a verb", () => {
  // The constraint the whole design rests on: this page may PERMUTE the
  // instance's set and nothing else, so it cannot become a second authority on
  // what an instance offers.
  for (const instance of [
    localInstanceOf(),
    localInstanceOf({ presence: "absent", version: undefined, checkout: undefined }),
    localInstanceOf({ presence: "installed-unreachable" }),
    localInstanceOf({ checkout: undefined }),
    localInstanceOf({
      imageSource: "checkout",
      rebuild: { commit: "abc1234", ref: "main", nodes: "", recordedAt: "2026-08-14T10:00:00Z" },
    }),
  ]) {
    for (const status of STATUSES) {
      assert.deepEqual(
        ids(runDetailActions({ instance, run: runOf({ status }) })).slice().sort(),
        ids(instanceActions(instance)).slice().sort(),
        `${instance.presence}/${String(instance.imageSource)}/${status} changed the verb set`
      );
    }
  }
});

test("a machine with nothing installed is offered only the install, whatever the run did", () => {
  // The lead is not applied when the instance does not offer it, so no button
  // appears whose only outcome is a refusal.
  const instance = localInstanceOf({ presence: "absent", version: undefined, checkout: undefined });
  assert.deepEqual(
    ids(runDetailActions({ instance, run: runOf({ status: "failed" }) })),
    ["createDeployment"]
  );
});

test("a machine with no recorded checkout is offered no rebuild", () => {
  // `checkout` is "" for a cluster registered by hand and for an install that
  // never reached the clone step -- instanceActions' own gate, unchanged here.
  const instance = localInstanceOf({ checkout: undefined, imageSource: "checkout" });
  assert.ok(!ids(runDetailActions({ instance, run: runOf() })).includes("rebuildFromCheckout"));
});

const OWNER: RoleVisibility = { kind: "resolved", role: "owner" };
const DEVELOPER: RoleVisibility = { kind: "resolved", role: "developer" };
const READER: RoleVisibility = { kind: "resolved", role: "reader" };

test("a remote rollout surfaces the caller's deploy-control set, and rollback is owner-only", () => {
  const instance = remoteInstanceOf();
  const run = runOf({ kind: "rollout", instance: "staging", status: "succeeded" });
  const offers = ["deploy", "cutVersion", "rolloutAction", "rollback"] as const;

  const owner = ids(runDetailActions({ instance, run, visibility: OWNER, pipelineOffers: offers }));
  assert.ok(owner.includes("rollback"), "an owner was not offered rollback");

  const developer = ids(
    runDetailActions({ instance, run, visibility: DEVELOPER, pipelineOffers: offers })
  );
  assert.ok(!developer.includes("rollback"), "a developer was offered rollback");
  assert.ok(developer.includes("cutVersion"), "a developer was not offered cut version");

  const reader = ids(runDetailActions({ instance, run, visibility: READER, pipelineOffers: offers }));
  assert.deepEqual(reader, [], "a reader was offered a deploy-control action");
});

test("a remote run is never reordered by its outcome", () => {
  // The lead rules are local-only. A remote instance's set arrives already
  // filtered by role and already in deploy/actions.ts's order, and reordering
  // it here would be this page overruling that catalog.
  const instance = remoteInstanceOf();
  const offers = ["deploy", "cutVersion", "rolloutAction", "rollback"] as const;
  const succeeded = ids(
    runDetailActions({
      instance,
      run: runOf({ kind: "rollout", status: "succeeded" }),
      visibility: OWNER,
      pipelineOffers: offers,
    })
  );
  const failed = ids(
    runDetailActions({
      instance,
      run: runOf({ kind: "rollout", status: "failed" }),
      visibility: OWNER,
      pipelineOffers: offers,
    })
  );
  assert.deepEqual(failed, succeeded);
});

test("a cluster with no deploy pipeline is offered nothing, whatever the role", () => {
  // pipelineState's `notConfigured`: an engine-only cluster refuses every
  // deploy-control action by design, so the role-permitted set drawn over one
  // would be a row of buttons whose only outcome is a refusal.
  const actions = runDetailActions({
    instance: remoteInstanceOf(),
    run: runOf({ kind: "rollout", status: "succeeded" }),
    visibility: OWNER,
    pipelineOffers: [],
  });
  assert.deepEqual(ids(actions), []);
});

test("a pipeline that has not answered yet leaves the role-gated set alone", () => {
  // "Not asked" is not "nothing offered". The status read is asynchronous and
  // the page paints before it lands; blanking the actions for that instant
  // would read as a cluster that lost its deploy console.
  const actions = runDetailActions({
    instance: remoteInstanceOf(),
    run: runOf({ kind: "rollout", status: "succeeded" }),
    visibility: OWNER,
  });
  assert.ok(ids(actions).length > 0);
});

test("a remote action is wired to the deploy route and a local one is not", () => {
  // Two routes, one set. The panel narrows `data-choose` against
  // `instanceActions` and runs `data-deploy` through the deploy controller;
  // posting a deploy-control id down the local route would land in a branch
  // that opens the tag picker for a remote cluster.
  const remote = detail({
    instance: remoteInstanceOf(),
    run: runOf({ kind: "rollout", instance: "staging", status: "succeeded" }),
  });
  assert.ok(remote.includes("data-deploy="), "a remote action was not wired to the deploy route");
  assert.ok(!remote.includes("data-choose="), "a remote action was wired to the local route");

  const local = detail({ run: runOf({ status: "succeeded" }) });
  assert.ok(local.includes("data-choose="));
  assert.ok(!local.includes("data-deploy="));
});

test("every value on the page is escaped", () => {
  // The webview CSP forbids inline handlers, so the remaining exposure is an
  // unescaped value closing an attribute. A cluster name comes from
  // clusters.yaml, which a human edits.
  const html = detail({
    instance: localInstanceOf({ name: '"><script>alert(1)</script>' }),
    run: runOf({ status: "failed", items: [{ label: "<img src=x>", status: "failed" }] }),
  });
  assert.ok(!html.includes("<script>alert(1)</script>"));
  assert.ok(!html.includes("<img src=x>"));
});
