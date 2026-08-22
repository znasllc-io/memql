// What a Deployments instance offers (memql#3736).
//
// The two assertions to read first: INSTALL IS OFFERED FOR `absent` AND FOR
// NOTHING ELSE -- an install over a cluster that already exists rebuilds a k3d
// cluster, a hosts block and a trust-store CA underneath a working parity stack
// -- and REMOTE TIERS MIRROR THE ENGINE'S rather than restating them, so the
// courtesy cannot drift into a second, wrong authority.

import test from "node:test";
import assert from "node:assert/strict";

import { actionById, roleVisibility } from "../src/deploy/actions.js";
import { instanceActions, offersAction } from "../src/deploy/instanceActions.js";
import { localInstance, remoteInstance, type Instance } from "../src/state/deployments.js";

function local(presence: Instance["presence"]): Instance {
  return localInstance({ presence, receipt: null, connected: presence === "installed-healthy" });
}

function remote(): Instance {
  return remoteInstance({ name: "staging", reachable: true, connected: true });
}

// -----------------------------------------------------------------------------
// local
// -----------------------------------------------------------------------------

test("a machine with no cluster is offered exactly one thing", () => {
  const actions = instanceActions(local("absent"));
  assert.deepEqual(actions.map((a) => a.id), ["createDeployment"]);
  assert.equal(actions[0].flow, "installGraph");
});

test("install is never offered over a cluster that already exists", () => {
  for (const presence of ["installed-healthy", "installed-unreachable"] as const) {
    const actions = instanceActions(local(presence));
    assert.equal(
      actions.some((a) => a.flow === "installGraph"),
      false,
      presence,
    );
  }
});

test("an installed local cluster offers create, repair and uninstall", () => {
  const actions = instanceActions(local("installed-healthy"));
  assert.deepEqual(actions.map((a) => a.id), ["createDeployment", "repair", "uninstall"]);
  // Create deployment on an installed machine MOVES the pinned checkout; it
  // does not re-run the install graph.
  assert.equal(actions[0].flow, "upgradeToTag");
  assert.equal(actions[1].flow, "repairGraph");
  assert.equal(actions[2].flow, "uninstallGraph");
});

test("an unreachable local cluster leads with repair", () => {
  const actions = instanceActions(local("installed-unreachable"));
  // Order is the only recommendation a list can make, and an operator looking
  // at a cluster that will not answer came to fix it.
  assert.equal(actions[0].id, "repair");
  assert.deepEqual(actions.map((a) => a.id).sort(), ["createDeployment", "repair", "uninstall"]);
});

test("uninstall is offered only where there is something to remove", () => {
  assert.equal(offersAction(local("absent"), "uninstall"), false);
  assert.equal(offersAction(local("installed-healthy"), "uninstall"), true);
  assert.equal(offersAction(local("installed-unreachable"), "uninstall"), true);
});

// -----------------------------------------------------------------------------
// rebuild from checkout (memql#4246)
// -----------------------------------------------------------------------------

/** An installed local cluster whose install recorded where it cloned the code. */
function withCheckout(presence: Instance["presence"]): Instance {
  return { ...local(presence), checkout: "/home/me/.memql/src" };
}

test("an installed cluster with a checkout is offered a rebuild, in both orders", () => {
  // It sits after Repair rather than beside Create deployment: an operator on
  // this row is choosing between "move to a release" and "run my own code", and
  // the second is the specialised one.
  assert.deepEqual(
    instanceActions(withCheckout("installed-healthy")).map((a) => a.id),
    ["createDeployment", "repair", "rebuildFromCheckout", "uninstall"],
  );
  // The unreachable ordering still leads with repair, and the rebuild keeps its
  // place relative to the other three.
  assert.deepEqual(
    instanceActions(withCheckout("installed-unreachable")).map((a) => a.id),
    ["repair", "createDeployment", "rebuildFromCheckout", "uninstall"],
  );
});

