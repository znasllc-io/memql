// The deploy-control buttons, each resolved to the request it sends.
//
// Three of the panel's four deploy-control actions filled their argument in at
// the click with a constant, and each constant was wrong:
//
//   - Rollout promote / abort sent `promote` with an EMPTY rollout name. The
//     SDK refuses that before sending (`rolloutAction: rollout is required`),
//     so the button could not succeed, and abort was never offered at all.
//   - Cut version sent `bump: "patch"`, so a minor or a major could not be cut
//     from the panel, and `previewNextVersion` had no caller.
//   - Roll back sent the newest `succeeded` deployment -- usually the one the
//     cluster is running (rollbackTarget.test.ts pins the rule that replaced it).
//
// These cases pin deploy/controls.ts, which now decides every argument from
// facts the page read. The panel's click path is driven end to end in
// deploymentPanelActions.test.ts.

import test from "node:test";
import assert from "node:assert/strict";

import type { NextVersionSuggestion, RolloutStatus } from "@znasllc-io/memql-sdk-core/deploy";

import { DEPLOY_ACTIONS } from "../src/deploy/actions.js";
import { runDeployAction } from "../src/deploy/controller.js";
import {
  actionOfKey,
  deployControlByKey,
  deployControls,
  rolloutsInFlight,
  type DeployControl,
  type DeployControlFacts,
} from "../src/deploy/controls.js";
import type { Run } from "../src/state/deployments.js";
import { DeployDispatcher } from "./support/deployDispatcher.js";

const ALL = DEPLOY_ACTIONS.map((action) => action.id);

function rollout(name: string, phase: string): RolloutStatus {
  return {
    name,
    kind: "bluegreen",
    phase,
    activeColor: "",
    previewColor: "",
    canaryWeight: 0,
    currentStep: -1,
    latestAnalysisResult: "",
  };
}

const SUGGESTION: NextVersionSuggestion = {
  currentVersion: "1.4.2",
  nextPatch: "1.4.3",
  nextMinor: "1.5.0",
  nextMajor: "2.0.0",
  source: "deployment",
};

const RUNS: Run[] = [
  { id: "d2", instance: "staging", kind: "rollout", startedAt: "2026-09-10T00:00:00Z", status: "succeeded", items: [], toVersion: "1.4.2" },
  { id: "d1", instance: "staging", kind: "rollout", startedAt: "2026-09-01T00:00:00Z", status: "succeeded", items: [], toVersion: "1.4.1" },
];

function facts(over: Partial<DeployControlFacts> = {}): DeployControlFacts {
  return {
    instance: { pendingDeploymentId: "d3", rollbackTargetId: "d1" },
    runs: RUNS,
    rollouts: [rollout("memql-bff", "Paused"), rollout("memql-agent", "Progressing"), rollout("memql-edge", "Healthy")],
    preview: { suggestion: SUGGESTION, message: "" },
    ...over,
  };
}

function only(controls: DeployControl[], key: string): DeployControl {
  const found = controls.filter((control) => control.key === key);
  assert.equal(found.length, 1, `expected exactly one control keyed ${key}, got ${controls.map((c) => c.key).join(", ")}`);
  return found[0];
}

// -----------------------------------------------------------------------------
// every request reaches the wire
// -----------------------------------------------------------------------------

test("every control that carries a request passes the SDK's own argument checks", async () => {
  // THE DEFECT, stated as the operator met it: the SDK refused the rollout
  // request before anything was sent. This drives each request through a REAL
  // DeployControlClient, so an empty argument anywhere fails here the way it
  // failed in the panel -- as an ERROR line with nothing on the wire.
  const dispatcher = new DeployDispatcher();
  const client = dispatcher.client();
  const controls = deployControls(ALL, facts()).filter((control) => control.request !== undefined);
  assert.ok(controls.length > 0);
  for (const control of controls) {
    const before = dispatcher.sent.length;
    const outcome = await runDeployAction(client, control.request!);
    assert.equal(outcome.kind, "success", `${control.key}: ${outcome.line}`);
    assert.equal(dispatcher.sent.length, before + 1, `${control.key} sent nothing`);
  }
});