test("a rebuild reaches the rebuild graph and nothing else", () => {
  const rebuild = instanceActions(withCheckout("installed-healthy")).find(
    (a) => a.id === "rebuildFromCheckout",
  );
  // Named for the machinery, like every other flow: a rebuild that routed into
  // `installGraph` would reinstall a working machine.
  assert.equal(rebuild?.flow, "rebuildGraph");
  assert.equal(rebuild?.tier, undefined);
});

test("no recorded checkout means no rebuild -- there is nothing to build from", () => {
  // `local()` builds from a null receipt, so `checkout` is absent: a machine
  // registered by hand, or an install that never reached the clone step. An
  // offered button whose only outcome is a refusal teaches the wrong thing.
  assert.equal(offersAction(local("installed-healthy"), "rebuildFromCheckout"), false);
  assert.equal(offersAction(local("installed-unreachable"), "rebuildFromCheckout"), false);
  // And never where nothing is installed, for the reason install is never
  // offered over a cluster that exists: there is no cluster to roll onto them.
  assert.equal(offersAction({ ...local("absent"), checkout: "/src" }, "rebuildFromCheckout"), false);
});

test("no local action carries a cluster role", () => {
  for (const presence of ["absent", "installed-healthy", "installed-unreachable"] as const) {
    for (const action of instanceActions(local(presence))) {
      // Nothing here asks a cluster for permission to change the machine it is
      // running on.
      assert.equal(action.tier, undefined, `${presence}/${action.id}`);
    }
  }
});

test("repair and uninstall are local only", () => {
  const owner = roleVisibility("owner");
  assert.equal(offersAction(remote(), "repair", owner), false);
  assert.equal(offersAction(remote(), "uninstall", owner), false);
});

// -----------------------------------------------------------------------------
// remote
// -----------------------------------------------------------------------------

test("a remote instance's tiers are the deploy catalog's, not a second copy", () => {
  const actions = instanceActions(remote(), roleVisibility("owner"));
  assert.equal(actions.find((a) => a.id === "createDeployment")?.tier, actionById("deploy").tier);
  assert.equal(actions.find((a) => a.id === "cutVersion")?.tier, actionById("cutVersion").tier);
  assert.equal(actions.find((a) => a.id === "rolloutAction")?.tier, actionById("rolloutAction").tier);
  assert.equal(actions.find((a) => a.id === "rolloutAction")?.tier, actionById("rolloutAction").tier);
  assert.equal(actions.find((a) => a.id === "rollback")?.tier, actionById("rollback").tier);
});

test("every remote action names the deploy-control action it drives", () => {
  for (const action of instanceActions(remote(), roleVisibility("owner"))) {
    assert.equal(action.flow, "deployControl", action.id);
    assert.notEqual(action.deployAction, undefined, action.id);
  }
});

test("a developer gets deploy and cut, and nothing admin or owner holds", () => {
  const actions = instanceActions(remote(), roleVisibility("developer"));
  assert.deepEqual(actions.map((a) => a.id).sort(), ["createDeployment", "cutVersion"]);
});

test("an admin gets rollout but not rollback", () => {
  const ids = instanceActions(remote(), roleVisibility("admin")).map((a) => a.id);
  assert.equal(ids.includes("rolloutAction"), true);
  // Rollback is owner-only in the service, and this table mirrors it exactly.
  assert.equal(ids.includes("rollback"), false);
});

test("an owner gets everything", () => {
  assert.equal(instanceActions(remote(), roleVisibility("owner")).length, 4);
});

test("a writer gets nothing", () => {
  assert.deepEqual(instanceActions(remote(), roleVisibility("writer")), []);
});

test("a role that could not be read is offered everything, with the engine deciding", () => {
  // The caller may well be an owner. Hiding the surface would lock them out of
  // something they are entitled to, while the engine refuses anything they are
  // not and names the role required.
  assert.equal(instanceActions(remote(), roleVisibility(undefined)).length, 4);
  assert.equal(instanceActions(remote()).length, 4);
});