test("a destructive control never carries a request without a phrase to type back", () => {
  // An action with nothing identifiable to re-type must not proceed
  // unchallenged (confirmationPhrase). Resolved here rather than at the click,
  // so it is a property of every control rather than of one code path.
  for (const control of deployControls(ALL, facts())) {
    if (control.destructive && control.request !== undefined) {
      assert.notEqual(control.confirm, "", `${control.key} is destructive with nothing to confirm`);
    }
  }
});

test("every key is unique, and resolves back to its own control", () => {
  const controls = deployControls(ALL, facts());
  const keys = controls.map((control) => control.key);
  assert.equal(new Set(keys).size, keys.length, `duplicate keys: ${keys.join(", ")}`);
  for (const control of controls) {
    assert.deepEqual(deployControlByKey(control.key, facts()), control);
    assert.equal(actionOfKey(control.key), control.action);
  }
});

// -----------------------------------------------------------------------------
// Rollout promote / abort
// -----------------------------------------------------------------------------

test("each rollout in flight gets a promote and an abort, naming it", () => {
  const controls = deployControls(["rolloutAction"], facts());
  assert.deepEqual(
    controls.map((control) => control.key),
    [
      "rolloutAction:promote:memql-bff",
      "rolloutAction:abort:memql-bff",
      "rolloutAction:promote:memql-agent",
      "rolloutAction:abort:memql-agent",
    ],
  );
  const promote = only(controls, "rolloutAction:promote:memql-bff");
  assert.deepEqual(promote.request, { id: "rolloutAction", rollout: "memql-bff", subAction: "promote" });
  assert.equal(promote.label, "Promote memql-bff");
  const abort = only(controls, "rolloutAction:abort:memql-bff");
  assert.deepEqual(abort.request, { id: "rolloutAction", rollout: "memql-bff", subAction: "abort" });
  assert.equal(abort.label, "Abort memql-bff");
});

test("abort is confirmed against the ROLLOUT's name, and promote is not confirmed", () => {
  // The phrase used to be the cluster's name, which says nothing about which
  // rollout is torn down. Promote advances a rollout already going where it
  // was told; confirming it would train the operator to type through prompts.
  const controls = deployControls(["rolloutAction"], facts());
  const abort = only(controls, "rolloutAction:abort:memql-agent");
  assert.equal(abort.confirm, "memql-agent");
  assert.equal(abort.destructive, true);
  const promote = only(controls, "rolloutAction:promote:memql-agent");
  assert.equal(promote.confirm, "");
  assert.equal(promote.destructive, false);
});

test("only Paused and Progressing rollouts are in flight", () => {
  const inFlight = rolloutsInFlight([
    rollout("paused", "Paused"),
    rollout("progressing", "Progressing"),
    rollout("healthy", "Healthy"),
    rollout("degraded", "Degraded"),
    rollout("unknown", ""),
    rollout("", "Paused"),
  ]);
  assert.deepEqual(
    inFlight.map((r) => r.name),
    ["paused", "progressing"],
  );
});

test("with no rollout in flight the action draws no button at all", () => {
  // The alternative is the button this replaced: one that can only send an
  // empty name. The page says why it is absent (deploymentPanelActions).
  assert.deepEqual(deployControls(["rolloutAction"], facts({ rollouts: [rollout("memql-bff", "Healthy")] })), []);
  assert.deepEqual(deployControls(["rolloutAction"], facts({ rollouts: [] })), []);
});

// -----------------------------------------------------------------------------
// Cut version
// -----------------------------------------------------------------------------

test("Cut offers every bump, and each names and sends the version the preview gave", () => {
  const controls = deployControls(["cutVersion"], facts());
  assert.deepEqual(
    controls.map((control) => [control.key, control.label]),
    [
      ["cutVersion:patch:1.4.3", "Cut 1.4.3 (patch)"],
      ["cutVersion:minor:1.5.0", "Cut 1.5.0 (minor)"],
      ["cutVersion:major:2.0.0", "Cut 2.0.0 (major)"],
    ],
  );
  // The VERSION is sent, not only the bump: the button promised 1.5.0, and a
  // bump alone would be recomputed by the engine from whatever is current then.
  assert.deepEqual(only(controls, "cutVersion:minor:1.5.0").request, {
    id: "cutVersion",
    bump: "minor",
    version: "1.5.0",
  });
});

test("without a preview, Cut is not blocked -- it names the bump and the engine computes", () => {
  for (const preview of [undefined, { suggestion: null, message: "suggestion unavailable (UNAVAILABLE)" }]) {
    const controls = deployControls(["cutVersion"], facts({ preview }));
    assert.deepEqual(
      controls.map((control) => [control.key, control.label]),
      [
        ["cutVersion:patch", "Cut next patch"],
        ["cutVersion:minor", "Cut next minor"],
        ["cutVersion:major", "Cut next major"],
      ],
    );
    assert.deepEqual(only(controls, "cutVersion:major").request, { id: "cutVersion", bump: "major", version: "" });
  }
});

test("a preview with no proposals falls back to the bump alone", () => {
  // The engine answers a current version it cannot parse with the version and
  // NO proposals (component/deploycontrol/cutversion.go), rather than failing
  // the read.
  const controls = deployControls(
    ["cutVersion"],
    facts({
      preview: {
        suggestion: { currentVersion: "not-semver", nextPatch: "", nextMinor: "", nextMajor: "", source: "deployment" },
        message: "",
      },
    }),
  );
  assert.deepEqual(
    controls.map((control) => control.key),
    ["cutVersion:patch", "cutVersion:minor", "cutVersion:major"],
  );
});

test("a cut key whose version has moved no longer resolves", () => {
  // Someone else cut 1.5.0 and the preview now proposes 1.6.0. The button that
  // said 1.5.0 must not cut 1.6.0 under its old label.
  const moved = facts({
    preview: { suggestion: { ...SUGGESTION, currentVersion: "1.5.0", nextMinor: "1.6.0" }, message: "" },
  });
  assert.equal(deployControlByKey("cutVersion:minor:1.5.0", moved), undefined);
  assert.equal(actionOfKey("cutVersion:minor:1.5.0"), "cutVersion");
});

// -----------------------------------------------------------------------------
// Roll back and Deploy
// -----------------------------------------------------------------------------

test("Roll back names its target, carries its version, and confirms against its id", () => {
  const control = only(deployControls(["rollback"], facts()), "rollback:d1");
  assert.deepEqual(control.request, { id: "rollback", toDeploymentId: "d1" });
  assert.equal(control.label, "Roll back to 1.4.1");
  assert.equal(control.confirm, "d1");
  assert.equal(control.destructive, true);
});

test("with nothing to roll back to, Roll back carries a refusal and no request", () => {
  const control = only(deployControls(["rollback"], facts({ instance: { pendingDeploymentId: "d3" } })), "rollback");
  assert.equal(control.request, undefined);
  assert.match(control.refusal ?? "", /nothing to roll back to/);
});

test("Deploy ships the pending record, or refuses in the sentence it always has", () => {
  assert.deepEqual(only(deployControls(["deploy"], facts()), "deploy").request, {
    id: "deploy",
    deploymentId: "d3",
  });
  const none = only(deployControls(["deploy"], facts({ instance: {} })), "deploy");
  assert.equal(none.request, undefined);
  assert.equal(none.refusal, "nothing is cut, so there is no pending deployment record to ship.");
});

test("a key the page never built resolves to nothing", () => {
  // What the old button posted, and what a forged message might: the action
  // with no target, or a target no read produced.
  for (const key of ["rolloutAction", "rolloutAction:promote:", "rolloutAction:promote:memql-edge", "rollback:d2", "cutVersion"]) {
    assert.equal(deployControlByKey(key, facts()), undefined, key);
  }
  assert.equal(actionOfKey("uninstall"), undefined);
  assert.equal(actionOfKey("deployment"), undefined, "a prefix of an id is not the id");
});
